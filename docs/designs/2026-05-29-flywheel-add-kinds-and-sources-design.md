# `flywheel add`: kinds (app / infra) and sources (local / image / helm)

**Status:** Superseded (not implemented) — see note below
**Date:** 2026-05-29
**Author:** matthijs (with collaboration from Claude)

> **Superseded by [2026-06-02 add-app worktree decoupling](2026-06-02-add-app-worktree-decoupling-design.md).**
> This design proposed a unified `flywheel add --kind --source` verb that would
> *remove* `add-app` and rename `internal/cli/addapp` → `internal/cli/add`. It
> was never built. The 2026-06-02 design instead kept `add-app` (and its
> package) and re-scoped it around a worktree `<dir>`. The kinds×sources matrix
> below remains a useful sketch of a possible future, but does **not** describe
> shipped or planned-next behaviour. (The companion implementation plan is
> correctly marked "not started".)

## Problem

`flywheel add-app <name>` is the only scaffolding verb today, and it is
hard-wired to a single, opinionated case: an application whose source
lives in a sibling directory with a `Dockerfile`, built locally via the
Kaniko builder pipeline and deployed as a Deployment/Service/Ingress in
the `apps/` tree. It scaffolds **both** halves — the builder bundle
(`builders/base/<name>/`) and the workload (`apps/base/<name>/`) — wired
to the local registry with a Flux `$imagepolicy` marker.

That covers exactly one of several common operations. Users also need to:

1. **Add an app they are not actively developing** — e.g. an `nginx`
   Deployment + Service + Ingress pulling a public pre-built image from
   ghcr.io / Docker Hub. No builder, no local source.
2. **Add infra components / operators**, which split two ways:
   * **locally-developed** (sibling dir + `Dockerfile`, same pattern as
     apps) — e.g. an operator the team is writing;
   * **public** — e.g. CloudNativePG (cnpg), the Altinity ClickHouse
     operator — installed from an upstream Helm chart.

These are all legitimate, frequent operations that `add-app` cannot
express. The functionality `add-app` provides is still required; it is
one cell of a larger matrix.

## Model

Two independent axes:

* **kind** — `app` vs `infra`. Selects the destination tree
  (`apps/` vs `infra/`), the namespace semantics, whether a
  Service/Ingress is emitted (app only), and whether a Namespace is
  emitted (infra only).
* **source** — `local` vs `image` vs `helm`. Selects whether a builder
  pipeline is scaffolded (`local` only) and what the deploy artifact is
  (a workload for `local`/`image`, a HelmRelease for `helm`).

The axes are orthogonal: `kind` decides *where it lands and its shape*,
`source` decides *where the bits come from*. All six cells are in scope.
(A fourth source — raw manifest URL / remote kustomize base — was
considered and **rejected** for now; revisit if a concrete operator
ships only plain YAML.)

## Approach

Generalise `add-app` into:

```
flywheel add <name> --kind=app|infra --source=local|image|helm [flags]
```

* **Defaults** `--kind=app --source=local`, reproducing today's
  `add-app` behavior byte-for-byte — i.e. `flywheel add <name>` with no
  flags equals the old `flywheel add-app <name>`.
* **`add-app` is removed completely** — no alias, no shim, clean break.
  Flywheel is pre-1.0 (`v0.1.0-alpha`) with no external consumers, so
  there's nothing to deprecate gracefully. Every in-repo caller migrates
  to `flywheel add` in the same change (see Migration).

### Composable fragments

Rather than one mega-template with conditionals, or ~6 near-duplicate
per-cell template trees, the generator assembles each artifact from
**single-resource fragment templates** selected per cell. This was the
chosen generation strategy: maximum flexibility, each fragment trivially
readable, no `{{if}}` soup inside YAML.

New embedded partials under `manifests/fragments/*.yaml.tmpl`, one
Kubernetes resource each:

* `deployment.yaml.tmpl`
* `service.yaml.tmpl`
* `ingress.yaml.tmpl`
* `namespace.yaml.tmpl`
* `helmrepository.yaml.tmpl`
* `helmrelease.yaml.tmpl`

A Go assembler picks the fragment set for `(kind, source)`, renders each
to its **own file** in the destination dir, and generates a
`kustomization.yaml` that lists **exactly** the files emitted. Separate
files (vs today's single multi-doc `deployment.yaml`) give cleaner diffs
and let users delete a piece (e.g. drop the Ingress) without editing a
multi-doc file.

The **builder bundle** is the existing `manifests/per-app-template`
tree (GitRepository + ImageRepository + ImagePolicy + build-config +
git-auto-sync). It renders into `builders/base/<name>/` only when
`source=local`, unchanged from today.

### Composition matrix

| kind  | source | `builders/base/<name>/` | `<tree>/base/<name>/` contents |
|-------|--------|-------------------------|--------------------------------|
| app   | local  | builder bundle          | deployment *(registry ref + `$imagepolicy` marker)* + service + ingress |
| app   | image  | —                       | deployment *(public ref, no marker)* + service + ingress |
| app   | helm   | —                       | helmrepository + helmrelease *(targetNamespace = apps ns)* |
| infra | local  | builder bundle          | namespace + deployment *(registry ref + marker)* |
| infra | image  | —                       | namespace + deployment *(public ref)* |
| infra | helm   | —                       | namespace + helmrepository + helmrelease *(targetNamespace = `<name>`)* |

Rules:

* **Service + Ingress** are emitted **iff `kind=app`**. A directly
  routable HTTP workload is an app concern; operators don't get an
  Ingress by default.
* **Namespace** is emitted **iff `kind=infra`**, named from
  `--namespace` (default `<name>`). For `infra+helm`, the HelmRelease
  `targetNamespace` points at it.
* **`$imagepolicy` marker** on the Deployment image line is present
  **iff `source=local`** (it's the Flux image-update-automation target
  fed by the local builder). `source=image` writes the public ref
  verbatim with no marker.
* **infra workloads are Deployment-only.** Operators that need RBAC
  (ServiceAccount/ClusterRole/Binding) or a webhook Service get those
  hand-added; the scaffold says so in NextSteps. (Public operators
  almost always arrive via `helm` and bring their own RBAC.)
* **`helmrelease`** ships a pinned `version`, an `interval`, and an
  empty `values: {}` block for the user to edit.

## Components

### CLI

* `internal/cli/addapp` is **renamed to `internal/cli/add`** and grows
  the assembler + the `Run(Options)` entry point. `Options` gains `Kind`,
  `Source`, `Namespace`, `Port`, and the Helm fields (`HelmRepo`,
  `Chart`, `ChartVersion`); existing `Image`, `Context`, `Dockerfile`
  stay. There is no `addapp` shim package.
* `cmd/flywheel/main.go`: **replace** the `add-app` case with an `add`
  case (flag parsing for `--kind`/`--source`/etc.). The `add-app`
  command name is deleted — invoking it falls through to the existing
  `unknown command` path.

### Tree placement & Flux wiring

* **app** → `apps/base/<name>/`; append to `apps/base/kustomization.yaml`.
  Already reconciled by the `client-apps` Flux Kustomization
  (`apps/overlays/local` → `../../base`). **No new wiring.**
* **infra** → new **`infra/base/<name>/`** tree; append to
  `infra/base/kustomization.yaml`. The existing `client-infra` Flux
  Kustomization watches `infra/overlays/local` — so the wiring becomes
  `infra/overlays/local` → `../../base` → `infra/base/<name>/`. **No new
  Flux Kustomization needed.**
* **builder** → `builders/base/<name>/`; append to
  `builders/base/kustomization.yaml` (unchanged).

### Skeleton change

The client skeleton currently ships only `infra/overlays/local/`
(a patch area, `resources: []`); there is no `infra/base/`. Two skeleton
edits (in `templates/client-skeleton/`):

1. Add `infra/base/kustomization.yaml` (`resources: []`, with a comment
   mirroring `apps/base`).
2. Change `infra/overlays/local/kustomization.yaml` to reference
   `../../base` (retaining its role as the patch layer — patches and the
   base reference coexist).

Golden testdata under `internal/cli/initcmd/testdata/golden/*` updates
accordingly.

### Helm UX

* **TTY:** prompt sequentially — repo URL → chart → version → (infra)
  namespace. Each prompt pre-fills from any flag already passed
  (`--helm-repo`, `--chart`, `--chart-version`, `--namespace`).
* **Non-TTY (CI):** no prompts; a missing required value errors with the
  exact flag name to set. Keeps `add --source=helm` scriptable.
* Emits `helmrepository.yaml` (in the target namespace) and
  `helmrelease.yaml` referencing it by name, with the pinned version and
  an empty `values:` stub.

## Error handling

* **Preflight existence check.** Stat every destination the selected
  cell will write (`builders/base/<name>` and/or `<tree>/base/<name>`)
  before any render; error listing all that exist. Never leave a
  half-scaffolded repo. (Generalises today's two-path check to the cell's
  actual write set.)
* **infra self-healing.** If `infra/base/kustomization.yaml` is missing
  (repo created before this change) `add --kind=infra` creates it and
  repoints `infra/overlays/local` at `../../base`. Idempotent.
* **`appendResource`** reused unchanged (idempotent; `resources: []` →
  block sequence transition; re-add of same name is a no-op).
* **Validation:**
  * `local.domain` required whenever an Ingress is emitted
    (`kind=app` with `source` in {`local`, `image`}).
  * `--image` required for `source=image`.
  * Helm repo/chart/version required (via prompt or flag).
  * `name` is a DNS-1123 label (existing check); `--namespace` likewise.
* **No transactional rollback** — deferred as YAGNI, as today; preflight
  makes partial writes rare.

## Test plan

Table-driven over all six `(kind, source)` cells in
`internal/cli/add/add_test.go`. For each cell assert:

* the **exact set of files** written in `<tree>/base/<name>/` (and the
  builder bundle present iff `source=local`);
* the generated `kustomization.yaml` lists exactly those files;
* correct `namespace` / `targetNamespace`;
* `$imagepolicy` marker present **iff** `source=local`;
* Service + Ingress present **iff** `kind=app`;
* Namespace resource present **iff** `kind=infra`;
* the right parent kustomization(s) appended.

Plus:

* **infra self-healing** — starting from a pre-change skeleton (no
  `infra/base`), `add --kind=infra` creates it and repoints the overlay.
* **app+local parity** — `add <name>` (default flags) produces an
  `apps/base/<name>/` + `builders/base/<name>/` tree identical to the
  prior `add-app` golden output, guarding the clean-break against
  behavior drift.
* **non-TTY helm** — errors without the required flags; succeeds with
  them.
* Existing `addapp_test.go` cases move to `internal/cli/add/add_test.go`
  as the `app+local` cell.

The TLS / Ingress shape for the app workload was validated live on
2026-05-28 (see the add-app workload-scaffolding design); no new live
TLS validation is required. A follow-up may add a dev-loop scenario that
exercises `add --kind=infra --source=helm` end-to-end against a real
chart (e.g. cnpg), but that is out of scope here.

## Migration

**Generated client repos** are unaffected at the GitOps level — the
files `add-app` previously produced (`apps/base/<name>/`,
`builders/base/<name>/`) are byte-identical to what `add <name>`
produces. Existing builders/workloads are untouched; repos lacking
`infra/base/` are self-healed on first `add --kind=infra`; new repos get
the updated skeleton (`infra/base/` + overlay reference).

**The `add-app` command name disappears**, so every in-repo caller and
reference migrates to `flywheel add` in the same change:

* `cmd/flywheel/main.go` — `add-app` case replaced by `add`.
* `internal/cli/addapp/` — renamed to `internal/cli/add/`.
* `testdata/scenarios/lib.sh` and any dev-loop scenario invoking
  `flywheel add-app`.
* `README.md`, `CHANGELOG.md`.
* Comment strings that say *"scaffolded by `flywheel add-app`"* in the
  client-skeleton / per-app / apps template kustomizations and the
  golden testdata under `internal/cli/initcmd/testdata/golden/*`.

The historical design docs (`2026-05-15`, `2026-05-28`) keep their
`add-app` references as a record of the time and are not rewritten.

## Open questions

* Should the `helm` path also offer a values **file** (`--values
  values.yaml`) in addition to the inline stub? Defer until a chart needs
  more than a hand-edited inline block.
* Should `infra+local` scaffold a starter RBAC set (ServiceAccount +
  empty ClusterRole/Binding) rather than Deployment-only? Defer until we
  actually develop an operator locally and know the shape.
* `app+helm` and `infra+image` have no concrete use case yet but are
  implemented for matrix completeness; watch for whether they earn their
  keep or should be hidden.
