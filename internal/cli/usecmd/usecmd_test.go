package usecmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/cobr-io/flywheel/internal/naming"
)

func TestBranchPatch(t *testing.T) {
	obj := BranchPatch("flux-system", "flux-system", "feat/x")

	if got := obj.GetName(); got != "flux-system" {
		t.Errorf("name = %q", got)
	}
	if got := obj.GetNamespace(); got != "flux-system" {
		t.Errorf("namespace = %q", got)
	}
	if gvk := obj.GroupVersionKind(); gvk.Group != "source.toolkit.fluxcd.io" || gvk.Version != "v1" || gvk.Kind != "GitRepository" {
		t.Errorf("gvk = %v", gvk)
	}
	// The deploy branch is now AUTHORED-selection only: it sets the annotation
	// and must NOT touch spec.ref (Flux tracks the constant DEPLOY branch).
	if ann := obj.GetAnnotations(); ann[naming.DeployBranchAnnotation] != "feat/x" {
		t.Errorf("deploy-branch annotation = %q, want feat/x", ann[naming.DeployBranchAnnotation])
	}
	if _, found, _ := unstructured.NestedMap(obj.Object, "spec"); found {
		t.Error("BranchPatch must not set spec (Flux ref is constant now)")
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
	if got := captured.GetAnnotations()[naming.DeployBranchAnnotation]; got != "feat/x" {
		t.Errorf("applied deploy-branch annotation = %q, want feat/x", got)
	}
}

func TestRun_RequiresBranch(t *testing.T) {
	if err := Run(context.Background(), Options{RepoDir: t.TempDir(), Stdout: io.Discard}); err == nil {
		t.Error("empty branch should be rejected")
	}
}

// TestRun_WarnsOnWorktreeMismatch covers design open question #3: selecting a
// branch the worktree isn't on warns that commits won't deploy until you switch.
func TestRun_WarnsOnWorktreeMismatch(t *testing.T) {
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
	noop := func(ctx context.Context, obj *unstructured.Unstructured) error { return nil }

	// On main, selecting feat/x → mismatch warning naming the switch command.
	var mismatch bytes.Buffer
	if err := Run(context.Background(), Options{RepoDir: repo, Branch: "feat/x", Stdout: &mismatch, applyObject: noop}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mismatch.String(), "git switch feat/x") {
		t.Errorf("expected a worktree-mismatch warning, got:\n%s", mismatch.String())
	}

	// On main, selecting main → no mismatch warning.
	var match bytes.Buffer
	if err := Run(context.Background(), Options{RepoDir: repo, Branch: "main", Stdout: &match, applyObject: noop}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(match.String(), "won't deploy until you") {
		t.Errorf("unexpected mismatch warning when on the selected branch:\n%s", match.String())
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
