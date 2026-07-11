package converge

import (
	"context"
	"io"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/cobr-io/flywheel/internal/cli/applier"
	"github.com/cobr-io/flywheel/internal/cli/style"
	"github.com/cobr-io/flywheel/internal/naming"
)

// The membership marker for "flywheel's own applied set" is
// naming.ManagedBySelector: every resource `flywheel up` applies imperatively
// (the dev-loop machinery via its kustomization, the flywheel-config ConfigMap,
// the bootstrap flux-system tree, the secrets) carries it. Resources Flux
// reconciles from the gitops repo (app/infra workloads, per-app git-auto-sync
// sidecars) are deliberately NOT labeled, so a label-scoped scan can never
// see — let alone delete — them.

// pruneDenylist names the GroupKinds the orphan prune must NEVER delete, even
// when they carry the managed-by label and have dropped out of the applied
// set. Each entry guards against an irreversible or cascading deletion:
//
//   - Namespace                — deleting it cascade-deletes everything inside.
//   - PersistentVolumeClaim    — state-bearing; `flywheel clean` reaps orphans.
//   - Secret                   — e.g. sops-age; losing it breaks SOPS decryption.
//   - Kustomization (Flux)     — deleting one makes Flux's finalizer prune every
//     workload it manages (app/infra). This is the
//     "never remove app/infra via Flux" guarantee.
//   - GitRepository (Flux)     — deleting one breaks the source every client
//     Kustomization reads from.
//
// Everything else flywheel applies (Deployments, DaemonSets, Services, RBAC,
// NetworkPolicies, ImageUpdateAutomations, ConfigMaps) is recreatable on the
// next `up`, so an orphan of those kinds is safe to reap.
var pruneDenylist = map[schema.GroupKind]bool{
	{Group: "", Kind: "Namespace"}:                                true,
	{Group: "", Kind: "PersistentVolumeClaim"}:                    true,
	{Group: "", Kind: "Secret"}:                                   true,
	{Group: "kustomize.toolkit.fluxcd.io", Kind: "Kustomization"}: true,
	{Group: "source.toolkit.fluxcd.io", Kind: "GitRepository"}:    true,
}

// PruneOrphanedMachinery deletes flywheel-applied machinery that this `up`
// did NOT re-apply — the superseded dev-loop resources a version bump stops
// rendering (issue #27: the old git-auto-sync-self Deployment). `keep` is the
// set of resources THIS run actually applied (from ApplyDevLoop + the bootstrap
// ApplyKustomizeTracked); only managed-by=flywheel resources of a kind that
// appears in `keep` are scanned, and any not in `keep` (and not on the safety
// denylist) is removed. Returns the count pruned. Best-effort: a list/delete
// failure is warned and skipped, never fatal.
func PruneOrphanedMachinery(ctx context.Context, a *applier.Applier, keep []applier.ResourceRef, out io.Writer) (int, error) {
	keepSet := make(map[string]bool, len(keep))
	for _, r := range keep {
		keepSet[refKey(r)] = true
	}

	pruned := 0
	for _, gk := range prunableGroupKinds(keep) {
		items, err := a.ListByKindLabeled(ctx, gk, naming.ManagedBySelector)
		if err != nil {
			style.Warn(out, "prune: list %s: %v", gk.String(), err)
			continue
		}
		var found []applier.ResourceRef
		for i := range items {
			found = append(found, applier.ResourceRef{
				Group:     gk.Group,
				Kind:      gk.Kind,
				Namespace: items[i].GetNamespace(),
				Name:      items[i].GetName(),
			})
		}
		for _, ref := range prunePlan(keepSet, found) {
			if err := a.DeleteResource(ctx, ref, out); err != nil {
				style.Warn(out, "prune: delete %s %s/%s: %v", ref.Kind, ref.Namespace, ref.Name, err)
				continue
			}
			pruned++
		}
	}
	return pruned, nil
}

// prunableGroupKinds returns the distinct GroupKinds present in `keep`, minus
// the safety denylist. Deriving the scan universe from the applied set (rather
// than a hardcoded list) keeps the prune in lockstep with the version being
// applied: it only ever scans kinds this `up` itself manages.
func prunableGroupKinds(keep []applier.ResourceRef) []schema.GroupKind {
	seen := make(map[schema.GroupKind]bool)
	var out []schema.GroupKind
	for _, r := range keep {
		gk := schema.GroupKind{Group: r.Group, Kind: r.Kind}
		if seen[gk] || pruneDenylist[gk] {
			continue
		}
		seen[gk] = true
		out = append(out, gk)
	}
	return out
}

// prunePlan returns the members of `found` that are not in `keepSet` — the
// orphans to delete. Pure (no cluster I/O) so the keep-vs-found decision is
// unit-testable on its own.
func prunePlan(keepSet map[string]bool, found []applier.ResourceRef) []applier.ResourceRef {
	var del []applier.ResourceRef
	for _, f := range found {
		if !keepSet[refKey(f)] {
			del = append(del, f)
		}
	}
	return del
}

// refKey is the identity of a resource for keep-set membership: group, kind,
// namespace, name. Version is intentionally excluded — the REST mapper resolves
// kinds without it, and a resource's identity doesn't change across API versions.
func refKey(r applier.ResourceRef) string {
	return r.Group + "|" + r.Kind + "|" + r.Namespace + "|" + r.Name
}
