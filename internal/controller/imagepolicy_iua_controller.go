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
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/cobr-io/flywheel/internal/fluxpoke"
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
	// Preflight: registering a watch for a kind we can't list makes the
	// manager's cache sync time out and crash-loops the whole pod. Gate on
	// canList; if ImagePolicy isn't listable yet (RBAC not landed, CRD absent),
	// register later — setupWhenListable re-probes on a slow ticker and enables
	// this watch when the permission appears, so a transient RBAC race no longer
	// needs a pod restart. Until then the IUA falls back to its own poll.
	return setupWhenListable(mgr, "imagepolicy-iua", imagePolicyGVK, func() error {
		ip := &unstructured.Unstructured{}
		ip.SetGroupVersionKind(imagePolicyGVK)
		return ctrl.NewControllerManagedBy(mgr).
			Named("imagepolicy-iua").
			For(ip, builder.WithPredicates(latestTagChanged())).
			Complete(r)
	})
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

// pokeIUA bumps the reconcile-request annotation on the IUA so it runs its
// commit cycle now. If the IUA isn't installed under the expected name the
// NotFound just waits out the poll interval — see fluxpoke for the shared
// merge-patch rationale.
func (r *ImagePolicyIUAReconciler) pokeIUA(ctx context.Context, namespace string) error {
	return fluxpoke.Poke(ctx, r.Client,
		fluxpoke.Unstructured(imageUpdateAutomationGVK, namespace, imageUpdateAutomationName),
		time.Now())
}
