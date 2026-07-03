package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-q", "-m", "init")
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
	gitCmd(t, repo, "worktree", "add", "-q", "-b", "feat", nested)

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
