// Package clientci hosts tests for the client-skeleton CI/pre-commit shell
// guards under templates/client-skeleton/scripts/ci. The scripts can't carry a
// Go test next to them (anything under templates/ is embedded into the asset
// tree and rendered), so the test lives here and shells out to the script.
package clientci

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// scriptPath resolves templates/client-skeleton/scripts/ci/check-local-only.sh
// relative to this test file (internal/cli/clientci/).
func scriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	return filepath.Join(repoRoot, "templates", "client-skeleton", "scripts", "ci", "check-local-only.sh")
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"bash", "git", "yq"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping shell-guard test", bin)
		}
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// fakeRepo builds a client-repo-shaped temp dir on branch `branch`, with
// flywheel.yaml (optionally setting git.integration_branch) and one app whose
// worktree is recorded in the workspace block per `source`: "local-only" → a
// local_only entry; a URL → a remote-backed entry; "" → no entry (the app's
// worktree is undeclared = legacy → treated as not-local-only).
func fakeRepo(t *testing.T, branch, integrationBranch, app, source string) string {
	t.Helper()
	dir := t.TempDir()

	yaml := "schema: v1alpha1\n"
	if integrationBranch != "" {
		yaml += "git:\n  integration_branch: " + integrationBranch + "\n"
	}
	if app != "" && source != "" {
		yaml += "workspace:\n  repos:\n    - name: " + app + "\n"
		if source == "local-only" {
			yaml += "      local_only: true\n"
		} else {
			yaml += "      url: " + source + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "flywheel.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	if app != "" {
		appDir := filepath.Join(dir, "builders", "base", app)
		if err := os.MkdirAll(appDir, 0o755); err != nil {
			t.Fatal(err)
		}
		gr := "apiVersion: source.toolkit.fluxcd.io/v1\nkind: GitRepository\nmetadata:\n  name: " + app +
			"\n  namespace: flywheel-system\nspec:\n  url: http://git-server/" + app + ".git\n"
		if err := os.WriteFile(filepath.Join(appDir, "gitrepository.yaml"), []byte(gr), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "t@example.com")
	git(t, dir, "config", "user.name", "t")
	git(t, dir, "checkout", "-q", "-b", branch)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// run executes the guard in dir with optional GITHUB_BASE_REF, returning the
// exit code.
func run(t *testing.T, dir, baseRef string) int {
	t.Helper()
	cmd := exec.Command("bash", scriptPath(t))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GITHUB_BASE_REF="+baseRef)
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("run: %v", err)
	return -1
}

func TestCheckLocalOnly_BlocksOnIntegrationBranch(t *testing.T) {
	requireTools(t)
	dir := fakeRepo(t, "main", "", "web", "local-only")
	if code := run(t, dir, ""); code == 0 {
		t.Fatalf("expected non-zero exit for local-only on main, got 0")
	}
}

func TestCheckLocalOnly_WarnsOnFeatureBranch(t *testing.T) {
	requireTools(t)
	dir := fakeRepo(t, "feature-x", "", "web", "local-only")
	if code := run(t, dir, ""); code != 0 {
		t.Fatalf("expected exit 0 (warn) on a feature branch, got %d", code)
	}
}

// $GITHUB_BASE_REF (the PR's base) drives the decision even from a feature HEAD.
func TestCheckLocalOnly_BlocksViaBaseRef(t *testing.T) {
	requireTools(t)
	dir := fakeRepo(t, "feature-x", "", "web", "local-only")
	if code := run(t, dir, "main"); code == 0 {
		t.Fatalf("expected non-zero exit when GITHUB_BASE_REF=main, got 0")
	}
}

func TestCheckLocalOnly_RemoteBackedPasses(t *testing.T) {
	requireTools(t)
	dir := fakeRepo(t, "main", "", "web", "https://example.com/acme/web.git")
	if code := run(t, dir, ""); code != 0 {
		t.Fatalf("expected exit 0 for a remote-backed app on main, got %d", code)
	}
}

// A pre-feature app whose worktree has no workspace entry is not-local-only.
func TestCheckLocalOnly_LegacyAppPasses(t *testing.T) {
	requireTools(t)
	dir := fakeRepo(t, "main", "", "web", "")
	if code := run(t, dir, ""); code != 0 {
		t.Fatalf("expected exit 0 for a legacy (unannotated) app on main, got %d", code)
	}
}

// A configured integration_branch is honored.
func TestCheckLocalOnly_ConfiguredIntegrationBranch(t *testing.T) {
	requireTools(t)
	block := fakeRepo(t, "develop", "develop", "web", "local-only")
	if code := run(t, block, ""); code == 0 {
		t.Fatalf("expected non-zero on the configured integration branch 'develop', got 0")
	}
	pass := fakeRepo(t, "main", "develop", "web", "local-only")
	if code := run(t, pass, ""); code != 0 {
		t.Fatalf("expected exit 0 on 'main' when integration branch is 'develop', got %d", code)
	}
}
