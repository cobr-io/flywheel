// Package clean implements `flywheel clean` (design § CLI surface):
// opt-in destructive cleanup of orphaned PVCs in the flywheel namespace.
// `flywheel up` tiers state-bearing resources out of auto-deletion, so
// removing an app or builder leaves its PVC behind until `clean` reaps it.
package clean

import (
	"context"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/cobr-io/flywheel/internal/cli/applier"
	"github.com/cobr-io/flywheel/internal/cli/style"
	"github.com/cobr-io/flywheel/internal/naming"
)

// Options for `flywheel clean`.
type Options struct {
	FlywheelNamespace string
	Orphaned          bool // delete orphaned PVCs (default true)
}

// Run executes the cleanup against the cluster the applier is bound to.
func Run(ctx context.Context, a *applier.Applier, opts Options, out io.Writer) error {
	if opts.Orphaned {
		if err := cleanOrphanedPVCs(ctx, a, opts.FlywheelNamespace, out); err != nil {
			return err
		}
	}
	return nil
}

// cleanOrphanedPVCs deletes PVCs in the flywheel namespace labeled
// managed-by=flywheel. (By design, `up` never auto-deletes PVCs;
// `clean` is the explicit removal path.)
func cleanOrphanedPVCs(ctx context.Context, a *applier.Applier, ns string, out io.Writer) error {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	// Label-scoped: only PVCs flywheel itself applied carry
	// naming.ManagedBySelector. Without this scoping, every PVC in the
	// namespace gets listed (and deleted) below, including app PVCs that
	// Flux/apps created — that was the bug this scoping fixes.
	items, err := a.ListUnstructuredLabeled(ctx, gvr, ns, naming.ManagedBySelector)
	if err != nil {
		return fmt.Errorf("list PVCs: %w", err)
	}
	n := 0
	for i := range items {
		pvc := &items[i]
		if err := a.DeleteResource(ctx, applier.ResourceRef{
			Kind: "PersistentVolumeClaim", Namespace: pvc.GetNamespace(), Name: pvc.GetName(),
		}, out); err != nil {
			style.Warn(out, "delete PVC %s/%s: %v", pvc.GetNamespace(), pvc.GetName(), err)
			continue
		}
		n++
	}
	style.Summary(out, "cleaned %d PVC(s) in %s", n, ns)
	return nil
}
