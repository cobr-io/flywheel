package converge

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/api/krusty"
	ktypes "sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	flywheel "github.com/cobr-io/flywheel"
)

// baseGitServerManifest is a minimal stand-in for manifests/dev-loop/base:
// just the git-server Deployment with the baked-in 128Mi limit, enough to
// prove the overlay's patch overrides it.
const baseGitServerManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: git-server
  namespace: flywheel-system
spec:
  template:
    spec:
      containers:
        - name: git-server
          image: ghcr.io/cobr-io/git-server:v0.1.0
          resources:
            requests:
              memory: 32Mi
            limits:
              memory: 128Mi
`

// buildKustomize mirrors applier.buildKustomize: a krusty build with the
// load-restriction relaxed so the transient kustomization's
// `../overlays/local` (and, transitively, its `../../base`) references
// resolve.
func buildKustomizeForTest(t *testing.T, dir string) string {
	t.Helper()
	opts := krusty.MakeDefaultOptions()
	opts.LoadRestrictions = ktypes.LoadRestrictionsNone
	rm, err := krusty.MakeKustomizer(opts).Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		t.Fatalf("kustomize build: %v", err)
	}
	b, err := rm.AsYaml()
	if err != nil {
		t.Fatalf("AsYaml: %v", err)
	}
	return string(b)
}

// devLoopFixture builds a fixture tree mirroring the real
// manifests/dev-loop/ layout — base/ (populated from baseFiles) and
// overlays/local/ (populated from overlayFiles, resourced alongside the
// mandatory `../../base`, same as manifests/dev-loop/overlays/local/
// kustomization.yaml) — plus a `transient` dir that is a sibling of both,
// standing in for the dir ApplyDevLoop creates via
// os.MkdirTemp(devLoopRoot, ...). Returns the transient dir, ready to
// receive a rendered kustomization.yaml. overlayFiles may be nil/empty,
// which reproduces the real overlay's current pure-passthrough shape.
func devLoopFixture(t *testing.T, baseFiles, overlayFiles map[string]string) string {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(root, "base")
	overlayLocal := filepath.Join(root, "overlays", "local")
	transient := filepath.Join(root, "transient")
	for _, d := range []string{base, overlayLocal, transient} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	writeResources := func(dir string, files map[string]string, extraResourceLines string) {
		kustomization := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n" + extraResourceLines
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			kustomization += "  - " + name + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(kustomization), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeResources(base, baseFiles, "")
	writeResources(overlayLocal, overlayFiles, "  - ../../base\n")

	return transient
}

// renderDevLoopKustomization's output must be valid kustomize that both
// rewrites the git-server image AND raises its memory limit — exercised
// through a real krusty build, the same engine the applier uses, and
// through the overlay indirection (transient -> ../overlays/local ->
// ../../base) that ApplyDevLoop now goes through.
func TestRenderDevLoopKustomization_PatchesGitServerMemory(t *testing.T) {
	transient := devLoopFixture(t, map[string]string{"git-server.yaml": baseGitServerManifest}, nil)

	refs := map[string]string{
		"git-server":               "k3d-reg:5000/git-server:dogfood-abc",
		"git-auto-sync":            "k3d-reg:5000/git-auto-sync:dogfood-abc",
		"image-builder-controller": "k3d-reg:5000/image-builder-controller:dogfood-abc",
	}
	k := renderDevLoopKustomization(refs, "512Mi")
	if err := os.WriteFile(filepath.Join(transient, "kustomization.yaml"), []byte(k), 0o644); err != nil {
		t.Fatal(err)
	}

	out := buildKustomizeForTest(t, transient)
	if !strings.Contains(out, "memory: 512Mi") {
		t.Errorf("patched git-server limit (512Mi) not in build output:\n%s", out)
	}
	if strings.Contains(out, "memory: 128Mi") {
		t.Errorf("base 128Mi limit should have been overridden:\n%s", out)
	}
	// The image rewrite still composes alongside the patch.
	if !strings.Contains(out, "k3d-reg:5000/git-server:dogfood-abc") {
		t.Errorf("git-server image rewrite missing:\n%s", out)
	}
	// The request floor is untouched (we only patch the limit).
	if !strings.Contains(out, "memory: 32Mi") {
		t.Errorf("request floor should be preserved:\n%s", out)
	}
}

// The default limit flows through unchanged (no override set).
func TestRenderDevLoopKustomization_DefaultLimit(t *testing.T) {
	refs := map[string]string{
		"git-server":               "ghcr.io/cobr-io/git-server:v0.1.0",
		"git-auto-sync":            "ghcr.io/cobr-io/git-auto-sync:v0.1.0",
		"image-builder-controller": "ghcr.io/cobr-io/image-builder-controller:v0.1.0",
	}
	k := renderDevLoopKustomization(refs, "128Mi")
	if !strings.Contains(k, "memory: 128Mi") {
		t.Errorf("expected the default 128Mi in the patch:\n%s", k)
	}
	if !strings.Contains(k, "name: git-server") {
		t.Errorf("patch should target git-server:\n%s", k)
	}
}

// T16: the transient kustomization must reference the overlay
// (`../overlays/local`), not `../base` directly, so content the overlay
// adds on top of base isn't silently dropped on the direct-apply (SSA)
// path. This fixture makes overlays/local add a *second* marker resource
// (not a pure `../../base` passthrough like the real overlay is today) —
// proving overlay-only content flows through, which `../base` would miss.
func TestApplyDevLoop_OverlayResourcesIncluded(t *testing.T) {
	const baseMarker = `apiVersion: v1
kind: ConfigMap
metadata:
  name: marker-base
data:
  from: base
`
	const overlayMarker = `apiVersion: v1
kind: ConfigMap
metadata:
  name: marker-overlay
data:
  from: overlay
`
	// The fixture's overlays/local is NOT a pure passthrough here — it adds
	// marker.yaml on top of ../../base, so a build that silently dropped the
	// overlay (i.e. resourced ../base directly, the pre-T16 behavior) would
	// be missing marker-overlay.
	transient := devLoopFixture(t, map[string]string{
		"marker.yaml":     baseMarker,
		"git-server.yaml": baseGitServerManifest, // target for the memory-limit patch below
	}, map[string]string{
		"marker.yaml": overlayMarker,
	})

	refs := map[string]string{
		"git-server":               "ghcr.io/cobr-io/git-server:v0.1.0",
		"git-auto-sync":            "ghcr.io/cobr-io/git-auto-sync:v0.1.0",
		"image-builder-controller": "ghcr.io/cobr-io/image-builder-controller:v0.1.0",
	}
	k := renderDevLoopKustomization(refs, "128Mi")
	if !strings.Contains(k, "- ../overlays/local\n") {
		t.Fatalf("transient kustomization must reference the overlay (../overlays/local), not base directly:\n%s", k)
	}
	if strings.Contains(k, "- ../base\n") {
		t.Errorf("transient kustomization should no longer reference ../base directly:\n%s", k)
	}
	if err := os.WriteFile(filepath.Join(transient, "kustomization.yaml"), []byte(k), 0o644); err != nil {
		t.Fatal(err)
	}

	out := buildKustomizeForTest(t, transient)
	if !strings.Contains(out, "marker-base") {
		t.Errorf("base resource missing from render:\n%s", out)
	}
	if !strings.Contains(out, "marker-overlay") {
		t.Errorf("overlay-only resource missing from render — the overlay is being ignored:\n%s", out)
	}
}

// TestApplyDevLoop_RealManifests_RewriteByName confirms the two-apply-paths
// invariant (docs/plans/2026-07-17-per-app-sync-controller-plan.md Phase 4
// checkbox 4) against the REAL manifests/dev-loop tree, not a fixture
// stand-in: renderDevLoopKustomization's `images:` block matches every
// schema.ImageNames entry by image NAME (`ghcr.io/cobr-io/<name>`), so
// git-auto-sync.yaml's Deployment — newly added in dev-loop/base alongside
// git-server, image-builder-controller and git-deploy-controller — gets its
// placeholder tag rewritten exactly like the others, with no per-image
// special-casing needed on the SSA direct-apply path (up step 11a). The Flux
// flywheel-dev-loop Kustomization's spec.images rewrite (the other apply
// path) is proven equivalent by TestBootstrapImages_TemplateUnionMatchesSchema
// + TestRenderBootstrap_ResolvesImageRefs: both derive their image list from
// the same schema.ImageNames / bootstrapImageOwners source, just rendered
// into a Kustomization's spec.images instead of a kustomize transformer.
func TestApplyDevLoop_RealManifests_RewriteByName(t *testing.T) {
	root := t.TempDir()
	sub, err := fs.Sub(flywheel.Assets, "manifests/dev-loop")
	if err != nil {
		t.Fatalf("embed manifests/dev-loop missing: %v", err)
	}
	if err := os.CopyFS(root, sub); err != nil {
		t.Fatalf("copy embedded dev-loop tree: %v", err)
	}

	// Sibling of base/ and overlays/, exactly like ApplyDevLoop's
	// os.MkdirTemp(devLoopRoot, ".flywheel-tmp-overlay-").
	transient := filepath.Join(root, "transient")
	if err := os.Mkdir(transient, 0o755); err != nil {
		t.Fatal(err)
	}

	refs := map[string]string{
		"git-server":               "flywheel-dev/git-server:dogfood",
		"git-auto-sync":            "flywheel-dev/git-auto-sync:dogfood",
		"image-builder-controller": "flywheel-dev/image-builder-controller:dogfood",
		"git-deploy-controller":    "flywheel-dev/git-deploy-controller:dogfood",
	}
	k := renderDevLoopKustomization(refs, "128Mi")
	if err := os.WriteFile(filepath.Join(transient, "kustomization.yaml"), []byte(k), 0o644); err != nil {
		t.Fatal(err)
	}

	out := buildKustomizeForTest(t, transient)
	if !strings.Contains(out, "image: flywheel-dev/git-auto-sync:dogfood") {
		t.Errorf("git-auto-sync.yaml's placeholder image was not rewritten by name on the real dev-loop tree:\n%s", out)
	}
	if strings.Contains(out, "ghcr.io/cobr-io/git-auto-sync:rewritten-by-flywheel-up") {
		t.Errorf("git-auto-sync.yaml still carries the unrewritten fail-loud placeholder:\n%s", out)
	}
}

func TestDeploymentDetail_ProgressingFalseReasonWins(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"replicas": int64(1)},
		"status": map[string]any{
			"availableReplicas": int64(0),
			"conditions": []any{
				map[string]any{
					"type":    "Progressing",
					"status":  "False",
					"reason":  "ImagePullBackOff",
					"message": "Back-off pulling image",
				},
			},
		},
	}}
	if got := deploymentDetail(u); got != "ImagePullBackOff" {
		t.Errorf("deploymentDetail = %q, want ImagePullBackOff", got)
	}
}

func TestDeploymentDetail_AvailableRatioFallback(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"replicas": int64(3)},
		"status": map[string]any{
			"availableReplicas": int64(1),
		},
	}}
	if got := deploymentDetail(u); got != "1/3 available" {
		t.Errorf("deploymentDetail = %q, want '1/3 available'", got)
	}
}
