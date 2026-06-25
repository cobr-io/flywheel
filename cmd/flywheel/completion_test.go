package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const completionFlywheelYAML = `schema: v1alpha1
flywheel:
  version: v0.1.0
client:
  name: acme
cluster:
  name: acme-local
  registry: acme-local-registry
  registry_port: 50001
  http_port: 50002
  https_port: 50003
namespaces:
  flywheel: flywheel-system
  apps: apps
flux:
  interval_local: 10s
workspace:
  repos:
    - name: webwt
      local_only: true
    - name: apiwt
      url: git@x:api.git
`

func writeGitRepo(t *testing.T, repo, app, worktree string) {
	t.Helper()
	dir := filepath.Join(repo, "builders", "base", app)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gr := "kind: GitRepository\nmetadata:\n  name: " + app +
		"\nspec:\n  url: http://git-server/" + worktree + ".git\n"
	if err := os.WriteFile(filepath.Join(dir, "gitrepository.yaml"), []byte(gr), 0o644); err != nil {
		t.Fatal(err)
	}
}

// completeLocalOnlyApps offers only apps whose worktree is local_only in the
// workspace block.
func TestCompleteLocalOnlyApps_FromBlock(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "flywheel.yaml"), []byte(completionFlywheelYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	writeGitRepo(t, repo, "web", "webwt") // local_only worktree → candidate
	writeGitRepo(t, repo, "api", "apiwt") // remote-backed worktree → not offered
	t.Chdir(repo)

	got, _ := completeLocalOnlyApps(nil, nil, "")
	if !slices.Equal(got, []string{"web"}) {
		t.Errorf("completeLocalOnlyApps = %v, want [web]", got)
	}
}
