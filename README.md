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
Four runtime images on `ghcr.io/cobr-io/` (`git-server`, `git-auto-sync`,
`image-builder-controller`, `git-deploy-controller`) provide the dev-loop
machinery, pinned by the version in your `flywheel.yaml`; bumping that line and
re-running `flywheel up`
re-converges the cluster onto the new binary — applying the new machinery and
reaping any superseded machinery the previous version left running.

## The dev loop

Once you've pointed Flywheel at a working directory with `flywheel add app <dir>`, every commit flows to a pod entirely on your machine:

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/devloop-dark.svg">
  <img alt="The Flywheel dev loop: you commit → git-auto-sync pushes to the in-cluster git-server → image-builder-controller builds an image → Flux image-automation rolls it into the Deployment → your pod runs the new code, all on your machine" src="docs/assets/devloop-light.svg" width="720">
</picture>

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
# 1. Install the CLI (prebuilt binary)
curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/install.sh | bash

# 2. Scaffold and launch a local GitOps environment
mkdir my-gitops && cd my-gitops
flywheel init            # scaffold the GitOps repo in-place
flywheel up              # bring up k3d + Flux, pull runtime images

# 3. Wire up an app with a live dev loop
flywheel add app <dir>   # scaffold a builder + workload from a worktree dir
```

Need a specific version, or an install without `sudo`? See
[Installation](#installation).

`flywheel up` prints the URL to visit (`https://<app>.<domain>:<https_port>/`).
Reaching it in a browser also needs local name resolution — see the
[Local DNS guide](docs/guides/local-dns.md).

**Joining a repo a teammate already created?** Don't run `init`. Clone the repo
and run `flywheel up --clone` — everything local needs is committed, including
the local SOPS age key at `clusters/local/age.key`. See the
[Onboarding guide](docs/guides/onboarding.md).

## Installation

### Install script (recommended)

Downloads a prebuilt binary for your OS/arch from the
[latest release](https://github.com/cobr-io/flywheel/releases), verifies its
checksum, and installs it on your `$PATH` (darwin/linux × amd64/arm64):

```sh
curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/install.sh | bash
```

Re-run it to upgrade — it's idempotent and no-ops when the target version is
already installed. It deliberately does **not** auto-update: Flywheel pins its
version in `flywheel.yaml` (the source of truth), so the binary should track
that pin rather than float ahead of it.

It also installs shell tab-completions for your login shell (`$SHELL`) into that
shell's canonical autoload dir — best-effort, so it warns and continues if the
dir isn't writable. Restart your shell to pick them up, or set
`SKIP_COMPLETIONS=true` to skip.

Tune it with environment variables — note these go on the **`bash`** side of the
pipe, not the `curl` side:

| Variable | Default | Effect |
|---|---|---|
| `TAG` | latest | Install a specific release, e.g. `TAG=v1.2.3`. |
| `INSTALL_DIR` | `/usr/local/bin` | Where to put the binary (uses `sudo` only if the dir isn't writable). |
| `USE_SUDO` | auto | Set `false` to never elevate (pair with a writable `INSTALL_DIR`). |
| `FORCE` | `false` | Reinstall even when the target version is already present. |
| `SKIP_COMPLETIONS` | `false` | Set `true` to skip installing shell tab-completions. |

```sh
# pin a specific version
curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/install.sh | TAG=v1.2.3 bash

# user-local install, no sudo
curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/install.sh \
  | INSTALL_DIR="$HOME/.local/bin" USE_SUDO=false bash
```

There is no native Windows build — run inside WSL2
([guide](docs/guides/windows-wsl.md)). A Homebrew tap is planned.

### From source

Flywheel builds with the Go toolchain (see [`go.mod`](go.mod)) plus the `docker`
CLI. From a checkout:

```sh
make install      # version-stamped binary + runtime images + shell completions
make build        # just the binary
```

`make install` installs the version-stamped binary into `$(go env GOBIN)` (put
it on your `$PATH`), builds the four runtime images locally for
[Dogfood mode](docs/dev/dogfood.md), and installs shell completions. You can
also `go install github.com/cobr-io/flywheel/cmd/flywheel@vX.Y.Z` (stamped
`v0.0.0-dev`).

### Uninstall

The inverse of installing. By default it removes only the binary and the shell
completions — **caches and config are left alone**, because
`~/.config/flywheel` holds age private keys that are recovery-critical (see the
caution below).

```sh
# undo the install-script install (mirrors install.sh's INSTALL_DIR / USE_SUDO)
curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/uninstall.sh | bash

# undo a `make install` (removes $(go env GOBIN)/flywheel + completions)
make uninstall
```

It accepts the same `INSTALL_DIR` / `USE_SUDO` overrides as `install.sh` (put
them on the **`bash`** side of the pipe), plus two opt-in cleanup flags:

| Flag | Effect |
|---|---|
| `--purge` | Also remove the embed cache `~/.cache/flywheel` (regenerated on the next `init`/`up`). |
| `--purge-config` | Also remove `~/.config/flywheel` **entirely**, including age keys and per-cluster state. **Destructive and irreversible** — see the caution. |

```sh
# binary + completions + embed cache, but keep age keys/config
curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/uninstall.sh | bash -s -- --purge

# a user-local install lives elsewhere — point INSTALL_DIR at it
curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/uninstall.sh \
  | INSTALL_DIR="$HOME/.local/bin" USE_SUDO=false bash
```

> **Caution — `--purge-config` deletes your age keys.**
> `~/.config/flywheel/<name>/age.key` is the private key that decrypts your
> SOPS-encrypted state. Deleting it can make that state **permanently
> unrecoverable**. A plain uninstall never touches `~/.config/flywheel`; only
> `--purge-config` does, and it warns loudly first. Back up your keys before
> using it if you might still need any encrypted secrets.

## Commands

Run `flywheel --help` or `flywheel <command> --help` for full flag details.

| Command | What it does |
|---|---|
| `flywheel init [<path>]` | Scaffold a GitOps repo (cwd, or the given path). |
| `flywheel up` | Reconcile the cluster to `flywheel.yaml` — creates k3d + Flux if needed; also prunes superseded flywheel-managed machinery a version bump no longer renders (never your app/infra workloads). |
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

# Optional blocks — omit to take the defaults shown.
git:
  integration_branch: main  # branch the local-only guard refuses to let
                            # remote-less apps reach (default: main)

git_server:
  memory_limit: 128Mi       # memory limit for the in-cluster git-server
                            # (default: 128Mi) — see note below
```

The optional `git_server.memory_limit` sizes the in-cluster git-server that
serves your app worktrees to the build jobs. The `128Mi` default suits small
repos, but git's pack compression on `git-upload-pack` of a large monorepo can
spike past it and get the pod OOMKilled mid-build (the build then fails and app
pods stay `Pending`). Raise it (e.g. `512Mi` or `1Gi`) when building from
sizeable repos; the new limit is applied on your next `flywheel up`. Any
Kubernetes memory quantity is accepted (`128Mi`, `512Mi`, `1Gi`).

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
* [Dev-loop validation](docs/dev/dev-loop-validation.md) — reproduce the full happy path (init→up→add app→commit→reload) to confirm it still works.
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
