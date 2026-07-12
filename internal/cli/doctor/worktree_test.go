package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cobr-io/flywheel/internal/testgit"
)

func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	testgit.Git(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testgit.Git(t, dir, "add", "-A")
	testgit.Git(t, dir, "commit", "-q", "-m", "init")
}

func TestWorktreeCheck_NormalClonePasses(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	initRepo(t, repo)
	if err := worktreeCheck(repo).Run(context.Background()); err != nil {
		t.Errorf("normal clone should pass, got: %v", err)
	}
}

func TestWorktreeCheck_NestedWorktreeFails(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	nested := filepath.Join(repo, ".claude", "worktrees", "agent-x")
	testgit.Git(t, repo, "worktree", "add", "-q", "-b", "feat", nested)

	err := worktreeCheck(nested).Run(context.Background())
	if err == nil {
		t.Fatal("nested worktree should fail the check")
	}
	if !strings.Contains(err.Error(), "nested") {
		t.Errorf("error should explain the nesting, got: %v", err)
	}
}

func TestWorktreeCheck_NonRepoPasses(t *testing.T) {
	// Outside a repo the check is a no-op.
	if err := worktreeCheck(t.TempDir()).Run(context.Background()); err != nil {
		t.Errorf("non-repo dir should pass, got: %v", err)
	}
}
