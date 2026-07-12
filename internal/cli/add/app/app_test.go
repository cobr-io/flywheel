package app

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/cobr-io/flywheel/internal/cli/schema"
)

// workspaceRepo loads repo/flywheel.yaml and returns the workspace entry for
// the given worktree name.
func workspaceRepo(t *testing.T, repo, name string) (schema.WorkspaceRepo, bool) {
	t.Helper()
	f, err := schema.Parse([]byte(readFile(t, filepath.Join(repo, "flywheel.yaml"))))
	if err != nil {
		t.Fatalf("parse flywheel.yaml: %v", err)
	}
	return f.WorkspaceRepo(name)
}

const fixtureFlywheelYAML = `schema: v1alpha1
client:
  name: acme
flywheel:
  version: v0.1.0
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
local:
  domain: localdev.me
`

const fixtureKustomization = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
`

// setupRepo creates a gitops repo at <root>/acme (root = t.TempDir()), so the
// repo's parent — the default workspaces_root — is a clean directory under which
// worktrees can be created with mkWorktree. Returns the repo dir.
func setupRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "acme")
	for _, sub := range []string{"builders/base", "apps/base"} {
		if err := os.MkdirAll(filepath.Join(repo, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, sub, "kustomization.yaml"), []byte(fixtureKustomization), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "flywheel.yaml"), []byte(fixtureFlywheelYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the gitops repo a real git repo on a feature branch — the Layer-1
	// local-only guard reads its current branch, and registering a local-only
	// app is only allowed off the integration branch. Tests that exercise the
	// refusal switch to the integration branch explicitly.
	gitDo(t, repo, "init", "-q")
	gitDo(t, repo, "config", "user.email", "t@example.com")
	gitDo(t, repo, "config", "user.name", "t")
	gitDo(t, repo, "checkout", "-q", "-b", "work")
	gitDo(t, repo, "add", "-A")
	gitDo(t, repo, "commit", "-q", "-m", "init")
	return repo
}

// mkWorktree creates a worktree directory `name` under the repo's
// workspaces_root (its parent), writing any provided files into it. Returns the
// directory path.
func mkWorktree(t *testing.T, repo, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(repo), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, content := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// add-app pre-flights a Dockerfile (the build needs one). Provide a
	// default at the worktree root unless the caller already supplied one;
	// tests using a non-default --context add their own Dockerfile.
	if _, ok := files["Dockerfile"]; !ok {
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAddApp_RendersBuilder(t *testing.T) {
	repo := setupRepo(t)
	mkWorktree(t, repo, "hello", nil)
	var out bytes.Buffer
	res, err := Run(Options{RepoDir: repo, Worktree: "hello", Stdout: &out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.BuilderDir != filepath.Join(repo, "builders", "base", "hello") {
		t.Errorf("BuilderDir = %q", res.BuilderDir)
	}
	// All seven per-app-template files should be rendered.
	wantFiles := []string{
		"kustomization.yaml", "gitrepository.yaml", "build-config.yaml",
		"git-auto-sync.yaml", "imagerepository.yaml", "imagepolicy.yaml", "README.md",
	}
	for _, f := range wantFiles {
		if _, err := os.Stat(filepath.Join(res.BuilderDir, f)); err != nil {
			t.Errorf("expected %s rendered: %v", f, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(res.BuilderDir, "gitrepository.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "name: hello") {
		t.Errorf("expected `name: hello` in gitrepository.yaml, got:\n%s", raw)
	}
	// All builder-side infra runs in flywheel-system, not apps.
	for _, f := range []string{"gitrepository.yaml", "build-config.yaml", "git-auto-sync.yaml"} {
		b, err := os.ReadFile(filepath.Join(res.BuilderDir, f))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "namespace: flywheel-system") {
			t.Errorf("%s should be in flywheel-system, got:\n%s", f, b)
		}
		if strings.Contains(string(b), "namespace: apps") {
			t.Errorf("%s should not reference the apps namespace, got:\n%s", f, b)
		}
	}
	gas, err := os.ReadFile(filepath.Join(res.BuilderDir, "git-auto-sync.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gas), "GITREPOSITORY_NAMESPACE") ||
		!strings.Contains(string(gas), "value: flywheel-system") {
		t.Errorf("git-auto-sync.yaml should patch the flywheel-system GitRepository, got:\n%s", gas)
	}
}

// A worktree with no origin is recorded local-only in the workspace block, and
// the rendered GitRepository carries no source annotation.
func TestAddApp_Workspace_LocalOnly(t *testing.T) {
	repo := setupRepo(t)
	mkWorktree(t, repo, "hello", nil) // plain dir, no git remote
	var out bytes.Buffer

	res, err := Run(Options{RepoDir: repo, Worktree: "hello", Stdout: &out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r, ok := workspaceRepo(t, repo, "hello")
	if !ok || !r.LocalOnly || r.URL != "" {
		t.Errorf("workspace entry = %+v, ok=%v; want local-only", r, ok)
	}
	gr := readFile(t, filepath.Join(res.BuilderDir, "gitrepository.yaml"))
	if strings.Contains(gr, "flywheel.cobr.io/source") {
		t.Errorf("GitRepository should no longer carry the source annotation:\n%s", gr)
	}
}

// A worktree with an origin remote is recorded remote-backed (url) in the block.
func TestAddApp_Workspace_RemoteBacked(t *testing.T) {
	repo := setupRepo(t)
	dir := mkWorktree(t, repo, "hello", nil)
	const remote = "https://example.com/acme/hello.git"
	for _, args := range [][]string{{"init"}, {"remote", "add", "origin", remote}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
	}
	var out bytes.Buffer

	if _, err := Run(Options{RepoDir: repo, Worktree: "hello", Stdout: &out}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	r, ok := workspaceRepo(t, repo, "hello")
	if !ok || r.LocalOnly || r.URL != remote {
		t.Errorf("workspace entry = %+v, ok=%v; want url %s", r, ok, remote)
	}
}

// mkSourceRepo builds a hermetic git repo (with a committed Dockerfile) to
// clone from, and returns a file:// URL pointing at it.
func mkSourceRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"},
	} {
		gitDo(t, dir, args...)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDo(t, dir, "add", "Dockerfile")
	gitDo(t, dir, "commit", "-m", "init")
	return "file://" + dir
}

func gitDo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, b)
	}
}

// add-app with a git URL clones the repo as a sibling and registers it,
// recording the clone URL as the source.
func TestAddApp_CloneMode(t *testing.T) {
	repo := setupRepo(t)
	url := mkSourceRepo(t, "hello")
	var out bytes.Buffer

	if _, err := Run(Options{RepoDir: repo, Worktree: url, Stdout: &out}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The sibling worktree was materialised under workspaces_root.
	wsRoot := filepath.Dir(repo)
	if _, statErr := os.Stat(filepath.Join(wsRoot, "hello", "Dockerfile")); statErr != nil {
		t.Errorf("expected cloned worktree at %s/hello: %v", wsRoot, statErr)
	}
	// Clone-mode records the clone URL as the worktree's workspace source.
	if r, ok := workspaceRepo(t, repo, "hello"); !ok || r.URL != url {
		t.Errorf("clone-mode workspace entry = %+v, ok=%v; want url %q", r, ok, url)
	}
}

// add-app --branch in clone-mode checks the requested branch out on the clone
// and records it in the workspace entry.
func TestAddApp_CloneMode_Branch(t *testing.T) {
	repo := setupRepo(t)
	url := mkSourceRepo(t, "hello")
	gitDo(t, strings.TrimPrefix(url, "file://"), "branch", "feature")
	var out bytes.Buffer

	if _, err := Run(Options{RepoDir: repo, Worktree: url, Branch: "feature", Stdout: &out}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	wsRoot := filepath.Dir(repo)
	gotBranch, err := exec.Command("git", "-C", filepath.Join(wsRoot, "hello"), "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if b := strings.TrimSpace(string(gotBranch)); b != "feature" {
		t.Errorf("cloned worktree on branch %q, want \"feature\"", b)
	}
	if r, ok := workspaceRepo(t, repo, "hello"); !ok || r.Branch != "feature" {
		t.Errorf("workspace entry = %+v, ok=%v; want branch \"feature\"", r, ok)
	}
}

// An invalid --name is rejected BEFORE the clone, leaving no stray directory.
func TestAddApp_CloneMode_InvalidNameRejectedBeforeClone(t *testing.T) {
	repo := setupRepo(t)
	url := mkSourceRepo(t, "hello")
	wsRoot := filepath.Dir(repo)

	_, err := Run(Options{RepoDir: repo, Worktree: url, Name: "Bad_Name", Stdout: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "invalid --name") {
		t.Fatalf("expected an invalid --name error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(wsRoot, "Bad_Name")); !os.IsNotExist(statErr) {
		t.Errorf("invalid --name must be rejected before cloning (no stray dir)")
	}
}

// Clone mode refuses to clone over an existing directory.
func TestAddApp_CloneMode_RefusesExisting(t *testing.T) {
	repo := setupRepo(t)
	wsRoot := filepath.Dir(repo)
	if err := os.MkdirAll(filepath.Join(wsRoot, "hello"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	_, err := Run(Options{RepoDir: repo, Worktree: "file:///tmp/whatever/hello", Stdout: &out})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected an already-exists error, got %v", err)
	}
}

// Layer 1 of the local-only guard: a local-only app is allowed on a feature
// branch (with a publish reminder) and refused on the integration branch.
func TestAddApp_LocalOnlyGuard_FeatureBranchAllowed(t *testing.T) {
	repo := setupRepo(t) // on branch "work"
	mkWorktree(t, repo, "hello", nil)
	var out bytes.Buffer

	if _, err := Run(Options{RepoDir: repo, Worktree: "hello", Stdout: &out}); err != nil {
		t.Fatalf("local-only on a feature branch should be allowed: %v", err)
	}
	if !strings.Contains(out.String(), "publish-app") {
		t.Errorf("expected a publish-app reminder, got: %s", out.String())
	}
}

func TestAddApp_LocalOnlyGuard_IntegrationBranchRefused(t *testing.T) {
	repo := setupRepo(t)
	gitDo(t, repo, "checkout", "-q", "-b", "main") // the integration branch
	mkWorktree(t, repo, "hello", nil)

	_, err := Run(Options{RepoDir: repo, Worktree: "hello", Stdout: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "refuse to register a local-only app") {
		t.Fatalf("local-only on the integration branch should be refused, got %v", err)
	}
	// Nothing scaffolded.
	if _, statErr := os.Stat(filepath.Join(repo, "builders", "base", "hello")); !os.IsNotExist(statErr) {
		t.Errorf("builder dir should not exist after a refused registration")
	}
}

// A remote-backed app is allowed on the integration branch (the guard only
// blocks local-only apps).
func TestAddApp_RemoteBackedAllowedOnIntegrationBranch(t *testing.T) {
	repo := setupRepo(t)
	gitDo(t, repo, "checkout", "-q", "-b", "main")
	dir := mkWorktree(t, repo, "hello", nil)
	gitDo(t, dir, "init")
	gitDo(t, dir, "remote", "add", "origin", "https://example.com/acme/hello.git")
	var out bytes.Buffer

	if _, err := Run(Options{RepoDir: repo, Worktree: "hello", Stdout: &out}); err != nil {
		t.Fatalf("remote-backed app on the integration branch should be allowed: %v", err)
	}
}

// A worktree with no Dockerfile is rejected before anything is scaffolded.
func TestAddApp_DockerfilePreflight_Missing(t *testing.T) {
	repo := setupRepo(t)
	// Create the worktree directly so it has NO Dockerfile (mkWorktree would
	// add a default one).
	dir := filepath.Join(filepath.Dir(repo), "nodfile")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	_, err := Run(Options{RepoDir: repo, Worktree: "nodfile", Stdout: &out})
	if err == nil || !strings.Contains(err.Error(), "no Dockerfile") {
		t.Fatalf("expected a no-Dockerfile error, got %v", err)
	}
	// Nothing should have been scaffolded.
	if _, statErr := os.Stat(filepath.Join(repo, "builders", "base", "nodfile")); !os.IsNotExist(statErr) {
		t.Errorf("builder dir should not exist after a pre-flight failure")
	}
}

func TestAddApp_TargetInBuildConfig(t *testing.T) {
	repo := setupRepo(t)
	mkWorktree(t, repo, "svc", map[string]string{"backend/Dockerfile": "FROM scratch\n"})
	var out bytes.Buffer

	// With --target set, build-config carries the target stage.
	res, err := Run(Options{
		RepoDir: repo, Worktree: "svc",
		Context: "backend", Dockerfile: "Dockerfile", Target: "production",
		Stdout: &out,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	bc, err := os.ReadFile(filepath.Join(res.BuilderDir, "build-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bc), `target: "production"`) {
		t.Errorf("expected `target: \"production\"` in build-config.yaml, got:\n%s", bc)
	}

	// Without a target, no `target:` line is emitted (preserves the
	// last-stage default and keeps existing repos byte-identical).
	repo2 := setupRepo(t)
	mkWorktree(t, repo2, "svc", nil)
	res2, err := Run(Options{RepoDir: repo2, Worktree: "svc", Stdout: &out})
	if err != nil {
		t.Fatalf("Run (no target): %v", err)
	}
	bc2, err := os.ReadFile(filepath.Join(res2.BuilderDir, "build-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bc2), "target:") {
		t.Errorf("expected no `target:` line when --target unset, got:\n%s", bc2)
	}
}

func TestAddApp_RendersAppsTemplate(t *testing.T) {
	repo := setupRepo(t)
	mkWorktree(t, repo, "hello", nil)
	res, err := Run(Options{RepoDir: repo, Worktree: "hello", Stdout: io.Discard})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AppsDir != filepath.Join(repo, "apps", "base", "hello") {
		t.Errorf("AppsDir = %q", res.AppsDir)
	}
	if want := "https://hello.localdev.me:50003/"; res.URL != want {
		t.Errorf("Result.URL = %q, want %q", res.URL, want)
	}
	dep, err := os.ReadFile(filepath.Join(res.AppsDir, "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(dep)
	for _, want := range []string{
		"name: hello", "namespace: apps",
		"k3d-acme-local-registry:5000/acme/hello",
		"host: hello.localdev.me",
		`{"$imagepolicy": "flux-system:hello"}`,
		"ingressClassName: traefik",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("deployment.yaml missing %q:\n%s", want, got)
		}
	}
}

// TestAddApp_DecouplesNameFromDir is the core Phase 2 behavior: the worktree
// directory and the app name are independent. The name drives logical identity
// (folders, resource names, Ingress host, image); the directory drives the
// physical bindings (the /workspaces mount, the bare-repo URL, the GR url).
func TestAddApp_DecouplesNameFromDir(t *testing.T) {
	repo := setupRepo(t)
	mkWorktree(t, repo, "sample-app", nil)
	res, err := Run(Options{RepoDir: repo, Worktree: "sample-app", Name: "frontend", Stdout: io.Discard})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Logical identity follows the name.
	if res.BuilderDir != filepath.Join(repo, "builders", "base", "frontend") {
		t.Errorf("BuilderDir = %q, want .../frontend", res.BuilderDir)
	}
	if res.URL != "https://frontend.localdev.me:50003/" {
		t.Errorf("URL = %q, want frontend host", res.URL)
	}
	gr, _ := os.ReadFile(filepath.Join(res.BuilderDir, "gitrepository.yaml"))
	if !strings.Contains(string(gr), "name: frontend") {
		t.Errorf("GitRepository should be named frontend:\n%s", gr)
	}
	// Physical bindings follow the directory.
	if !strings.Contains(string(gr), "/sample-app.git") {
		t.Errorf("GitRepository url should point at sample-app.git:\n%s", gr)
	}
	gas, _ := os.ReadFile(filepath.Join(res.BuilderDir, "git-auto-sync.yaml"))
	s := string(gas)
	if !strings.Contains(s, "/workspaces/sample-app") || !strings.Contains(s, "/sample-app.git") {
		t.Errorf("git-auto-sync should mount /workspaces/sample-app and push sample-app.git:\n%s", s)
	}
	if !strings.Contains(s, "value: frontend") { // GITREPOSITORY_NAME = app name
		t.Errorf("git-auto-sync GITREPOSITORY_NAME should be the app name frontend:\n%s", s)
	}
}

// TestAddApp_DerivesNameFromManifest: with no --name, the app name comes from
// the worktree's project manifest, not the directory name.
func TestAddApp_DerivesNameFromManifest(t *testing.T) {
	repo := setupRepo(t)
	mkWorktree(t, repo, "myrepo", map[string]string{"package.json": `{"name":"cool-app"}`})
	res, err := Run(Options{RepoDir: repo, Worktree: "myrepo", Stdout: io.Discard})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.BuilderDir != filepath.Join(repo, "builders", "base", "cool-app") {
		t.Errorf("BuilderDir = %q, want .../cool-app (derived)", res.BuilderDir)
	}
	gas, _ := os.ReadFile(filepath.Join(res.BuilderDir, "git-auto-sync.yaml"))
	if !strings.Contains(string(gas), "/workspaces/myrepo") {
		t.Errorf("worktree mount should still be /workspaces/myrepo:\n%s", gas)
	}
}

func TestAddApp_AppendsToKustomization(t *testing.T) {
	repo := setupRepo(t)
	mkWorktree(t, repo, "hello", nil)
	if _, err := Run(Options{RepoDir: repo, Worktree: "hello", Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(repo, "builders", "base", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "  - ./hello") {
		t.Errorf("kustomization.yaml missing `  - ./hello`:\n%s", raw)
	}
	if strings.Contains(string(raw), "resources: []") {
		t.Errorf("kustomization.yaml still has inline empty list:\n%s", raw)
	}
}

func TestAddApp_AppendsToAppsKustomization(t *testing.T) {
	repo := setupRepo(t)
	for _, name := range []string{"foo", "bar"} {
		mkWorktree(t, repo, name, nil)
		if _, err := Run(Options{RepoDir: repo, Worktree: name, Stdout: io.Discard}); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(repo, "apps", "base", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{"  - ./foo", "  - ./bar"} {
		if !strings.Contains(s, want) {
			t.Errorf("apps/base/kustomization.yaml missing %q:\n%s", want, s)
		}
	}
	if strings.Index(s, "./foo") > strings.Index(s, "./bar") {
		t.Errorf("entries out of order:\n%s", s)
	}
}

func TestAddApp_MultipleApps(t *testing.T) {
	repo := setupRepo(t)
	for _, name := range []string{"first", "second", "third"} {
		mkWorktree(t, repo, name, nil)
		if _, err := Run(Options{RepoDir: repo, Worktree: name, Stdout: io.Discard}); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(repo, "builders", "base", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	i1, i2, i3 := strings.Index(s, "./first"), strings.Index(s, "./second"), strings.Index(s, "./third")
	if !(i1 >= 0 && i1 < i2 && i2 < i3) {
		t.Errorf("entries missing/out of order: first=%d second=%d third=%d\n%s", i1, i2, i3, s)
	}
}

func TestAddApp_RefusesExistingBuilder(t *testing.T) {
	repo := setupRepo(t)
	mkWorktree(t, repo, "hello", nil)
	if _, err := Run(Options{RepoDir: repo, Worktree: "hello", Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(Options{RepoDir: repo, Worktree: "hello", Stdout: io.Discard}); err == nil {
		t.Fatal("expected error when builder dir already exists")
	}
}

func TestAddApp_RefusesIfAppsDirExists(t *testing.T) {
	repo := setupRepo(t)
	mkWorktree(t, repo, "hello", nil)
	if err := os.MkdirAll(filepath.Join(repo, "apps", "base", "hello"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(Options{RepoDir: repo, Worktree: "hello", Stdout: io.Discard}); err == nil {
		t.Fatal("expected error when apps dir already exists")
	}
	if _, err := os.Stat(filepath.Join(repo, "builders", "base", "hello")); !os.IsNotExist(err) {
		t.Errorf("pre-flight failed too late: builders/base/hello was created (err=%v)", err)
	}
}

// TestAddApp_RenderFailureLeavesRepoUntouched is the transactional invariant: a
// render failure (injected via a bad Options.TemplateFS) must leave flywheel.yaml
// byte-for-byte untouched and scaffold nothing — the workspace registration is
// the LAST thing Run does, after the renders succeed.
func TestAddApp_RenderFailureLeavesRepoUntouched(t *testing.T) {
	repo := setupRepo(t)
	mkWorktree(t, repo, "hello", nil)

	before := readFile(t, filepath.Join(repo, "flywheel.yaml"))
	beforeBuildersKust := readFile(t, filepath.Join(repo, "builders", "base", "kustomization.yaml"))

	// A template referencing an undefined key fails at render time
	// (render.Tree renders with missingkey=error).
	badFS := fstest.MapFS{
		"kustomization.yaml.tmpl": &fstest.MapFile{Data: []byte("name: {{ .DoesNotExist }}\n")},
	}
	_, err := Run(Options{RepoDir: repo, Worktree: "hello", TemplateFS: badFS, Stdout: io.Discard})
	if err == nil {
		t.Fatal("expected a render failure")
	}

	// flywheel.yaml is untouched: no workspace entry was written.
	if after := readFile(t, filepath.Join(repo, "flywheel.yaml")); after != before {
		t.Errorf("flywheel.yaml was mutated despite a render failure:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
	if _, ok := workspaceRepo(t, repo, "hello"); ok {
		t.Errorf("workspace entry should not exist after a render failure")
	}
	// The kustomizations and scaffold dirs are untouched too.
	if after := readFile(t, filepath.Join(repo, "builders", "base", "kustomization.yaml")); after != beforeBuildersKust {
		t.Errorf("builders/base/kustomization.yaml was mutated despite a render failure:\n%s", after)
	}
	if _, statErr := os.Stat(filepath.Join(repo, "builders", "base", "hello")); !os.IsNotExist(statErr) {
		t.Errorf("builder dir should not exist after a render failure")
	}
	if _, statErr := os.Stat(filepath.Join(repo, "apps", "base", "hello")); !os.IsNotExist(statErr) {
		t.Errorf("apps dir should not exist after a render failure")
	}
	// The staging directory must be cleaned up (no leftover under the repo root).
	entries, err := os.ReadDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".flywheel-add-app-") {
			t.Errorf("staging directory leaked: %s", e.Name())
		}
	}
}

func TestAddApp_MissingWorktreeErrors(t *testing.T) {
	repo := setupRepo(t)
	// No worktree dir created → resolution fails with the minimum-requirements
	// guidance (#2): mention the Dockerfile requirement and the clone escape hatch.
	_, err := Run(Options{RepoDir: repo, Worktree: "ghost", Stdout: io.Discard})
	if err == nil {
		t.Fatal("expected error for a non-existent worktree directory")
	}
	if !strings.Contains(err.Error(), "Dockerfile") || !strings.Contains(err.Error(), "git URL to clone") {
		t.Errorf("expected minimum-requirements guidance, got: %v", err)
	}
}

func TestAddApp_RejectsInvalidNameOverride(t *testing.T) {
	repo := setupRepo(t)
	mkWorktree(t, repo, "src", nil)
	for _, name := range []string{"UpperCase", "with_underscore", "-leading-dash", "trailing-dash-", "spaces in name"} {
		if _, err := Run(Options{RepoDir: repo, Worktree: "src", Name: name, Stdout: io.Discard}); err == nil {
			t.Errorf("expected rejection for invalid --name %q", name)
		}
	}
}

func TestAddApp_ErrorsOnMissingLocalDomain(t *testing.T) {
	repo := setupRepo(t)
	withoutDomain := strings.Replace(fixtureFlywheelYAML,
		"local:\n  domain: localdev.me\n", "", 1)
	if err := os.WriteFile(filepath.Join(repo, "flywheel.yaml"), []byte(withoutDomain), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(Options{RepoDir: repo, Worktree: "hello", Stdout: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "local.domain") {
		t.Fatalf("expected local.domain error, got %v", err)
	}
}

// TestAddApp_PinsCanonicalGitAutoSyncImage locks in the design invariant: the
// committed per-app git-auto-sync.yaml pins the *canonical* ghcr.io ref
// (imagepin.DefaultRef at the flywheel version), NOT a resolved/dogfood ref —
// even when flywheel.yaml(.local) carries a dogfood images override. The
// override reaches the sidecar at `up` time via the client-builders
// Kustomization's spec.images rewrite, so baking it into the committed manifest
// would only pin a digest that goes stale on the next dogfood rebuild.
func TestAddApp_PinsCanonicalGitAutoSyncImage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		overlay func(t *testing.T, repo string)
	}{
		{"override in flywheel.yaml.local", func(t *testing.T, repo string) {
			localYAML := "flywheel:\n  images:\n    git-auto-sync: flywheel-dev/git-auto-sync:dogfood\n"
			if err := os.WriteFile(filepath.Join(repo, "flywheel.yaml.local"), []byte(localYAML), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"override in flywheel.yaml", func(t *testing.T, repo string) {
			with := strings.Replace(fixtureFlywheelYAML, "flywheel:\n  version: v0.1.0\n",
				"flywheel:\n  version: v0.1.0\n  images:\n    git-auto-sync: flywheel-dev/git-auto-sync:dogfood\n", 1)
			if err := os.WriteFile(filepath.Join(repo, "flywheel.yaml"), []byte(with), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := setupRepo(t)
			mkWorktree(t, repo, "hello", nil)
			tc.overlay(t, repo)
			if _, err := Run(Options{RepoDir: repo, Worktree: "hello", Stdout: io.Discard}); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(filepath.Join(repo, "builders", "base", "hello", "git-auto-sync.yaml"))
			if !strings.Contains(string(raw), "ghcr.io/cobr-io/git-auto-sync:v0.1.0") {
				t.Errorf("git-auto-sync.yaml should pin the canonical ghcr.io ref:\n%s", raw)
			}
			if strings.Contains(string(raw), "flywheel-dev/git-auto-sync:dogfood") {
				t.Errorf("git-auto-sync.yaml must not bake the dogfood override (it goes stale):\n%s", raw)
			}
		})
	}
}

func TestAppURL(t *testing.T) {
	cases := []struct {
		name, domain string
		port         int
		want         string
	}{
		{"hello-py", "localdev.me", 8540, "https://hello-py.localdev.me:8540/"},
		{"app", "localdev.me", 443, "https://app.localdev.me/"},
		{"app", "localdev.me", 0, "https://app.localdev.me/"},
	}
	for _, tc := range cases {
		if got := appURL(tc.name, tc.domain, tc.port); got != tc.want {
			t.Errorf("appURL(%q,%q,%d) = %q, want %q", tc.name, tc.domain, tc.port, got, tc.want)
		}
	}
}

func TestDeriveName(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		want    string
		source  string
		wantErr bool
	}{
		{"package.json scoped", map[string]string{"package.json": `{"name":"@acme/Web_App"}`}, "web-app", "package.json", false},
		{"pyproject [project]", map[string]string{"pyproject.toml": "[project]\nname = \"my_service\"\n"}, "my-service", "pyproject.toml", false},
		{"pyproject poetry", map[string]string{"pyproject.toml": "[tool.poetry]\nname = \"poetry-app\"\n"}, "poetry-app", "pyproject.toml", false},
		{"go.mod basename", map[string]string{"go.mod": "module github.com/acme/hello-app\n\ngo 1.23\n"}, "hello-app", "go.mod", false},
		{"go.mod vN suffix", map[string]string{"go.mod": "module github.com/acme/foo/v2\n\ngo 1.23\n"}, "foo", "go.mod", false},
		{"cargo", map[string]string{"Cargo.toml": "[package]\nname = \"rust_app\"\n"}, "rust-app", "Cargo.toml", false},
		{"composer vendor", map[string]string{"composer.json": `{"name":"acme/php-app"}`}, "php-app", "composer.json", false},
		{"setup.cfg", map[string]string{"setup.cfg": "[metadata]\nname = setup_pkg\nversion = 1.0\n"}, "setup-pkg", "setup.cfg", false},
		{"pom.xml artifactId", map[string]string{"pom.xml": `<project><artifactId>my-maven-app</artifactId><parent><artifactId>parent-pom</artifactId></parent></project>`}, "my-maven-app", "pom.xml", false},
		{"gemspec", map[string]string{"thing.gemspec": "Gem::Specification.new do |spec|\n  spec.name = \"ruby-gem\"\nend\n"}, "ruby-gem", "*.gemspec", false},
		{"no manifest", map[string]string{"README.md": "hi"}, "", "", false},
		{"agree", map[string]string{"package.json": `{"name":"same"}`, "go.mod": "module x/same\n"}, "same", "package.json", false},
		{"conflict", map[string]string{"package.json": `{"name":"web"}`, "go.mod": "module x/api\n"}, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for f, c := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, src, err := deriveName(dir)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected a conflict error, got name=%q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("deriveName: %v", err)
			}
			if got != tc.want {
				t.Errorf("name = %q, want %q", got, tc.want)
			}
			if tc.source != "" && src != tc.source {
				t.Errorf("source = %q, want %q", src, tc.source)
			}
		})
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"Hello_World":   "hello-world",
		"@scope/My.App": "my-app",
		"vendor/pkg":    "pkg",
		"  spaced  ":    "spaced",
		"--dashes--":    "dashes",
		"___":           "",
		"a.b_c/d":       "d",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeName(strings.Repeat("a", 70)); len(got) != 63 {
		t.Errorf("sanitizeName truncation: len=%d, want 63", len(got))
	}
}

func TestResolveWorktree(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "gitops")
	app := filepath.Join(root, "app")
	for _, d := range []string{cwd, app, filepath.Join(root, "sub", "deep")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "afile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	ok := func(arg string) {
		name, abs, err := resolveWorktree(arg, root, cwd)
		if err != nil {
			t.Errorf("resolveWorktree(%q) unexpected err: %v", arg, err)
			return
		}
		if name != "app" || abs != app {
			t.Errorf("resolveWorktree(%q) = (%q,%q), want (app,%q)", arg, name, abs, app)
		}
	}
	ok("app")    // bare name → child of root
	ok("../app") // relative to cwd
	ok(app)      // absolute

	bad := func(arg string) {
		if _, _, err := resolveWorktree(arg, root, cwd); err == nil {
			t.Errorf("resolveWorktree(%q) should have errored", arg)
		}
	}
	bad("ghost")                            // missing
	bad("afile")                            // not a directory
	bad(filepath.Join(root, "sub", "deep")) // not a direct child of root
}

func TestWorkspaceDirs(t *testing.T) {
	repo := setupRepo(t)
	root := filepath.Dir(repo)
	// git worktrees + a plain dir; mark the gitops repo itself as a git dir too.
	for _, d := range []string{"app1", "app2"} {
		if err := os.MkdirAll(filepath.Join(root, d, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirs, err := WorkspaceDirs(repo)
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, d := range dirs {
		set[d] = true
	}
	if !set["app1"] || !set["app2"] {
		t.Errorf("expected app1+app2 in %v", dirs)
	}
	if set["plain"] {
		t.Errorf("plain (no .git) should be excluded: %v", dirs)
	}
	if set["acme"] {
		t.Errorf("the gitops repo itself should be excluded: %v", dirs)
	}
}

// TestAddApp_DefaultNamespace keeps the pre-flag behaviour byte-for-byte: the
// workload targets namespaces.apps and NO apps/base/namespaces.yaml is created
// (the default namespace is owned cluster-side).
func TestAddApp_DefaultNamespace(t *testing.T) {
	repo := setupRepo(t)
	mkWorktree(t, repo, "hello", nil)
	res, err := Run(Options{RepoDir: repo, Worktree: "hello", Stdout: io.Discard})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	dep, err := os.ReadFile(filepath.Join(res.AppsDir, "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dep), "namespace: apps") {
		t.Errorf("deployment.yaml should target the default namespace:\n%s", dep)
	}
	if _, err := os.Stat(filepath.Join(repo, "apps", "base", "namespaces.yaml")); !os.IsNotExist(err) {
		t.Errorf("apps/base/namespaces.yaml should NOT exist for the default namespace (err=%v)", err)
	}
	kust, _ := os.ReadFile(filepath.Join(repo, "apps", "base", "kustomization.yaml"))
	if strings.Contains(string(kust), "namespaces.yaml") {
		t.Errorf("kustomization should not reference namespaces.yaml for the default namespace:\n%s", kust)
	}
}

// TestAddApp_CustomNamespace: --namespace stamps the workload AND creates the
// managed Namespace object + registers it in the kustomization.
func TestAddApp_CustomNamespace(t *testing.T) {
	repo := setupRepo(t)
	mkWorktree(t, repo, "hello", nil)
	res, err := Run(Options{RepoDir: repo, Worktree: "hello", Namespace: "team-x", Stdout: io.Discard})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	dep, _ := os.ReadFile(filepath.Join(res.AppsDir, "deployment.yaml"))
	if !strings.Contains(string(dep), "namespace: team-x") {
		t.Errorf("deployment.yaml should target team-x:\n%s", dep)
	}
	ns, err := os.ReadFile(filepath.Join(repo, "apps", "base", "namespaces.yaml"))
	if err != nil {
		t.Fatalf("apps/base/namespaces.yaml should exist: %v", err)
	}
	for _, want := range []string{"kind: Namespace", "name: team-x"} {
		if !strings.Contains(string(ns), want) {
			t.Errorf("namespaces.yaml missing %q:\n%s", want, ns)
		}
	}
	kust, _ := os.ReadFile(filepath.Join(repo, "apps", "base", "kustomization.yaml"))
	if !strings.Contains(string(kust), "- ./namespaces.yaml") {
		t.Errorf("kustomization should reference ./namespaces.yaml:\n%s", kust)
	}
}

// TestAddApp_SharedNamespaceIdempotent: two apps in the same non-default
// namespace yield exactly ONE Namespace doc and ONE kustomization entry.
func TestAddApp_SharedNamespaceIdempotent(t *testing.T) {
	repo := setupRepo(t)
	for _, name := range []string{"app-a", "app-b"} {
		mkWorktree(t, repo, name, nil)
		if _, err := Run(Options{RepoDir: repo, Worktree: name, Namespace: "shared", Stdout: io.Discard}); err != nil {
			t.Fatalf("Run %s: %v", name, err)
		}
	}
	ns, _ := os.ReadFile(filepath.Join(repo, "apps", "base", "namespaces.yaml"))
	if got := strings.Count(string(ns), "kind: Namespace"); got != 1 {
		t.Errorf("expected exactly 1 Namespace doc, got %d:\n%s", got, ns)
	}
	kust, _ := os.ReadFile(filepath.Join(repo, "apps", "base", "kustomization.yaml"))
	if got := strings.Count(string(kust), "- ./namespaces.yaml"); got != 1 {
		t.Errorf("expected exactly 1 namespaces.yaml entry, got %d:\n%s", got, kust)
	}
}

func TestAddApp_RejectsInvalidNamespace(t *testing.T) {
	repo := setupRepo(t)
	mkWorktree(t, repo, "hello", nil)
	for _, ns := range []string{"Team_X", "UPPER", "bad ns", "-leading", "x.y"} {
		if _, err := Run(Options{RepoDir: repo, Worktree: "hello", Namespace: ns, Stdout: io.Discard}); err == nil {
			t.Errorf("namespace %q should be rejected", ns)
		}
	}
}
