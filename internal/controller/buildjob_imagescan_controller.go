package controller

// BuildJobReconciler watches build *Pods* and, the moment the build
// container terminates successfully, pokes the matching Flux ImageRepository
// to scan the local registry — instead of waiting out its poll interval. That
// scan is the first poll-bound hop after a build; triggering it on the
// build-complete event removes the wait without changing what Flux does, so
// production parity is preserved. The poke is best-effort: on any failure the
// ImageRepository simply falls back to its normal interval.
//
// Why watch the Pod's container status and not the Job's Complete condition:
// the image is pushed to the registry the instant the build *container*
// exits 0, but the Job isn't marked Complete until the kubelet tears the pod
// down and the job-controller observes it — ~4s later, measured, squarely on
// the commit-to-pod critical path. Keying off the container terminated state
// reclaims that gap.
//
// Scope note: an earlier revision also tried to chain the ImagePolicy wait and
// an ImageUpdateAutomation poke here. Live tracing showed that was dead weight:
// the IUA already self-reconciles every 5s and commits the bump within ~8s of
// the build regardless, and the policy snapshot raced the reflector so the
// chained poke never fired usefully. The real commit-to-pod tail was a
// dependsOn requeue in the kustomize layer (fixed via --requeue-dependency +
// longer mirror-tier intervals), not anything reachable from here. So this
// reconciler is deliberately limited to the one poke that measurably helps.
//
// The ImageRepository is patched as an unstructured object so the controller
// need not vendor the image-reflector API types; the GVK is resolved against
// the cluster's RESTMapper at runtime (the CRD is installed by Flux).

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/cobr-io/flywheel/internal/naming"
)

// buildContainerName is the build container in the build Job template whose
// successful exit means "image pushed". Must match the container `name` in
// templates/build-job.yaml (guarded by TestRenderJob_BuildKitWiring).
const buildContainerName = "build"

// imageRepositoryGVK identifies Flux image-reflector ImageRepository objects
// (served at v1 in the Flux version flywheel pins).
var imageRepositoryGVK = schema.GroupVersionKind{
	Group: "image.toolkit.fluxcd.io", Version: "v1", Kind: "ImageRepository",
}

// BuildJobReconciler pokes an app's ImageRepository to scan as soon as that
// app's build container exits successfully.
type BuildJobReconciler struct {
	client.Client
	Config Config
}

func (r *BuildJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("buildjob-imagescan").
		For(&corev1.Pod{}, builder.WithPredicates(buildPodPredicate())).
		Complete(r)
}

// buildPodPredicate limits the watch to build Pods (label
// app=image-builder), so we don't reconcile on unrelated Pods in the cluster.
func buildPodPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()["app"] == "image-builder"
	})
}

func (r *BuildJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("buildpod", req.NamespacedName)

	// Build Pods only ever live in the controller's own namespace.
	if req.Namespace != r.Config.Namespace {
		return ctrl.Result{}, nil
	}

	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pod.Labels["app"] != "image-builder" || !buildSucceeded(&pod) {
		return ctrl.Result{}, nil
	}

	repo := imageRepoNameForPod(&pod)
	if repo == "" {
		return ctrl.Result{}, nil
	}

	if err := r.pokeImageRepository(ctx, repo); err != nil {
		// Best-effort: a failed poke just means the scan waits out its poll
		// interval, so log and move on rather than requeue-storming.
		log.Error(err, "image-scan trigger failed; falling back to poll", "imagerepository", repo)
		return ctrl.Result{}, nil
	}
	log.Info("poked ImageRepository to scan after build container exited", "imagerepository", repo)
	return ctrl.Result{}, nil
}

// pokeImageRepository bumps the reconcile-request annotation on the named
// ImageRepository in the Flux namespace. It uses a JSON merge patch of just
// the annotation rather than a read-modify-write Update: image-reflector
// re-resolves the ImageRepository status on every scan, so a get-then-update
// races those writes and loses with "the object has been modified" (observed
// in practice). A merge patch touches only our key and never conflicts. A
// NotFound is not an error: a multi-image repo may name its ImageRepositories
// differently than the source, in which case the scan simply falls back to its
// poll interval.
func (r *BuildJobReconciler) pokeImageRepository(ctx context.Context, name string) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(imageRepositoryGVK)
	u.SetNamespace(naming.FluxNamespace)
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

// buildSucceeded reports whether the pod's build container has terminated
// with exit code 0 (image pushed). Checked on container status rather than
// pod phase: the pod may still be Running (init/sidecar teardown) or only
// reach Succeeded a few seconds later, but the registry already has the image
// the moment the build container exits 0.
func buildSucceeded(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != buildContainerName {
			continue
		}
		t := cs.State.Terminated
		return t != nil && t.ExitCode == 0
	}
	return false
}

// imageRepoNameForPod derives the ImageRepository name from a build Pod's
// `repo` label. The per-app template names the GitRepository, build Pod
// `repo` label, and ImageRepository identically (the app name), so the label
// is the join key. Returns "" when the label is absent.
func imageRepoNameForPod(pod *corev1.Pod) string {
	return pod.Labels["repo"]
}
