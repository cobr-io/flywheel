package app

// T24 corpus — kustomization `resources:` editing + namespace-stream editing.
//
// The kustomization editors (appendResource / preflightResources) and the
// namespace-doc editor (ensureNamespace) are the add-app half of the client-file
// YAML surface. This corpus pins their EXACT behavior on the nasty-but-legal
// inputs the plan calls out: inline comments on `resources:`, 4-space indent,
// empty inline lists, CRLF, duplicate keys.
//
// Committed in two states, mirroring config/corpus_test.go:
//
//   - Baseline (commit 1): `want`/`wantErr` record what the CURRENT line-scanner
//     does, including the corruption/refusal bugs tagged `BASELINE BUG:`.
//   - Post-migration (commit 2): the yamledit-backed editors fix the tagged
//     bugs; each flips to correct behavior tagged `FIXED:`.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type kCase struct {
	name    string
	in      string
	op      func(path string) error
	wantErr string // substring; "" => expect success
	want    string // exact output; checked only when wantErr == ""
	notes   string
}

func appendRes(name string) func(string) error {
	return func(p string) error { return appendResource(p, name) }
}

func runKCorpus(t *testing.T, cases []kCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "kustomization.yaml")
			if err := os.WriteFile(p, []byte(tc.in), 0o644); err != nil {
				t.Fatal(err)
			}
			err := tc.op(p)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("%s\nwant error containing %q, got %v", tc.notes, tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.notes, err)
			}
			got, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("%s\n--- got ---\n%q\n--- want ---\n%q", tc.notes, string(got), tc.want)
			}
		})
	}
}

const kHead = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"

func TestCorpus_Kustomization(t *testing.T) {
	runKCorpus(t, []kCase{
		{
			name:  "empty_inline_list_becomes_block",
			in:    kHead + "resources: []\n",
			op:    appendRes("bar"),
			want:  kHead + "resources:\n  - ./bar\n",
			notes: "resources: [] rewritten to a block sequence with the first entry",
		},
		{
			name:  "append_to_block_sequence",
			in:    kHead + "resources:\n  - ./foo\n",
			op:    appendRes("bar"),
			want:  kHead + "resources:\n  - ./foo\n  - ./bar\n",
			notes: "append after the last existing entry",
		},
		{
			name:  "idempotent_readd",
			in:    kHead + "resources:\n  - ./foo\n",
			op:    appendRes("foo"),
			want:  kHead + "resources:\n  - ./foo\n",
			notes: "re-adding an existing entry is a no-op",
		},
		{
			name:    "inline_comment_on_resources",
			in:      kHead + "resources:  # keep sorted\n  - ./foo\n",
			op:      appendRes("bar"),
			wantErr: "missing a `resources:` key",
			// BASELINE BUG: the line scanner matches `trim == "resources:"` only,
			// so `resources:  # keep sorted` (a valid key with a line comment) is
			// not recognized and append REFUSES with a bogus "missing key" error.
			notes: "BASELINE BUG: inline comment on resources: defeats the scanner",
		},
		{
			name: "four_space_indent",
			in:   kHead + "resources:\n    - ./foo\n    - ./baz\n",
			op:   appendRes("bar"),
			// BASELINE BUG: lastResourceEntry only recognizes 2-space `  - ` items,
			// so a 4-space-indented list is treated as empty: the new entry is
			// inserted right after the key at 2-space indent, LANDING ABOVE the
			// original items and breaking indentation/order.
			want:  kHead + "resources:\n  - ./bar\n    - ./foo\n    - ./baz\n",
			notes: "BASELINE BUG: 4-space indent corrupts placement + indentation",
		},
		{
			name:    "crlf_line_endings",
			in:      "apiVersion: kustomize.config.k8s.io/v1beta1\r\nkind: Kustomization\r\nresources:\r\n  - ./foo\r\n",
			op:      appendRes("bar"),
			wantErr: "missing a `resources:` key",
			// BASELINE BUG: TrimRight(line, " \t") leaves the trailing \r, so
			// `resources:\r` != `resources:` and the scanner refuses the file.
			notes: "BASELINE BUG: CRLF trailing \\r defeats the scanner",
		},
		{
			name:  "duplicate_resources_key_edits_first",
			in:    "resources:\n  - ./foo\nresources:\n  - ./baz\n",
			op:    appendRes("bar"),
			want:  "resources:\n  - ./foo\n  - ./bar\nresources:\n  - ./baz\n",
			notes: "duplicate resources keys: append into the first block",
		},
	})
}

// TestCorpus_PreflightResources pins the read-only preflight against the same
// inputs — it must stay consistent with appendResource (same accept/reject set).
func TestCorpus_PreflightResources(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
		notes   string
	}{
		{"block_sequence", kHead + "resources:\n  - ./foo\n", false, "present block key accepted"},
		{"empty_inline_list", kHead + "resources: []\n", false, "empty inline list accepted"},
		{"missing_key", kHead + "patches: []\n", true, "no resources key rejected"},
		// BASELINE BUG: preflight mirrors appendResource's scanner, so an inline
		// comment on resources: is rejected here too — meaning add-app fails its
		// pre-flight on a perfectly valid kustomization.
		{"inline_comment", kHead + "resources:  # keep sorted\n", true, "BASELINE BUG: inline comment rejected by preflight"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "kustomization.yaml")
			if err := os.WriteFile(p, []byte(tc.in), 0o644); err != nil {
				t.Fatal(err)
			}
			err := preflightResources(p)
			if (err != nil) != tc.wantErr {
				t.Fatalf("%s: preflightResources err=%v, wantErr=%v", tc.notes, err, tc.wantErr)
			}
		})
	}
}

// TestCorpus_Namespace pins ensureNamespace's stream-append + idempotency.
func TestCorpus_Namespace(t *testing.T) {
	nsDoc := func(ns string) string {
		return "---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + ns + "\n  labels:\n    kubernetes.io/metadata.name: " + ns + "\n"
	}
	t.Run("create_then_idempotent", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "namespaces.yaml")
		if err := ensureNamespace(p, "myapps"); err != nil {
			t.Fatal(err)
		}
		if err := ensureNamespace(p, "myapps"); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(p)
		if string(got) != nsDoc("myapps") {
			t.Errorf("create/idempotent\n--- got ---\n%q\n--- want ---\n%q", string(got), nsDoc("myapps"))
		}
	})
	t.Run("append_second_namespace", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "namespaces.yaml")
		if err := os.WriteFile(p, []byte(nsDoc("myapps")), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ensureNamespace(p, "other"); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(p)
		want := nsDoc("myapps") + nsDoc("other")
		if string(got) != want {
			t.Errorf("append second\n--- got ---\n%q\n--- want ---\n%q", string(got), want)
		}
	})
}
