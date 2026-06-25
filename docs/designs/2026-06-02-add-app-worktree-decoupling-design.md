# Decouple `flywheel add-app` from the worktree directory

**Status:** approved (design)
**Date:** 2026-06-02

## Problem

`flywheel add-app <name>` forces the app **name** to equal the host **directory**
name. The single `<name>` argument drives two unrelated concerns:

* **Logical identity** — the builder folder (`builders/base/<name>/`), the app
  folder (`apps/base/<name>/`), every Kubernetes resource name (GitRepository,
  build-config ConfigMap, git-auto-sync Deployment, ImageRepository/Policy, app
  Deployment/Service/Ingress), the Ingress host (`<name>.<domain>`), and the
  image name.
* **Physical location** — git-auto-sync mounts `WORKTREE=/workspaces/<name>` and
  pushes to `BARE_REPO_URL=…/<name>.git`; the in-cluster git-server creates that
  bare repo by scanning `/workspaces/*` and keying on the **directory basename**.
  So the host repo directory (under `paths.workspaces_root`, bind-mounted at
  `/workspaces`) **must** be named exactly `<name>`.

You can't deploy an app called `frontend` from a repo dir named `sample-app`
without renaming the repo. There is also no validation: point `add-app` at a
name with no matching worktree and the builds just silently never happen.

### Scope decided in brainstorming

App repos continue to live **under `workspaces_root`** (the single bind-mounted
root) — we are *not* supporting arbitrary absolute paths, which would require
per-app k3d mounts and a cluster recreate. We only decouple the logical name
from the directory. Additionally we improve the ergonomics: the **directory**
becomes the primary argument, the name is **derived** (or `--name`-overridden),
and the directory argument gets **shell autocompletion** — which motivates a
migration of the CLI to cobra.

## Approach

Introduce one new concept — the **worktree directory** — that carries the
physical bindings, leaving the app **name** for everything logical. Delivered in
two independently-reviewable phases:

1. **Phase 1 — behavior-preserving cobra migration.** Replace the hand-rolled
   subcommand `switch` + `flag.FlagSet`s with a cobra command tree. No UX change
   except that `flywheel completion <shell>` becomes available. cobra (v1.10.2)
   and pflag are already in the module graph, so no new heavyweight dependency.
2. **Phase 2 — the new `add-app` + name derivation + completion**, built on
   Phase 1.

## Phase 1 — Cobra migration

* **Root command** `flywheel` owns the two globals as **persistent flags**:
  `--no-color` and `-v/--verbose`. The current manual pre-parse interception is
  removed; `PersistentPreRun` calls `style.Init`, `style.SetVerbose`, and
  `silenceKlog`, preserving today's behavior exactly.
* **One subcommand per existing case** (`doctor`, `init`, `up`, `down`,
  `destroy`, `clean`, `add-app`, `version`), each wrapping the existing `run*`
  bodies. The logic in `internal/cli/*` is untouched; flags move from `FlagSet`
  to cobra flags 1:1.
* **Stubs preserved**: `update`, `snapshot`, `allocator gc` keep printing
  "not implemented in v0.1.0-alpha" with the same exit codes.
* **`new` removed entirely** — the retired-command stub is dropped; `flywheel
  new` falls through to cobra's standard "unknown command" error. (The
  `internal/cli/initcmd` *package* stays — `init` uses it internally.)
* **Exit codes** preserved: command error → `1`, usage error → `2`, via cobra's
  error return plus a top-level handler.

## Phase 2 — `add-app` redesign

### CLI shape

```
flywheel add-app <dir> [--name <name>] [--image <img>] [--context <path>] [--dockerfile <path>]
```

* **`<dir>`** is the host worktree directory under `workspaces_root`. It accepts
  a bare name, a relative path, or an absolute path; flywheel resolves it and
  uses its **basename**.
* **`<dir>` drives the physical bindings**: `WORKTREE=/workspaces/<dir>`, the
  bare-repo URL `…/<dir>.git`, and `GitRepository.spec.url`.
* **The app name drives everything logical** — folder names, all resource names,
  the GitRepository *name*, the Ingress host, and the image name. The name is
  `--name` if given, else derived (below).
* **Validation:** `<dir>` must resolve to a directory that exists **and** is a
  direct child of `workspaces_root` (read from `flywheel.yaml` + `.local`). A
  typo or out-of-root path errors immediately — closing the silent-failure gap.

### Name derivation

When `--name` is omitted, derive from `<dir>`:

1. **Scan for recognized manifests and extract a raw name:**

   | Manifest | Field | Notes |
   |---|---|---|
   | `package.json` | `.name` | strip npm scope: `@acme/web` → `web` |
   | `pyproject.toml` | `[project].name` ‖ `[tool.poetry].name` | |
   | `setup.cfg` | `[metadata].name` | |
   | `go.mod` | `module <path>` | basename of the module path |
   | `Cargo.toml` | `[package].name` | |
   | `composer.json` | `.name` | strip vendor: `acme/web` → `web` |
   | `pom.xml` | `<artifactId>` | |
   | `*.gemspec` | `.name = "<x>"` | |

   *(As shipped: the original design deferred `pom.xml`, `setup.cfg`, and
   `*.gemspec`; all three landed in the implementation — see
   `internal/cli/addapp/derive.go`.)*

2. **Sanitize** each raw name to a DNS-1123 label: lowercase; `_ . / @` and other
   invalid runs → `-`; collapse repeated dashes; trim leading/trailing dashes;
   truncate to 63. Empty after sanitizing ⇒ treated as "no name".

3. **Decide — three deterministic cases:**
   * **No name found** → use the **directory basename** (sanitized). Print
     *"no project manifest found; using directory name 'X'."*
   * **One distinct name** (one manifest, or several that agree) → use it. Print
     *"derived name 'X' from package.json."*
   * **Conflicting names** (≥2 manifests yielding *different* names) → **do not
     guess**. Error listing each `<file>: <name>` and require `--name`.

This needs no arbitrary cross-language precedence — the only ambiguous case
errors instead of guessing — is always transparent about what it chose, and
`--name` overrides everything. For the existing fixtures it is a no-op:
`hello-app` (go.mod basename), `hello-react` (package.json `.name`), `hello-py`
(no name field in requirements.txt → dir name) all keep their current names.

Parsing `pyproject.toml`/`Cargo.toml` needs a TOML library; add
`pelletier/go-toml/v2` if not already transitively present.

## Completion

Phase 1 yields `flywheel completion bash|zsh|fish` for free. `add-app` registers
a `ValidArgsFunction` on its positional that reads `workspaces_root` and returns
its **direct child directories that are git worktrees** (have a `.git`). It must
**degrade silently** — if cwd isn't a gitops repo or config is missing, return
no candidates (never error or hang the shell). `--name` stays freeform.

## `make install`

A new root `Makefile`:

* **`make build`** — `go build` with the version stamp
  `-ldflags "-X github.com/cobr-io/flywheel.BuildVersion=$(git describe --tags --always --dirty)"`,
  output to `$(go env GOBIN)` (fallback `~/go/bin`). This makes `flywheel
  version` and the `flywheel.version` recorded by `init` a real ref instead of
  the default `v0.0.0-dev` — which is what made the embed-cache key ambiguous in
  practice (an unstamped binary's `v0.0.0-dev` becomes the cache directory key,
  so a stale cache can silently ship old embedded manifests).
* **`make install`** — depends on `build`, then installs completions: detect the
  shell from `$SHELL`, run `flywheel completion <shell>`, and write it to that
  shell's canonical user location, **overwriting** any previous copy (idempotent;
  removes a stale `flywheel` completion at the standard path first so it can't
  shadow the new one). Prints where it landed plus any one-time `fpath`/sourcing
  hint.
  * zsh → a dir on `fpath` (e.g. `~/.zsh/completions/_flywheel`)
  * bash → `~/.local/share/bash-completion/completions/flywheel`
  * fish → `~/.config/fish/completions/flywheel.fish`
* **`make completions`** — the completion step alone (re-run after a flag change).
* Shell-detection/path logic lives in `scripts/install-completions.sh` to keep the
  Makefile readable.

## Error handling

* `<dir>` missing / not a directory / not a direct child of `workspaces_root` →
  clear error naming the resolved path and the expected root.
* Conflicting derived names → error listing each manifest + name, requiring
  `--name`.
* `--name` (or a derived name) that isn't a valid DNS-1123 label after
  sanitization → error.
* Completion never errors — missing config yields zero candidates.

## Test plan

* **Phase 1**: existing `k3d-e2e` (`init + up + scenarios`) and `go-test` are the
  regression net. Add a `cmd`-level smoke test: every command + flag is
  registered, globals are persistent, exit codes hold (unknown command non-zero,
  usage `2`).
* **Phase 2**: table-driven unit tests for derivation (each manifest type,
  scoped/vendor names, `go.mod` basename, sanitization edge cases, no-name→dir,
  conflict→error, `--name` override); decoupling tests (worktree drives
  `WORKTREE`/bare/url, name drives resource names/Ingress/image); validation
  errors (missing dir / outside root); a completion test (returns workspace dirs
  from a temp root, degrades gracefully).
* The e2e fixture pins `--name` only if its derived name would differ from the
  directory.

## Migration

* Two PRs, each independently mergeable and CI-gated: (1) cobra migration,
  (2) `add-app` redesign.
* Backward compatibility: `flywheel add-app hello-py` still works — `dir=hello-py`
  and the derived name is `hello-py` whenever the directory declares no
  conflicting manifest name.
* One CHANGELOG note flags the single behavior change: a manifest-declared name
  now wins over the directory name; pin with `--name`.

## Open questions

* Completion install for **zsh** must land on a directory already in `fpath`;
  if none is writable, `make install` should fall back to `~/.zsh/completions`
  and print the one-line `fpath+=` hint rather than silently doing nothing.
* Should `make install` optionally install completions for **all** detected
  shells (not just `$SHELL`)? Deferred; `$SHELL` detection covers the common case.
* `pom.xml`/`setup.cfg`/`*.gemspec` derivation is deferred — revisit if a real
  fixture needs it.
