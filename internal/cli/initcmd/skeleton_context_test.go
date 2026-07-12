package initcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cobr-io/flywheel/internal/cli/render"
	"github.com/cobr-io/flywheel/internal/cli/schema"
)

// TestSkeletonContext_RendersEveryPlaceholder is the smoke test that used to
// live in the render package with a drift-prone anonymous struct. It now renders
// the real templates/client-skeleton tree with the REAL skeletonContext type, so
// a template placeholder that hasn't been wired into skeletonContext (nor the
// embedded schema.Core) fails render.Tree here — a struct-field error, caught in
// CI, instead of surfacing only when `flywheel init` renders that file.
func TestSkeletonContext_RendersEveryPlaceholder(t *testing.T) {
	ctx := skeletonContext{
		Core: schema.NewCore(&schema.File{
			Client: schema.Client{Name: "dogfood"},
			Local:  schema.Local{Domain: "localdev.me"},
			Flux:   schema.Flux{IntervalLocal: "10s"},
		}),
		RepoBaseName:    "dogfood",
		Org:             "cobr-io",
		ClusterName:     "dogfood-local",
		Registry:        "dogfood-local-registry",
		RegistryPort:    50001,
		HttpPort:        8080,
		HttpsPort:       8540,
		FlywheelVersion: "v0.1.0",
		FlywheelRepoURL: "https://github.com/cobr-io/flywheel",
		AgePublicKey:    "age1qx5vhc3z8q",
	}

	dest := t.TempDir()
	if err := render.Tree(os.DirFS(skeletonDir(t)), ".", dest, ctx); err != nil {
		t.Fatalf("Tree on real skeleton with typed context: %v", err)
	}

	yaml := readFile(t, filepath.Join(dest, "flywheel.yaml"))
	if !strings.Contains(yaml, "name: dogfood") {
		t.Errorf("flywheel.yaml missing client.name; got:\n%s", yaml)
	}
	if !strings.Contains(yaml, "registry_port: 50001") {
		t.Errorf("flywheel.yaml missing registry_port; got:\n%s", yaml)
	}
	// FluxIntervalLocal is now templated (was a hardcoded literal) — prove the
	// wired knob flows through.
	if !strings.Contains(yaml, "interval_local: 10s") {
		t.Errorf("flywheel.yaml missing templated interval_local; got:\n%s", yaml)
	}
	// Domain (formerly split "Domain" vs "LocalDomain") resolves via Core.
	if !strings.Contains(yaml, "domain: localdev.me") {
		t.Errorf("flywheel.yaml missing local.domain; got:\n%s", yaml)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
