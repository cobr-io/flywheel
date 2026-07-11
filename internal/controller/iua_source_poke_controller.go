package controller

// IUASourcePokeReconciler watches ImageUpdateAutomation objects and, the moment
// the IUA pushes a new commit (status.lastPushCommit advances), pokes the
// GitRepository it sources from to reconcile immediately.
//
// That GitRepository (the gitops repo) is what Flux's kustomize-controller
// watches to roll the app Deployment, so the poke collapses the source-poll
// wait between "IUA committed the image bump" and "the app Kustomization
// applies it" — the dominant remaining hop in the commit-to-pod tail once the
// build (spike #1) and IUA (spike #2) waits are removed.
//
// Without this the bump still propagates, but gated by git-auto-sync-self's
// fetch loop: it notices the IUA commit only on its next poll, then pokes.
// Poking straight off the IUA push event removes that detection latency.
//
// Poking the *source* and not the Kustomization is deliberate:
// kustomize-controller re-runs dependent Kustomizations when their source
// artifact advances, so a single source poke rolls the app without the
// stale-source race that poking the Kustomization directly would hit (it could
// reconcile before its source observed the new commit, then wait out its
// interval). Best-effort: any failure just falls back to the poll path.

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

// gitRepositoryGVK identifies Flux source-controller GitRepository objects
// (served at v1). Patched as unstructured to avoid a typed dependency here.
var gitRepositoryGVK = schema.GroupVersionKind{
	Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository",
}

// IUASourcePokeReconciler pokes an IUA's source GitRepository whenever the IUA
// pushes a new commit.
type IUASourcePokeReconciler struct {
	client.Client
}

func (r *IUASourcePokeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Preflight: skip (don't crash-loop) if we can't list ImageUpdateAutomation.
	if ok, err := canList(mgr.GetConfig(), imageUpdateAutomationGVK); !ok {
		mgr.GetLogger().WithName("iua-source-poke").Info(
			"disabled: cannot list ImageUpdateAutomation (RBAC or CRD absent); source falls back to poll", "error", err)
		return nil
	}
	iua := &unstructured.Unstructured{}
	iua.SetGroupVersionKind(imageUpdateAutomationGVK)
	return ctrl.NewControllerManagedBy(mgr).
		Named("iua-source-poke").
		For(iua, builder.WithPredicates(lastPushCommitChanged())).
		Complete(r)
}

// lastPushCommitChanged fires only when status.lastPushCommit actually moves,
// so a routine IUA status rewrite (no push) doesn't poke the source.
func lastPushCommitChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return lastPushCommit(e.Object) != "" },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return lastPushCommit(e.ObjectNew) != "" && lastPushCommit(e.ObjectNew) != lastPushCommit(e.ObjectOld)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func lastPushCommit(o client.Object) string {
	u, ok := o.(*unstructured.Unstructured)
	if !ok {
		return ""
	}
	c, _, _ := unstructured.NestedString(u.Object, "status", "lastPushCommit")
	return c
}

func (r *IUASourcePokeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("imageupdateautomation", req.NamespacedName)

	iua := &unstructured.Unstructured{}
	iua.SetGroupVersionKind(imageUpdateAutomationGVK)
	if err := r.Get(ctx, req.NamespacedName, iua); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The IUA's sourceRef is the gitops GitRepository to poke. namespace
	// defaults to the IUA's own namespace when unset (Flux semantics).
	srcName, _, _ := unstructured.NestedString(iua.Object, "spec", "sourceRef", "name")
	if srcName == "" {
		return ctrl.Result{}, nil
	}
	srcNS, _, _ := unstructured.NestedString(iua.Object, "spec", "sourceRef", "namespace")
	if srcNS == "" {
		srcNS = req.Namespace
	}

	if err := r.pokeGitRepository(ctx, srcNS, srcName); err != nil {
		log.Error(err, "source GitRepository poke failed; falling back to poll", "gitrepository", srcNS+"/"+srcName)
		return ctrl.Result{}, nil
	}
	log.Info("poked source GitRepository after IUA push", "gitrepository", srcNS+"/"+srcName)
	return ctrl.Result{}, nil
}

// pokeGitRepository bumps the reconcile-request annotation on the named
// GitRepository via a JSON merge patch (same rationale as the other pokes:
// avoids racing source-controller's own status writes). NotFound is ignored.
func (r *IUASourcePokeReconciler) pokeGitRepository(ctx context.Context, namespace, name string) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gitRepositoryGVK)
	u.SetNamespace(namespace)
	u.SetName(name)
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
