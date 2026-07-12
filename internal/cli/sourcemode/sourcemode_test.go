package sourcemode_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/sourcemode"
)

// writeApp drops a builders/base/<app>/gitrepository.yaml whose spec.url points
// at worktree `wt` (the join key), mirroring the real per-app manifest.
func writeApp(t *testing.T, repoDir, app, wt string) {
	t.Helper()
	dir := filepath.Join(repoDir, "builders", "base", app)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gr := "apiVersion: source.toolkit.fluxcd.io/v1\nkind: GitRepository\n" +
		"metadata:\n  name: " + app + "\n  namespace: flywheel-system\n" +
		"spec:\n  url: http://git-server.flywheel-system.svc.cluster.local:8080/" + wt + ".git\n"
	if err := os.WriteFile(filepath.Join(dir, "gitrepository.yaml"), []byte(gr), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cfg(repos ...schema.WorkspaceRepo) *schema.File {
	return &schema.File{Workspace: schema.Workspace{Repos: repos}}
}

func TestIsLocalOnly(t *testing.T) {
	if !sourcemode.IsLocalOnly(schema.WorkspaceRepo{Name: "web", LocalOnly: true}) {
		t.Error("local_only entry should be local-only")
	}
	if sourcemode.IsLocalOnly(schema.WorkspaceRepo{Name: "web", URL: "u"}) {
		t.Error("remote-backed entry should not be local-only")
	}
}

func TestValidSource(t *testing.T) {
	cases := []struct {
		r    schema.WorkspaceRepo
		want bool
	}{
		{schema.WorkspaceRepo{URL: "u"}, true},
		{schema.WorkspaceRepo{LocalOnly: true}, true},
		{schema.WorkspaceRepo{}, false},                          // neither
		{schema.WorkspaceRepo{URL: "u", LocalOnly: true}, false}, // both
	}
	for _, c := range cases {
		if got := sourcemode.ValidSource(c.r); got != c.want {
			t.Errorf("ValidSource(%+v) = %v, want %v", c.r, got, c.want)
		}
	}
}

func TestJoin(t *testing.T) {
	repo := t.TempDir()
	writeApp(t, repo, "web", "web")     // local-only worktree
	writeApp(t, repo, "api", "api")     // remote-backed worktree
	writeApp(t, repo, "orphan", "gone") // worktree not in workspace block
	c := cfg(
		schema.WorkspaceRepo{Name: "web", LocalOnly: true},
		schema.WorkspaceRepo{Name: "api", URL: "https://example.com/api.git"},
	)

	apps, err := sourcemode.Join(repo, c)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]sourcemode.App{}
	for _, a := range apps {
		byName[a.Name] = a
	}
	if len(apps) != 3 {
		t.Fatalf("want 3 joined apps, got %d: %+v", len(apps), apps)
	}
	if !byName["web"].Declared || !byName["web"].LocalOnly() {
		t.Errorf("web should be declared local-only: %+v", byName["web"])
	}
	if !byName["api"].Declared || byName["api"].LocalOnly() {
		t.Errorf("api should be declared remote-backed: %+v", byName["api"])
	}
	if byName["orphan"].Declared || byName["orphan"].LocalOnly() {
		t.Errorf("orphan should be undeclared and not local-only: %+v", byName["orphan"])
	}
	if byName["orphan"].Worktree != "gone" {
		t.Errorf("orphan worktree = %q, want gone", byName["orphan"].Worktree)
	}
}

func TestLocalOnlyApps(t *testing.T) {
	repo := t.TempDir()
	writeApp(t, repo, "web", "web")
	writeApp(t, repo, "api", "api")
	c := cfg(
		schema.WorkspaceRepo{Name: "web", LocalOnly: true},
		schema.WorkspaceRepo{Name: "api", URL: "https://example.com/api.git"},
	)
	got, err := sourcemode.LocalOnlyApps(repo, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "web" {
		t.Fatalf("LocalOnlyApps = %+v, want [web]", got)
	}
}

func TestUndeclared(t *testing.T) {
	repo := t.TempDir()
	writeApp(t, repo, "web", "web")
	writeApp(t, repo, "orphan", "gone")
	c := cfg(schema.WorkspaceRepo{Name: "web", LocalOnly: true})
	got, err := sourcemode.Undeclared(repo, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "orphan" || got[0].Worktree != "gone" {
		t.Fatalf("Undeclared = %+v, want [orphan→gone]", got)
	}
}

func TestGuard(t *testing.T) {
	local := []sourcemode.App{{Name: "web", Worktree: "web"}}
	cases := []struct {
		name          string
		localOnly     []sourcemode.App
		target, integ string
		want          sourcemode.Verdict
	}{
		{"none present", nil, "main", "main", sourcemode.Allow},
		{"present on integration branch", local, "main", "main", sourcemode.Block},
		{"present on feature branch", local, "feature", "main", sourcemode.Warn},
		{"present, configured integration branch", local, "develop", "develop", sourcemode.Block},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sourcemode.Guard(c.localOnly, c.target, c.integ); got != c.want {
				t.Errorf("Guard = %v, want %v", got, c.want)
			}
		})
	}
}

func TestOnIntegrationBranch(t *testing.T) {
	if !sourcemode.OnIntegrationBranch("main", "main") {
		t.Error("main == main should be on integration branch")
	}
	if sourcemode.OnIntegrationBranch("feature", "main") {
		t.Error("feature != main should not be on integration branch")
	}
}
