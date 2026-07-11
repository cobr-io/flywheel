package controller

// ImagePolicyIUAReconciler watches Flux ImagePolicy objects and, the moment a
// policy resolves a *new* latest tag, pokes the cluster's ImageUpdateAutomation
// to run its commit cycle — instead of waiting out the IUA's poll interval.
//
// Why this is the right event source (and the build-exit poke was not):
// buildjob_imagescan_controller's comment records an earlier, abandoned attempt
// to poke the IUA straight off the build container's exit. That raced the
// image-reflector: at build-exit the ImagePolicy had not yet re-resolved its
// latest tag, so the IUA reconciled against a stale policy and the poke did
// nothing. ImagePolicy.status.latestRef changing IS the "a new tag is ready to
// commit" signal — poking on that edge fires exactly once the IUA has fresh
// input, which is what removes the poll-interval wait without changing what
// Flux does. Best-effort: any failure just falls back to the IUA's interval.

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/cobr-io/flywheel/internal/naming"
)

// imageUpdateAutomationName is the single cluster-wide IUA installed by the
// dev-loop overlay (manifests/dev-loop/base/image-update-automation.yaml).
const imageUpdateAutomationName = "flywheel-self"

// imagePolicyGVK / imageUpdateAutomationGVK identify the Flux image-automation
// types (served at v1 in the Flux version flywheel pins). Patched as
// unstructured so the controller need not vendor the image-reflector API.
var imagePolicyGVK = schema.GroupVersionKind{
	Group: "image.toolkit.fluxcd.io", Version: "v1", Kind: "ImagePolicy",
}

var imageUpdateAutomationGVK = schema.GroupVersionKind{
	Group: "image.toolkit.fluxcd.io", Version: "v1", Kind: "ImageUpdateAutomation",
}

// ImagePolicyIUAReconciler pokes the IUA when any ImagePolicy resolves a new
// latest tag.
type ImagePolicyIUAReconciler struct {
	client.Client
}

func (r *ImagePolicyIUAReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Preflight: if we can't list ImagePolicy (RBAC reverted or CRD absent),
	// skip registering rather than crash-looping the manager on cache sync.
	// The IUA then just falls back to its own poll interval.
	if ok, err := canList(mgr.GetConfig(), imagePolicyGVK); !ok {
		mgr.GetLogger().WithName("imagepolicy-iua").Info(
			"disabled: cannot list ImagePolicy (RBAC or CRD absent); IUA falls back to poll", "error", err)
		return nil
	}
	ip := &unstructured.Unstructured{}
	ip.SetGroupVersionKind(imagePolicyGVK)
	return ctrl.NewControllerManagedBy(mgr).
		Named("imagepolicy-iua").
		For(ip, builder.WithPredicates(latestTagChanged())).
		Complete(r)
}

// latestTagChanged fires only when status.latestRef.tag actually moves, so a
// routine ImagePolicy status rewrite (same tag) doesn't poke the IUA. Create
// events with a tag already present count too (controller restart / first
// resolve).
func latestTagChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return latestTag(e.Object) != ""
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return latestTag(e.ObjectNew) != "" && latestTag(e.ObjectNew) != latestTag(e.ObjectOld)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// latestTag extracts status.latestRef.tag from an unstructured ImagePolicy,
// returning "" when absent.
func latestTag(o client.Object) string {
	u, ok := o.(*unstructured.Unstructured)
	if !ok {
		return ""
	}
	tag, _, _ := unstructured.NestedString(u.Object, "status", "latestRef", "tag")
	return tag
}

func (r *ImagePolicyIUAReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("imagepolicy", req.NamespacedName)
	if err := r.pokeIUA(ctx, req.Namespace); err != nil {
		log.Error(err, "IUA poke failed; falling back to poll", "imageupdateautomation", imageUpdateAutomationName)
		return ctrl.Result{}, nil
	}
	log.Info("poked ImageUpdateAutomation after new latest tag resolved", "imageupdateautomation", imageUpdateAutomationName)
	return ctrl.Result{}, nil
}

// pokeIUA bumps the reconcile-request annotation on the IUA, using a JSON merge
// patch (same rationale as the ImageRepository poke: a get-then-update races
// the image-automation controller's own status writes). NotFound is ignored:
// if the IUA isn't installed under the expected name the bump simply waits out
// the poll interval.
func (r *ImagePolicyIUAReconciler) pokeIUA(ctx context.Context, namespace string) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(imageUpdateAutomationGVK)
	u.SetNamespace(namespace)
	u.SetName(imageUpdateAutomationName)
	patch := fmt.Appendf(nil,
		`{"metadata":{"annotations":{%q:%q}}}`,
		naming.ReconcileRequestAnnotation, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err := r.Patch(ctx, u, client.RawPatch(types.MergePatchType, patch)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}
