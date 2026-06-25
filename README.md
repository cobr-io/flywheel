# Flywheel

**A single-binary CLI for a production-faithful local GitOps dev loop — `git
commit` lands in a running pod on a real Flux-driven k3d cluster in seconds.**

[![CI](https://github.com/cobr-io/flywheel/actions/workflows/test.yml/badge.svg)](https://github.com/cobr-io/flywheel/actions/workflows/test.yml)
[![Latest release](https://img.shields.io/github/v/release/cobr-io/flywheel?sort=semver)](https://github.com/cobr-io/flywheel/releases)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

> **Status:** under active development (v0.1).

## What is Flywheel?

Flywheel gives you a local Kubernetes environment that runs the **same GitOps
control plane you'd run in production** — [Flux](https://fluxcd.io) reconciling
Kustomize overlays, [SOPS](https://github.com/getsops/sops)-encrypted secrets,
Traefik ingress with real TLS — and wires a **fast inner loop** on top so a `git
commit` becomes a running pod in seconds, with no external CI or registry in the
path.

The point is fidelity. You develop against the same machinery you ship to: the
`base/` + `overlays/` layout Flux reconciles on your laptop is the same one that
promotes to a real cluster; only the cloud-specific infra and the local build
loop differ. No "works in the Helm-templated dev shim, breaks in the real Flux
pipeline" surprises — the pipeline *is* the real one.

One command brings the whole thing up:

```sh
flywheel up
```

…and from then on you just edit code and commit. Flywheel handles the k3d
cluster, Flux bootstrap, in-cluster build, and image rollout.

**Who it's for:** developers and platform engineers who run (or want to run)
their apps on GitOps-driven Kubernetes and want a local inner loop that behaves
like production instead of a bespoke `docker compose` approximation of it.

## The dev loop

This is the core mechanic. Once you've pointed Flywheel at a working directory
with `flywheel add app <dir>`, every commit flows to a pod entirely on your
machine:

```
  you save + git commit            (in your app's worktree)
        │
        ▼
  git-auto-sync  ──pushes──▶  in-cluster git-server   (a bare mirror of the worktree)
        │
        ▼
  image-builder-controller   builds an image on the new commit (Kaniko / BuildKit)
        │
        ▼
  Flux image-automation      rolls the new image into the Deployment
        │
        ▼
  your pod is running the new code        (typically a few seconds; ~30s worst case)
```

No CI round-trip, no pushing to a remote registry, no `docker build && kubectl
set image`. Save, commit, and the cluster converges — the same way Flux would
converge production from a git push, just with the build happening in-cluster
instead of in CI.

### Why "local" resembles "production"

| Concern | Locally (Flywheel) | In production |
|---|---|---|
| Reconciliation | Flux pull-based GitOps | **same** — Flux |
| Manifests | Kustomize `base/` + `overlays/local` | **same base**, `overlays/prod` |
| Secrets | SOPS + age | **same** — SOPS + age |
| Ingress / TLS | Traefik + mkcert | Traefik / your ingress + real certs |
| Image rollout | Flux image-automation | **same** — Flux image-automation |
| Image source | in-cluster build (git-server + builder) | your CI → registry |

The only local-only pieces are the inner-loop machinery — the in-cluster
git-server, the git-auto-sync sidecar, and in-cluster image builds. Those drop
away when you promote to a real cluster, where images come from CI. Everything
else is the production control plane. See
[Promoting to production](#promoting-to-production).

### How it works

* `flywheel` — a single static Go binary CLI. It embeds the Flux-consumable
  manifest tree and the GitOps-repo skeleton, so once `flywheel` is installed
  nothing else needs to be cloned.
* Three runtime container images on `ghcr.io/cobr-io/`: `git-server`,
  `git-auto-sync`, and `image-builder-controller` — the dev-loop machinery. The
  CLI pins them by the release version it was built from.
* A `templates/client-skeleton/` tree rendered into your repo by `flywheel
  init`, plus the dev-loop manifests (`manifests/dev-loop/`) Flux consumes from
  an in-cluster mirror.

Your repo pins a Flywheel version in `flywheel.yaml`. Bumping that line and
re-running `flywheel up` re-converges the live cluster onto the new binary — so
the dev-loop machinery upgrades without you hand-editing manifests.

## Quickstart

Prerequisites: `git`, `k3d`, the `docker` CLI + daemon, and `mkcert` (see
[Prerequisites](#prerequisites)). Run `flywheel doctor` to check them.

```sh
# 1. Install the CLI (from source — see Installation)
git clone https://github.com/cobr-io/flywheel && cd flywheel && make install

# 2. Scaffold and launch a local GitOps environment
mkdir my-gitops && cd my-gitops
flywheel init            # scaffold the GitOps repo in-place
flywheel up              # bring up k3d + Flux, pull runtime images

# 3. Wire up an app with a live dev loop
flywheel add app <dir>   # scaffold a builder + workload from a worktree dir
```

`flywheel add app` scaffolds a builder + workload from a worktree directory
under `workspaces_root` — it tab-completes the available dirs and derives the
app name from the repo (or pass `--name`). It also records the worktree in
`flywheel.yaml` so the GitOps repo stays reproducible across machines; see
[Apps & workspaces](#apps--workspaces) for sharing, cloning, and publishing.

`flywheel up` prints the URL to visit (`https://<app>.<domain>:<https_port>/`).
To reach it in a browser you also need local name resolution — see
[Local DNS](#local-dns--reaching-your-apps-in-the-browser).

`flywheel init <path>` also works if you'd rather pass the target dir
explicitly. The age key is written to
`~/.config/flywheel/<name>/age.key` (mode 0600); the ports + absolute repo path
are recorded in `~/.config/flywheel/allocations.json`.

## Joining an existing Flywheel repo

The Quickstart above is for creating a **new** environment. If a teammate
already ran `flywheel init` and pushed the repo, you bring it up on your machine
without `init` — and with no key handoff. Everything local needs is committed:
the config (ports, cluster, namespaces, domain) in `flywheel.yaml`, *and* the
local SOPS age key at `clusters/local/age.key`:

```sh
# 1. Clone the existing gitops repo
git clone <repo-url> && cd <repo>

# 2. Bring up the cluster (also clones any declared app worktrees)
flywheel up --clone
```

> **Do not run `flywheel init` in a clone.** `init` is for scaffolding a new
> repo — it refuses a non-empty directory and would re-allocate ports and
> re-scaffold over the committed config.

The committed local key is safe by design — it only ever decrypts
`clusters/local/*` dev secrets on your localhost cluster, and every other
environment's key (prod, staging) stays out of git. See the
[Onboarding guide](docs/guides/onboarding.md) for why, the legacy fallback for
older repos that predate the committed key, and the port-collision caveat.

## Installation

### From source (recommended today)

Flywheel builds with the Go toolchain (see [`go.mod`](go.mod) for the required
version) plus the `docker` CLI for the runtime images.

```sh
git clone https://github.com/cobr-io/flywheel
cd flywheel
make install      # build (version-stamped) + runtime images + shell completions
```

`make install` runs three steps:

* **`make build`** — `go install -ldflags "-X …BuildVersion=$(git describe …)"`
  into `$(go env GOBIN)` (defaults to `~/go/bin`). The stamp is what
  `flywheel version` prints and what `init` records as `flywheel.version`; an
  unstamped `go build` reports `v0.0.0-dev`.
* **`make images`** — builds the three runtime images locally as
  `flywheel-dev/<name>:dogfood` (needed for [Dogfood mode](#dogfood-mode); a
  normal install against published images doesn't require them).
* **`make completions`** — installs the shell-completion script for your
  `$SHELL` (zsh/bash/fish), replacing any older copy. `make completions-all`
  does all three shells; you can also emit one with `flywheel completion <shell>`.

Make sure `$(go env GOBIN)` (or `~/go/bin`) is on your `$PATH`. Run
`make help` to list every target.

To build just the binary without the images or completions:

```sh
make build
```

### Release binaries

Tagged releases are published from [`.github/workflows/release.yml`](.github/workflows/release.yml);
see the [Releases page](https://github.com/cobr-io/flywheel/releases) for
prebuilt artifacts. You can also install a pinned tag straight from the module
path:

```sh
go install github.com/cobr-io/flywheel/cmd/flywheel@vX.Y.Z
```

Note that `go install` produces a binary stamped `v0.0.0-dev` rather than a real
version; `make build` from a checkout stamps the git ref. A Homebrew tap is
planned for a later release.

### Prerequisites

`git`, `k3d`, and the `docker` CLI + daemon (Docker Desktop, or Colima/podman
with a separately-installed `docker` CLI), plus `mkcert`
(`brew install mkcert`) for local TLS. Run:

```sh
flywheel doctor          # full host prerequisite check
flywheel doctor --quick  # the minimal subset `up` requires
```

### Windows (WSL)

There is no native Windows build — run Flywheel inside a **WSL2** Linux distro.
A few things need WSL-specific care (filesystem location, the docker daemon, and
mkcert trust across the WSL/Windows boundary). See the
[Windows (WSL) guide](docs/guides/windows-wsl.md).

## Usage

Flywheel is a [cobra](https://github.com/spf13/cobra) CLI; run `flywheel --help`
or `flywheel <command> --help` for full flag details. The core commands:

| Command | What it does |
|---|---|
| `flywheel init [<path>]` | Scaffold a GitOps repo (cwd, or the given path). |
| `flywheel up` | Reconcile the cluster to `flywheel.yaml` — creates k3d + Flux if needed. |
| `flywheel down` | Delete the cluster + local registry (destructive). |
| `flywheel add app <dir>` | Scaffold a per-app builder + workload from a worktree dir. |
| `flywheel publish-app <name>` | Promote a `local_only` app once its worktree has a remote. |
| `flywheel use <branch>` | Choose which gitops branch Flux deploys (opt-in branch following). |
| `flywheel doctor` | Check host prerequisites (`--quick` for the minimal subset). |
| `flywheel clean` | Opt-in destructive cleanup of orphaned PVCs. |
| `flywheel version` | Print the build version. |

Global flags: `-v/--verbose` surfaces k3d/docker/kubectl chatter that's hidden
by default; `--no-color` (or `NO_COLOR`) disables ANSI colour and glyphs.

`down` tears the environment down (deletes cluster + registry); `up` recreates
it from scratch. Several commands support shell completion — e.g.
`flywheel add app <TAB>` lists worktree dirs under `workspaces_root` and
`flywheel use <TAB>` lists local branches.

### Apps & workspaces

`flywheel add app <dir>` records each app's worktree in a `workspace:` block in
`flywheel.yaml` — its remote URL, or `local_only: true` when the worktree has no
remote yet. Because that block is committed, a fresh clone of the GitOps repo on
another machine can populate every sibling repo in one step: `flywheel up`
clones any declared worktree that's missing (it prompts on a TTY; `flywheel up
--clone` / `--no-clone` skip the prompt).

A `local_only` app must be published before it can be merged to the integration
branch — push its worktree to a remote, then run `flywheel publish-app <name>`.

### Deeper guides

* [Joining an existing repo](docs/guides/onboarding.md) — bring up a repo a
  teammate already created (the age key, SOPS recipients, port collisions).
* [Branch following & `flywheel use`](docs/guides/branch-following.md) — why the
  gitops/self repo is opt-in follow, and how `flywheel use` works.
* [Build secrets](docs/guides/build-secrets.md) — supplying secrets to builds.
* [Local DNS](docs/guides/local-dns.md) — resolving `*.<domain>` to your apps in
  the browser (dnsmasq, gotchas, TLS).
* [Dogfood mode](docs/dev/dogfood.md) — hacking on the runtime images.
* [Design doc](docs/designs/2026-05-15-harness-template-design.md) — the
  approved architecture.

## Configuration

Each repo is driven by a [`flywheel.yaml`](templates/client-skeleton/flywheel.yaml.tmpl)
at its root, written by `flywheel init`. Key sections:

```yaml
schema: v1alpha1

flywheel:
  version: v0.1.0          # tag of cobr-io/flywheel the repo is pinned to

client:
  name: acme               # names the cluster/registry/labels; not a tenancy concept
  org: cobr-io

cluster:
  name: acme-local
  registry: acme-local-registry
  registry_port: 50001
  http_port: 8080
  https_port: 8540         # browser URLs are https://<app>.<domain>:8540/
  servers: 1
  agents: 2
  k3s_image: v1.34.1-k3s1

namespaces:
  flywheel: flywheel-system
  apps: apps

flux:
  interval_local: 10s      # Flux reconcile cadence (apps tier)
  iac_interval: 10s        # reconcile cadence for the infra tier

local:
  domain: localdev.me      # apps are served at <app>.<domain>

sops:
  age_recipients_local:
    - age1...               # age recipients for SOPS-encrypted local secrets
```

Per-developer overrides go in a gitignored `flywheel.yaml.local` (see
[Dogfood mode](#dogfood-mode)). Ports and the repo path are also tracked
globally in `~/.config/flywheel/allocations.json` so multiple local clusters
don't collide.

### Local DNS — reaching your apps in the browser

`flywheel up` tells you to visit `https://<app>.<domain>/`, where `<domain>` is
`local.domain` from `flywheel.yaml` (default `localdev.me`). Two things have to
line up for that to work in a browser:

* **Port.** The cluster publishes HTTPS on `cluster.https_port` (default
  `8540`), not `443` — so the URL is `https://<app>.<domain>:8540/`.
* **Name resolution.** `<app>.<domain>` must resolve to `127.0.0.1`;
  `localdev.me` is not a public wildcard resolver, so you provide it locally.

The quick way is one `/etc/hosts` line per app:

```sh
echo "127.0.0.1 hello.localdev.me" | sudo tee -a /etc/hosts
```

For a wildcard that covers every current and future app (dnsmasq), plus the
`dig`-vs-`dscacheutil` and non-standard-port gotchas and the mkcert/TLS step,
see the [Local DNS guide](docs/guides/local-dns.md).

## Promoting to production

Because the local cluster runs the real GitOps control plane, promoting an app
to a remote cluster is mostly a matter of adding a sibling overlay — the same
`apps/base` flows `overlays/local` → `overlays/prod`, reconciled by the same
Flux. What drops out in prod is exactly the local-only machinery: the dev loop
(git-server, git-auto-sync, in-cluster builds) and Flywheel's local infra
(Traefik + mkcert), since prod brings its own ingress, DNS, certs, and CI-built
images.

Today this is a documented convention rather than a CLI verb — Flywheel does not
provision or bootstrap a remote cluster for you. The boundary, rationale, and
the shape of a `clusters/prod` tree are written up in the
[prod-promotion feasibility doc](docs/designs/2026-06-04-prod-promotion-feasibility.md).

## Dogfood mode

"Dogfood mode" is for hacking on the three runtime images (`git-server`,
`git-auto-sync`, `image-builder-controller`) themselves, rather than running the
published `ghcr.io/cobr-io/*` ones — you build them locally and pin the refs via
a gitignored `flywheel.yaml.local`. The full workflow, plus the two dev-loop
gotchas (Flux reverting `kubectl` edits, and the stale embedded-manifest cache),
is in the [Dogfood mode guide](docs/dev/dogfood.md).

## Contributing

Contributions are welcome. Flywheel is a standard Go module.

### Dev setup

```sh
git clone https://github.com/cobr-io/flywheel
cd flywheel
make build        # compile + install the version-stamped binary into GOBIN
```

For work that touches the runtime images or a live cluster, see
[Dogfood mode](#dogfood-mode) and use `make install` / `make images`.

### Tests

```sh
go test ./...     # unit + integration tests
make e2e          # the full k3d end-to-end suite (runs scripts/e2e.sh)
```

`make e2e` runs `scripts/e2e.sh` — the same suite as the `k3d-e2e` job in CI
([`.github/workflows/test.yml`](.github/workflows/test.yml)) — which spins up a
real k3d cluster and walks the init → up → scenarios → down lifecycle. It needs
`k3d` and a running docker daemon.

### Branch & PR conventions

* Branch off `main`; name branches by intent, e.g. `docs/…`, `fix/…`,
  `feat/…`, `design/…`.
* Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`type(scope): summary`), e.g. `feat(up): …`, `fix(add): …`,
  `docs(readme): …`.
* Open a PR against `main`. CI (`test` workflow: `go-test` + `k3d-e2e`) must be
  green before merge.

### Filing issues

File bugs and feature requests on the
[issue tracker](https://github.com/cobr-io/flywheel/issues). For a bug, include
your OS, docker runtime (Docker Desktop / Colima / podman), `flywheel version`,
and the failing command with `-v/--verbose` output.

## License

See [`LICENSE`](LICENSE) for the full terms. © cobr.io.
