package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cobr-io/flywheel/internal/cli/schema"
)

// A flywheel.yaml with comments and several sections — the realistic shape the
// writer must edit without trashing surrounding content.
const sampleYAML = `schema: v1alpha1

flywheel:
  version: v0.1.0    # pinned tag

client:
  name: acme

git:
  # keep this comment
  integration_branch: main
`

func write(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "flywheel.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// reparse round-trips the file through the schema so tests assert on the parsed
// shape rather than brittle byte layout.
func reparse(t *testing.T, path string) *schema.File {
	t.Helper()
	f, err := schema.Parse([]byte(read(t, path)))
	if err != nil {
		t.Fatalf("re-parse after edit: %v", err)
	}
	return f
}

func TestUpsertWorkspaceRepo_CreatesBlockAndPreservesComments(t *testing.T) {
	p := write(t, sampleYAML)
	if err := UpsertWorkspaceRepo(p, schema.WorkspaceRepo{Name: "sample-app", URL: "git@github.com:acme/sample-app.git"}); err != nil {
		t.Fatal(err)
	}
	got := read(t, p)

	// Comments and unrelated keys survive.
	if !strings.Contains(got, "# keep this comment") {
		t.Errorf("lost the git comment:\n%s", got)
	}
	if !strings.Contains(got, "# pinned tag") {
		t.Errorf("lost the inline flywheel.version comment:\n%s", got)
	}
	// The new entry parses back correctly.
	f := reparse(t, p)
	r, ok := f.WorkspaceRepo("sample-app")
	if !ok || r.URL != "git@github.com:acme/sample-app.git" || r.LocalOnly {
		t.Fatalf("entry round-trip = %+v, ok=%v", r, ok)
	}
	if f.Client.Name != "acme" || f.IntegrationBranch() != "main" {
		t.Errorf("unrelated fields changed: client=%q branch=%q", f.Client.Name, f.IntegrationBranch())
	}
}

func TestSetFlywheelVersion_UpdatesValueAndKeepsComment(t *testing.T) {
	p := write(t, sampleYAML)
	if err := SetFlywheelVersion(p, "v0.2.0"); err != nil {
		t.Fatal(err)
	}
	got := read(t, p)

	if !strings.Contains(got, "# pinned tag") {
		t.Errorf("lost the inline flywheel.version comment:\n%s", got)
	}
	if strings.Contains(got, "v0.1.0") {
		t.Errorf("old version still present:\n%s", got)
	}
	f := reparse(t, p)
	if f.Flywheel.Version != "v0.2.0" {
		t.Errorf("flywheel.version = %q, want v0.2.0", f.Flywheel.Version)
	}
	// Unrelated sections untouched.
	if f.Client.Name != "acme" || f.IntegrationBranch() != "main" {
		t.Errorf("unrelated fields changed: client=%q branch=%q", f.Client.Name, f.IntegrationBranch())
	}
}

func TestUpsertWorkspaceRepo_LocalOnly(t *testing.T) {
	p := write(t, sampleYAML)
	if err := UpsertWorkspaceRepo(p, schema.WorkspaceRepo{Name: "hello-py", LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	f := reparse(t, p)
	r, ok := f.WorkspaceRepo("hello-py")
	if !ok || !r.LocalOnly || r.URL != "" {
		t.Fatalf("local-only entry = %+v, ok=%v", r, ok)
	}
}

func TestUpsertWorkspaceRepo_Branch(t *testing.T) {
	p := write(t, sampleYAML)
	if err := UpsertWorkspaceRepo(p, schema.WorkspaceRepo{Name: "sample-svc", URL: "git@github.com:acme/sample-svc.git", Branch: "develop"}); err != nil {
		t.Fatal(err)
	}
	f := reparse(t, p)
	if r, ok := f.WorkspaceRepo("sample-svc"); !ok || r.Branch != "develop" {
		t.Fatalf("entry = %+v, ok=%v; want branch \"develop\"", r, ok)
	}
	// An empty Branch must not emit a key.
	if err := UpsertWorkspaceRepo(p, schema.WorkspaceRepo{Name: "nobranch", URL: "git@github.com:acme/n.git"}); err != nil {
		t.Fatal(err)
	}
	if r, _ := reparse(t, p).WorkspaceRepo("nobranch"); r.Branch != "" {
		t.Errorf("empty Branch emitted a value: %q", r.Branch)
	}
}

func TestUpsertWorkspaceRepo_IdempotentReplaceByName(t *testing.T) {
	p := write(t, sampleYAML)
	for _, u := range []string{"git@github.com:acme/a.git", "git@github.com:acme/a-renamed.git"} {
		if err := UpsertWorkspaceRepo(p, schema.WorkspaceRepo{Name: "a", URL: u}); err != nil {
			t.Fatal(err)
		}
	}
	f := reparse(t, p)
	if n := len(f.Workspace.Repos); n != 1 {
		t.Fatalf("upsert created a duplicate: %d entries", n)
	}
	if r, _ := f.WorkspaceRepo("a"); r.URL != "git@github.com:acme/a-renamed.git" {
		t.Errorf("second upsert did not replace: url=%q", r.URL)
	}
}

func TestSetWorkspaceRepoURL_FlipsLocalOnly(t *testing.T) {
	p := write(t, sampleYAML)
	if err := UpsertWorkspaceRepo(p, schema.WorkspaceRepo{Name: "hello-py", LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := SetWorkspaceRepoURL(p, "hello-py", "git@github.com:acme/hello-py.git"); err != nil {
		t.Fatal(err)
	}
	f := reparse(t, p)
	r, _ := f.WorkspaceRepo("hello-py")
	if r.LocalOnly || r.URL != "git@github.com:acme/hello-py.git" {
		t.Fatalf("flip did not take: %+v", r)
	}
	// The exactly-one invariant must hold: no leftover local_only key.
	if strings.Contains(read(t, p), "local_only") {
		t.Errorf("local_only key survived the flip:\n%s", read(t, p))
	}
}

func TestSetWorkspaceRepoURL_MissingEntry(t *testing.T) {
	p := write(t, sampleYAML)
	if err := SetWorkspaceRepoURL(p, "nope", "git@github.com:acme/nope.git"); err == nil {
		t.Fatal("expected an error for a worktree with no entry")
	}
}

const clusterYAML = `schema: v1alpha1

client:
  name: acme

cluster:
  name: acme-local
  registry: acme-local-registry
  registry_port: 50001
  http_port: 8080      # host → loadbalancer:80
  https_port: 8540
`

func TestSetClusterPort_ChangesValuePreservingCommentAndSiblings(t *testing.T) {
	p := write(t, clusterYAML)
	if err := SetClusterPort(p, "http_port", 8081); err != nil {
		t.Fatal(err)
	}
	got := read(t, p)
	// The inline comment on the edited line survives.
	if !strings.Contains(got, "# host → loadbalancer:80") {
		t.Errorf("lost the http_port comment:\n%s", got)
	}
	f := reparse(t, p)
	if f.Cluster.HttpPort != 8081 {
		t.Errorf("http_port = %d, want 8081", f.Cluster.HttpPort)
	}
	// Sibling ports and unrelated fields are untouched.
	if f.Cluster.RegistryPort != 50001 || f.Cluster.HttpsPort != 8540 {
		t.Errorf("siblings changed: registry=%d https=%d", f.Cluster.RegistryPort, f.Cluster.HttpsPort)
	}
	if f.Cluster.Name != "acme-local" || f.Client.Name != "acme" {
		t.Errorf("unrelated fields changed: cluster=%q client=%q", f.Cluster.Name, f.Client.Name)
	}
}

func TestSetClusterPort_AppendsWhenKeyAbsent(t *testing.T) {
	// A cluster block missing https_port — SetClusterPort must add it.
	const noHTTPS = `schema: v1alpha1
client:
  name: acme
cluster:
  name: acme-local
  registry: acme-local-registry
  registry_port: 50001
  http_port: 8080
`
	p := write(t, noHTTPS)
	if err := SetClusterPort(p, "https_port", 8540); err != nil {
		t.Fatal(err)
	}
	if f := reparse(t, p); f.Cluster.HttpsPort != 8540 {
		t.Errorf("https_port = %d, want 8540 (should have been appended)", f.Cluster.HttpsPort)
	}
}
