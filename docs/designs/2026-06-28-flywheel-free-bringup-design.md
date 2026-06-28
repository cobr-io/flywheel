# Flywheel-free bring-up — a committed vanilla Flux entrypoint

**Status:** implemented
**Date:** 2026-06-28
**Author:** matthijs (with collaboration from Claude)

> **Update (2026-06-28, post-merge):** the committed entrypoint was renamed from
> `clusters/local/flux-system/` to **`clusters/vanilla/flux-system/`** to keep it
> clearly distinct from flywheel's own (managed, runtime-rendered) `clusters/local/`
> entrypoint. Paths below reflect the new name; the in-cluster object names
> (`flux-system` GitRepository, `client-apps`/`client-infra`) and the shared
> `clusters/local/age.key` reference are unchanged.

## Problem

Once a repo is bootstrapped by `flywheel init`, the fast local dev loop is —
by design — only obtainable through the `flywheel` binary. That is the whole
reason flywheel exists and is not in question.

The problem is a side effect: the bootstrapped gitops repo cannot be brought up
**at all** without the binary, even by a client who is happy to forgo the fast
loop. This is implicit lock-in, and it is sharper than it first looks.

The investigation surfaced a non-obvious truth: flywheel does **not** pollute
the committed repo with dev-loop wiring. It deliberately keeps that wiring out
of git — the Flux entrypoint and all dev-loop plumbing are rendered to a tmpdir
at `flywheel up` time and applied imperatively (`internal/cli/converge/bootstrap.go`
doc comment: "we intentionally keep these files out of the client's committed
gitops repo"). What is committed is genuinely clean:

- `apps/base`, `apps/overlays/local` — plain Kustomize.
- `infra/base`, `infra/overlays/local` — plain Kustomize.
- `builders/base/<app>/*` — dev-loop-specific, but isolated under `builders/`.

So the lock-in is the **opposite** of pollution: the repo is *content without a
control plane*. There is no committed `clusters/local/flux-system/` — no Flux
`GitRepository`, no root `Kustomization`, nothing for plain Flux to consume. The
only entrypoint that exists is flywheel's runtime-rendered one, and it points at
artifacts only flywheel creates (an in-cluster git mirror, the
`flywheel/local-deploy` DEPLOY branch maintained by `git-deploy-controller`).

The fix is therefore **not** "decouple the dev loop from the manifests" (already
done) — it is "**write the missing vanilla entrypoint down in git**" so a client
can stand the system up with stock Flux and zero flywheel binary.

## Goals / non-goals

**Goals**

- A client can bring up **the same local k3d cluster**, sourcing the committed
  repo, with plain `flux` / `kubectl` and **no flywheel binary** — accepting no
  fast local dev loop.
- The complete answer to "how does this repo come up?" lives in committed YAML
  the client owns, not in the binary's head or in a bootstrap command's flags.
- Reuse the existing bootstrap template structure, stripped to the two client
  tiers (`apps/`, `infra/`). No new `flywheel up` runtime behaviour.
- Zero interference with normal flywheel operation.

**Non-goals**

- A real/remote/prod environment. Target is the same local k3d, minus the
  dev loop. (Prod promotion remains deferred — see
  `2026-06-04-prod-promotion-feasibility.md`.)
- Making the default app's dev-loop placeholder images pull. The success bar is
  **wiring self-sufficiency + a documented image step** (see § Image-ref gap),
  not an automated end-to-end running pod.
- An `eject`/`export` command. Delivery is static-at-`init`; reintroducing a
  binary step to obtain the escape hatch would be self-defeating.

## Decisions (from brainstorming)

| Dimension | Decision |
|---|---|
| Target environment | Same local k3d cluster, minus the dev loop |
| Delivery | Static — written by `init`, committed, owned by the client as plain YAML |
| Success bar | Wiring self-sufficiency + a documented image-ref step |
| Approach | Commit the full self-managed Flux entrypoint (source wiring lives in git) |

## Approach

Add a new skeleton subtree `templates/client-skeleton/clusters/vanilla/flux-system/`.
Because `init` already walks the whole skeleton with `render.Tree(SkeletonFS,
".", repoDir, values)`, new template files are rendered, written, and committed
automatically — **no new Go logic**, just templates plus a golden-tree update.

The new entrypoint is the existing bootstrap entrypoint stripped to the two
client tiers. Everything mirror-sourced or dev-loop-sourced is dropped:
`flywheel-infra`, `flywheel-dev-loop`, the builders Kustomizations,
`flywheel-config`, the `flywheel` mirror `GitRepository`, and the `self-source`
`GitRepository` on `flywheel/local-deploy`. The remaining `GitRepository` points
at **GitHub on the integration branch**, not the in-cluster mirror.

### Committed files

`clusters/vanilla/flux-system/`:

- **`source.yaml`** — `GitRepository/flux-system` (namespace `flux-system`):
  - `url: https://github.com/<org>/<name>.git` — best-effort from `client.org` /
    `client.name`, with a `# REPLACE if your remote differs` comment. (At `init`
    time the repo is freshly `git init`'d with no remote, so the real URL is not
    knowable — the client owns and corrects this line.)
  - `ref.branch: main` — the integration branch at init time.
  - `interval: 1m` — slow; there is no fast loop here.
  - a commented-out `secretRef: { name: flux-system }` stanza for private repos.
- **`infra.yaml`** — `Kustomization/client-infra` → `path: ./infra/overlays/local`,
  SOPS decryption via the `sops-age` secret.
- **`apps.yaml`** — `Kustomization/client-apps` → `path: ./apps/overlays/local`,
  `dependsOn: client-infra`, SOPS decryption.
- **`namespaces.yaml`** — the `apps` namespace only. (`flywheel-system` is
  dev-loop-only and not needed in vanilla mode.)
- **`kustomization.yaml`** — aggregator listing the four, so
  `kubectl apply -k clusters/vanilla/flux-system` works and CI can
  `kustomize build` it.

### Bring-up flow (documented, binary-free)

A new client-facing guide, `docs/guides/flywheel-free-bringup.md` (ships to
clients via the embed + copy path that all `docs/guides/*` follow):

```
1. flux install                                   # Flux controllers only
2. kubectl create secret generic sops-age \       # decrypt key, from committed
     -n flux-system --from-file=age.agekey=clusters/local/age.key
3. # (private repo only) create the `flux-system` git-auth secret (PAT / deploy key)
4. kubectl apply -k clusters/vanilla/flux-system    # source + apps + infra
```

No flywheel binary anywhere. The cluster can be any k8s — including the same k3d,
brought up with plain `k3d cluster create`.

### Image-ref gap (the documented image step)

The guide carries an explicit section. App manifests ship
`image: ...:0-placeholder # {"$imagepolicy": "flux-system:<app>"}`, and that
placeholder is rewritten **only** by the in-cluster IUA / builder / registry,
which do not exist in vanilla mode. So vanilla Flux reconciles the wiring green,
but pods cannot pull placeholder tags.

Resolution is documented, not automated: in a non-dev-loop world the client's own
CI publishes pullable images and the committed manifests reference those real
tags (or the client overrides per app). Vanilla mode proves the gitops wiring is
self-sufficient; running pods follow once real image refs are committed — which
is the client's responsibility outside the dev loop.

### Non-interference with `flywheel up`

Key safety property: flywheel's Flux `Kustomization`s use explicit `path:`
values (`./apps/overlays/local`, `./infra/overlays/local`, mirror paths) and
**never reconcile `./clusters/`**. The committed `clusters/vanilla/flux-system/`
therefore rides along in git (and onto the DEPLOY branch) but is inert during
normal flywheel operation — it is activated only by an explicit
`kubectl apply -k`.

The `GitRepository` reuses the canonical name `flux-system`, so applying the
vanilla entrypoint cleanly **toggles** the cluster's source from the in-cluster
mirror to GitHub. The two workflows are mutually exclusive ("with flywheel" vs
"without"); this is documented.

## Test plan

- **Golden tree.** Add the five rendered files to
  `internal/cli/initcmd/testdata/golden/default/`; the existing `init` golden
  test then asserts they are written and committed.
- **Kustomize build.** A test that `kustomize build clusters/vanilla/flux-system`
  succeeds (the entrypoint is well-formed). Confirm the client CI's `kubeconform`
  already tolerates Flux CRs — precedent exists because `builders/base/<app>/`
  already commits Flux CRs.
- **Manual verification.** A live reconcile cannot be unit-tested: on real k3d,
  run the four bring-up steps and confirm `client-infra` and `client-apps` go
  Ready with zero flywheel binary present.

## Open questions

- **URL placeholder strategy** — best-effort `github.com/<org>/<name>` guess
  (recommended) vs. a literal `REPLACE_ME`. The guess is friendlier and the
  client owns the line regardless.
- **GitRepository naming** — reuse the canonical `flux-system` name for a clean
  source toggle (recommended) vs. a distinct name to make accidental dual-apply
  impossible.
- **Branch / interval templating** — `branch: main` and `interval: 1m` are
  hardcoded literals in the new templates (the client-skeleton render does not
  currently expose `integration_branch` or the Flux interval as values). If a
  client changes `git.integration_branch`, they update the entrypoint too — it
  is their YAML. Plumbing these as real template values is a possible later
  refinement, deferred under YAGNI.
- **CI scope** — confirm the client repo's `kustomize-build` / `kubeconform` CI
  steps pick up (and pass on) `clusters/vanilla/flux-system/` without extra
  schema wiring.
