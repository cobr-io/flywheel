package usecmd

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestBranchPatch(t *testing.T) {
	obj := BranchPatch("flux-system", "flux-system", "feat/x", "2026-06-11T14:00:00Z")

	if got := obj.GetName(); got != "flux-system" {
		t.Errorf("name = %q", got)
	}
	if got := obj.GetNamespace(); got != "flux-system" {
		t.Errorf("namespace = %q", got)
	}
	if gvk := obj.GroupVersionKind(); gvk.Group != "source.toolkit.fluxcd.io" || gvk.Version != "v1" || gvk.Kind != "GitRepository" {
		t.Errorf("gvk = %v", gvk)
	}
	if got, _, _ := unstructured.NestedString(obj.Object, "spec", "ref", "branch"); got != "feat/x" {
		t.Errorf("spec.ref.branch = %q, want feat/x", got)
	}
	ann := obj.GetAnnotations()
	if ann[DeployBranchAnnotation] != "feat/x" {
		t.Errorf("deploy-branch annotation = %q, want feat/x", ann[DeployBranchAnnotation])
	}
	if ann["kustomize.toolkit.fluxcd.io/reconcile"] != "disabled" {
		t.Errorf("reconcile annotation = %q, want disabled", ann["kustomize.toolkit.fluxcd.io/reconcile"])
	}
	if ann["reconcile.fluxcd.io/requestedAt"] != "2026-06-11T14:00:00Z" {
		t.Errorf("requestedAt annotation = %q", ann["reconcile.fluxcd.io/requestedAt"])
	}
}

func TestLocalBranches(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "t@t")
	runGit(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "init")
	runGit(t, repo, "branch", "feat/a")
	runGit(t, repo, "branch", "feat/b")

	got, err := LocalBranches(repo)
	if err != nil {
		t.Fatalf("LocalBranches: %v", err)
	}
	want := map[string]bool{"main": true, "feat/a": true, "feat/b": true}
	if len(got) != len(want) {
		t.Fatalf("branches = %v, want keys %v", got, want)
	}
	for _, b := range got {
		if !want[b] {
			t.Errorf("unexpected branch %q in %v", b, got)
		}
	}
}

// TestRun_AppliesBranchPatch exercises Run with a stubbed applier and a real
// temp git repo + flywheel.yaml, so the cluster apply is captured, not sent.
func TestRun_AppliesBranchPatch(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "t@t")
	runGit(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "c")
	runGit(t, repo, "branch", "feat/x")
	if err := os.WriteFile(filepath.Join(repo, "flywheel.yaml"),
		[]byte("schema: v1alpha1\ncluster:\n  name: acme-local\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var captured *unstructured.Unstructured
	err := Run(context.Background(), Options{
		RepoDir: repo,
		Branch:  "feat/x",
		Stdout:  io.Discard,
		applyObject: func(ctx context.Context, obj *unstructured.Unstructured) error {
			captured = obj
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if captured == nil {
		t.Fatal("applyObject was not called")
	}
	if got, _, _ := unstructured.NestedString(captured.Object, "spec", "ref", "branch"); got != "feat/x" {
		t.Errorf("applied branch = %q, want feat/x", got)
	}
}

func TestRun_RequiresBranch(t *testing.T) {
	if err := Run(context.Background(), Options{RepoDir: t.TempDir(), Stdout: io.Discard}); err == nil {
		t.Error("empty branch should be rejected")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
