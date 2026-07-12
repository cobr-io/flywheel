package yamledit

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func mustEq(t *testing.T, got []byte, want, name string) {
	t.Helper()
	if string(got) != want {
		t.Errorf("%s\n--- got ---\n%q\n--- want ---\n%q", name, string(got), want)
	}
}

// --- SetScalar: byte-stability ---

func TestSetScalar_InPlaceTokenOnly(t *testing.T) {
	in := "schema: v1alpha1\n\n# a header comment\ncluster:\n  name: acme-local\n  http_port: 8080      # host comment\n  https_port: 8540\n"
	out, err := SetScalar([]byte(in), []string{"cluster", "http_port"}, "!!int", "8081")
	if err != nil {
		t.Fatal(err)
	}
	// Byte-identical except the single token 8080 -> 8081.
	want := strings.Replace(in, "http_port: 8080 ", "http_port: 8081 ", 1)
	mustEq(t, out, want, "only the port token changes; comment spacing/blanks/CRLF intact")
}

func TestSetScalar_AppendsAbsentLeaf(t *testing.T) {
	in := "schema: v1alpha1\ncluster:\n  name: acme-local\n  http_port: 8080\n"
	out, err := SetScalar([]byte(in), []string{"cluster", "https_port"}, "!!int", "8540")
	if err != nil {
		t.Fatal(err)
	}
	mustEq(t, out, in+"  https_port: 8540\n", "absent leaf appended into cluster block")
}

func TestSetScalar_CreatesAbsentChain(t *testing.T) {
	in := "schema: v1alpha1\nclient:\n  name: acme\n"
	out, err := SetScalar([]byte(in), []string{"cluster", "http_port"}, "!!int", "8081")
	if err != nil {
		t.Fatal(err)
	}
	mustEq(t, out, in+"cluster:\n  http_port: 8081\n", "absent chain created at EOF")
}

func TestSetScalar_CRLFPreserved(t *testing.T) {
	in := "schema: v1alpha1\r\n\r\ncluster:\r\n  http_port: 8080\r\n"
	out, err := SetScalar([]byte(in), []string{"cluster", "http_port"}, "!!int", "8081")
	if err != nil {
		t.Fatal(err)
	}
	mustEq(t, out, "schema: v1alpha1\r\n\r\ncluster:\r\n  http_port: 8081\r\n", "CRLF preserved, only token changed")
}

// --- AppendListItem ---

func TestAppendListItem_Cases(t *testing.T) {
	cases := []struct{ name, in, item, want string }{
		{"empty_inline", "resources: []\n", "./bar", "resources:\n  - ./bar\n"},
		{"block_append", "resources:\n  - ./foo\n", "./bar", "resources:\n  - ./foo\n  - ./bar\n"},
		{"four_space_indent", "resources:\n    - ./foo\n    - ./baz\n", "./bar", "resources:\n    - ./foo\n    - ./baz\n    - ./bar\n"},
		{"inline_comment", "resources:  # keep sorted\n  - ./foo\n", "./bar", "resources:  # keep sorted\n  - ./foo\n  - ./bar\n"},
		{"idempotent", "resources:\n  - ./foo\n", "./foo", "resources:\n  - ./foo\n"},
		{"crlf", "resources:\r\n  - ./foo\r\n", "./bar", "resources:\r\n  - ./foo\r\n  - ./bar\r\n"},
		{"bare_null_key", "resources:\n", "./bar", "resources:\n  - ./bar\n"},
		{"nonempty_flow_to_block", "resources: [./foo]\n", "./bar", "resources:\n  - ./foo\n  - ./bar\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := AppendListItem([]byte(tc.in), []string{"resources"}, tc.item)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			mustEq(t, out, tc.want, tc.name)
		})
	}
}

func TestAppendListItem_NoKey(t *testing.T) {
	_, err := AppendListItem([]byte("patches: []\n"), []string{"resources"}, "./x")
	if !errors.Is(err, ErrNoSequenceKey) {
		t.Fatalf("want ErrNoSequenceKey, got %v", err)
	}
}

// --- HasSequenceKey ---

func TestHasSequenceKey(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"resources:\n  - ./foo\n", true},
		{"resources: []\n", true},
		{"resources:\n", true},
		{"resources:  # comment\n", true},
		{"patches: []\n", false},
		{"resources: notalist\n", false},
	}
	for _, tc := range cases {
		got, err := HasSequenceKey([]byte(tc.in), []string{"resources"})
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("HasSequenceKey(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// --- HasNamespace ---

func TestHasNamespace(t *testing.T) {
	stream := "---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: myapps\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: notns\n"
	for _, tc := range []struct {
		ns   string
		want bool
	}{
		{"myapps", true},
		{"notns", false}, // a ConfigMap named notns must not count
		{"missing", false},
	} {
		got, err := HasNamespace([]byte(stream), tc.ns)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("HasNamespace(%q) = %v, want %v", tc.ns, got, tc.want)
		}
	}
	if got, _ := HasNamespace(nil, "x"); got {
		t.Error("HasNamespace(nil) should be false")
	}
}

// --- EditBlock: the bug-fix cases (foot comment, multi-line scalar) ---

func TestEditBlock_FootCommentNotDuplicated(t *testing.T) {
	in := "schema: v1alpha1\n\nworkspace:\n  repos:\n    - name: existing-app\n      url: git@github.com:acme/existing-app.git\n  # foot comment inside workspace\n\ngit:\n  integration_branch: main\n"
	out, err := EditBlock([]byte(in), "workspace", func(ws *yaml.Node) error {
		repos := ws.Content[1] // repos value (repos is the only key)
		repos.Content = append(repos.Content, entry("new-app", "u"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(out), "# foot comment inside workspace"); n != 1 {
		t.Errorf("foot comment count = %d, want 1\n%s", n, out)
	}
	if !strings.Contains(string(out), "name: new-app") {
		t.Errorf("new entry missing:\n%s", out)
	}
	// Surroundings preserved.
	if !strings.HasPrefix(string(out), "schema: v1alpha1\n\nworkspace:") ||
		!strings.HasSuffix(string(out), "\ngit:\n  integration_branch: main\n") {
		t.Errorf("surroundings changed:\n%s", out)
	}
}

func TestEditBlock_MultilineScalarNotOrphaned(t *testing.T) {
	in := "schema: v1alpha1\n\nworkspace:\n  repos:\n    - name: existing-app\n      url: >-\n        long-url-value\n\ngit:\n  integration_branch: main\n"
	out, err := EditBlock([]byte(in), "workspace", func(ws *yaml.Node) error {
		repos := ws.Content[1]
		repos.Content = append(repos.Content, entry("new-app", "u"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The folded content must appear exactly once (no orphaned dangling copy).
	if n := strings.Count(string(out), "long-url-value"); n != 1 {
		t.Errorf("long-url-value count = %d, want 1\n%s", n, out)
	}
	// And the file must still parse and round-trip.
	var probe yaml.Node
	if err := yaml.Unmarshal(out, &probe); err != nil {
		t.Fatalf("result no longer parses: %v\n%s", err, out)
	}
}

func entry(name, url string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	n.Content = append(n.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "url"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: url},
	)
	return n
}
