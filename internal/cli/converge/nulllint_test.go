package converge

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// lintNoExplicitNulls fails the rendered YAML tree at dir if any document
// contains an explicit null (issue #117, Tier 3a). Go's text/template
// silently renders an empty `{{ range }}` as nothing, so a template shaped
// like:
//
//	spec:
//	  images:
//	{{- range .Images }}
//	    - name: {{ .Name }}
//	{{- end }}
//
// becomes a bare `images:` key when .Images is empty — valid YAML for an
// explicit null, but rejected by any CRD whose schema types that field as an
// array (Flux's Kustomization spec.images, caught live by PR #115). A typed
// strict-decode would NOT catch this: Go happily decodes YAML null into a
// nil slice with no error. Walking the generic document tree for literal
// nulls is what would have caught it in a unit test instead of a 20-minute
// e2e timeout.
//
// Only .yaml/.yml files are walked; a whole document that decodes to nil
// (e.g. a stray leading/trailing `---`) is not itself an error — applier.go's
// applyYAML already tolerates and skips those — only a null NESTED inside an
// otherwise-real document is flagged.
func lintNoExplicitNulls(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(p); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		return lintDocumentNulls(rel, raw)
	})
	if err != nil {
		t.Error(err)
	}
}

// lintDocumentNulls decodes every `---`-separated document in raw and fails
// on the first explicit null found nested inside one.
func lintDocumentNulls(name string, raw []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for i := 0; ; i++ {
		var doc interface{}
		if err := dec.Decode(&doc); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("%s: decode document %d: %w", name, i, err)
		}
		if path := findNull(doc, ""); path != "" {
			return fmt.Errorf("%s: document %d has an explicit null at %q — a bare `key:` (e.g. an empty template range) parses as YAML null, not an empty value", name, i, path)
		}
	}
}

// findNull walks a generically-decoded YAML value and returns a dotted path
// to the first nested null it finds, or "" if none. A nil at the root (path
// "") is intentionally not reported — see lintNoExplicitNulls.
func findNull(v interface{}, path string) string {
	switch t := v.(type) {
	case nil:
		return path
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if p := findNull(t[k], path+"."+k); p != "" {
				return p
			}
		}
	case []interface{}:
		for i, val := range t {
			if p := findNull(val, fmt.Sprintf("%s[%d]", path, i)); p != "" {
				return p
			}
		}
	}
	return ""
}
