# Orphan prune (`up` step 11e)

`flywheel up` reaps its **own** superseded machinery — the dev-loop resources a
version bump stops rendering (the bug in issue #27: the old `git-auto-sync-self`
Deployment left Running after the deploy-ref migration). This note is the
contract for maintainers; the code is `internal/cli/converge/prune.go`
(`PruneOrphanedMachinery`), wired in at `internal/cli/up/up.go` step 11e.

This is **not** destructive reconciliation of the git-managed layers — Flux owns
those (every Flux Kustomization is `prune: true`). `up` only reaps the
*recreatable* machinery it applies imperatively itself.

## The label is the membership marker

Every resource `up` applies imperatively carries
`app.kubernetes.io/managed-by: flywheel`:

| Resource set | Where the label is set |
|---|---|
| dev-loop base (git-server, controllers, buildkitd, IUA, inotify, RBAC, netpol) | `manifests/dev-loop/base/kustomization.yaml` `labels:` block |
| `flywheel-config` ConfigMap | `converge.ApplyFlywheelConfig` (Go) |
| bootstrap flux-system tree (namespaces, GitRepositories, Flux Kustomizations) | `templates/bootstrap/clusters/local/flux-system/kustomization.yaml.tmpl` `labels:` block |
| secrets (`sops-age`, `local-cert`, `mkcert-ca`) | `up.managedByFlywheel()` (Go) |

**Anything Flux reconciles from the gitops repo is deliberately NOT labelled** —
app/infra workloads, and the per-app `git-auto-sync-{app}` sidecars
(`manifests/per-app-template/git-auto-sync.yaml.tmpl`). The label-scoped scan
therefore can't even see them. **If you add a new resource that `up` applies
directly, label it. If you add a resource Flux applies, do not.**

## How the prune decides

1. **Keep set** = every resource *this* `up` run actually applied, captured from
   `ApplyDevLoop` + the bootstrap `ApplyKustomizeTracked` (the apply path returns
   a `ResourceRef` per object that landed).
2. **Scan universe** = the distinct GroupKinds present in the keep set, minus the
   denylist. Deriving it from the keep set keeps the prune in lockstep with the
   version being applied — it only scans kinds this `up` itself manages.
3. For each scanned kind, list `managed-by=flywheel` objects cluster-wide and
   delete any **not** in the keep set.

Gated on both imperative applies (11a dev-loop, 11d bootstrap) succeeding, so a
resource that *failed* to apply this run is never mistaken for an orphan.

## Safety: `up` must never remove a running app/infra workload

Three independent guards (see also the test `prune_test.go` and the design doc
`docs/designs/2026-06-28-isolate-dev-loop-image-bumps-design.md`):

1. **Label scope** — app/infra + per-app sidecars are unlabelled, so the scan
   never lists them.
2. **Kind denylist** (`pruneDenylist`) — never delete:
   - `Namespace` — deleting one cascade-deletes everything inside;
   - `PersistentVolumeClaim` — state; `flywheel clean` reaps these;
   - `Secret` — e.g. `sops-age`; losing it breaks SOPS decryption;
   - `Kustomization` (`kustomize.toolkit.fluxcd.io`) — deleting one makes Flux's
     finalizer prune every workload it manages. **This is what guarantees `up`
     can't tear down app/infra via a Flux cascade.**
   - `GitRepository` (`source.toolkit.fluxcd.io`) — deleting one breaks the
     source every client Kustomization reads.
3. **Keep-from-this-run** + the apply-success gate (above).

For denylisted kinds the label is provenance only — present, but the prune never
acts on them.

## Known caveat

Orphans created by a version that **predates the label** are not auto-reaped
(they were never labelled). That's intentional — going forward, any machinery a
labelling version applies and later supersedes *is* caught. A pre-label orphan
(e.g. an existing `git-auto-sync-self` on a long-lived cluster) needs a one-time
`kubectl delete deploy git-auto-sync-self -n flywheel-system`.

## End-to-end validation

Verified on a throwaway k3d cluster: inject a labelled `git-auto-sync-self`
Deployment plus spare-cases (unlabelled sidecar, unlabelled `apps` workload,
labelled Secret, labelled Flux Kustomization), re-run `up` → only
`git-auto-sync-self` is pruned; all four spare-cases and the real dev-loop
machinery survive.
