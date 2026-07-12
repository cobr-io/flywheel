// Package fluxpoke centralizes the single way flywheel triggers an immediate
// Flux reconcile: a JSON merge-patch that bumps the reconcile-request annotation
// (naming.ReconcileRequestAnnotation) on a Flux object to "now".
//
// This one poke was previously copy-pasted four times across the controllers
// and the self-sync loop (pokeImageRepository, pokeIUA, pokeGitRepository,
// K8sFlux.pokeReconcile). Every copy shared the same three load-bearing
// decisions, so they live here once:
//
//   - a JSON *merge* patch of just our annotation, never a read-modify-write
//     Update: Flux re-resolves the object's status on every scan/commit, so a
//     get-then-update races those writes and loses with "the object has been
//     modified" (observed in practice). A merge patch touches only our key and
//     never conflicts.
//   - NotFound is success: a poke target that isn't installed (a differently
//     named ImageRepository, an absent IUA/Kustomization) simply falls back to
//     its normal Flux poll interval, which is the correct degraded behaviour.
//   - the value is opaque to Flux — it only has to differ from the last handled
//     value — so any monotonic timestamp works.
//
// Both callers hold a controller-runtime client.Client (the controllers embed
// it; selfsync.K8sFlux carries one), so a single function over client.Client
// serves everyone — no dynamic-client adapter is needed. Callers pass the
// object reference: a typed object (e.g. sourcev1.GitRepository) where flywheel
// vendors the API, or Unstructured(gvk, ns, name) for the Flux kinds it does
// not (ImageRepository, ImagePolicy, ImageUpdateAutomation, Kustomization).
package fluxpoke

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cobr-io/flywheel/internal/naming"
)

// Poke bumps obj's reconcile-request annotation to now via a JSON merge patch,
// asking Flux to reconcile obj immediately instead of waiting out its poll
// interval. obj must carry its GVK, namespace, and name (a typed object the
// client's scheme recognises, or one built by Unstructured). A NotFound is
// treated as success — the poke is best-effort and falls back to the poll path.
func Poke(ctx context.Context, c client.Client, obj client.Object, now time.Time) error {
	patch := fmt.Appendf(nil,
		`{"metadata":{"annotations":{%q:%q}}}`,
		naming.ReconcileRequestAnnotation, now.UTC().Format(time.RFC3339Nano),
	)
	if err := c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

// Unstructured returns a bare object carrying just gvk/namespace/name — the
// reference form Poke (and Get/Patch generally) need for Flux kinds flywheel
// does not vendor a typed API for.
func Unstructured(gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}
