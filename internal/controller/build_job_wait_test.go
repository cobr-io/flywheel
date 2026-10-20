package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// runWaitBuildkitd renders a build Job and runs its wait-buildkitd
// initContainer's command on the host, with fake `buildctl` and `sleep` on
// PATH. buildctl fails its first `failures` calls (buildkitd still starting:
// connection refused), then succeeds. Returns how often buildctl ran and the
// script's error.
func runWaitBuildkitd(t *testing.T, failures int) (int, error) {
	t.Helper()
	r := &GitRepositoryBuildReconciler{Config: Config{
		Namespace: "flywheel-system", ClientName: "acme", ClusterName: "acme-local", Registry: "acme-local-registry",
	}}
	gr := sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "sample-app", Namespace: "apps"},
		Spec:       sourcev1.GitRepositorySpec{URL: "http://git-server.flywheel-system.svc.cluster.local:8080/sample-app.git"},
		Status:     sourcev1.GitRepositoryStatus{Artifact: &meta.Artifact{URL: "http://src/artifact.tar.gz"}},
	}
	job, err := r.renderJob("build-sample-app-1-abc1234", gr,
		BuildEntry{Image: "sample-app", Context: ".", Dockerfile: "Dockerfile"},
		strings.Repeat("a", 40), "abc1234", 1)
	if err != nil {
		t.Fatalf("renderJob: %v", err)
	}
	var wait *corev1.Container
	for i, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name == "wait-buildkitd" {
			wait = &job.Spec.Template.Spec.InitContainers[i]
		}
	}
	if wait == nil {
		t.Fatal("build Job has no wait-buildkitd initContainer: a build started before buildkitd is up fails and lingers as an Error pod")
	}

	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	writeScript(t, filepath.Join(bin, "buildctl"), `
echo "$BUILDKIT_HOST $*" >> "`+calls+`"
n=$(wc -l < "`+calls+`")
[ "$n" -gt `+strconv.Itoa(failures)+` ] || { echo "connection refused" >&2; exit 1; }
`)
	writeScript(t, filepath.Join(bin, "sleep"), "") // don't actually wait

	cmd := exec.Command(wait.Command[0], wait.Command[1:]...)
	cmd.Args = append(cmd.Args, wait.Args...)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	for _, e := range wait.Env {
		cmd.Env = append(cmd.Env, e.Name+"="+e.Value)
	}
	runErr := cmd.Run()

	raw, _ := os.ReadFile(calls)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, l := range lines {
		if l != "tcp://buildkitd.flywheel-system:1234 debug workers" {
			t.Errorf("buildctl invoked as %q, want the buildkitd readiness check against the configured address", l)
		}
	}
	return len(lines), runErr
}

// TestBuildJob_WaitsForBuildkitd locks issue #170: a build Job created while
// buildkitd is still starting must wait for it, not fail (each failed attempt
// lingers as a red Error pod for the Job's 24h TTL).
func TestBuildJob_WaitsForBuildkitd(t *testing.T) {
	calls, err := runWaitBuildkitd(t, 3)
	if err != nil {
		t.Fatalf("wait-buildkitd failed although buildkitd came up on the 4th check: %v", err)
	}
	if calls != 4 {
		t.Fatalf("buildctl ran %d times, want 4 (3 refused + 1 ready)", calls)
	}
}

// TestBuildJob_WaitForBuildkitdIsBounded: a buildkitd that never comes up
// must still fail the Job eventually, so its retries and failure surface.
func TestBuildJob_WaitForBuildkitdIsBounded(t *testing.T) {
	calls, err := runWaitBuildkitd(t, 1_000_000)
	if err == nil {
		t.Fatalf("wait-buildkitd succeeded although buildkitd never answered (%d checks)", calls)
	}
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}
