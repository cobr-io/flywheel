package config

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// T0.5 — `.local`-merge tests: scalar override wins; nested map deep-merges
// recursively; .local arrays replace the committed list wholesale.

func unmarshal(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	return v
}

func TestMerge_EmptyLocalIsNoop(t *testing.T) {
	committed := []byte("a: 1\nb: 2\n")
	got, err := MergeYAML(committed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(committed) {
		t.Errorf("empty .local should be no-op; got %q", got)
	}
}

func TestMerge_ScalarOverride(t *testing.T) {
	committed := []byte(`cluster:
  http_port: 8083
`)
	local := []byte(`cluster:
  http_port: 9083
`)
	got, err := MergeYAML(committed, local)
	if err != nil {
		t.Fatal(err)
	}
	merged := unmarshal(t, got)
	cluster := merged.(map[string]any)["cluster"].(map[string]any)
	if cluster["http_port"] != float64(9083) {
		t.Errorf("scalar override failed: cluster.http_port = %v, want 9083", cluster["http_port"])
	}
}

func TestMerge_NestedMapDeepMerges(t *testing.T) {
	committed := []byte(`
cluster:
  name: acme-local
  http_port: 8083
  https_port: 8543
`)
	local := []byte(`
cluster:
  http_port: 9083
`)
	got, err := MergeYAML(committed, local)
	if err != nil {
		t.Fatal(err)
	}
	merged := unmarshal(t, got).(map[string]any)
	cluster := merged["cluster"].(map[string]any)
	if cluster["name"] != "acme-local" {
		t.Errorf("nested map should preserve committed keys; got cluster.name=%v", cluster["name"])
	}
	if cluster["http_port"] != float64(9083) {
		t.Errorf("nested map should override matching keys; got cluster.http_port=%v", cluster["http_port"])
	}
	if cluster["https_port"] != float64(8543) {
		t.Errorf("nested map should preserve unspecified keys; got cluster.https_port=%v", cluster["https_port"])
	}
}

func TestMerge_TopLevelKeyAdded(t *testing.T) {
	committed := []byte(`schema: v1alpha1
`)
	local := []byte(`paths:
  workspaces_root: /Users/dev/src
`)
	got, err := MergeYAML(committed, local)
	if err != nil {
		t.Fatal(err)
	}
	merged := unmarshal(t, got).(map[string]any)
	if _, ok := merged["paths"]; !ok {
		t.Error("top-level paths key not added")
	}
	if merged["schema"] != "v1alpha1" {
		t.Error("schema not preserved")
	}
}

func TestMerge_ArrayReplaceWholesale(t *testing.T) {
	// Headline T0.5 invariant: arrays are replaced, not merged.
	cases := []struct {
		name      string
		committed string
		local     string
		want      []any
	}{
		{
			name: "sops.age_recipients_local",
			committed: `sops:
  age_recipients_local:
    - age1aaa
    - age1bbb
`,
			local: `sops:
  age_recipients_local:
    - age1ccc
`,
			want: []any{"age1ccc"},
		},
		{
			name: "nested list replace (operatorConfig.defaultTags)",
			committed: `operatorConfig:
  defaultTags:
    - tag:committed1
    - tag:committed2
`,
			local: `operatorConfig:
  defaultTags:
    - tag:local-only
`,
			want: []any{"tag:local-only"},
		},
		{
			name: "top-level list",
			committed: `clusters:
  - one
  - two
  - three
`,
			local: `clusters:
  - just-this-one
`,
			want: []any{"just-this-one"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MergeYAML([]byte(tc.committed), []byte(tc.local))
			if err != nil {
				t.Fatal(err)
			}
			merged := unmarshal(t, got).(map[string]any)
			// Walk the merged tree to find the only list.
			var found []any
			var walk func(v any)
			walk = func(v any) {
				switch t := v.(type) {
				case map[string]any:
					for _, vv := range t {
						walk(vv)
					}
				case []any:
					found = t
				}
			}
			walk(merged)
			if len(found) != len(tc.want) {
				t.Fatalf("array length = %d, want %d (got %v)", len(found), len(tc.want), found)
			}
			for i := range tc.want {
				if found[i] != tc.want[i] {
					t.Errorf("array[%d] = %v, want %v", i, found[i], tc.want[i])
				}
			}
		})
	}
}

func TestMerge_ArrayNotConcatenated(t *testing.T) {
	// Explicit anti-test: under no circumstance should we see committed
	// AND local items in the result.
	committed := []byte(`sops:
  age_recipients_local:
    - age1aaa
`)
	local := []byte(`sops:
  age_recipients_local:
    - age1bbb
`)
	got, err := MergeYAML(committed, local)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "age1aaa") {
		t.Errorf("arrays were concatenated, not replaced: got %s", got)
	}
}

func TestMerge_NullDeletes(t *testing.T) {
	committed := []byte(`cluster:
  http_port: 8083
  https_port: 8543
`)
	local := []byte(`cluster:
  http_port: null
`)
	got, err := MergeYAML(committed, local)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "http_port") {
		t.Errorf("null should delete the key; got %s", got)
	}
	if !strings.Contains(string(got), "https_port") {
		t.Errorf("unrelated keys should survive; got %s", got)
	}
}
