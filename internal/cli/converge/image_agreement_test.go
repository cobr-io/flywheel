package converge

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	flywheelSchema "github.com/cobr-io/flywheel/internal/cli/schema"
	"sigs.k8s.io/yaml"
)

// TestBootstrapImages_TemplateUnionMatchesSchema renders the two bootstrap
// Kustomization templates and asserts the UNION of their spec.images entries
// equals schema.ImageNames exactly (and that the two blocks partition the set,
// with no image in both). bootstrapValues derives both `images:` blocks from
// schema.ImageNames via the bootstrapImageOwners split table, so an image added
// to ImageNames without an owner entry renders into NEITHER block — this test
// turns that omission into a CI failure instead of a runtime ImagePullBackOff.
// (Task T18: the test that makes "forgot to wire the image" fail here.)
func TestBootstrapImages_TemplateUnionMatchesSchema(t *testing.T) {
	cfg := &flywheelSchema.File{}
	cfg.Client.Name = "acme"
	cfg.Cluster.Name = "acme-local"
	cfg.Cluster.Registry = "acme-local-registry"
	cfg.Cluster.RegistryPort = 50001
	cfg.Flux.IntervalLocal = "10s"
	cfg.Local.Domain = "localdev.me"

	// One resolved ref per known image, each with an explicit tag.
	refs := map[string]string{}
	for _, name := range flywheelSchema.ImageNames {
		refs[name] = "flywheel-dev/" + name + ":dogfood"
	}

	dir, err := RenderBootstrap(cfg, refs, "abc", "acme-gitops", "")
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}
	defer os.RemoveAll(dir)

	got := map[string]bool{}
	for _, f := range []string{"builders-kustomization.yaml", "client-builders-kustomization.yaml"} {
		for _, name := range imageNamesIn(t, filepath.Join(dir, f)) {
			if got[name] {
				t.Errorf("image %q appears in more than one bootstrap Kustomization; the split must be a partition", name)
			}
			got[name] = true
		}
	}

	gotSorted := sortedImageSet(got)
	wantSorted := append([]string(nil), flywheelSchema.ImageNames...)
	sort.Strings(wantSorted)
	if !reflect.DeepEqual(gotSorted, wantSorted) {
		t.Errorf("bootstrap template images %v != schema.ImageNames %v\n"+
			"every image must have a bootstrapImageOwners entry — see docs/dev/add-controller-image.md",
			gotSorted, wantSorted)
	}
}

// imageNamesIn parses a rendered Flux Kustomization and returns its
// spec.images[].name values with the ghcr.io/cobr-io/ prefix stripped (i.e. the
// bare schema image key).
func imageNamesIn(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var k struct {
		Spec struct {
			Images []struct {
				Name string `json:"name"`
			} `json:"images"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	const prefix = "ghcr.io/cobr-io/"
	var names []string
	for _, img := range k.Spec.Images {
		names = append(names, strings.TrimPrefix(img.Name, prefix))
	}
	return names
}

func sortedImageSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
