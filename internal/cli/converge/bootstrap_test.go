package converge

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	flywheelSchema "github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/naming"
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
	cfg.Namespaces.Apps = "apps" // loader-defaulted in production; set explicitly here

	refs := map[string]string{
		"git-server":               "flywheel-dev/git-server:dogfood",
		"git-auto-sync":            "flywheel-dev/git-auto-sync:dogfood",
		"image-builder-controller": "flywheel-dev/image-builder-controller:dogfood",
		"git-deploy-controller":    "flywheel-dev/git-deploy-controller:dogfood",
	}

	dir, err := RenderBootstrap(cfg, refs, "abc123def456abc123def456abc123def456abcd", "acme-gitops", "")
	if err != nil {
		t.Fatalf("renderBootstrap: %v", err)
	}
	defer os.RemoveAll(dir)

	// builders-kustomization.yaml: the ghcr.io refs that have a Deployment
	// under the dev-loop overlay (git-server, image-builder-controller,
	// git-deploy-controller, git-auto-sync) rewrite to the resolved
	// name+tag, no k3d-registry form anywhere.
	bk := mustRead(t, filepath.Join(dir, "builders-kustomization.yaml"))
	for _, want := range []string{
		"newName: flywheel-dev/git-server",
		"newTag: dogfood",
		"newName: flywheel-dev/image-builder-controller",
		// Must be rewritten on the Flux path too, or its pod ErrImagePulls the
		// base ghcr ref while up's dev-loop step applies the local one (two-apply-paths).
		"newName: flywheel-dev/git-deploy-controller",
		// git-auto-sync joined this bucket in the Go-port design (its Deployment
		// moved from the per-app builders tree into dev-loop/base — see
		// docs/designs/2026-07-17-per-app-sync-controller-design.md); a missing
		// rewrite here would leave the placeholder tag on the cluster.
		"newName: flywheel-dev/git-auto-sync",
	} {
		if !strings.Contains(bk, want) {
			t.Errorf("builders-kustomization.yaml missing %q:\n%s", want, bk)
		}
	}
	// client-builders-kustomization.yaml's images bucket is presently empty
	// (no schema.ImageNames image is per-app-only anymore); assert it stays
	// that way rather than silently reacquiring a stale rewrite.
	cbk := mustRead(t, filepath.Join(dir, "client-builders-kustomization.yaml"))
	if strings.Contains(cbk, "ghcr.io/cobr-io/") {
		t.Errorf("client-builders-kustomization.yaml should render no image rewrites, got:\n%s", cbk)
	}
	// With the bucket empty the `images:` KEY must be omitted entirely, not
	// rendered bare: a bare `images:` parses as YAML null, the Flux CRD rejects
	// an explicit null for the array ("spec.images in body must be of type
	// array"), and the bootstrap apply then silently drops the whole
	// client-builders Kustomization — no per-app builders ever reconcile
	// (caught live by PR #115's per-PR e2e).
	if regexp.MustCompile(`(?m)^\s*images:`).MatchString(cbk) {
		t.Errorf("client-builders-kustomization.yaml renders a bare images: key (YAML null; Flux CRD rejects it):\n%s", cbk)
	}
	// The flywheel-dev-loop Kustomization patches git-server's memory limit so
	// Flux's reconcile agrees with the step-11a direct apply. cfg leaves
	// git_server.memory_limit unset here, so it must render the default.
	for _, want := range []string{"patches:", "name: git-server", "memory: 128Mi"} {
		if !strings.Contains(bk, want) {
			t.Errorf("builders-kustomization.yaml missing git-server memory patch %q:\n%s", want, bk)
		}
	}
	if strings.Contains(bk, "k3d-acme-local-registry") {
		t.Errorf("builders-kustomization.yaml leaked legacy local-registry form:\n%s", bk)
	}

	// The self sync moved to the git-deploy-controller in manifests/dev-loop/base,
	// so the bootstrap render no longer emits a self-git-auto-sync Deployment.
	if _, err := os.Stat(filepath.Join(dir, "self-git-auto-sync.yaml")); err == nil {
		t.Error("bootstrap render still emits self-git-auto-sync.yaml; it moved to dev-loop/base")
	}

	// self-source.yaml: Flux tracks the constant DEPLOY branch
	// (flywheel/local-deploy), not the worktree's branch — git-deploy-controller
	// keeps that branch = AUTHORED + image bumps.
	selfSrc := mustRead(t, filepath.Join(dir, "self-source.yaml"))
	if !strings.Contains(selfSrc, "branch: "+naming.DeployBranch) {
		t.Errorf("self-source.yaml should track %s, got:\n%s", naming.DeployBranch, selfSrc)
	}

	// flywheel-config.yaml: the keys git-deploy-controller reads.
	cm := mustRead(t, filepath.Join(dir, "flywheel-config.yaml"))
	for _, want := range []string{`repo.base_name: "acme-gitops"`, `git.integration_branch: "main"`} {
		if !strings.Contains(cm, want) {
			t.Errorf("flywheel-config.yaml missing %q:\n%s", want, cm)
		}
	}

	// flywheel-source.yaml: spec.ref.commit = the supplied SHA.
	src := mustRead(t, filepath.Join(dir, "flywheel-source.yaml"))
	if !strings.Contains(src, "commit: abc123def456abc123def456abc123def456abcd") {
		t.Errorf("flywheel-source.yaml missing commit SHA:\n%s", src)
	}
}

// TestRenderBootstrap_NoExplicitNulls is the shift-left guard for issue #117
// Tier 3a: walk every document the bootstrap tree renders and fail on any
// explicit YAML null. This is the general form of the regex assertion above
// (the specific `images:` case #115 hit) — it would have caught that
// regression, and any future one shaped the same way (an empty
// `{{ range }}` leaving a bare `key:` behind), directly in a unit test
// instead of a live API server 400 buried in a 20-minute e2e timeout.
// bootstrapImageOwners currently routes every image to imgOwnerDevLoop, so
// this cfg — deliberately identical to TestRenderBootstrap_ResolvesImageRefs
// — already exercises the empty-ClientBuilderImages path that regressed.
func TestRenderBootstrap_NoExplicitNulls(t *testing.T) {
	cfg := &flywheelSchema.File{}
	cfg.Client.Name = "acme"
	cfg.Cluster.Name = "acme-local"
	cfg.Cluster.Registry = "acme-local-registry"
	cfg.Cluster.RegistryPort = 50001
	cfg.Flux.IntervalLocal = "10s"
	cfg.Local.Domain = "localdev.me"
	cfg.Namespaces.Apps = "apps"

	refs := map[string]string{
		"git-server":               "flywheel-dev/git-server:dogfood",
		"git-auto-sync":            "flywheel-dev/git-auto-sync:dogfood",
		"image-builder-controller": "flywheel-dev/image-builder-controller:dogfood",
		"git-deploy-controller":    "flywheel-dev/git-deploy-controller:dogfood",
	}

	dir, err := RenderBootstrap(cfg, refs, "abc123def456abc123def456abc123def456abcd", "acme-gitops", "")
	if err != nil {
		t.Fatalf("renderBootstrap: %v", err)
	}
	defer os.RemoveAll(dir)

	lintNoExplicitNulls(t, dir)
}

// A configured git_server.memory_limit flows into the flywheel-dev-loop
// Kustomization's patch, so Flux reconciles the cluster to the raised limit.
func TestRenderBootstrap_GitServerMemoryLimit(t *testing.T) {
	cfg := &flywheelSchema.File{}
	cfg.Client.Name = "acme"
	cfg.Cluster.Name = "acme-local"
	cfg.Cluster.Registry = "acme-local-registry"
	cfg.Cluster.RegistryPort = 50001
	cfg.Flux.IntervalLocal = "10s"
	cfg.Local.Domain = "localdev.me"
	cfg.Namespaces.Apps = "apps" // loader-defaulted in production; set explicitly here
	cfg.GitServer.MemoryLimit = "512Mi"

	refs := map[string]string{
		"git-server":               "ghcr.io/cobr-io/git-server:v0.1.0",
		"git-auto-sync":            "ghcr.io/cobr-io/git-auto-sync:v0.1.0",
		"image-builder-controller": "ghcr.io/cobr-io/image-builder-controller:v0.1.0",
		"git-deploy-controller":    "ghcr.io/cobr-io/git-deploy-controller:v0.1.0",
	}
	dir, err := RenderBootstrap(cfg, refs, "abc", "acme-gitops", "")
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}
	defer os.RemoveAll(dir)

	bk := mustRead(t, filepath.Join(dir, "builders-kustomization.yaml"))
	if !strings.Contains(bk, "memory: 512Mi") {
		t.Errorf("configured memory_limit not rendered into the dev-loop patch:\n%s", bk)
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
	cfg.Namespaces.Apps = "apps" // loader-defaulted in production; set explicitly here

	refs := map[string]string{
		"git-server":               "flywheel-dev/git-server", // no tag!
		"git-auto-sync":            "flywheel-dev/git-auto-sync:dogfood",
		"image-builder-controller": "flywheel-dev/image-builder-controller:dogfood",
		"git-deploy-controller":    "flywheel-dev/git-deploy-controller:dogfood",
	}

	_, err := RenderBootstrap(cfg, refs, "abc", "acme", "")
	if err == nil {
		t.Fatal("expected renderBootstrap to reject untagged override")
	}
	if !strings.Contains(err.Error(), "git-server") {
		t.Errorf("error should name the offending image, got: %v", err)
	}
}

// The bootstrap tree is applied by `up`'s apply-flux-system step, so every object it emits
// must carry app.kubernetes.io/managed-by=flywheel (directive: label
// everything `up` creates, issue #27). The label lives in the root
// kustomization's `labels:` block, which only materialises when kustomize
// builds the tree — so this builds it (the same krusty engine the applier
// uses) and asserts the marker lands on EVERY resource. A build failure here
// would also catch the labels block breaking the kustomization.
func TestRenderBootstrap_EveryResourceLabeledManagedBy(t *testing.T) {
	cfg := &flywheelSchema.File{}
	cfg.Client.Name = "acme"
	cfg.Cluster.Name = "acme-local"
	cfg.Cluster.Registry = "acme-local-registry"
	cfg.Cluster.RegistryPort = 50001
	cfg.Flux.IntervalLocal = "10s"
	cfg.Local.Domain = "localdev.me"
	cfg.Namespaces.Apps = "apps" // loader-defaulted in production; set explicitly here

	refs := map[string]string{
		"git-server":               "ghcr.io/cobr-io/git-server:v0.1.0",
		"git-auto-sync":            "ghcr.io/cobr-io/git-auto-sync:v0.1.0",
		"image-builder-controller": "ghcr.io/cobr-io/image-builder-controller:v0.1.0",
		"git-deploy-controller":    "ghcr.io/cobr-io/git-deploy-controller:v0.1.0",
	}
	dir, err := RenderBootstrap(cfg, refs, "abc", "acme-gitops", "")
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}
	defer os.RemoveAll(dir)

	built := buildKustomizeForTest(t, dir)

	// Walk each emitted document; every one must carry the marker label. We
	// also assert the kinds we specifically widened the label onto (the Flux
	// objects + namespaces, which weren't labeled before) actually appear, so
	// the test can't pass vacuously on an empty build.
	docs := strings.Split(built, "\n---\n")
	kinds := map[string]bool{}
	resources := 0
	for _, doc := range docs {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		resources++
		if !strings.Contains(doc, naming.ManagedByLabelKey+": "+naming.ManagedByLabelValue) {
			t.Errorf("a bootstrap resource is missing the managed-by label:\n%s", doc)
		}
		for _, line := range strings.Split(doc, "\n") {
			if strings.HasPrefix(line, "kind: ") {
				kinds[strings.TrimSpace(strings.TrimPrefix(line, "kind:"))] = true
			}
		}
	}
	if resources == 0 {
		t.Fatal("bootstrap build produced no resources")
	}
	for _, want := range []string{"Namespace", "GitRepository", "Kustomization", "ConfigMap"} {
		if !kinds[want] {
			t.Errorf("expected the bootstrap build to include a %s (got kinds %v)", want, kinds)
		}
	}
}

// TestRenderBootstrap_CreatesConfiguredAppsNamespace proves the global default
// apps namespace is a real knob (task T15): a non-"apps" namespaces.apps in cfg
// must make the bootstrap CREATE that namespace (namespaces.yaml.tmpl) — not the
// literal "apps" — so workloads `add app` scaffolds into cfg.Namespaces.Apps
// land in a namespace something actually creates. It also confirms the same
// value reaches the flywheel-config ConfigMap in one render (the two templated
// apps-namespace sites agree).
func TestRenderBootstrap_CreatesConfiguredAppsNamespace(t *testing.T) {
	cfg := &flywheelSchema.File{}
	cfg.Client.Name = "acme"
	cfg.Cluster.Name = "acme-local"
	cfg.Cluster.Registry = "acme-local-registry"
	cfg.Cluster.RegistryPort = 50001
	cfg.Flux.IntervalLocal = "10s"
	cfg.Local.Domain = "localdev.me"
	cfg.Namespaces.Apps = "myapps"

	refs := map[string]string{
		"git-server":               "ghcr.io/cobr-io/git-server:v0.1.0",
		"git-auto-sync":            "ghcr.io/cobr-io/git-auto-sync:v0.1.0",
		"image-builder-controller": "ghcr.io/cobr-io/image-builder-controller:v0.1.0",
		"git-deploy-controller":    "ghcr.io/cobr-io/git-deploy-controller:v0.1.0",
	}
	dir, err := RenderBootstrap(cfg, refs, "abc", "acme-gitops", "")
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}
	defer os.RemoveAll(dir)

	// (a) the Namespace object the bootstrap creates is the configured one.
	ns := mustRead(t, filepath.Join(dir, "namespaces.yaml"))
	for _, want := range []string{"name: myapps", "kubernetes.io/metadata.name: myapps"} {
		if !strings.Contains(ns, want) {
			t.Errorf("namespaces.yaml missing %q — bootstrap didn't create the configured apps namespace:\n%s", want, ns)
		}
	}
	// The old hardcoded "apps" namespace must be gone. "name: myapps" does not
	// contain the substring "name: apps", so this catches a stale literal.
	if strings.Contains(ns, "name: apps") {
		t.Errorf("namespaces.yaml still creates the hardcoded \"apps\" namespace:\n%s", ns)
	}

	// (b) the same value reaches the flywheel-config ConfigMap (the other
	// templated apps-namespace site) in this render.
	cm := renderedFlywheelConfigData(t, dir)
	if cm["namespaces.apps"] != "myapps" {
		t.Errorf("flywheel-config namespaces.apps = %q, want \"myapps\"", cm["namespaces.apps"])
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
