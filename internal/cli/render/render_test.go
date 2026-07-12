package render

import (
	"os"
	"path/filepath"
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

// TestTree_EmbeddedStructFieldsPromote confirms the semantics the typed render
// contexts (schema.Core embedded by SkeletonContext/BootstrapContext/AppContext)
// rely on: text/template resolves a promoted field from an anonymous embedded
// struct just like a top-level one, and a placeholder that names NO field on the
// struct (nor any embed) is a render error — the struct analogue of
// missingkey=error, so a mistyped placeholder still fails loudly at render time.
func TestTree_EmbeddedStructFieldsPromote(t *testing.T) {
	type core struct{ ClientName string }
	type ctx struct {
		core
		AppName string
	}
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "ok.tmpl"), "{{ .ClientName }}/{{ .AppName }}\n")

	dest := t.TempDir()
	if err := Tree(os.DirFS(src), ".", dest, ctx{core: core{ClientName: "acme"}, AppName: "web"}); err != nil {
		t.Fatalf("Tree with embedded-core struct: %v", err)
	}
	if got := mustRead(t, filepath.Join(dest, "ok")); got != "acme/web\n" {
		t.Errorf("promoted field render = %q, want %q", got, "acme/web\n")
	}

	// A placeholder naming no struct field must fail (not silently blank).
	bad := t.TempDir()
	mustWrite(t, filepath.Join(bad, "bad.tmpl"), "{{ .Nope }}")
	if err := Tree(os.DirFS(bad), ".", t.TempDir(), ctx{}); err == nil {
		t.Fatal("Tree with unknown struct field should fail at render time")
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
