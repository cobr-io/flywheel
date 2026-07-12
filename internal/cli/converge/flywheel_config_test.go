package converge

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	flywheel "github.com/cobr-io/flywheel"
	flywheelSchema "github.com/cobr-io/flywheel/internal/cli/schema"
)

// flywheelConfigTestCfg is a representative merged config for the
// flywheel-config producer/render tests. Namespaces are set to NON-default
// values so a test asserting cfg values flow through can't pass vacuously
// against the old hardcoded "flywheel-system"/"apps" literals.
func flywheelConfigTestCfg() *flywheelSchema.File {
	cfg := &flywheelSchema.File{}
	cfg.Client.Name = "acme"
	cfg.Cluster.Name = "acme-local"
	cfg.Cluster.Registry = "acme-local-registry"
	cfg.Cluster.RegistryPort = 50001
	cfg.Flux.IntervalLocal = "10s"
	cfg.Local.Domain = "localdev.me"
	cfg.Namespaces.Flywheel = "custom-flywheel"
	cfg.Namespaces.Apps = "custom-apps"
	return cfg
}

func flywheelConfigTestRefs() map[string]string {
	return map[string]string{
		"git-server":               "flywheel-dev/git-server:dogfood",
		"git-auto-sync":            "flywheel-dev/git-auto-sync:dogfood",
		"image-builder-controller": "flywheel-dev/image-builder-controller:dogfood",
		"git-deploy-controller":    "flywheel-dev/git-deploy-controller:dogfood",
	}
}

// consumerConfigMapKeys extracts every ConfigMap key a static manifest reads via
// `configMapKeyRef` against the flywheel-config ConfigMap. The manifests are
// plain YAML the kubelet resolves (not Go), so the only guard against a consumer
// referencing a key the producer never writes is a test that reads both sides.
func consumerConfigMapKeys(t *testing.T, manifestPath string) []string {
	t.Helper()
	raw, err := flywheel.Assets.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read %s: %v", manifestPath, err)
	}
	var keys []string
	inRef := false
	namesFlywheelConfig := false
	for _, ln := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(ln)
		switch {
		case trimmed == "configMapKeyRef:":
			inRef = true
			namesFlywheelConfig = false
		case inRef && strings.HasPrefix(trimmed, "name:"):
			namesFlywheelConfig = strings.TrimSpace(strings.TrimPrefix(trimmed, "name:")) == "flywheel-config"
		case inRef && strings.HasPrefix(trimmed, "key:"):
			if namesFlywheelConfig {
				keys = append(keys, strings.TrimSpace(strings.TrimPrefix(trimmed, "key:")))
			}
			inRef = false
		}
	}
	return keys
}

// TestFlywheelConfig_ConsumersAgreeWithProducer asserts that every
// configMapKeyRef key the two dev-loop controllers read exists in the single
// producer map (FlywheelConfigData). A consumer referencing a key the producer
// never writes would leave the controller pod stuck in CreateContainerConfigError
// on a live cluster — this catches that drift at `go test` time.
func TestFlywheelConfig_ConsumersAgreeWithProducer(t *testing.T) {
	produced := FlywheelConfigData(flywheelConfigTestCfg(), "acme-gitops")

	consumers := []string{
		"manifests/dev-loop/base/image-builder-controller.yaml",
		"manifests/dev-loop/base/git-deploy-controller.yaml",
	}
	total := 0
	for _, m := range consumers {
		keys := consumerConfigMapKeys(t, m)
		if len(keys) == 0 {
			t.Errorf("%s: found no flywheel-config configMapKeyRef keys — extractor broke or the manifest changed shape", m)
		}
		for _, k := range keys {
			total++
			if _, ok := produced[k]; !ok {
				t.Errorf("%s reads flywheel-config key %q, but FlywheelConfigData never writes it (producer keys: %v)",
					m, k, sortedKeys(produced))
			}
		}
	}
	if total == 0 {
		t.Fatal("no consumer keys checked — the agreement test is vacuous")
	}
}

// renderedFlywheelConfigData parses the data block of the rendered
// flywheel-config.yaml (bootstrap-tree copy) back into a map, so a test can
// assert the template emitted exactly the producer's keys/values.
func renderedFlywheelConfigData(t *testing.T, dir string) map[string]string {
	t.Helper()
	raw := mustRead(t, filepath.Join(dir, "flywheel-config.yaml"))
	data := map[string]string{}
	inData := false
	for _, ln := range strings.Split(raw, "\n") {
		if strings.TrimSpace(ln) == "data:" {
			inData = true
			continue
		}
		if !inData {
			continue
		}
		// The data block ends at the first non-blank, non-indented line.
		if ln != "" && !strings.HasPrefix(ln, " ") {
			break
		}
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		idx := strings.Index(trimmed, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:idx])
		val := strings.Trim(strings.TrimSpace(trimmed[idx+1:]), `"`)
		data[key] = val
	}
	return data
}

// TestFlywheelConfig_TemplateRendersFromProducer asserts the bootstrap-tree
// ConfigMap is rendered from the single producer map — same keys, same values,
// nothing hardcoded. In particular a NON-default namespaces.apps / .flywheel in
// cfg must reach the rendered ConfigMap (the template used to hardcode
// "apps"/"flywheel-system"), proving the two writers can't diverge.
func TestFlywheelConfig_TemplateRendersFromProducer(t *testing.T) {
	cfg := flywheelConfigTestCfg()
	dir, err := RenderBootstrap(cfg, flywheelConfigTestRefs(), "abc123def456abc123def456abc123def456abcd", "acme-gitops")
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}
	defer os.RemoveAll(dir)

	want := FlywheelConfigData(cfg, "acme-gitops")
	got := renderedFlywheelConfigData(t, dir)

	if len(got) != len(want) {
		t.Errorf("rendered ConfigMap has %d keys, producer has %d\n  rendered: %v\n  producer: %v",
			len(got), len(want), sortedKeys(got), sortedKeys(want))
	}
	for k, v := range want {
		if gv, ok := got[k]; !ok {
			t.Errorf("rendered ConfigMap is missing producer key %q", k)
		} else if gv != v {
			t.Errorf("rendered ConfigMap key %q = %q, producer = %q", k, gv, v)
		}
	}

	// Explicit non-default-namespace assertions: the template no longer
	// hardcodes these, so cfg's values must appear verbatim.
	if got["namespaces.apps"] != "custom-apps" {
		t.Errorf("namespaces.apps = %q, want the cfg value \"custom-apps\" — template still hardcoding?", got["namespaces.apps"])
	}
	if got["namespaces.flywheel"] != "custom-flywheel" {
		t.Errorf("namespaces.flywheel = %q, want the cfg value \"custom-flywheel\" — template still hardcoding?", got["namespaces.flywheel"])
	}
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
