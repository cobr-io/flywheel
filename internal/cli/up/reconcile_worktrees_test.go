package up

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cobr-io/flywheel/internal/cli/schema"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// srcRepo builds a hermetic git repo with one commit and returns a file:// URL.
func srcRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "t@example.com")
	git(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "init")
	return "file://" + dir
}

// writeApp writes a per-app gitrepository.yaml whose spec.url encodes worktreeName.
func writeApp(t *testing.T, repoDir, name, worktreeName string) {
	t.Helper()
	dir := filepath.Join(repoDir, "builders", "base", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gr := "kind: GitRepository\nmetadata:\n  name: " + name +
		"\nspec:\n  url: http://git-server/" + worktreeName + ".git\n"
	if err := os.WriteFile(filepath.Join(dir, "gitrepository.yaml"), []byte(gr), 0o644); err != nil {
		t.Fatal(err)
	}
}

func boolPtr(b bool) *bool { return &b }

func cfgWith(repos ...schema.WorkspaceRepo) *schema.File {
	return &schema.File{Workspace: schema.Workspace{Repos: repos}}
}

// --clone clones a missing remote-backed worktree into workspaces_root.
func TestReconcileWorktrees_ClonesWithFlag(t *testing.T) {
	repo := t.TempDir()
	wsRoot := t.TempDir()
	cfg := cfgWith(schema.WorkspaceRepo{Name: "web", URL: srcRepo(t)})

	var out bytes.Buffer
	reconcileWorktrees(context.Background(), Options{RepoDir: repo, Clone: boolPtr(true), Stdout: &out}, cfg, wsRoot, &out)

	if _, err := os.Stat(filepath.Join(wsRoot, "web", "Dockerfile")); err != nil {
		t.Fatalf("expected web worktree cloned into workspaces_root: %v\nout: %s", err, out.String())
	}
}

// --clone honours a workspace entry's branch: the requested branch is checked
// out on the fresh clone.
func TestReconcileWorktrees_ClonesRequestedBranch(t *testing.T) {
	repo := t.TempDir()
	wsRoot := t.TempDir()
	url := srcRepo(t)
	git(t, strings.TrimPrefix(url, "file://"), "branch", "feature")
	cfg := cfgWith(schema.WorkspaceRepo{Name: "web", URL: url, Branch: "feature"})

	var out bytes.Buffer
	reconcileWorktrees(context.Background(), Options{RepoDir: repo, Clone: boolPtr(true), Stdout: &out}, cfg, wsRoot, &out)

	b, err := exec.Command("git", "-C", filepath.Join(wsRoot, "web"), "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v\nout: %s", err, out.String())
	}
	if got := strings.TrimSpace(string(b)); got != "feature" {
		t.Errorf("cloned worktree on branch %q, want \"feature\"", got)
	}
}

// A branch absent from the remote is not a clone failure: the clone lands on
// the remote default and reconcile warns rather than leaving no worktree.
func TestReconcileWorktrees_MissingBranchFallsBackAndWarns(t *testing.T) {
	repo := t.TempDir()
	wsRoot := t.TempDir()
	cfg := cfgWith(schema.WorkspaceRepo{Name: "web", URL: srcRepo(t), Branch: "nonexistent"})

	var out bytes.Buffer
	reconcileWorktrees(context.Background(), Options{RepoDir: repo, Clone: boolPtr(true), Stdout: &out}, cfg, wsRoot, &out)

	if _, err := os.Stat(filepath.Join(wsRoot, "web", "Dockerfile")); err != nil {
		t.Fatalf("worktree should still be materialised on the default branch: %v\nout: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "not found") {
		t.Errorf("expected a missing-branch warning, got: %s", out.String())
	}
}

// --no-clone skips, leaving nothing on disk.
func TestReconcileWorktrees_NoCloneSkips(t *testing.T) {
	repo := t.TempDir()
	wsRoot := t.TempDir()
	cfg := cfgWith(schema.WorkspaceRepo{Name: "web", URL: srcRepo(t)})

	var out bytes.Buffer
	reconcileWorktrees(context.Background(), Options{RepoDir: repo, Clone: boolPtr(false), Stdout: &out}, cfg, wsRoot, &out)

	if _, err := os.Stat(filepath.Join(wsRoot, "web")); !os.IsNotExist(err) {
		t.Fatalf("--no-clone should not clone anything")
	}
}

// No flag + non-TTY stdin (a bytes.Reader, not *os.File): skip with a hint.
func TestReconcileWorktrees_NonTTYSkips(t *testing.T) {
	repo := t.TempDir()
	wsRoot := t.TempDir()
	cfg := cfgWith(schema.WorkspaceRepo{Name: "web", URL: srcRepo(t)})

	var out bytes.Buffer
	reconcileWorktrees(context.Background(), Options{RepoDir: repo, Stdin: strings.NewReader(""), Stdout: &out}, cfg, wsRoot, &out)

	if _, err := os.Stat(filepath.Join(wsRoot, "web")); !os.IsNotExist(err) {
		t.Fatalf("non-TTY without a flag should not clone")
	}
	if !strings.Contains(out.String(), "--clone") {
		t.Errorf("expected a --clone hint, got: %s", out.String())
	}
}

// A failing clone is warned and reconcile continues to the other repos.
func TestReconcileWorktrees_CloneFailureContinues(t *testing.T) {
	repo := t.TempDir()
	wsRoot := t.TempDir()
	cfg := cfgWith(
		schema.WorkspaceRepo{Name: "bad", URL: "file:///nonexistent/repo.git"},
		schema.WorkspaceRepo{Name: "good", URL: srcRepo(t)},
	)

	var out bytes.Buffer
	reconcileWorktrees(context.Background(), Options{RepoDir: repo, Clone: boolPtr(true), Stdout: &out}, cfg, wsRoot, &out)

	if _, err := os.Stat(filepath.Join(wsRoot, "good", "Dockerfile")); err != nil {
		t.Fatalf("reconcile should continue past a failed clone and materialise 'good': %v\nout: %s", err, out.String())
	}
	if _, e := os.Stat(filepath.Join(wsRoot, "bad", "Dockerfile")); e == nil {
		t.Errorf("'bad' should not have cloned successfully")
	}
	if !strings.Contains(out.String(), "could not materialise") {
		t.Errorf("expected a failure summary, got: %s", out.String())
	}
}

// A local-only missing worktree can't be materialised — warn, clone nothing.
func TestReconcileWorktrees_LocalOnlyWarns(t *testing.T) {
	repo := t.TempDir()
	wsRoot := t.TempDir()
	cfg := cfgWith(schema.WorkspaceRepo{Name: "web", LocalOnly: true})

	var out bytes.Buffer
	reconcileWorktrees(context.Background(), Options{RepoDir: repo, Clone: boolPtr(true), Stdout: &out}, cfg, wsRoot, &out)

	if _, err := os.Stat(filepath.Join(wsRoot, "web")); !os.IsNotExist(err) {
		t.Fatalf("local-only app must not be cloned")
	}
	if !strings.Contains(out.String(), "local-only") {
		t.Errorf("expected a local-only warning, got: %s", out.String())
	}
}

// An existing worktree is left untouched (not re-cloned, branch preserved).
func TestReconcileWorktrees_ExistingSkipped(t *testing.T) {
	repo := t.TempDir()
	wsRoot := t.TempDir()
	// Pre-create the worktree with a sentinel file the clone source lacks.
	if err := os.MkdirAll(filepath.Join(wsRoot, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(wsRoot, "web", "WIP")
	if err := os.WriteFile(sentinel, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := cfgWith(schema.WorkspaceRepo{Name: "web", URL: srcRepo(t)})

	var out bytes.Buffer
	reconcileWorktrees(context.Background(), Options{RepoDir: repo, Clone: boolPtr(true), Stdout: &out}, cfg, wsRoot, &out)

	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("existing worktree must be left untouched: %v", err)
	}
	// A present worktree is reported so detection isn't silent (#issue: up
	// gave no feedback that sibling repos were found).
	got := out.String()
	if !strings.Contains(got, "web present") {
		t.Errorf("expected a 'present' line for the existing worktree, got: %s", got)
	}
	if !strings.Contains(got, filepath.Join(wsRoot, "web")) {
		t.Errorf("expected the present line to include the worktree path, got: %s", got)
	}
}

// Two builders sharing one worktree have a single block entry → cloned once,
// and neither is flagged undeclared.
func TestReconcileWorktrees_SharedWorktreeClonedOnce(t *testing.T) {
	repo := t.TempDir()
	wsRoot := t.TempDir()
	writeApp(t, repo, "sample-backend", "sample-app")
	writeApp(t, repo, "sample-frontend", "sample-app")
	cfg := cfgWith(schema.WorkspaceRepo{Name: "sample-app", URL: srcRepo(t)})

	var out bytes.Buffer
	reconcileWorktrees(context.Background(), Options{RepoDir: repo, Clone: boolPtr(true), Stdout: &out}, cfg, wsRoot, &out)

	if _, err := os.Stat(filepath.Join(wsRoot, "sample-app", "Dockerfile")); err != nil {
		t.Fatalf("shared worktree should be cloned: %v\nout: %s", err, out.String())
	}
	if strings.Contains(out.String(), "could not materialise") || strings.Contains(out.String(), "not declared") {
		t.Errorf("shared worktree should clone cleanly with no undeclared warning, got: %s", out.String())
	}
}

// An app whose worktree is missing and not declared in the block is warned.
func TestReconcileWorktrees_UndeclaredWarns(t *testing.T) {
	repo := t.TempDir()
	wsRoot := t.TempDir()
	writeApp(t, repo, "orphan", "orphan-wt")
	cfg := cfgWith() // empty block

	var out bytes.Buffer
	reconcileWorktrees(context.Background(), Options{RepoDir: repo, Clone: boolPtr(true), Stdout: &out}, cfg, wsRoot, &out)

	if !strings.Contains(out.String(), "not declared in workspace.repos") {
		t.Errorf("expected an undeclared-worktree warning, got: %s", out.String())
	}
}
