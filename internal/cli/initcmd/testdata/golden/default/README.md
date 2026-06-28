# acme-gitops

A [Flywheel](https://example.invalid/flywheel)-powered GitOps repository (pinned at
`v0.1.0`). It runs your apps on a local k3d cluster using the
**same GitOps control plane you'd run in production** — Flux reconciling
Kustomize overlays, SOPS-encrypted secrets, Traefik ingress with real TLS — with
a fast inner loop on top: `git commit` lands in a running pod in seconds, no CI
or registry in the path.

## Quick start

```
cd acme-gitops
flywheel up
```

This brings up a local k3d cluster `acme-local` with the full
sub-30-second commit-to-pod dev loop. First `up` takes ~60–90s on a warm
machine.

> **Cloning this repo fresh?** Don't run `flywheel init` (that's for creating a
> new repo) — just `flywheel up`. It reuses the committed config, and the local
> cluster's age key is committed at `clusters/local/age.key`, so a fresh clone
> decrypts and comes up with no key handoff. See
> [docs/onboarding.md](docs/onboarding.md) for the full flow.

## Layout

* `apps/` — your application workloads. See [apps/README.md](apps/README.md).
* `builders/` — build pipelines for images you build yourself. See
  [builders/README.md](builders/README.md).
* `infra/` — cluster-wide infrastructure patches. See
  [infra/README.md](infra/README.md).
* `flywheel.yaml` — version pin + answers. Bump the pin to roll Flywheel
  forward; everything else is set once at init time.
* `flywheel.yaml.local` — gitignored, per-developer overrides
  (`paths.workspaces_root`, optional image/port overrides).
* `AGENTS.md` — orientation for AI coding agents working in this repo.
* `.pre-commit-config.yaml` — commit hooks (see below).

## Commit hooks

This repo ships [pre-commit](https://pre-commit.com) hooks that run on every
commit:

* **yamllint** (`.yamllint`) — YAML style.
* **gitleaks** (`.gitleaks.toml`) — blocks committed secrets.
* **SOPS-shape guard** (`scripts/ci/check-sops-shape.sh`) — refuses any
  `*.enc.yaml` that isn't actually SOPS-encrypted, and any plaintext
  `kind: Secret` outside `*.enc.*`. This is the guard that keeps real
  secrets out of git.

`flywheel init` activates them automatically when `pre-commit` is installed.
If you cloned this repo fresh, activate them once:

```
pre-commit install
```

The hooks need `pre-commit` and mikefarah [`yq`](https://github.com/mikefarah/yq)
on your PATH — `flywheel doctor` checks for both.

## Continuous integration

Local hooks are bypassable (`git commit --no-verify`, or never running
`pre-commit install`), so the same checks run server-side in
[`.github/workflows/ci.yaml`](.github/workflows/ci.yaml) on every PR (and on
pushes to the integration branch):

* **lint** — `pre-commit run --all-files` (yamllint, gitleaks, the SOPS-shape
  guard, and the local-only app guard).
* **kustomize-build** — `kustomize build` on every `*/overlays/<env>`
  entrypoint; catches broken refs, missing files, and bad patches.
* **kubeconform** — strict schema validation of the rendered manifests.

To make these non-bypassable, mark the three jobs as **required status checks**
on your integration branch (Settings → Branches → branch protection).

## Decrypting secrets locally

`flywheel` generated this repo's age private key at a deterministic,
host-only path (never committed):

```
~/.config/flywheel/acme/age.key
```

`sops` doesn't probe that path on its own, so this repo ships an `.envrc` that
points it there via `SOPS_AGE_KEY_FILE`. With [direnv](https://direnv.net)
installed, run `direnv allow` once and `sops -d secret.enc.yaml` Just Works.

Without direnv, export it yourself before decrypting:

```
export SOPS_AGE_KEY_FILE="$HOME/.config/flywheel/acme/age.key"
sops -d secret.enc.yaml
```

## Adding things by hand

Every addition starts with one question: **do you build it yourself, or
consume it off the shelf?**

* **Build it** (active development) — run `flywheel add app <worktree>`. It
  scaffolds an app workload under `apps/base/<name>/` and a build pipeline
  under `builders/base/<name>/`.
* **Consume it** (a public Helm chart or prebuilt image) — add a
  `HelmRelease` (with its `HelmRepository` / `OCIRepository`) or plain
  manifests under `apps/` (an application) or `infra/` (a cluster-wide
  operator). No builder required.

Each directory's `README.md` has the details.

## Flux entrypoint (`clusters/`)

Two things are committed under `clusters/local/`:

* **`clusters/local/age.key`** — the SOPS age key for the **local dev cluster**.
  It's checked in on purpose: it only ever decrypts `clusters/local/*` dev
  secrets on your localhost cluster, so shipping it means a teammate can
  `git clone` and `flywheel up` with no key handoff. Every **other**
  environment's key (prod, staging, …) stays out of git — `.gitignore` ignores
  `clusters/*/age.key` except the local one, so those keys live in your homedir,
  never the repo.
* **`clusters/local/flux-system/`** — a vanilla, stock-Flux entrypoint
  (`GitRepository` → your remote, plus `apps`/`infra` Kustomizations). It lets
  you bring this cluster up with plain `flux` + `kubectl` and **no `flywheel`
  binary** — you forgo the fast loop, but the repo isn't captive to flywheel.
  See [docs/flywheel-free-bringup.md](docs/flywheel-free-bringup.md). `flywheel
  up` never touches it (it renders its own entrypoint — see below), so it just
  sits there inertly until you `kubectl apply -k` it.

The entrypoint **`flywheel up` actually uses is not committed**: it's rendered to
a tmpdir and applied directly on every run, because it carries per-developer,
per-run values (resolved image refs, the pinned Flywheel commit) that must not be
committed. It wires up two `GitRepository` sources — this repo (served by the
in-cluster git-server) and the Flywheel mirror at the pinned SHA — plus the Flux
Kustomizations that reconcile `apps/`, `builders/`, and `infra/`. That's the
machinery behind the fast loop; the committed vanilla entrypoint above is the
stripped-down, flywheel-free alternative.

## Bumping Flywheel

Edit `flywheel.version` in `flywheel.yaml`, then re-run `flywheel up`:

```
flywheel up   # re-converges the cluster to the new pinned SHA
```

See the Flywheel design and plan docs for the full mechanics.
