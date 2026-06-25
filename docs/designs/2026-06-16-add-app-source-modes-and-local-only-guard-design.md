# `flywheel add-app` source modes + local-only main-branch guard

**Status:** accepted & implemented; **amended 2026-06-17** — see
[Addendum: the `workspace:` block supersedes the source annotation](#addendum-2026-06-17-the-workspace-block-supersedes-the-source-annotation)
**Date:** 2026-06-16

> **Amendment 2026-06-17 (proposed).** Dogfooding the shipped feature on
> `acme-gitops` showed the per-app `flywheel.cobr.io/source` annotation is keyed
> wrong for the facts it records. The Addendum at the end of this document
> replaces it with a worktree-keyed `workspace:` block in `flywheel.yaml` as the
> single source of truth. **§2, §4 (detection), §5, §6, and the annotation
> decisions below are superseded by the Addendum** — read it before relying on
> those sections.

## Problem

`flywheel add-app <dir>` today registers an app only when its worktree **already
exists on disk** as a direct child of `workspaces_root` (see
[`2026-06-02-add-app-worktree-decoupling-design.md`](2026-06-02-add-app-worktree-decoupling-design.md)).
That leaves the developer experience underspecified across the four states a new
app can be in, crossing two axes — *does the source exist on disk?* and *does it
have an external git remote (e.g. GitHub origin)?*

| | no remote | has remote |
|---|---|---|
| **on disk** | #1 works today, but unguarded | #4 works today, remote ignored |
| **not on disk** | #2 — fails `worktree not found` | #3 — fails `worktree not found` |

Two concrete problems fall out:

1. **No clear path for #3** (code lives in a remote you haven't cloned). The
   developer must manually `git clone` into exactly the right directory under
   `workspaces_root` before `add-app` will work — undiscoverable and easy to get
   wrong.

2. **#1 silently pollutes `main`.** A *local-only* app (no external remote) has
   source that exists **only on one developer's machine**. flywheel's deploy
   pipeline is identical for every app — git-auto-sync pushes the local worktree
   to an in-cluster bare repo, Flux reconciles from there — so a local-only app's
   manifests reconcile fine on *that* developer's cluster. But if those manifests
   are merged to `main`, every **other** developer (and any shared/staging
   cluster reconciling `main`) gets an app whose in-cluster bare repo is fed by a
   worktree they don't have: a permanently-broken, unreconcilable workload. There
   is nothing today that warns about or prevents this.

A subtle consequence makes #2 worth solving correctly: the GitRepository
`spec.url` for **every** app points at the in-cluster bare repo
(`{{ .GitServerURL }}/{{ .Worktree }}.git`,
`manifests/per-app-template/gitrepository.yaml.tmpl:19`), regardless of where the
source originally came from. So **the manifests alone cannot tell a local-only
app apart from a remote-backed one** — we must record source provenance
explicitly for any guard to key on it.

### In scope

- Input-shape dispatch for `add-app`: clone a remote (#3), register an existing
  worktree (#1/#4), or error with a descriptive minimum-requirements message
  (#2).
- A `flywheel.cobr.io/source` provenance annotation on each app's GitRepository.
- A three-layer guard preventing local-only apps from reaching `main`:
  add-app-time gate, a pre-commit hook, and a CI check on PRs to `main`.
- A Dockerfile pre-flight check at add-app time.
- A `flywheel publish-app` command that promotes a local-only app to
  remote-backed once its worktree has been pushed to a remote.
- A `flywheel up` step that materializes missing app worktrees as siblings under
  `workspaces_root` by cloning their recorded `source` remote — turning a fresh
  gitops-repo clone into a one-command dev-environment bootstrap.

### Out of scope

- **Scaffolding empty projects (#2).** flywheel will *not* generate
  per-language/framework project templates. The nothing-nowhere case errors with
  guidance instead. Source: explicit product decision to keep scope minimal.
- Changing the deploy pipeline. Source mode is metadata/governance only; it
  changes **no** runtime reconciliation behavior. Every app still deploys via the
  same **local** path — git-auto-sync → in-cluster bare repo → in-cluster build →
  `ImageRepository`/`ImagePolicy` — identically.
- **Production reconciliation.** Production is image-based (app-CI → registry →
  Flux Image Update Automation), not covered here. See "Relationship to
  production" below.
- Private-remote credential management for clone — we use the developer's ambient
  git credentials and do nothing special.
- **Manual, tag-based release trains and promotion gates.** flywheel records a
  source *repo*, never a ref or tag; deployment tracks newest-`main` continuously.
  See the guiding principle.

## Guiding principle: trunk-based correspondence

flywheel assumes **the latest commit on `main` is the source of truth for
production** — for both the gitops repo and each app repo. Production runs
HEAD-of-`main`; there are no tagged release trains. This single assumption is
what makes the rest coherent:

- It justifies why `flywheel up` can clone a missing app repo and land on its
  **default branch** (the repo's trunk): a fresh clone reproduces the
  production-equivalent state. An *existing* worktree is left on whatever branch
  the developer had checked out (their work-in-progress); git-auto-sync
  reconciles whatever is checked out.
- It is why the `flywheel.cobr.io/source` annotation records only a **repo URL,
  never a ref or tag**: there is nothing to pin.

**Locally, dev tracks a branch HEAD, not a pinned version.** For the `flywheel up`
reconcile below, a fresh clone lands on the repo's **default branch** (its
trunk) — it does not pin to an immutable commit, because local dev is the active
inner loop where you *want* HEAD. That is why the annotation records a repo URL
with no ref. What the trunk-based stance declines is **manual, curated release
trains** — cutting `v1.2.3` and promoting that artifact through environment
gates — not pinning per se.

### Relationship to production (out of scope here)

This document describes the **local dev inner loop**: git-auto-sync → in-cluster
bare repo → in-cluster buildkit build → `ImageRepository`/`ImagePolicy` watch the
locally-built image. Production uses **none** of that machinery. There, the app
repo's own CI builds and pushes an image to a private registry, and Flux **Image
Update Automation** (the same `ImageRepository`/`ImagePolicy` primitives, plus
`ImageUpdateAutomation`) watches the registry and writes the resolved tag back
into the gitops repo. The local inner loop is a faithful stand-in for that
image-automation reconcile — only the *image source* differs (in-cluster build vs
app-CI/registry). Two consequences pin down scope:

- The `flywheel.cobr.io/source` annotation is **local-dev + governance only** — it
  drives `up`-clone and the local-only guard. It is **not** a production reconcile
  source; production reconciles images, not app git source.
- Production is **not** unpinned: IUA records the deployed image tag in the gitops
  repo, so the gitops commit history *is* the deployment history and `git revert`
  rolls back — automatically, with no manual release train. (See also
  `2026-06-04-prod-promotion-feasibility.md`.)
- **Stand-in fidelity depends on tag-selection alignment.** The per-app template
  renders `imagepolicy.yaml`/`imagerepository.yaml`; locally they watch the
  in-cluster-built image, in prod the registry image. For the local inner loop to
  faithfully predict prod, the `ImagePolicy` tag-selection scheme (sort order /
  filter pattern) should match the tagging the app's CI pushes to the registry —
  otherwise local and prod can select different tags. Reconciling those schemes is
  a prod-promotion concern, tracked in
  `2026-06-04-prod-promotion-feasibility.md`, not resolved here.

## Approach

### 1. Input-shape dispatch

`add-app` keeps its single positional argument but branches on its **shape**:

```
flywheel add-app <dir|git-url> [--name <name>] [--image <img>] \
                 [--context <path>] [--dockerfile <path>] [--target <stage>] \
                 [--namespace <ns>]
```

* **Arg looks like a git URL** (`https://…`, `http://…`, `ssh://…`, `git://…`,
  or scp-style `user@host:path`) → **clone mode (#3)**:
  1. Derive the destination directory name: `--name` if given, else the URL repo
     basename with any `.git` suffix stripped (sanitized to DNS-1123, reusing the
     existing sanitizer in `internal/cli/addapp/derive.go`).
  2. Clone into `workspaces_root/<dir>` with `git clone <url> <dest>` using the
     developer's ambient credentials and the remote's default branch. **Refuse**
     if `<dest>` already exists (never clobber).
  3. Fall through to the **exact** existing registration path. Source =
     the clone URL.

* **Arg is a path/name** (today's behavior) → resolve under `workspaces_root`:
  * **exists on disk** → inspect `git -C <dir> remote get-url origin`:
    * has an `origin` remote → register **remote-backed (#4)**; source = the
      origin URL.
    * no remote → register **local-only (#1)**; source = `local-only` (subject to
      the guard below).
  * **does not exist** (and isn't a URL) → **error (#2)**, see below.

A small helper `looksLikeGitURL(arg) bool` does the classification. It is
deliberately conservative: a bare name or a path under `workspaces_root` never
matches (those don't carry a URL scheme or scp-style `host:` prefix).

### 2. Source provenance annotation

Because `spec.url` is uninformative (always the in-cluster bare repo), we record
provenance on the GitRepository — the natural anchor: exactly one per app, it is
the Flux source object, and it is trivially greppable from
`builders/base/<name>/gitrepository.yaml`. There is precedent — git-auto-sync
already writes `flywheel.cobr.io/deploy-branch` (`scripts/git-auto-sync/sync.sh:68`).

```yaml
# manifests/per-app-template/gitrepository.yaml.tmpl
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
  annotations:
    flywheel.cobr.io/source: {{ .Source }}   # "local-only" or the external remote URL
```

`.Source` is injected by `buildValues()` (`internal/cli/addapp/addapp.go:319`).
The value is either the literal `local-only` or the external remote URL. **Absence**
of the annotation denotes a legacy app created before this feature — guards treat
absence as "not local-only" (pass), since those apps predate the concept and
were registered intentionally.

This annotation is the **single source of truth** every guard layer keys on, and
the field `publish-app` flips.

### 3. Dockerfile pre-flight

After resolving `context`/`dockerfile` (defaults `.`/`Dockerfile`,
`addapp.go:97-101`), verify `filepath.Join(worktree, context, dockerfile)` exists
before rendering anything. Today there is **no** such check — add-app happily
registers an app with no Dockerfile and the failure surfaces only later, in the
buildkit build Job (`internal/controller/templates/build-job.yaml:57`). The new
check errors early:

> `no Dockerfile at <context>/<dockerfile> in <worktree>; an app needs at least a
> Dockerfile to build. Pass --context/--dockerfile if it lives elsewhere.`

This runs for both clone mode (after the clone lands) and existing-path mode.

### 4. The local-only guard — three layers

All three layers detect a local-only app the same way: the
`flywheel.cobr.io/source: local-only` annotation on a GitRepository under
`builders/base/`. The detection logic lives in **one** place — a
dependency-free shell script `scripts/ci/check-local-only.sh` in the client
skeleton — invoked by both the pre-commit hook and CI, so there is a single
source of truth. This mirrors the existing `sops-shape` guard, which is exactly
this shape: `scripts/ci/check-sops-shape.sh`, wired into pre-commit via
`entry: bash scripts/ci/check-sops-shape.sh`
(`.pre-commit-config.yaml.tmpl:33`). (An earlier draft proposed a
`flywheel check local-only` subcommand; the script matches house style and
needs no flywheel binary in CI.)

The script resolves two inputs:

* **The protected integration branch** — read from `flywheel.yaml`
  (`git.integration_branch`, default `main`) via `yq`, which the skeleton already
  requires for the SOPS guard.
* **The effective target branch** — `$GITHUB_BASE_REF` when set (a PR's base
  branch in GitHub Actions), else the current branch (`git rev-parse
  --abbrev-ref HEAD`). It **blocks** (exit non-zero) when the effective target is
  the integration branch and any local-only app is present; otherwise it
  **warns** (exit 0).

**Layer 1 — add-app-time gate.** When the resolved source is `local-only`, read
the **gitops repo's** current branch via the existing
`converge.CurrentBranch(repoDir)` (`internal/cli/converge/bootstrap.go:114`) and
compare it to the configured integration branch (`git.integration_branch` in
`flywheel.yaml`, default `main`):

* on the integration branch → **refuse**:
  > `refuse to register a local-only app on 'main'. A local-only app's source
  > exists only on your machine and must not reach main. Switch to a feature
  > branch (git checkout -b <branch>) and re-run.`
* on a feature branch → **proceed**, with an informative log:
  > `'<name>' has no external git remote — its source exists only on this
  > machine. Before this branch merges to main, push the worktree to a remote and
  > run 'flywheel publish-app <name>'.`

**Layer 2 — pre-commit hook.** A new local hook in
`templates/client-skeleton/.pre-commit-config.yaml.tmpl`, copying the `sops-shape`
hook's exact shape (`bash scripts/ci/<script>`, `language: system`):

```yaml
- repo: local
  hooks:
    - id: local-only-guard
      name: local-only app guard
      entry: bash scripts/ci/check-local-only.sh
      language: system
      pass_filenames: false
      always_run: true
```

The script scans `builders/base/*/gitrepository.yaml` for the `local-only`
source annotation and applies the block/warn logic above. `pass_filenames: false`
+ `always_run: true` because the guard must scan the **whole tree** (a local-only
app committed in an earlier commit on the branch must still be caught), not just
the staged files. Hooks are already installed by `flywheel init` via
`pre-commit install` (`internal/cli/initcmd/init.go:299`), so no new wiring.

**Layer 3 — server-side CI.** Local hooks do **not** run on a server-side PR
merge, so Layer 2 alone relies on discipline (`git commit --no-verify` bypasses
it). The real enforcement rides on **issue #35**, which scaffolds
`.github/workflows/ci.yaml` whose lint job runs `pre-commit run --all-files` on
PRs. Because the local-only guard is a pre-commit hook with `always_run: true`,
that job runs it server-side automatically — and in that context the script reads
`$GITHUB_BASE_REF`, so a PR **into** the integration branch blocks even when the
PR head is a feature branch. This makes #35 the delivery vehicle for Layer 3:
this feature contributes the script + hook; #35 contributes the workflow that
makes them non-bypassable.

> **Dependency on #35:** if this feature ships before #35 lands, it must either
> block on #35 or carry a minimal `pre-commit run --all-files` workflow itself.
> Either way the workflow is GitHub Actions (per #35); a non-GitHub client swaps
> only the workflow file and still gets the script + hook.

### 5. `flywheel publish-app`

Promotes a local-only app to remote-backed once the developer has pushed its
worktree to an external remote:

```
flywheel publish-app <name>
```

1. Locate `builders/base/<name>/gitrepository.yaml`; resolve its worktree
   directory from `spec.url`'s `<dir>.git` basename.
2. Verify the worktree now has an `origin` remote and it is reachable
   (`git -C <worktree> ls-remote origin`), and the current branch is pushed.
   Error with guidance if not.
3. Rewrite the `flywheel.cobr.io/source` annotation from `local-only` to the
   origin URL.
4. Refresh the `.flywheel-state.yaml` baseline hash for the edited file (same
   mechanism add-app uses, `addapp.go` step 10) so `flywheel update` doesn't flag
   the edit as a user change.

After this the guards no longer fire and the app can be merged to `main`.

### 6. `flywheel up` worktree reconcile

A fresh clone of a gitops repo declares N apps in `builders/base/` but has none of
their source worktrees on disk. A new reconcile step materializes them, making
"clone gitops repo → `flywheel up`" a complete bootstrap.

**Placement.** Before k3d cluster create (step 7 in `internal/cli/up/up.go`), so
every worktree is present when `workspaces_root` is bind-mounted at `/workspaces`
(`internal/cli/k3d/k3d.go:95`) and before the git-server scans it. The step needs
only on-disk gitops files, so it runs early.

**Declared-apps parser** (new helper, alongside `addapp.WorkspaceDirs()`): scans
`builders/base/*/gitrepository.yaml`, extracting per app — the worktree dirname
(basename of `spec.url`, minus `.git`) and the `flywheel.cobr.io/source`
annotation.

**Per app:**
- worktree dir **exists** under `workspaces_root` → skip, leave it on its current
  branch (the developer's WIP). Per the guiding principle, only fresh clones get
  the repo's default branch.
- **missing** and `source` is a remote URL → **clone** it as
  `workspaces_root/<dir>`, landing on the repo's **default branch** (its trunk,
  the production-corresponding branch — no forced `main` checkout). git-auto-sync
  then patches the GitRepository ref to track it.
- **missing** and `source` is `local-only` (or annotation absent) → **cannot
  materialize**; warn. The main-branch guard is designed so this never happens on
  `main`; it can only occur on a shared feature branch carrying an
  unpublished local-only app.

**Clone mechanism.** Shell out to host `git` (`git clone`), inheriting the
developer's ambient git auth (SSH agent, credential helpers, 2FA) so private
repos work without extra plumbing. This is the **same shared helper** add-app's
clone mode uses. *Note:* this is a deliberate divergence from `mirror.go` /
`embedcache.go`, which use go-git to avoid a host-git dependency — accepted
because cloning developer repos needs the developer's own credential setup, which
go-git would force us to re-implement. (Host `git` is already a soft runtime dep
via `converge.CurrentBranch`.)

**Trigger & safety.** Because the step writes **outside** the gitops repo, it is
explicit:
- `flywheel up --clone` → reconcile without prompting.
- `flywheel up --no-clone` → skip entirely.
- neither flag, **interactive TTY** → list the worktrees that would be cloned and
  their destinations, then prompt for confirmation.
- neither flag, **non-interactive** (no TTY, e.g. CI) → skip and warn (never
  clone unattended into the filesystem).

**Failure mode.** Best-effort: a clone that fails (auth/network) or a
local-only-missing app is **warned and skipped**, and `up` continues — one
unreachable remote must not block the whole environment. `up` prints a summary of
which apps could not be materialized (they simply won't build until resolved).

**Idempotency.** Steady state is ~free: once worktrees exist, the step is just
`stat` checks. It never modifies the contents of an existing worktree.

## API / data model changes

**CLI:**

- `flywheel add-app <dir|git-url> …` — positional now accepts a git URL (clone
  mode) in addition to a worktree path/name. Existing flags unchanged.
- `flywheel publish-app <name>` — **new** command. Flips a local-only app to
  remote-backed.
- `flywheel up [--clone | --no-clone]` — **new** flags gating the worktree
  reconcile step. No flag ⇒ interactive confirm on a TTY, skip otherwise.

**New helpers** (a new `internal/cli/worktree/` package, imported by `add-app`,
`publish-app`, and `up`):

- `GitRemoteURL`, `Clone` (host-git, lands on the remote's default branch),
  `LooksLikeGitURL`, and
- `ParseAppGitRepository` / `DeclaredApps` over `builders/base/*/gitrepository.yaml`
  (worktree dirname from `spec.url` + `source` annotation).

**Config schema** (`internal/cli/schema/schema.go`):

- new optional block `git: { integration_branch: string }`, default `main`. A new
  `Git` struct on `File` (`json:"git,omitempty"`), read by the Layer-1 add-app
  gate and by `check-local-only.sh` (via `yq`). Added to the schema validator and
  to `flywheel.yaml.tmpl`.

**Manifest template:**

- `manifests/per-app-template/gitrepository.yaml.tmpl` — add
  `metadata.annotations.flywheel.cobr.io/source: {{ .Source }}`.

**Template values** (`internal/cli/addapp` value struct + `buildValues()`):

- new field `Source string` — `"local-only"` or the external remote URL.

**Skeleton additions** (`templates/client-skeleton/`):

- `scripts/ci/check-local-only.sh` — the shared detection script (alongside the
  existing `check-sops-shape.sh`).
- `.pre-commit-config.yaml.tmpl` — add the `local-only-guard` local hook.
- The server-side workflow is **issue #35's** `.github/workflows/ci.yaml`
  (`pre-commit run --all-files`), not a separate workflow shipped here.

**Annotation contract:**

- `flywheel.cobr.io/source` on a GitRepository: `local-only` | `<remote-url>`.
  Absent ⇒ legacy app, treated as not-local-only.

No database, no HTTP endpoints.

## Migration plan

Greenfield for data — there is no persisted state to migrate. Notes on
compatibility:

- **Existing apps** have no `flywheel.cobr.io/source` annotation, and there is **no
  backfill**: `flywheel update` does not manage per-app builder files
  (`renderTheirs` covers only `templates/client-skeleton` + `docs/guides`; per-app
  files are never tracked as merge candidates), so pre-feature apps stay
  unannotated. Guards treat absence as not-local-only (pass) — correct, since those
  apps were already registered/merged intentionally. Only `add-app` writes the
  annotation, on new apps.
- **`add-app` backward compatibility:** every existing invocation form
  (`add-app hello-py`, relative/absolute paths) behaves exactly as before; only
  the URL shape is newly recognized.
- **No deploy-time behavior change:** source mode is metadata; reconciliation is
  byte-for-byte unchanged for all existing apps.
- **Staged delivery** (each independently mergeable + CI-gated):
  1. Source annotation + Dockerfile pre-flight (no behavior gate yet).
  2. Clone mode (#3) + the descriptive #2 error.
  3. Local-only guard layers 1–3 (`git.integration_branch` config + add-app gate
     + `check-local-only.sh` + pre-commit hook). Server-side enforcement (Layer 3)
     depends on issue #35 — sequence after it, or carry a minimal workflow.
  4. `flywheel publish-app`.
  5. `flywheel up` worktree reconcile (`--clone`/`--no-clone` + interactive
     confirm), reusing the shared clone helper from stage 2.

## Test plan

- **Unit (`internal/cli/addapp`):** `looksLikeGitURL` classification table
  (URLs vs bare names vs paths vs scp-style); destination-name derivation from a
  URL (strip `.git`, sanitize); source resolution (origin present → URL, absent →
  `local-only`); Dockerfile pre-flight (present/missing, custom context).
- **Script test (`check-local-only.sh`):** a `bats`/shell test (or a Go test that
  shells out) over a temp `builders/base` tree — local-only with effective target
  = integration branch → non-zero; with target = feature branch → zero + warning;
  no local-only → zero; legacy app missing the annotation → zero; honors
  `$GITHUB_BASE_REF` and `git.integration_branch`.
- **Unit (`publish-app`):** worktree resolution from `spec.url`; refuses when no
  reachable origin; flips the annotation and refreshes the state hash.
- **Unit (`up` reconcile):** declared-apps parser over a temp `builders/base`
  tree (dirname from `spec.url`, source annotation); reconcile logic — existing
  worktree skipped (branch untouched), missing+remote → clone planned on `main`,
  missing+local-only → warned; `--no-clone` skips; non-TTY without a flag skips;
  a clone failure is warned and `up` continues.
- **Integration / e2e (extend `k3d-e2e`):** clone-mode add-app from a local bare
  repo fixture registers and builds; local-only add-app on a feature branch
  succeeds and on `main` is refused; `publish-app` clears the guard; **bootstrap**
  — from a gitops repo declaring a remote-backed app with no worktree on disk,
  `flywheel up --clone` materializes the sibling worktree and the app builds.
- **Manual:** the pre-commit hook blocks a commit on the integration branch and
  warns on a feature branch; the #35 CI workflow fails a PR (into the integration
  branch) carrying a local-only app; `--no-verify` is caught server-side.
- **Edge cases:** clone destination already exists (refuse); URL with no `.git`
  suffix; worktree with multiple remotes (origin wins); remote-backed worktree
  whose current branch isn't pushed (publish-app reachability check).

## Resolved decisions

- **Protected branch is configurable.** New `git.integration_branch` field in
  `flywheel.yaml` (default `main`), not a hardcoded constant.
- **Detection is a shell script**, `scripts/ci/check-local-only.sh` — matches the
  `sops-shape` precedent, needs no flywheel binary in CI. (Supersedes the earlier
  `flywheel check local-only` subcommand idea.)
- **Server-side enforcement rides on issue #35's** `.github/workflows/ci.yaml`
  (`pre-commit run --all-files`) rather than a separate workflow. GitHub Actions
  is the assumed CI; a non-GitHub client swaps only the workflow file.
- **`publish-app` takes the app `<name>`** (registered identity), not the worktree
  dir.
- **No #4 unpushed-branch warning in `add-app`.** add-app stays lenient; push-state
  is enforced where it matters — `publish-app` **blocks** unless the origin is
  reachable **and** the current branch is pushed (a reachable origin alone doesn't
  make the app recoverable).
- **Fresh clones land on the repo's default branch** (its trunk) — both `add-app`
  clone mode and the `up` reconcile; no forced `main` checkout (robust to repos
  whose default is `master`/other).
- **No backfill onto existing apps** — `flywheel update` doesn't manage per-app
  builder files; pre-feature apps stay unannotated (= not-local-only). Only
  `add-app` writes the annotation (see Migration).
- **`git.integration_branch` is validated** when present (an empty/typo'd value is
  rejected rather than silently disabling the guard).
- **Shared helpers live in `internal/cli/worktree/`** (`GitRemoteURL`, `Clone`,
  `LooksLikeGitURL`, `ParseAppGitRepository`, `DeclaredApps`).

## Open questions

- **Sequencing vs #35.** Preference is to land #35 first so Layer 3 is real on day
  one; otherwise stage 3 carries a minimal interim workflow. Confirm ordering.

---

## Addendum (2026-06-17): the `workspace:` block supersedes the source annotation

**Status:** proposed (revises §2, §4-detection, §5, §6, and the
annotation-related decisions above).

### Why revise

The shipped design records source provenance as a per-app
`flywheel.cobr.io/source` annotation on each GitRepository (§2); both consumers —
the local-only guard (§4) and the `up` worktree reconcile (§6) — key on it.
Dogfooding on a fresh `acme-gitops` clone exposed a keying mismatch:

- **The annotation is keyed by *app*; the facts it records are properties of the
  *worktree/repo*.** "Where does this source come from" and "is it published yet"
  belong to the repo, not the app. Several builders can share one worktree
  (`sample-app-backend` + `sample-app-frontend` both build from `sample-app`), so the
  annotation stores the **same fact N times**, once per sharing app, with nothing
  keeping the copies consistent. local-only is intrinsically a per-repo state: you
  cannot have one builder published and another local-only when they are the same
  source repo. The annotation duplicates a worktree fact across apps even *within*
  a single repo.
- **The reconcile inherits the mismatch.** `MissingWorktrees`
  (`internal/cli/worktree/worktree.go:144`) iterates apps, not worktrees, and does
  no dedup — a shared worktree is listed (and clone-attempted) once per sharing
  app.
- **The write path is fragile and scattered.** `publish-app` flips the annotation
  with a `strings.Replace` on raw manifest YAML (`publishapp.go:106`), and the
  data is smeared across N manifests instead of living in one legible place.
- **It did not deliver the headline onboarding promise.** On a fresh `acme-gitops`
  clone, `flywheel up` did **not** offer to clone the sibling repos: every app
  predated the feature, so no annotation existed, every worktree classified as
  "unmaterializable," and the reconcile returned before reaching the clone prompt.
  (The annotation *is* committed and *does* survive a clone — the gap is that
  nothing declares siblings for pre-feature apps, and `flywheel update` doesn't
  backfill per-app files.)

### Revised model: one worktree-keyed declaration

Replace the annotation with a **`workspace:` block in the committed
`flywheel.yaml`**, keyed by worktree. It is the **single source of truth** for both
source provenance and local-only governance; every consumer derives from it.

```yaml
# flywheel.yaml
workspace:
  repos:
    - name: sample-app                                     # == workspaces_root/<name>, the git-auto-sync worktree
      url: git@github.com:example-org/sample-app.git    # remote-backed
    - name: hello-py
      local_only: true                                      # no remote yet — warn, never clone
```

- **Keyed by worktree dir** (`name`) — the basename of every sharing app's
  `spec.url` minus `.git`, exactly the key `RepoNameFromURL` already produces and
  the only thing the flat `/workspaces` mount supports. **Exactly one entry per
  worktree**, even when several builders share it.
- Each entry is **either `url` (remote-backed) xor `local_only: true`**. **No
  `ref`** — consistent with the guiding principle ("trunk-based correspondence"):
  a fresh clone lands on the repo's default branch and there is nothing to pin.
  (Issue #36 floated an advisory `ref`; the accepted trunk-based stance overrides
  it, so it is dropped here.)
- **Absence of an entry** for an app's worktree = legacy/undeclared. The guard
  treats it as **not-local-only (pass)**, exactly as it treated an absent
  annotation; `up`/`doctor` surface it as a **warning** ("app *X* references
  worktree *Y*, not declared in `workspace.repos`").

### Where each annotation role moves

| Role (shipped) | Anchor (shipped) | Anchor (revised) |
|---|---|---|
| `up` clone source | `flywheel.cobr.io/source` URL, **per app** | `workspace.repos[].url`, **per worktree** |
| local-only governance | `…/source: local-only`, per app | `workspace.repos[].local_only`, per worktree; guard derives per-app via `spec.url → worktree → entry` |
| `publish-app` target field | annotation `strings.Replace` on a manifest | structured edit of the entry (`local_only: true` → `url:`) |

The per-app GitRepository **drops** the `flywheel.cobr.io/source` annotation
entirely; `.Source` is removed from the template values.

### Component changes vs. the shipped design

- **§2 (source annotation)** — removed. Provenance lives only in
  `workspace.repos`.
- **§4 (guard detection)** — `check-local-only.sh` no longer greps manifests for
  the annotation. It reads `workspace.repos` from `flywheel.yaml` (via `yq`,
  already a skeleton dependency), builds the set of `local_only` worktrees, and for
  each app under `builders/base/*` resolves its worktree from `spec.url`, blocking
  when one maps to a `local_only` entry **and** the effective target is the
  integration branch. The block/warn logic and the `git.integration_branch` input
  are unchanged. Net: the guard graduates from a grep to a parse-and-resolve — a
  bounded increase, and more robust because the governance list is human-curated in
  one place. Layer 1 (add-app gate) keys on what it is about to write to the block,
  which it knows directly.
- **§5 (`publish-app`)** — resolves app `<name>` → `spec.url` → worktree, then
  performs a **structured, comment-preserving edit** of that worktree's
  `workspace.repos` entry (`local_only: true` → `url: <origin>`), replacing the
  `strings.Replace`-on-manifest approach. State-hash refresh now targets
  `flywheel.yaml` (verify whether `.flywheel-state.yaml` tracks it; if not, no
  refresh is needed).
- **§6 (`up` reconcile)** — reads `workspace.repos` (worktree-keyed) instead of
  scanning annotations. Per entry: worktree present → skip; `url` + missing →
  clone (default branch); `local_only` + missing → warn. Declared apps whose
  worktree has no entry → warn (undeclared). Worktree-keyed iteration makes the
  double-clone-on-shared-worktree case structurally impossible. The
  `--clone`/`--no-clone`/interactive-confirm trigger semantics are unchanged.
- **Config schema** (`internal/cli/schema/schema.go`) — add
  `workspace: { repos: [ { name: string, url?: string, local_only?: bool } ] }`
  plus a commented stub in `flywheel.yaml.tmpl`. Validation: `name` required and
  dir-safe; **exactly one** of `url` / `local_only`. `git.integration_branch` is
  retained.
- **Write machinery** — `add-app`/`publish-app` now write `flywheel.yaml`. Today
  **nothing does**, and the config is parsed via `sigs.k8s.io/yaml` (a JSON
  round-trip that drops comments and reorders keys, `internal/cli/config/merge.go:10`).
  The block upsert must therefore use a comment-preserving editor
  (`gopkg.in/yaml.v3` `Node`, or a targeted append) so a hand-authored
  `flywheel.yaml` keeps its comments. This is net-new machinery, but it replaces
  the fragile manifest `strings.Replace` and consolidates the edit into one
  flywheel-owned file.
- **`add-app`** — instead of injecting `.Source` into the manifest template, it
  **upserts** the worktree's `workspace.repos` entry (idempotent: a shared
  worktree's second builder finds the entry already present). `--local-only` and
  origin-detection logic are unchanged in intent; only the write target moves from
  the per-app manifest to the block.

### Migration

Greenfield-ish — the only live consumer is `acme-gitops`, and hand-migrating it is
accepted (single repo, few apps):

- `acme-gitops`: add a `workspace.repos` block declaring `sample-app` (`url`) and
  `hello-py` (`local_only: true`). Its legacy apps carry no annotation to remove.
- Any app created during the shipped feature that *did* receive an annotation:
  read it once, write the equivalent block entry, delete the annotation line.
  (Outside test fixtures, likely none.)
- No deploy-time behavior change: provenance remains metadata/governance only.

### Decisions revised

- ~~The `flywheel.cobr.io/source` annotation is the single source of truth every
  guard layer keys on~~ → **the `workspace.repos` block is the single source of
  truth**, keyed by worktree; the annotation is removed.
- ~~No backfill onto existing apps (consume path depends on per-app annotations)~~
  → still no *automatic* backfill, but the consume path no longer depends on
  per-app annotations: siblings are declared once in the committed block, so a
  fresh clone populates via `up` regardless of when the apps were created —
  restoring the original #36 onboarding promise.
- **No advisory `ref`** in block entries (consistent with trunk-based
  correspondence; supersedes #36's advisory-ref idea).
- **Relationship to #36:** this revision adopts #36's worktree-keyed, committed
  `workspace:` block as the source-of-truth mechanism, while keeping the shipped
  three-layer guard and `publish-app`. #36 remains the umbrella issue.
