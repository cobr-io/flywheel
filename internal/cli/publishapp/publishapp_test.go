package publishapp

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cobr-io/flywheel/internal/cli/config"
	"github.com/cobr-io/flywheel/internal/cli/schema"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// setupRepo makes a gitops repo at <root>/acme with a minimal flywheel.yaml.
// workspaces_root defaults to the repo's parent (root), where worktrees live.
func setupRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "acme")
	if err := os.MkdirAll(filepath.Join(repo, "builders", "base"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "flywheel.yaml"), []byte("schema: v1alpha1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// writeApp writes builders/base/<name>/gitrepository.yaml whose spec.url
// encodes worktreeName (how publish-app resolves the worktree).
func writeApp(t *testing.T, repo, name, worktreeName string) {
	t.Helper()
	dir := filepath.Join(repo, "builders", "base", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gr := "apiVersion: source.toolkit.fluxcd.io/v1\nkind: GitRepository\nmetadata:\n  name: " + name +
		"\n  namespace: flywheel-system\nspec:\n  url: http://git-server/" + worktreeName + ".git\n"
	if err := os.WriteFile(filepath.Join(dir, "gitrepository.yaml"), []byte(gr), 0o644); err != nil {
		t.Fatal(err)
	}
}

// declare adds (or replaces) a workspace.repos entry in repo/flywheel.yaml,
// exercising the real writer the way add-app/publish-app do.
func declare(t *testing.T, repo string, r schema.WorkspaceRepo) {
	t.Helper()
	if err := config.UpsertWorkspaceRepo(filepath.Join(repo, "flywheel.yaml"), r); err != nil {
		t.Fatal(err)
	}
}

// wsEntry returns the workspace entry for the given worktree in repo/flywheel.yaml.
func wsEntry(t *testing.T, repo, worktree string) (schema.WorkspaceRepo, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, "flywheel.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := schema.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return f.WorkspaceRepo(worktree)
}

// mkWorktree creates a worktree under workspaces_root. withOrigin pushes it to
// a fresh bare repo (the returned URL). pushed=false leaves an unpushed commit.
func mkWorktree(t *testing.T, wsRoot, name string, withOrigin, pushed bool) string {
	t.Helper()
	dir := filepath.Join(wsRoot, name)
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

	if withOrigin {
		bare := filepath.Join(t.TempDir(), name+".git")
		git(t, ".", "init", "-q", "--bare", bare)
		git(t, dir, "remote", "add", "origin", bare)
		git(t, dir, "push", "-q", "-u", "origin", "HEAD")
		if !pushed {
			// A local commit that never reaches origin.
			if err := os.WriteFile(filepath.Join(dir, "more.txt"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "add", "-A")
			git(t, dir, "commit", "-q", "-m", "unpushed")
		}
	}
	return dir
}

func TestPublish_Success(t *testing.T) {
	repo := setupRepo(t)
	writeApp(t, repo, "web", "web")
	declare(t, repo, schema.WorkspaceRepo{Name: "web", LocalOnly: true})
	mkWorktree(t, filepath.Dir(repo), "web", true, true)

	var out bytes.Buffer
	if err := Run(Options{RepoDir: repo, Name: "web", Stdout: &out}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	r, ok := wsEntry(t, repo, "web")
	if !ok || r.LocalOnly {
		t.Errorf("entry should no longer be local-only: %+v", r)
	}
	if !strings.Contains(r.URL, "web.git") { // flipped to the origin (bare) URL
		t.Errorf("entry url should be the origin URL, got %q", r.URL)
	}
}

func TestPublish_NoOrigin(t *testing.T) {
	repo := setupRepo(t)
	writeApp(t, repo, "web", "web")
	declare(t, repo, schema.WorkspaceRepo{Name: "web", LocalOnly: true})
	mkWorktree(t, filepath.Dir(repo), "web", false, false) // no origin

	err := Run(Options{RepoDir: repo, Name: "web", Stdout: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("expected a no-origin error, got %v", err)
	}
	if r, _ := wsEntry(t, repo, "web"); !r.LocalOnly {
		t.Errorf("entry must stay local-only after a failed publish: %+v", r)
	}
}

func TestPublish_BranchNotPushed(t *testing.T) {
	repo := setupRepo(t)
	writeApp(t, repo, "web", "web")
	declare(t, repo, schema.WorkspaceRepo{Name: "web", LocalOnly: true})
	mkWorktree(t, filepath.Dir(repo), "web", true, false) // origin, but unpushed commit

	err := Run(Options{RepoDir: repo, Name: "web", Stdout: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "pushed") {
		t.Fatalf("expected an unpushed-branch error, got %v", err)
	}
	if r, _ := wsEntry(t, repo, "web"); !r.LocalOnly {
		t.Errorf("entry must stay local-only after a failed publish: %+v", r)
	}
}

func TestPublish_AlreadyRemoteBacked(t *testing.T) {
	repo := setupRepo(t)
	writeApp(t, repo, "web", "web")
	declare(t, repo, schema.WorkspaceRepo{Name: "web", URL: "https://example.com/acme/web.git"})

	var out bytes.Buffer
	if err := Run(Options{RepoDir: repo, Name: "web", Stdout: &out}); err != nil {
		t.Fatalf("Run (no-op) should not error: %v", err)
	}
	if !strings.Contains(out.String(), "already remote-backed") {
		t.Errorf("expected an already-remote-backed message, got: %s", out.String())
	}
}

// An app whose worktree has no workspace entry is a no-op (nothing to publish).
func TestPublish_NotDeclared(t *testing.T) {
	repo := setupRepo(t)
	writeApp(t, repo, "web", "web") // no declare()

	var out bytes.Buffer
	if err := Run(Options{RepoDir: repo, Name: "web", Stdout: &out}); err != nil {
		t.Fatalf("Run (no-op) should not error: %v", err)
	}
	if !strings.Contains(out.String(), "not declared in the workspace block") {
		t.Errorf("expected a not-declared message, got: %s", out.String())
	}
}

func TestPublish_UnknownApp(t *testing.T) {
	repo := setupRepo(t)
	err := Run(Options{RepoDir: repo, Name: "ghost", Stdout: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "no app") {
		t.Fatalf("expected a no-app error, got %v", err)
	}
}
