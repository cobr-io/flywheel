# Documented manual-authoring structure in the `flywheel init` skeleton

**Date:** 2026-06-03
**Status:** Approved — *partially superseded* (see note)

> **The "`infra/` has no `base/`" decision in this doc was reversed the next day
> by [2026-06-04 flywheel new-env](2026-06-04-flywheel-new-env-design.md).** To
> make client infra promotable across environments, `flywheel init` now ships an
> `infra/base/` and points `infra/overlays/local` at `../../base`, and
> `infra/README.md` no longer carries the "why there is no base" framing. The
> rest of this design (the per-area `README.md`s and manual-authoring guidance)
> shipped as written and still stands.

## Problem

After `flywheel init`, the bootstrapped client repo gives almost no guidance
for adding apps, builders, or infra **by hand** — i.e. without running
`flywheel add-app`. The only hints today are:

- one-line comments in the otherwise-empty `apps/base/kustomization.yaml`,
  `builders/base/kustomization.yaml`, and `infra/overlays/local/kustomization.yaml`;
- a terse "Layout" list in the top-level `README.md`, part of which is
  actively misleading (it presents `clusters/local/flux-system/` as a repo
  directory, when that tree is rendered ephemerally by `flywheel up` and never
  committed).

A user who wants to add something the CLI doesn't scaffold — most importantly an
**off-the-shelf** dependency (a public Helm chart, an OCI artifact, a
community operator) for which there is *no* `add-app` generator — has to
reverse-engineer the conventions from the embedded templates.

## Goal

Add scoped, durable documentation to the bootstrapped repo that explains each
editable directory's role and conventions, organised around the repo's real
decision point:

> **Are you building this yourself (needs the builders / dev-loop mechanism),
> or consuming it off-the-shelf (a Helm chart / OCI artifact — no builder)?**

This is purely a documentation change. The platform already supports every path
described (verified below); nothing in the runtime or the CLI changes.

## Context discovered

The decisions below rest on these facts about the current (v0.1) system:

- **`flywheel init` output** (from the `new` golden fixtures) is exactly:
  `apps/base/kustomization.yaml`, `apps/overlays/local/kustomization.yaml`,
  `builders/base/kustomization.yaml`, `builders/overlays/local/kustomization.yaml`,
  `infra/overlays/local/kustomization.yaml`, `flywheel.yaml`, `README.md`,
  `.gitignore`, `.sops.yaml`, plus state files. **No `clusters/` directory.**
- **`clusters/local/flux-system/` is ephemeral.** `flywheel up` renders it from
  embedded templates into a tmpdir, applies it directly, and deletes it
  (`renderBootstrap` / `defer os.RemoveAll(bootstrapDir)` in
  `internal/cli/up/bootstrap.go` + `up.go`). It is intentionally kept out of the
  committed repo (avoids leaking per-developer image overrides from
  `flywheel.yaml.local`, and avoids the git-auto-sync ↔ overlay reset race).
- **`add-app` scaffolds two trees** and appends to two kustomizations:
  `builders/base/<name>/` (GitRepository, build-config ConfigMap, git-auto-sync,
  ImageRepository, ImagePolicy) from `manifests/per-app-template`, and
  `apps/base/<name>/` (Deployment/Service/Ingress) from `manifests/apps-template`.
- **Off-the-shelf is fully supported today.** The embedded Flux install
  (`internal/cli/flux/install.yaml`) includes the full suite —
  `source-controller, kustomize-controller, helm-controller,
  notification-controller, image-reflector-controller,
  image-automation-controller` — with the `HelmRelease`, `HelmRepository`,
  `HelmChart`, and `OCIRepository` CRDs present. So consuming a public chart
  needs no new capability, only documentation.
- **Image automation scans the whole repo.** The single
  `ImageUpdateAutomation` (`manifests/dev-loop/base/image-update-automation.yaml`)
  uses `update.path: ./`, so `{"$imagepolicy": ...}` setter markers are honoured
  anywhere — including a self-built operator workload placed under `infra/`.
- **Why `infra/` has no `base/`.** The base/overlay split exists where the
  *client* owns the base content (`apps/base/`, `builders/base/` — where
  `add-app` writes; `overlays/local/` references `../../base`). The infra base is
  **Flywheel's**, not the client's: it lives in the flywheel mirror at
  `manifests/infra/overlays/local-<profile>` and is reconciled by a *separate*
  Flux Kustomization (`flywheel-infra`, sourcing the `flywheel` GitRepository).
  kustomize cannot reference another git source inline, so the "wrap" is
  expressed as that separate Kustomization. The client's `infra/overlays/local/`
  is a second, purely-additive Kustomization (`client-infra`) layered on top —
  there is no client-owned base for an overlay to point at, hence only
  `overlays/local/`.
- **v0.1 is local-only by design.** Every overlay is `local` (or the
  `local-mkcert` / `local-tailscale` profiles). The approved architecture doc
  (`2026-05-15-harness-template-design.md`) defers `clusters/prod/`,
  `*/overlays/prod/`, and a `flywheel new-env prod` subcommand to v0.2+. The
  generated READMEs therefore describe **only what exists today** and make no
  reference to prod / multi-environment.

## Approach

Add a `README.md` at the **root** of each editable top-level directory (`apps/`,
`builders/`, `infra/`) and update the top-level `README.md`. Root placement
means the files sit beside no `kustomization.yaml`, so there is zero kustomize
interaction (and the explicit `resources:` lists ignore stray files regardless).

All files live in `templates/client-skeleton/` as `.tmpl` and render through the
existing Go-template pipeline (same as today's `README.md.tmpl`), so they can
interpolate `{{ .ClientName }}`, `{{ .Profile }}`, `{{ .FlywheelRepoURL }}`, etc.

Depth is **conventions + pointer, prose only** — no copy-pasteable YAML:

- For the **self-built** path the pointer is `flywheel add-app`, which generates
  the manifests, so prose + a pointer suffices.
- For the **off-the-shelf** path there is no generator, but the decision was to
  keep every README uniformly prose-only: name the kinds to use and where they
  go, and let the reader author the manifest. (Revisit if users find this too
  thin in practice.)

### Files added / changed (all under `templates/client-skeleton/`)

**`apps/README.md.tmpl`** *(new)*
- Apps hold deployable workloads under `apps/base/<name>/`, assembled for the
  cluster by `apps/overlays/local/`, wired via `apps/base/kustomization.yaml`'s
  `resources:` list.
- *Self-built* → run `flywheel add-app` (generates the builder + workload).
- *Off-the-shelf* → add a `HelmRepository` + `HelmRelease` (or an
  `OCIRepository`, or a remote kustomize resource) under `apps/base/<name>/`.
  **No builder needed.**
- Cross-link: *building it yourself? see `builders/README.md`.*

**`builders/README.md.tmpl`** *(new)*
- Builder folders under `builders/base/<name>/` exist **only for images you
  build yourself**. Lists the resources a folder contains (GitRepository,
  build-config ConfigMap, git-auto-sync Deployment, ImageRepository,
  ImagePolicy) and which namespace each lands in.
- Off-the-shelf charts/images **never** go here.
- Pointer to `flywheel add-app`; cross-link to `apps/README.md`.

**`infra/README.md.tmpl`** *(new)*
- Explains **why there is no `base/`**: the infra base is Flywheel's (reconciled
  from the mirror by the separate `flywheel-infra` Flux Kustomization);
  `infra/overlays/local/` is your additive layer (reconciled by `client-infra`).
- *Off-the-shelf operator* (e.g. CNPG, cert-manager) → a `HelmRelease` here,
  no builder.
- *Self-built operator* → a builder folder under `builders/base/<name>/` plus
  the operator workload here, with an `$imagepolicy` setter marker (image
  automation scans the whole repo, so the bump lands).
- Notes that a client may create their own `infra/base/` if they accumulate
  resources worth sharing — the skeleton just doesn't ship an empty one.

**`README.md.tmpl`** *(top-level, edited)*
- Replace the current "Layout" list with:
  1. an **"Adding things by hand"** section framing self-built vs off-the-shelf
     across `apps/` + `builders/` + `infra/`, linking the three per-directory
     READMEs;
  2. a corrected **"Flux entrypoint (`clusters/`)"** note explaining that
     `clusters/local/flux-system/` is rendered and applied by `flywheel up` and
     is **not a file in this repo** (fixing today's misleading wording). No
     `clusters/README.md` is created.

## Out of scope (YAGNI)

- **No empty `infra/base/`.** Documented as an option; not scaffolded.
- **No committed `clusters/`.** The ephemeral design is intentional (avoids
  leaking per-developer image overrides and the git-auto-sync reset race);
  `clusters/` is documented in the top-level README instead of getting its own
  directory README.
- **No inline YAML examples.** Prose conventions only.
- **No prod / multi-environment documentation.** v0.1 is local-only; the v0.2+
  multi-env story is not described in the generated READMEs.

## Test plan

- Regenerate the `new` golden fixtures under
  `internal/cli/initcmd/testdata/golden/{mkcert,tailscale}/` — adding skeleton files
  changes them.
- Update `internal/cli/initcmd/embedded_skeleton_test.go` if it asserts file
  counts / paths.
- `go test ./internal/cli/initcmd/...` green.
- Spot-check a rendered `flywheel init` in a temp dir: correct `{{ .ClientName }}`
  / `{{ .Profile }}` / `{{ .FlywheelRepoURL }}` substitution and intact
  cross-links between the READMEs.

## Open questions / risks

- The READMEs reference live names (`flywheel add-app`, the `flywheel-infra` /
  `client-infra` Kustomizations, the apps/flywheel-system namespaces). If any are
  renamed later the docs drift — the same risk the existing READMEs already
  carry. Acceptable for v0.1.
- Prose-only off-the-shelf guidance may prove too thin given there's no
  generator to lean on; if so, a follow-up can add a minimal `HelmRepository` +
  `HelmRelease` snippet to `apps/README.md` and `infra/README.md`.
