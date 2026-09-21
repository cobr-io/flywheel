package controller

// StatefulSetUnstickReconciler deletes a StatefulSet Pod that is stuck pulling
// an image its StatefulSet no longer asks for, so the StatefulSet controller
// recreates it from the current template.
//
// Why this exists: a flywheel app's workload starts on the `:0-placeholder`
// tag, which never exists in the registry, and only the first build's image
// bump makes it pullable. A Deployment rolls its stuck Pod over on that bump.
// A StatefulSet does not: under the default OrderedReady policy the
// StatefulSet controller waits for every Pod to be Running and Ready before it
// replaces any of them (kubernetes/kubernetes#67250), and a Pod stuck in
// ImagePullBackOff never gets there, so the app sits on the placeholder
// forever. `podManagementPolicy: Parallel` only helps while the
// MaxUnavailableStatefulSet feature gate is off, and the field is immutable,
// so it isn't a fix clients can rely on.
//
// Deleting the Pod is the upstream-documented remedy ("forced rollback"). This
// reconciler applies it only when it is safe: the Pod is on a superseded
// revision, and none of its containers has ever started, so no process runs
// or ever ran in it. A Pod the StatefulSet's partition or OnDelete strategy
// holds back is left alone.

import (
	"context"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// imagePullWaitReasons are the kubelet waiting reasons for a container whose
// image cannot be pulled.
var imagePullWaitReasons = map[string]bool{
	"ErrImagePull":     true,
	"ImagePullBackOff": true,
}

// StatefulSetUnstickReconciler replaces StatefulSet Pods that are stuck on an
// unpullable image from a superseded revision.
type StatefulSetUnstickReconciler struct {
	client.Client
}

func (r *StatefulSetUnstickReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Owns(Pod) re-queues the StatefulSet when one of its Pods changes, so the
	// reconciler sees both halves of the trigger: the template moving to a new
	// revision, and a Pod falling into ImagePullBackOff.
	return ctrl.NewControllerManagedBy(mgr).
		Named("statefulset-unstick").
		For(&appsv1.StatefulSet{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}

func (r *StatefulSetUnstickReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("statefulset", req.NamespacedName)

	var sts appsv1.StatefulSet
	if err := r.Get(ctx, req.NamespacedName, &sts); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if sts.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType || sts.Status.UpdateRevision == "" {
		return ctrl.Result{}, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
	if err != nil {
		return ctrl.Result{}, nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(sts.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return ctrl.Result{}, err
	}

	partition := rollingUpdatePartition(&sts)
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !metav1.IsControlledBy(pod, &sts) || !stuckOnSupersededImage(pod, sts.Status.UpdateRevision, partition) {
			continue
		}
		// The UID precondition keeps a stale cache read from deleting the
		// replacement Pod, which reuses the same name.
		err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID})
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		log.Info("deleted StatefulSet Pod stuck pulling an image from a superseded revision",
			"pod", pod.Name,
			"revision", pod.Labels[appsv1.ControllerRevisionHashLabelKey],
			"updateRevision", sts.Status.UpdateRevision)
	}
	return ctrl.Result{}, nil
}

// rollingUpdatePartition is the ordinal below which the StatefulSet keeps Pods
// on the old revision; 0 when no partition is set.
func rollingUpdatePartition(sts *appsv1.StatefulSet) int {
	ru := sts.Spec.UpdateStrategy.RollingUpdate
	if ru == nil || ru.Partition == nil {
		return 0
	}
	return int(*ru.Partition)
}

// stuckOnSupersededImage reports whether pod is safe to replace: it is not
// already terminating, it belongs to a revision other than updateRevision, its
// ordinal is not held back by the partition, at least one of its containers
// cannot pull its image, and none of its containers has ever started.
func stuckOnSupersededImage(pod *corev1.Pod, updateRevision string, partition int) bool {
	if pod.DeletionTimestamp != nil || pod.Labels[appsv1.ControllerRevisionHashLabelKey] == updateRevision {
		return false
	}
	ordinal, ok := podOrdinal(pod.Name)
	if !ok || ordinal < partition {
		return false
	}

	pulling := false
	statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if cs.State.Running != nil || cs.State.Terminated != nil || cs.LastTerminationState.Terminated != nil {
			return false
		}
		if cs.State.Waiting != nil && imagePullWaitReasons[cs.State.Waiting.Reason] {
			pulling = true
		}
	}
	return pulling
}

// podOrdinal parses the ordinal suffix of a StatefulSet Pod name
// (`<statefulset>-<ordinal>`).
func podOrdinal(name string) (int, bool) {
	i := strings.LastIndexByte(name, '-')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(name[i+1:])
	return n, err == nil && n >= 0
}
