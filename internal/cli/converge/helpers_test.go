package converge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/api/krusty"
	ktypes "sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
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
// load-restriction relaxed so the overlay's `../base` reference resolves.
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

// renderDevLoopKustomization's output must be valid kustomize that both
// rewrites the git-server image AND raises its memory limit — exercised
// through a real krusty build, the same engine the applier uses.
func TestRenderDevLoopKustomization_PatchesGitServerMemory(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "base")
	overlay := filepath.Join(root, "overlay")
	for _, d := range []string{base, overlay} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "git-server.yaml"), []byte(baseGitServerManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "kustomization.yaml"),
		[]byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - git-server.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	refs := map[string]string{
		"git-server":               "k3d-reg:5000/git-server:dogfood-abc",
		"git-auto-sync":            "k3d-reg:5000/git-auto-sync:dogfood-abc",
		"image-builder-controller": "k3d-reg:5000/image-builder-controller:dogfood-abc",
	}
	k := renderDevLoopKustomization(refs, "512Mi")
	if err := os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte(k), 0o644); err != nil {
		t.Fatal(err)
	}

	out := buildKustomizeForTest(t, overlay)
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
