# AGENTS.md

Guidance for AI coding agents working in this Flywheel-managed GitOps repo.

This repo drives the **acme** cluster by GitOps: Flux reconciles
what's committed here into a local k3d cluster (`flywheel up`). For images you
build yourself, commit-to-pod is sub-30 seconds.

## The one question for every addition

**Do you build it yourself, or consume it off the shelf?** This decides where
things go and whether the dev loop applies.

* **In-house** — you have the source and are actively developing it. Gets the
  full dev loop: local buildkit build → push to the local registry → Flux image
  automation rolls the new tag out. Needs a **builder** under
  `builders/base/<name>/`.
* **Off the shelf** — a public Helm chart or a prebuilt image (CNPG,
  cert-manager, nginx, …). Pulled from upstream, **no local build, no builder** —
  just a `HelmRelease` (+ its `HelmRepository`/`OCIRepository`) or plain
  manifests.

## Layout

* `apps/` — your application workloads (Deployments/Services/Ingresses).
* `builders/` — build pipelines, one folder per in-house image. Off-the-shelf
  things never appear here.
* `infra/` — cluster-wide operators and platform services. `infra/base/` is
  **promotable** across environments.
* `flywheel.yaml` — version pin + answers; `flywheel.yaml.local` — gitignored
  per-developer overrides.
* No committed `clusters/local/` — the local Flux entrypoint is rendered by
  `flywheel up` on every run and intentionally not committed. (A `clusters/<env>/`
  *is* committed when you promote to a real cluster.)

Each directory has its own `README.md`. **Read it before editing that area** —
this file is only the map.

## Add an in-house app (the dev loop)

```
flywheel add app <worktree>
```

Scaffolds the workload under `apps/base/<name>/` (its image line carries an
`{"$imagepolicy": ...}` setter) **and** the build pipeline under
`builders/base/<name>/` (GitRepository, build-config, git-auto-sync,
ImageRepository, ImagePolicy), wiring both `kustomization.yaml`s. That is the
whole dev loop. See [apps/README.md](apps/README.md) and
[builders/README.md](builders/README.md).

## Add an off-the-shelf app

No builder. Add resources under `apps/base/<name>/`, then list the folder in
`apps/base/kustomization.yaml`:

* public Helm chart → `HelmRepository` + `HelmRelease`;
* OCI-packaged chart → `OCIRepository` + `HelmRelease`;
* plain upstream manifests → reference them as kustomize `resources`.

## Add infra / an operator

* **Off the shelf** (CNPG, cert-manager, …) → a `HelmRelease` (+ source) under
  `infra/base/<name>/` to run it everywhere, or `infra/overlays/local/` for
  local-only. No builder.
* **Built in-house** → a builder under `builders/base/<name>/` plus the operator
  workload in `infra/`, with an `{"$imagepolicy": ...}` setter on its image.
  Flux image automation scans the whole repo, so infra images bump just like
  apps.

See [infra/README.md](infra/README.md).

## Conventions for agents

* When you add a folder, add it to the parent `kustomization.yaml` under
  `resources:` — kustomize ignores anything not listed.
* Validate before committing: `kustomize build apps/overlays/local` (or
  `infra/overlays/local`).
* This is GitOps — Flux reverts live edits. Commit changes; don't `kubectl
  patch`/`edit` the cluster to make something stick.
* Never commit `flywheel.yaml.local`, and don't hand-author a
  `clusters/local/` entrypoint (`flywheel up` owns it). Committing a
  `clusters/<env>/` for promotion is fine.
* Secrets go in `*.enc.yaml` files, SOPS-encrypted (see `.sops.yaml`).
  A commit hook (`.pre-commit-config.yaml`) enforces this: a SOPS-shape guard
  that rejects unencrypted `*.enc.yaml` and any plaintext `kind: Secret`.
  Don't bypass it with `--no-verify`.
* Decrypting secrets locally: the age private key lives at
  `~/.config/flywheel/acme/age.key` (host-only, never committed).
  This repo ships an `.envrc` that exports `SOPS_AGE_KEY_FILE` to that path, so
  with [direnv](https://direnv.net) `sops -d secret.enc.yaml` Just Works.
  Without direnv, export it yourself first:
  `export SOPS_AGE_KEY_FILE="$HOME/.config/flywheel/acme/age.key"`.
