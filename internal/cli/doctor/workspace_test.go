package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRepo lays down a flywheel.yaml (with paths.workspaces_root pointing at
// wsRoot so the check doesn't fall back to the parent dir) and an optional
// workspace block, plus per-app gitrepository.yaml manifests.
func writeRepo(t *testing.T, wsRoot, workspaceBlock string, apps map[string]string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "builders", "base"), 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "schema: v1alpha1\npaths:\n  workspaces_root: " + wsRoot + "\n" + workspaceBlock
	if err := os.WriteFile(filepath.Join(repo, "flywheel.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, worktree := range apps {
		dir := filepath.Join(repo, "builders", "base", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		gr := "kind: GitRepository\nmetadata:\n  name: " + name +
			"\nspec:\n  url: http://git-server/" + worktree + ".git\n"
		if err := os.WriteFile(filepath.Join(dir, "gitrepository.yaml"), []byte(gr), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func mkGitDir(t *testing.T, wsRoot, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(wsRoot, name, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func runWorkspace(t *testing.T, repo string) error {
	t.Helper()
	return workspaceCheck(repo).Run(context.Background())
}

func TestWorkspaceCheck_AllPresent(t *testing.T) {
	wsRoot := t.TempDir()
	mkGitDir(t, wsRoot, "web")
	repo := writeRepo(t, wsRoot,
		"workspace:\n  repos:\n    - name: web\n      url: git@x:web.git\n",
		map[string]string{"web": "web"})
	if err := runWorkspace(t, repo); err != nil {
		t.Errorf("all-present should pass, got: %v", err)
	}
}

func TestWorkspaceCheck_Findings(t *testing.T) {
	wsRoot := t.TempDir()
	// "occupied" exists but is not a git checkout.
	if err := os.MkdirAll(filepath.Join(wsRoot, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := writeRepo(t, wsRoot,
		"workspace:\n  repos:\n    - name: rem\n      url: git@x:rem.git\n    - name: loc\n      local_only: true\n    - name: occupied\n      url: git@x:occ.git\n",
		map[string]string{"orphan": "orphan-wt"}) // app referencing an undeclared worktree

	err := runWorkspace(t, repo)
	if err == nil {
		t.Fatal("expected findings, got nil")
	}
	for _, want := range []string{"rem", "loc", "occupied", "orphan→orphan-wt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("findings %q should mention %q", err.Error(), want)
		}
	}

	// Workspace findings are advisories (T25): never gate `up`, so they must
	// surface as SeverityWarn, not the default SeverityError.
	res := Run([]Check{workspaceCheck(repo)})[0]
	if res.OK() {
		t.Fatal("expected the same findings via Run")
	}
	if res.Severity != SeverityWarn {
		t.Errorf("Severity = %q, want %q (workspace findings must not fail `flywheel doctor`)", res.Severity, SeverityWarn)
	}
}

// Outside a flywheel repo the check is a no-op (passes) and writes nothing.
func TestWorkspaceCheck_NotARepoNoop(t *testing.T) {
	if err := runWorkspace(t, t.TempDir()); err != nil {
		t.Errorf("no flywheel.yaml should be a no-op, got: %v", err)
	}
}
