# Flywheel

**A single-binary CLI for a production-faithful local GitOps dev loop — `git
commit` lands in a running pod on a real Flux-driven k3d cluster in seconds.**

[![CI](https://github.com/cobr-io/flywheel/actions/workflows/test.yml/badge.svg)](https://github.com/cobr-io/flywheel/actions/workflows/test.yml)
[![Latest release](https://img.shields.io/github/v/release/cobr-io/flywheel?sort=semver)](https://github.com/cobr-io/flywheel/releases)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

> **Status:** under active development (v0.1).

Flywheel gives you a local Kubernetes environment running the **same GitOps
control plane you'd run in production** — [Flux](https://fluxcd.io) reconciling
Kustomize overlays, [SOPS](https://github.com/getsops/sops)-encrypted secrets,
Traefik ingress with real TLS — with a **fast inner loop** wired on top so a
`git commit` becomes a running pod in seconds, no external CI or registry in the
path.

The point is fidelity. The `base/` + `overlays/` layout Flux reconciles on your
laptop is the same one that promotes to a real cluster; only the cloud-specific
infra and the local build loop differ. No "works in the dev shim, breaks in real
Flux" surprises — the pipeline *is* the real one.

`flywheel` is a single static Go binary that embeds the Flux manifest tree and
the GitOps-repo skeleton, so nothing else needs cloning once it's installed.
Three runtime images on `ghcr.io/cobr-io/` (`git-server`, `git-auto-sync`,
`image-builder-controller`) provide the dev-loop machinery, pinned by the
version in your `flywheel.yaml`; bumping that line and re-running `flywheel up`
re-converges the cluster onto the new binary.

## The dev loop

Once you've pointed Flywheel at a working directory with `flywheel add app
<dir>`, every commit flows to a pod entirely on your machine:

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

No CI round-trip, no remote registry, no `docker build && kubectl set image` —
the cluster converges the same way Flux would converge production from a git
push, just with the build happening in-cluster.

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
git-server, the git-auto-sync sidecar, and in-cluster image builds — which drop
away when you promote to a real cluster where images come from CI. Everything
else is the production control plane; see
[Promoting to production](docs/designs/2026-06-04-prod-promotion-feasibility.md).

## Quickstart

Prerequisites: `git`, `k3d`, the `docker` CLI + daemon, and `mkcert`. Run
`flywheel doctor` to check them (`--quick` for the minimal subset `up` needs).

```sh
# 1. Install the CLI from source
git clone https://github.com/cobr-io/flywheel && cd flywheel && make install

# 2. Scaffold and launch a local GitOps environment
mkdir my-gitops && cd my-gitops
flywheel init            # scaffold the GitOps repo in-place
flywheel up              # bring up k3d + Flux, pull runtime images

# 3. Wire up an app with a live dev loop
flywheel add app <dir>   # scaffold a builder + workload from a worktree dir
```

`flywheel up` prints the URL to visit (`https://<app>.<domain>:<https_port>/`).
Reaching it in a browser also needs local name resolution — see the
[Local DNS guide](docs/guides/local-dns.md).

**Joining a repo a teammate already created?** Don't run `init`. Clone the repo
and run `flywheel up --clone` — everything local needs is committed, including
the local SOPS age key at `clusters/local/age.key`. See the
[Onboarding guide](docs/guides/onboarding.md).

## Installation

Flywheel builds with the Go toolchain (see [`go.mod`](go.mod)) plus the `docker`
CLI. From a checkout:

```sh
make install      # version-stamped binary + runtime images + shell completions
make build        # just the binary
```

`make install` installs the version-stamped binary into `$(go env GOBIN)` (put
it on your `$PATH`), builds the three runtime images locally for
[Dogfood mode](docs/dev/dogfood.md), and installs shell completions. You can
also `go install github.com/cobr-io/flywheel/cmd/flywheel@vX.Y.Z` (stamped
`v0.0.0-dev`). There is no native Windows build — run inside WSL2
([guide](docs/guides/windows-wsl.md)). A Homebrew tap is planned.

## Commands

Run `flywheel --help` or `flywheel <command> --help` for full flag details.

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

Global flags: `-v/--verbose` surfaces k3d/docker/kubectl chatter; `--no-color`
(or `NO_COLOR`) disables ANSI colour. `down` tears the environment down; `up`
recreates it. Several commands support completion (e.g. `flywheel add app
<TAB>` lists worktree dirs).

## Configuration

Each repo is driven by a
[`flywheel.yaml`](templates/client-skeleton/flywheel.yaml.tmpl) at its root,
written by `flywheel init`:

```yaml
schema: v1alpha1

flywheel:
  version: v0.1.0          # tag of cobr-io/flywheel the repo is pinned to

client:
  name: acme               # names the cluster/registry/labels
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
  interval_local: 10s      # reconcile cadence (apps tier)
  iac_interval: 10s        # reconcile cadence (infra tier)

local:
  domain: localdev.me      # apps are served at <app>.<domain>

sops:
  age_recipients_local:
    - age1...               # age recipients for SOPS-encrypted local secrets
```

Per-developer overrides go in a gitignored `flywheel.yaml.local`. Ports and the
repo path are also tracked in `~/.config/flywheel/allocations.json` so multiple
local clusters don't collide.

## Guides

* [Onboarding](docs/guides/onboarding.md) — join a repo a teammate created (age key, SOPS recipients, port collisions).
* [Upgrading & the version pin](docs/guides/upgrading.md) — how `up` keeps your binary and `flywheel.version` in sync.
* [Local DNS](docs/guides/local-dns.md) — resolve `*.<domain>` to your apps in the browser.
* [Branch following & `flywheel use`](docs/guides/branch-following.md) — opt-in branch deploys.
* [Build secrets](docs/guides/build-secrets.md) — supplying secrets to builds.
* [Bring-up without flywheel](docs/guides/flywheel-free-bringup.md) — run the cluster with stock Flux and no `flywheel` binary (no fast loop, no lock-in).
* [Dogfood mode](docs/dev/dogfood.md) — hacking on the runtime images.
* [Promoting to production](docs/designs/2026-06-04-prod-promotion-feasibility.md) — the prod-overlay boundary.
* [Design doc](docs/designs/2026-05-15-harness-template-design.md) — the approved architecture.

## Contributing

Contributions welcome — Flywheel is a standard Go module. Branch off `main`
(name by intent: `docs/…`, `fix/…`, `feat/…`), use
[Conventional Commits](https://www.conventionalcommits.org/), and open a PR
against `main` with green CI (`go-test` + `k3d-e2e`).

```sh
go test ./...     # unit + integration tests
make e2e          # full k3d end-to-end suite (scripts/e2e.sh)
```

File bugs and feature requests on the
[issue tracker](https://github.com/cobr-io/flywheel/issues); for bugs include
your OS, docker runtime, `flywheel version`, and the failing command with
`-v` output.

## License

See [`LICENSE`](LICENSE) for the full terms. © cobr.io.
