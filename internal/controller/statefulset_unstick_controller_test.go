package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	unstickNS          = "apps"
	unstickSTS         = "probe"
	unstickOldRevision = "probe-86d7b9c678"
	unstickNewRevision = "probe-79d65578b5"
)

// probeStatefulSet is a StatefulSet whose template has moved on to
// unstickNewRevision (the image bump after the first build landed).
func probeStatefulSet() *appsv1.StatefulSet {
	labels := map[string]string{"app": unstickSTS}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: unstickSTS, Namespace: unstickNS, UID: "sts-uid"},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			},
		},
		Status: appsv1.StatefulSetStatus{
			CurrentRevision: unstickOldRevision,
			UpdateRevision:  unstickNewRevision,
		},
	}
}

// probePod is ordinal `ordinal` of probeStatefulSet, on the given revision,
// with a single container stuck in ImagePullBackOff on the placeholder tag.
func probePod(sts *appsv1.StatefulSet, ordinal, revision string) *corev1.Pod {
	isController := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      unstickSTS + "-" + ordinal,
			Namespace: unstickNS,
			UID:       types.UID("pod-uid-" + ordinal),
			Labels: map[string]string{
				"app":                                 unstickSTS,
				appsv1.ControllerRevisionHashLabelKey: revision,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "StatefulSet",
				Name: sts.Name, UID: sts.UID, Controller: &isController,
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "probe",
				Image: "registry:5000/acme/probe:0-placeholder",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
			}},
		},
	}
}

func unstickScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// reconcileUnstick runs one Reconcile for probeStatefulSet against a cluster
// holding sts and pods, and reports which Pods still exist afterwards.
func reconcileUnstick(t *testing.T, sts *appsv1.StatefulSet, pods ...*corev1.Pod) map[string]bool {
	t.Helper()
	objs := []client.Object{sts}
	for _, p := range pods {
		objs = append(objs, p)
	}
	c := fake.NewClientBuilder().WithScheme(unstickScheme(t)).WithObjects(objs...).Build()
	r := &StatefulSetUnstickReconciler{Client: c}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: sts.Namespace, Name: sts.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	remaining := map[string]bool{}
	for _, p := range pods {
		err := c.Get(context.Background(), client.ObjectKeyFromObject(p), &corev1.Pod{})
		switch {
		case err == nil:
			remaining[p.Name] = true
		case !apierrors.IsNotFound(err):
			t.Fatalf("get %s: %v", p.Name, err)
		}
	}
	return remaining
}

// TestUnstick_DeletesPodStuckOnSupersededPlaceholder is the bug: the first
// build's image bump moved the StatefulSet to a new revision, but its Pod is
// still pulling the never-existing placeholder tag and the StatefulSet
// controller will not replace a Pod that isn't Ready.
func TestUnstick_DeletesPodStuckOnSupersededPlaceholder(t *testing.T) {
	sts := probeStatefulSet()
	pod := probePod(sts, "0", unstickOldRevision)

	if remaining := reconcileUnstick(t, sts, pod); remaining[pod.Name] {
		t.Fatalf("Pod %s stuck in ImagePullBackOff on superseded revision was not deleted", pod.Name)
	}
}

// TestUnstick_DeletesPodStuckInInitContainerPull covers a placeholder image
// on an init container: the app containers never started either.
func TestUnstick_DeletesPodStuckInInitContainerPull(t *testing.T) {
	sts := probeStatefulSet()
	pod := probePod(sts, "0", unstickOldRevision)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name:  "init",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}},
	}}
	pod.Status.ContainerStatuses[0].State.Waiting.Reason = "PodInitializing"

	if remaining := reconcileUnstick(t, sts, pod); remaining[pod.Name] {
		t.Fatalf("Pod %s stuck pulling its init container on superseded revision was not deleted", pod.Name)
	}
}

// TestUnstick_LeavesPodsAlone lists the Pods the controller must not delete.
func TestUnstick_LeavesPodsAlone(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(sts *appsv1.StatefulSet, pod *corev1.Pod)
	}{
		{
			// Before the first build lands the template still names the
			// placeholder; deleting would only recreate the same stuck Pod.
			name: "pod on the current revision",
			mutate: func(_ *appsv1.StatefulSet, pod *corev1.Pod) {
				pod.Labels[appsv1.ControllerRevisionHashLabelKey] = unstickNewRevision
			},
		},
		{
			name: "a container is running (sidecar started)",
			mutate: func(_ *appsv1.StatefulSet, pod *corev1.Pod) {
				pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
					Name:  "sidecar",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				})
			},
		},
		{
			// imagePullPolicy: Always re-pulls on restart: the process ran
			// before, so this is not a never-started Pod.
			name: "container ran before and now fails to re-pull",
			mutate: func(_ *appsv1.StatefulSet, pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 1}
			},
		},
		{
			name: "container waiting for a reason other than an image pull",
			mutate: func(_ *appsv1.StatefulSet, pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].State.Waiting.Reason = "ContainerCreating"
			},
		},
		{
			name: "ordinal held back by the rolling-update partition",
			mutate: func(sts *appsv1.StatefulSet, _ *corev1.Pod) {
				partition := int32(1)
				sts.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{Partition: &partition}
			},
		},
		{
			name: "OnDelete update strategy",
			mutate: func(sts *appsv1.StatefulSet, _ *corev1.Pod) {
				sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}
			},
		},
		{
			name: "pod matches the selector but another controller owns it",
			mutate: func(_ *appsv1.StatefulSet, pod *corev1.Pod) {
				pod.OwnerReferences[0].UID = "someone-else"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sts := probeStatefulSet()
			pod := probePod(sts, "0", unstickOldRevision)
			tc.mutate(sts, pod)

			if remaining := reconcileUnstick(t, sts, pod); !remaining[pod.Name] {
				t.Fatalf("Pod %s was deleted, want it left alone", pod.Name)
			}
		})
	}
}
