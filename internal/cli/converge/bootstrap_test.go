package converge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	flywheelSchema "github.com/cobr-io/flywheel/internal/cli/schema"
)

// TestRenderBootstrap_ResolvesImageRefs renders the bootstrap tree
// into a tmpdir and asserts the key invariants for client-builders +
// self-git-auto-sync: image refs come from the resolved (override-
// aware) map, and the embedded SHA + repo basename land in the
// expected files. The full templates are exercised indirectly — this
// is a contract test, not a snapshot.
func TestRenderBootstrap_ResolvesImageRefs(t *testing.T) {
	cfg := &flywheelSchema.File{}
	cfg.Client.Name = "acme"
	cfg.Cluster.Name = "acme-local"
	cfg.Cluster.Registry = "acme-local-registry"
	cfg.Cluster.RegistryPort = 50001
	cfg.Flux.IntervalLocal = "10s"
	cfg.Local.Domain = "localdev.me"

	refs := map[string]string{
		"git-server":               "flywheel-dev/git-server:dogfood",
		"git-auto-sync":            "flywheel-dev/git-auto-sync:dogfood",
		"image-builder-controller": "flywheel-dev/image-builder-controller:dogfood",
	}

	dir, err := RenderBootstrap(cfg, refs, "abc123def456abc123def456abc123def456abcd", "acme-gitops", "feat/x")
	if err != nil {
		t.Fatalf("renderBootstrap: %v", err)
	}
	defer os.RemoveAll(dir)

	// builders-kustomization.yaml: the ghcr.io refs that have a Deployment
	// under the dev-loop overlay (git-server, image-builder-controller)
	// rewrite to the resolved name+tag, no k3d-registry form anywhere.
	bk := mustRead(t, filepath.Join(dir, "builders-kustomization.yaml"))
	for _, want := range []string{
		"newName: flywheel-dev/git-server",
		"newTag: dogfood",
		"newName: flywheel-dev/image-builder-controller",
	} {
		if !strings.Contains(bk, want) {
			t.Errorf("builders-kustomization.yaml missing %q:\n%s", want, bk)
		}
	}
	// git-auto-sync has no Deployment under this overlay, so it must NOT be
	// rewritten here — its override flows through the per-app builders and the
	// self sync instead (asserted below). A rewrite entry here would be a
	// silent no-op.
	if strings.Contains(bk, "ghcr.io/cobr-io/git-auto-sync") {
		t.Errorf("builders-kustomization.yaml has a dead git-auto-sync image rewrite:\n%s", bk)
	}
	if strings.Contains(bk, "k3d-acme-local-registry") {
		t.Errorf("builders-kustomization.yaml leaked legacy local-registry form:\n%s", bk)
	}

	// self-git-auto-sync.yaml: image ref pinned + worktree/URL use the
	// repo basename, not the client name.
	sgas := mustRead(t, filepath.Join(dir, "self-git-auto-sync.yaml"))
	for _, want := range []string{
		"image: flywheel-dev/git-auto-sync:dogfood",
		"/workspaces/acme-gitops",
		"/acme-gitops.git",
	} {
		if !strings.Contains(sgas, want) {
			t.Errorf("self-git-auto-sync.yaml missing %q:\n%s", want, sgas)
		}
	}

	// self-source.yaml: spec.ref.branch tracks the worktree's current branch,
	// not a hardcoded `main` — so `flywheel up` mid-feature doesn't clobber the
	// branch git-auto-sync-self set (issue #6).
	selfSrc := mustRead(t, filepath.Join(dir, "self-source.yaml"))
	if !strings.Contains(selfSrc, "branch: feat/x") {
		t.Errorf("self-source.yaml should track the current branch, got:\n%s", selfSrc)
	}

	// flywheel-source.yaml: spec.ref.commit = the supplied SHA.
	src := mustRead(t, filepath.Join(dir, "flywheel-source.yaml"))
	if !strings.Contains(src, "commit: abc123def456abc123def456abc123def456abcd") {
		t.Errorf("flywheel-source.yaml missing commit SHA:\n%s", src)
	}
}

// TestRenderBootstrap_RejectsUntaggedOverride asserts that overrides
// without an explicit `:tag` are rejected at render time rather than
// producing a kustomize-build failure later.
func TestRenderBootstrap_RejectsUntaggedOverride(t *testing.T) {
	cfg := &flywheelSchema.File{}
	cfg.Client.Name = "acme"
	cfg.Cluster.Name = "acme-local"
	cfg.Cluster.Registry = "acme-local-registry"
	cfg.Cluster.RegistryPort = 50001
	cfg.Flux.IntervalLocal = "10s"
	cfg.Local.Domain = "localdev.me"

	refs := map[string]string{
		"git-server":               "flywheel-dev/git-server", // no tag!
		"git-auto-sync":            "flywheel-dev/git-auto-sync:dogfood",
		"image-builder-controller": "flywheel-dev/image-builder-controller:dogfood",
	}

	_, err := RenderBootstrap(cfg, refs, "abc", "acme", "main")
	if err == nil {
		t.Fatal("expected renderBootstrap to reject untagged override")
	}
	if !strings.Contains(err.Error(), "git-server") {
		t.Errorf("error should name the offending image, got: %v", err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
