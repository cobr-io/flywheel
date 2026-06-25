# Prod clusters: feasibility & scope decision

**Date:** 2026-06-04
**Status:** Decided (feasibility) — defer CLI, ship docs + example
**Supersedes detail in:** `2026-05-15-harness-template-design.md` § Non-goals,
§ Open issues #4 (the original "v0.2+ when the data-platform client migrates" deferral — now with a
concrete rationale and a chosen boundary).

## Question

Should Flywheel support deploying to **prod** (a real remote cluster), and if so,
how far should its responsibility extend? This is a feasibility/scope decision,
not yet a design.

## Starting state (reviewed)

- **Design intent is local-only.** v0.1 deliberately writes only `clusters/local/`
  and `*/overlays/local/`. Multi-env is a stated Non-goal; a `flywheel new-env
  prod` subcommand and the config model ("per-env overlays vs a separate
  `flywheel.<env>.yaml`") are Open issue #4. A CI guard was even planned to fail
  the build if any `*/prod/` path appeared.
- **The code is local/k3d-coupled, not env-aware.** The `schema.File` has a single
  `cluster:` block of k3d concepts (`registry`, `registry_port`, `servers`,
  `agents`, `k3s_image`) and a top-level `local:` block; "local" is baked into
  field names (`flux.interval_local`, `sops.age_recipients_local`). The `up`
  pipeline *is* "create + bootstrap a local k3d cluster": `k3d.CreateRegistry`,
  `k3d.CreateCluster`, mirroring the Flywheel images into that local registry,
  an **in-cluster git-server mirror** as the Flux source, and a mkcert
  local-cert Secret.
- **What's reusable across envs:** the env-agnostic `apps/base/` and
  `builders/base/` content, plus the Flux Kustomization / SOPS / version-pin
  patterns. The base/overlay split was deliberately designed to allow a sibling
  `overlays/prod/` referencing the same base (no cloning).

## Decisions

### 1. Boundary: promotion-minimal (A), not provisioning (B)

Flywheel may *manage* a prod cluster **it did not provision** — render the env
overlay, point Flux at the real repo, apply to an existing remote kubecontext.
It does **not** provision the cluster or the cloud surface (EKS, ALB controller,
external-dns, cert-manager prod, IRSA). That surface is irreducibly
client-specific (the foundational doc keeps the data-platform client's operators "in the client repo
forever — never Flywheel territory") and stays out of scope.

### 2. Prod drops BOTH the dev-loop and flywheel-infra

The dev-loop (git-auto-sync, in-cluster git-server, image-builder-controller,
inotify) is a local-development construct with no prod relevance — dropped.
Flywheel's own infra base (traefik + mkcert/tailscale TLS profiles) is also
dropped: in prod the client brings their own ingress (ALB), DNS, and certs. So a
prod cluster runs **apps + the client's own infra only**:

```
prod cluster reconciles:
  apps/overlays/prod        # shared apps/base + prod patches
  infra/overlays/prod       # client-owned: ALB, external-dns, certs
  (NO dev-loop, NO flywheel-infra)
```

Both are already separate Flux Kustomizations, so excluding them is clean.

### 3. Consequence: prod support collapses to a thin wrapper — so defer the CLI

Once both are dropped, Flywheel's *unique* prod contribution is only:

1. **Bootstrap convenience** — one command to `flux install`, wire the
   `GitRepository` to the real GitHub repo (with a deploy-key Secret), and lay
   down the cluster Kustomizations pointing at the prod overlays.
2. **The cross-env promotion structure** — the same `apps/base` flowing
   local → prod (which is just kustomize, and already exists).

Critically, **Flywheel's signature value — propagating dev-loop/infra
improvements to N client repos via a one-line version bump — does not reach
prod**, because prod reconciles no Flywheel-versioned manifests. The pin is inert
in prod. Even the builders mechanism is dev-loop-flavored (in-cluster build via
Kaniko); in prod, images come from real CI, so at most stock Flux image
automation survives — nothing Flywheel-unique.

That makes a `flywheel up --env=prod` verb a thin `flux bootstrap` wrapper +
overlay convention + git-credential plumbing. Real but modest value (fleet
consistency, ergonomics, one mental model, lowering the Flux-expertise bar) — and
not justified to build speculatively with no concrete prod client and no
requirements to design against.

## What we'll do instead (now, cheap)

- A **"promoting to prod" guide** documenting the manual path: add a sibling
  `apps/overlays/prod` + a client-owned `infra/overlays/prod`, a `clusters/prod`
  Flux entrypoint pointed at the real repo, with the dev-loop and flywheel-infra
  excluded.
- An **example `clusters/prod` tree** showing the Flux bootstrap shape.
- The env-agnostic `apps/base` / `builders/base` already support this — no code
  change required.

## Deferred (until a concrete prod client forces requirements)

- ~~The `flywheel up --env=prod` (or `flywheel new-env`) CLI verb.~~ **Update:**
  the pure-scaffolder half shipped same-day as
  [`flywheel new-env`](2026-06-04-flywheel-new-env-design.md) (writes the
  `clusters/<env>/` entrypoint + overlays; touches no cluster). Still deferred:
  the cluster-touching `up --env=prod` bootstrap wrapper.
- The env-aware config model (Open issue #4) — decided when the verb is.
- The decoupling of "create k3d cluster" from "bootstrap Flux + apply overlays"
  in `up` (a healthy refactor the verb would need; worth doing then).
- Remote-cluster guard rails (`up`/`down` must hard-refuse k3d ops and remote
  deletion) — designed alongside the verb.

## Docs + example: decisions (resolved 2026-06-04) and how it's built

1. **Cloud-specificity → concrete EKS/ALB recipe.** The guide shows a real AWS
   path (ALB via the AWS Load Balancer Controller, external-dns on Route 53, ACM
   certs, IRSA), not a cloud-agnostic sketch — more immediately useful to the
   first real client. Account-specific values are literal `<placeholders>`.
2. **Placement → both the client skeleton and Flywheel's `docs/`, single source.**
   The canonical file lives once at `docs/guides/promoting-to-prod.md`. It is
   added to `//go:embed` and `flywheel init` copies it verbatim into the client
   repo at `<repo>/docs/promoting-to-prod.md` (a new `GuidesFS` option +
   `render.Tree` copy step, hashed into `.flywheel-state.yaml` so it rolls
   forward on `flywheel update`). There is no second editable copy — the
   skeleton does not duplicate it.
   - It's a plain `.md` (not `.md.tmpl`) using literal `<placeholders>`, so the
     Flywheel-docs copy and the client-repo copy are byte-identical with no
     "raw template vs rendered" divergence.
   - A docs guide is inert (no Flux reconciles it), so shipping it into fresh
     repos doesn't violate the v0.1 "no active prod scaffolding" rule.

Implemented in: `docs/guides/promoting-to-prod.md`, `embed.go`,
`internal/cli/initcmd/init.go` (+ `embedded_skeleton_test.go` guard, regenerated
goldens).
