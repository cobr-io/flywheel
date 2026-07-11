package converge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	flywheel "github.com/cobr-io/flywheel"
	flywheelSchema "github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/naming"
)

// These tests pin the identity strings in internal/naming to the literals
// baked into the embedded templates/manifests. The templates are plain YAML
// (git-deploy-controller / Flux read the rendered strings, not the Go
// constants), so a rename on one side must fail `go test` here rather than on a
// live cluster. Editing a covered literal in a template WITHOUT updating the
// matching constant (or vice versa) breaks one of these.

// renderBootstrapForAgreement renders the bootstrap tree with representative
// inputs and returns its tmpdir (registered for cleanup).
func renderBootstrapForAgreement(t *testing.T) string {
	t.Helper()
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
		"git-deploy-controller":    "flywheel-dev/git-deploy-controller:dogfood",
	}
	dir, err := RenderBootstrap(cfg, refs, "abc123def456abc123def456abc123def456abcd", "acme-gitops")
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestNamingAgreement_SelfSourceTemplate asserts self-source.yaml.tmpl embeds
// naming.DeployBranch and naming.GitServerURL(naming.FlywheelNamespace).
func TestNamingAgreement_SelfSourceTemplate(t *testing.T) {
	dir := renderBootstrapForAgreement(t)
	selfSrc := mustRead(t, filepath.Join(dir, "self-source.yaml"))

	if want := "branch: " + naming.DeployBranch; !strings.Contains(selfSrc, want) {
		t.Errorf("self-source.yaml is out of agreement with naming.DeployBranch (%q); rendered:\n%s", naming.DeployBranch, selfSrc)
	}
	if want := naming.GitServerURL(naming.FlywheelNamespace); !strings.Contains(selfSrc, want) {
		t.Errorf("self-source.yaml is out of agreement with naming.GitServerURL (%q); rendered:\n%s", want, selfSrc)
	}
}

// TestNamingAgreement_FlywheelSourceTemplate asserts flywheel-source.yaml.tmpl
// embeds naming.GitServerURL(naming.FlywheelNamespace).
func TestNamingAgreement_FlywheelSourceTemplate(t *testing.T) {
	dir := renderBootstrapForAgreement(t)
	src := mustRead(t, filepath.Join(dir, "flywheel-source.yaml"))

	if want := naming.GitServerURL(naming.FlywheelNamespace); !strings.Contains(src, want) {
		t.Errorf("flywheel-source.yaml is out of agreement with naming.GitServerURL (%q); rendered:\n%s", want, src)
	}
}

// TestNamingAgreement_IUAManifest asserts the static
// manifests/dev-loop/base/image-update-automation.yaml commits image bumps to
// naming.DeployBranch (via the tracked GitRepository's ref). It is a plain YAML
// comment/annotation the IUA depends on, so it must match the constant.
func TestNamingAgreement_IUAManifest(t *testing.T) {
	raw, err := flywheel.Assets.ReadFile("manifests/dev-loop/base/image-update-automation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), naming.DeployBranch) {
		t.Errorf("image-update-automation.yaml is out of agreement with naming.DeployBranch (%q):\n%s", naming.DeployBranch, string(raw))
	}
}
