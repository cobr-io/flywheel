package gitcheckout

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// mainRepo creates a committed repo at <root>/<name> and returns its path.
func mainRepo(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func TestInspect_NormalClone(t *testing.T) {
	root := t.TempDir()
	repo := mainRepo(t, root, "repo")

	got, err := Inspect(repo)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got.IsWorktree {
		t.Errorf("normal clone classified as worktree: %+v", got)
	}
	if got.CommonDir != "" || got.MainWorktree != "" || got.Nested {
		t.Errorf("normal clone should leave resolution fields empty: %+v", got)
	}
}

func TestInspect_SiblingWorktree(t *testing.T) {
	root := t.TempDir()
	repo := mainRepo(t, root, "repo")
	// A sibling worktree: <root>/repo-feat, next to <root>/repo.
	wt := filepath.Join(root, "repo-feat")
	git(t, repo, "worktree", "add", "-q", "-b", "feat", wt)

	got, err := Inspect(wt)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !got.IsWorktree {
		t.Fatalf("sibling worktree not detected: %+v", got)
	}
	if got.Nested {
		t.Errorf("sibling worktree wrongly flagged nested: %+v", got)
	}
	// CommonDir must be the main repo's .git, and it must exist.
	wantCommon := filepath.Join(repo, ".git")
	if resolveSymlinks(got.CommonDir) != resolveSymlinks(wantCommon) {
		t.Errorf("CommonDir = %q, want %q", got.CommonDir, wantCommon)
	}
	if _, err := os.Stat(got.CommonDir); err != nil {
		t.Errorf("CommonDir %q does not exist: %v", got.CommonDir, err)
	}
	if resolveSymlinks(got.MainWorktree) != resolveSymlinks(repo) {
		t.Errorf("MainWorktree = %q, want %q", got.MainWorktree, repo)
	}
}

func TestInspect_NestedWorktree(t *testing.T) {
	root := t.TempDir()
	repo := mainRepo(t, root, "repo")
	// A nested worktree: inside the main repo (mirrors .claude/worktrees/<id>).
	wt := filepath.Join(repo, "sub", "nested")
	git(t, repo, "worktree", "add", "-q", "-b", "nested", wt)

	got, err := Inspect(wt)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !got.IsWorktree {
		t.Fatalf("nested worktree not detected as worktree: %+v", got)
	}
	if !got.Nested {
		t.Errorf("nested worktree not flagged nested: %+v", got)
	}
}

func TestInspect_MissingGit(t *testing.T) {
	// A dir with no .git is a non-worktree with a nil error (callers treat it
	// as a clone; up's quick-check already asserts git is present).
	got, err := Inspect(t.TempDir())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got.IsWorktree {
		t.Errorf("dir without .git classified as worktree: %+v", got)
	}
}

func TestRemediations_MentionKeyPaths(t *testing.T) {
	l := Layout{Dir: "/w/repo/sub/x", MainWorktree: "/w/repo", CommonDir: "/w/repo/.git"}
	if s := NestedRemediation(l); !strings.Contains(s, l.Dir) ||
		!strings.Contains(s, l.MainWorktree) || !strings.Contains(s, AllowNestedEnv) {
		t.Errorf("NestedRemediation missing key details: %q", s)
	}
	if s := UnreachableCommonDirRemediation(l); !strings.Contains(s, l.CommonDir) {
		t.Errorf("UnreachableCommonDirRemediation missing common dir: %q", s)
	}
}
