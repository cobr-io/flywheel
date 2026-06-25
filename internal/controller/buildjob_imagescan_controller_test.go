package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// podWithBuild builds a build Pod whose build container has the given
// terminated state (nil = not terminated yet). An extra non-build container
// is included to ensure only the build one is consulted.
func podWithBuild(term *corev1.ContainerStateTerminated) *corev1.Pod {
	build := corev1.ContainerStatus{Name: buildContainerName}
	if term != nil {
		build.State.Terminated = term
	}
	return &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				// a sidecar/other container that should be ignored
				{Name: "other", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				build,
			},
		},
	}
}

func TestBuildSucceeded(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "build terminated exit 0",
			pod:  podWithBuild(&corev1.ContainerStateTerminated{ExitCode: 0}),
			want: true,
		},
		{
			name: "build terminated non-zero",
			pod:  podWithBuild(&corev1.ContainerStateTerminated{ExitCode: 1}),
			want: false,
		},
		{
			name: "build still running (not terminated)",
			pod:  podWithBuild(nil),
			want: false,
		},
		{
			name: "no build container at all",
			pod:  &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "other"}}}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildSucceeded(tc.pod); got != tc.want {
				t.Fatalf("buildSucceeded = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestImageRepoNameForPod(t *testing.T) {
	withLabels := func(l map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: l}}
	}
	if got := imageRepoNameForPod(withLabels(map[string]string{"repo": "sample-app"})); got != "sample-app" {
		t.Fatalf("imageRepoNameForPod = %q, want %q", got, "sample-app")
	}
	if got := imageRepoNameForPod(withLabels(nil)); got != "" {
		t.Fatalf("imageRepoNameForPod(no labels) = %q, want empty", got)
	}
}

func TestBuildPodPredicate(t *testing.T) {
	p := buildPodPredicate()
	builderPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "image-builder"}}}
	otherPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "something-else"}}}

	if !p.Create(event.CreateEvent{Object: builderPod}) {
		t.Fatal("predicate should pass image-builder Pods")
	}
	if p.Create(event.CreateEvent{Object: otherPod}) {
		t.Fatal("predicate should drop non-image-builder Pods")
	}
}
