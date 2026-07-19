package converge

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/yannh/kubeconform/pkg/validator"

	flywheelSchema "github.com/cobr-io/flywheel/internal/cli/schema"
)

// kubeconformSchemaLocation points at CRD JSON schemas vendored under
// testdata/crd-schemas/ (issue #117's deferred kubeconform option), in the
// same {Group}/{ResourceKind}_{ResourceAPIVersion}.json layout as the
// datreeio community catalog the client-skeleton CI's kubeconform job
// already trusts (templates/client-skeleton/.github/workflows/ci.yaml) —
// vendored here so this unit test validates offline and deterministically,
// rather than depending on a live fetch mid `go test`. Schema-typed
// validation catches what neither a generic-tree null walk (Tier 3a,
// nulllint_test.go) nor a typed Go decode can: a field a CRD types as an
// array/enum/etc. that got the wrong shape.
const kubeconformSchemaLocation = "testdata/crd-schemas/{{ .Group }}/{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json"

// lintKubeconformSchema validates every document in rawYAML against the
// vendored CRD schemas, failing on any schema violation. A kind with no
// vendored schema — every core Kubernetes kind flywheel renders (ConfigMap,
// Namespace, ...) — is skipped, not treated as an error: those are already
// covered by the render/apply tests, and vendoring the full
// kubernetes-json-schema catalog just to cover this local dev tool's
// handful of core-kind manifests would cost far more than it returns.
func lintKubeconformSchema(t *testing.T, rawYAML string) {
	t.Helper()
	v, err := validator.New([]string{kubeconformSchemaLocation}, validator.Opts{
		Strict:               true,
		IgnoreMissingSchemas: true,
	})
	if err != nil {
		t.Fatalf("kubeconform validator.New: %v", err)
	}
	for _, res := range v.Validate("rendered", io.NopCloser(strings.NewReader(rawYAML))) {
		if res.Status != validator.Invalid && res.Status != validator.Error {
			continue
		}
		t.Errorf("kubeconform: %v", res.Err)
		for _, ve := range res.ValidationErrors {
			t.Errorf("  %s: %s", ve.Path, ve.Msg)
		}
	}
}

// TestRenderBootstrap_KubeconformSchema is issue #117's deferred kubeconform
// option: build the rendered bootstrap tree the same way apply-flux-system
// does (a real krusty build, buildKustomizeForTest) and schema-validate the
// result against the vendored Flux CRD schemas. This is a stricter check
// than Tier 3a's null lint — it would have caught #115's regression too
// (spec.images: null fails the array-typed schema) — plus any other
// CRD-schema violation a generic null walk can't see.
func TestRenderBootstrap_KubeconformSchema(t *testing.T) {
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

	lintKubeconformSchema(t, buildKustomizeForTest(t, dir))
}

// TestKubeconformSchema_CatchesNullImages is a focused unit test for the
// exact #115 regression shape, independent of the full bootstrap render: a
// Flux Kustomization whose spec.images rendered as an explicit YAML null
// (an empty {{ range }} leaves a bare `images:` key behind) must fail
// kubeconform's array-typed schema check.
func TestKubeconformSchema_CatchesNullImages(t *testing.T) {
	const brokenKustomization = `
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: client-builders
  namespace: flux-system
spec:
  interval: 10s
  path: ./builders/overlays/local
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  images:
`
	v, err := validator.New([]string{kubeconformSchemaLocation}, validator.Opts{
		Strict:               true,
		IgnoreMissingSchemas: true,
	})
	if err != nil {
		t.Fatalf("kubeconform validator.New: %v", err)
	}
	results := v.Validate("broken", io.NopCloser(strings.NewReader(brokenKustomization)))
	if len(results) != 1 || results[0].Status != validator.Invalid {
		t.Fatalf("kubeconform results = %+v, want exactly one Invalid result for null spec.images", results)
	}
	joined := ""
	for _, ve := range results[0].ValidationErrors {
		joined += ve.Path + " "
	}
	if !strings.Contains(joined, "images") {
		t.Errorf("expected a validation error naming spec.images, got paths: %q", joined)
	}
}
