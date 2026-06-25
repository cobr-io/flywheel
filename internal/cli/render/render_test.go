package render

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T0.2 — renders templates/client-skeleton/ with sample values; output
// is byte-identical to a checked-in golden. For this Phase-0 unit test
// we use a synthetic source tree (not the real client-skeleton) because
// the goldens for the real skeleton come together once Phase 1.1
// (`flywheel new`) is implementing all eight template vars.

func TestTree_RendersAndCopies(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "README.md.tmpl"),
		"# {{ .ClientName }}-gitops\n")
	mustWrite(t, filepath.Join(src, "nested", "kustomization.yaml.tmpl"),
		"resources:\n  - {{ .ClientName }}\n")
	mustWrite(t, filepath.Join(src, "static", "verbatim.txt"),
		"copy me verbatim\n")

	dest := t.TempDir()
	values := struct{ ClientName string }{ClientName: "acme"}
	if err := Tree(os.DirFS(src), ".", dest, values); err != nil {
		t.Fatalf("Tree: %v", err)
	}

	got := mustRead(t, filepath.Join(dest, "README.md"))
	if got != "# acme-gitops\n" {
		t.Errorf("README.md = %q, want %q", got, "# acme-gitops\n")
	}
	got = mustRead(t, filepath.Join(dest, "nested", "kustomization.yaml"))
	if got != "resources:\n  - acme\n" {
		t.Errorf("nested kustomization = %q", got)
	}
	got = mustRead(t, filepath.Join(dest, "static", "verbatim.txt"))
	if got != "copy me verbatim\n" {
		t.Errorf("static file lost content: %q", got)
	}

	// The .tmpl suffix must be stripped from the dest path.
	if _, err := os.Stat(filepath.Join(dest, "README.md.tmpl")); !os.IsNotExist(err) {
		t.Errorf(".tmpl suffix not stripped: file still exists at dest")
	}
}

func TestTree_MissingKeyIsAnError(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "x.tmpl"), "{{ .Unknown }}")
	if err := Tree(os.DirFS(src), ".", t.TempDir(), struct{ Known string }{}); err == nil {
		t.Fatal("Tree with unknown template var should fail (missingkey=error)")
	}
}

func TestTree_RealClientSkeletonRendersWithAllPlaceholders(t *testing.T) {
	// Smoke test: every template in templates/client-skeleton/ must
	// render with a fully-populated values struct. Catches template
	// vars that haven't been wired into the renderer's value type yet.
	values := struct {
		ClientName        string
		RepoBaseName      string
		Org               string
		Domain            string
		ClusterName       string
		Registry          string
		RegistryPort      int
		HttpPort          int
		HttpsPort         int
		FlywheelVersion   string
		FlywheelSHA       string
		FlywheelRepoURL   string
		FluxIntervalLocal string
		AgePublicKey      string
	}{
		ClientName:        "dogfood",
		RepoBaseName:      "dogfood",
		Org:               "cobr-io",
		Domain:            "localdev.me",
		ClusterName:       "dogfood-local",
		Registry:          "dogfood-local-registry",
		RegistryPort:      50001,
		HttpPort:          8080,
		HttpsPort:         8540,
		FlywheelVersion:   "v0.1.0",
		FlywheelSHA:       "0123456789abcdef0123456789abcdef01234567",
		FlywheelRepoURL:   "https://github.com/cobr-io/flywheel",
		FluxIntervalLocal: "10s",
		AgePublicKey:      "age1qx5vhc3z8q",
	}

	wd, _ := os.Getwd()
	// Walk up to repo root to find templates/.
	root := wd
	for {
		if _, err := os.Stat(filepath.Join(root, "templates", "client-skeleton")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("could not locate templates/client-skeleton from test cwd")
		}
		root = parent
	}

	dest := t.TempDir()
	if err := Tree(os.DirFS(filepath.Join(root, "templates", "client-skeleton")), ".", dest, values); err != nil {
		t.Fatalf("Tree on real skeleton: %v", err)
	}

	// Spot-check a few rendered files contain the expected substitutions.
	yaml := mustRead(t, filepath.Join(dest, "flywheel.yaml"))
	if !strings.Contains(yaml, "name: dogfood") {
		t.Errorf("flywheel.yaml missing client.name; got:\n%s", yaml)
	}
	if !strings.Contains(yaml, "registry_port: 50001") {
		t.Errorf("flywheel.yaml missing registry_port; got:\n%s", yaml)
	}
}

// Helpers.

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Unused but useful when adding goldens later.
var _ = fs.WalkDir
