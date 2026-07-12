package config

// T24 corpus — flywheel.yaml editing (workspace splice + root scalar sets).
//
// This is the "brutal" golden corpus for the client-file YAML editors. Each case
// feeds a nasty-but-legal input (foot comments, multi-line block scalars, inline
// comments, blank-line structure, CRLF, duplicate keys) to one of the public
// editors and pins the EXACT bytes written back.
//
// It is committed in two states:
//
//   - Baseline (commit 1): every `want` records what the CURRENT implementation
//     actually does — INCLUDING the known data-corruption bugs, tagged
//     `BASELINE BUG:`. This is the regression net: it proves the corpus captures
//     reality before any refactor touches the editors.
//   - Post-migration (commit 2): the yamledit-backed editors fix the tagged
//     bugs; each `BASELINE BUG:` case flips to the correct output tagged
//     `FIXED:`, and the clean cases must stay byte-identical (proving the
//     refactor preserved good behavior).
//
// A diff between the two commits of this file is therefore a precise, reviewable
// statement of exactly which bytes the refactor changed.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cobr-io/flywheel/internal/cli/schema"
)

type corpusCase struct {
	name  string
	in    string
	op    func(path string) error
	want  string
	notes string
}

func runCorpus(t *testing.T, cases []corpusCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "flywheel.yaml")
			if err := os.WriteFile(p, []byte(tc.in), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := tc.op(p); err != nil {
				t.Fatalf("op: %v", err)
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

func upsert(repo schema.WorkspaceRepo) func(string) error {
	return func(p string) error { return UpsertWorkspaceRepo(p, repo) }
}

func setPort(key string, port int) func(string) error {
	return func(p string) error { return SetClusterPort(p, key, port) }
}

func setVersion(v string) func(string) error {
	return func(p string) error { return SetFlywheelVersion(p, v) }
}

// TestCorpus_Workspace pins UpsertWorkspaceRepo / SetWorkspaceRepoURL against the
// surgical-splice edge cases.
func TestCorpus_Workspace(t *testing.T) {
	runCorpus(t, []corpusCase{
		{
			name: "clean_block_preserves_surroundings",
			in:   "schema: v1alpha1\n\n# top note\nflywheel:\n  version: v0.1.0    # pinned tag\n\nworkspace:\n  repos:\n    - name: existing-app\n      url: git@github.com:acme/existing-app.git\n\ngit:\n  # keep this comment\n  integration_branch: main\n",
			op:   upsert(schema.WorkspaceRepo{Name: "new-app", URL: "git@github.com:acme/new-app.git"}),
			want: "schema: v1alpha1\n\n# top note\nflywheel:\n  version: v0.1.0    # pinned tag\n\nworkspace:\n  repos:\n    - name: existing-app\n      url: git@github.com:acme/existing-app.git\n    - name: new-app\n      url: git@github.com:acme/new-app.git\n\ngit:\n  # keep this comment\n  integration_branch: main\n",
			notes: "clean upsert: only the workspace block grows; every other byte is preserved",
		},
		{
			name: "append_new_block_keeps_prior_bytes",
			in:   "schema: v1alpha1\n\nclient:\n  name: acme\n",
			op:   upsert(schema.WorkspaceRepo{Name: "sample-app", URL: "git@github.com:acme/sample-app.git"}),
			want: "schema: v1alpha1\n\nclient:\n  name: acme\n\nworkspace:\n  repos:\n    - name: sample-app\n      url: git@github.com:acme/sample-app.git\n",
			notes: "no workspace block yet: append it after one blank line, prior bytes intact",
		},
		{
			name: "foot_comment_inside_block",
			in:   "schema: v1alpha1\n\nworkspace:\n  repos:\n    - name: existing-app\n      url: git@github.com:acme/existing-app.git\n  # foot comment inside workspace\n\ngit:\n  integration_branch: main\n",
			op:   upsert(schema.WorkspaceRepo{Name: "new-app", URL: "u"}),
			// BASELINE BUG: maxNodeLine stops at the last CONTENT node (the url on
			// line 6) and never counts the foot comment on line 7, so the splice
			// keeps the original foot-comment line AND marshalBlock re-emits it —
			// the comment is DUPLICATED.
			want:  "schema: v1alpha1\n\nworkspace:\n  repos:\n    - name: existing-app\n      url: git@github.com:acme/existing-app.git\n    - name: new-app\n      url: u\n  # foot comment inside workspace\n  # foot comment inside workspace\n\ngit:\n  integration_branch: main\n",
			notes: "BASELINE BUG: foot comment duplicated",
		},
		{
			name: "multiline_block_scalar_last",
			in:   "schema: v1alpha1\n\nworkspace:\n  repos:\n    - name: existing-app\n      url: >-\n        long-url-value\n\ngit:\n  integration_branch: main\n",
			op:   upsert(schema.WorkspaceRepo{Name: "new-app", URL: "u"}),
			// BASELINE BUG: maxNodeLine records the block scalar's START line (the
			// `>-` on line 6) but not its continuation (line 7), so the splice
			// leaves the folded content line orphaned after the freshly-marshaled
			// block — producing a dangling, mis-indented `long-url-value`.
			want:  "schema: v1alpha1\n\nworkspace:\n  repos:\n    - name: existing-app\n      url: >-\n        long-url-value\n    - name: new-app\n      url: u\n        long-url-value\n\ngit:\n  integration_branch: main\n",
			notes: "BASELINE BUG: multi-line block scalar continuation orphaned",
		},
		{
			name: "flip_local_only_to_url",
			in:   "schema: v1alpha1\n\nworkspace:\n  repos:\n    - name: hello-py\n      local_only: true\n",
			op:   func(p string) error { return SetWorkspaceRepoURL(p, "hello-py", "git@github.com:acme/hello-py.git") },
			want: "schema: v1alpha1\n\nworkspace:\n  repos:\n    - name: hello-py\n      url: git@github.com:acme/hello-py.git\n",
			notes: "publish-app flip: local_only replaced in place by url (exactly-one invariant)",
		},
	})
}

// TestCorpus_Root pins SetClusterPort / SetFlywheelVersion — the port-heal and
// version-drift writers that run automatically during `up` against a user's
// committed file.
func TestCorpus_Root(t *testing.T) {
	runCorpus(t, []corpusCase{
		{
			name: "set_port_reformats_whole_doc",
			in:   "schema: v1alpha1\n\n# a header comment\nclient:\n  name: acme\n\ncluster:\n  name: acme-local\n  http_port: 8080      # host comment\n  https_port: 8540\n",
			op:   setPort("http_port", 8081),
			// BASELINE BUG: editRoot re-encodes the ENTIRE document. Every blank
			// line between sections is stripped and the 6-space gap before
			// `# host comment` collapses to one space — port-heal silently
			// reformats a file the user committed by hand.
			want:  "schema: v1alpha1\n# a header comment\nclient:\n  name: acme\ncluster:\n  name: acme-local\n  http_port: 8081 # host comment\n  https_port: 8540\n",
			notes: "BASELINE BUG: SetClusterPort reformats the whole document",
		},
		{
			name: "set_version_reformats_whole_doc",
			in:   "schema: v1alpha1\n\nflywheel:\n  version: v0.1.0    # pinned tag\n\nclient:\n  name: acme\n",
			op:   setVersion("v0.2.0"),
			// BASELINE BUG: same whole-doc re-encode — blank lines gone, the
			// 4-space comment gap collapsed to one space.
			want:  "schema: v1alpha1\nflywheel:\n  version: v0.2.0 # pinned tag\nclient:\n  name: acme\n",
			notes: "BASELINE BUG: SetFlywheelVersion reformats the whole document",
		},
		{
			name: "set_port_appends_absent_key",
			in:   "schema: v1alpha1\ncluster:\n  name: acme-local\n  http_port: 8080\n",
			op:   setPort("https_port", 8540),
			want: "schema: v1alpha1\ncluster:\n  name: acme-local\n  http_port: 8080\n  https_port: 8540\n",
			notes: "absent key appended into the cluster block",
		},
		{
			name: "set_port_crlf_normalized_to_lf",
			in:   "schema: v1alpha1\r\n\r\ncluster:\r\n  http_port: 8080\r\n",
			op:   setPort("http_port", 8081),
			// BASELINE BUG: the whole-doc re-encode drops the \r from every line
			// and strips the blank line — a CRLF file is silently rewritten to LF.
			want:  "schema: v1alpha1\ncluster:\n  http_port: 8081\n",
			notes: "BASELINE BUG: CRLF normalized to LF and blank line stripped",
		},
		{
			name: "duplicate_cluster_key_edits_first",
			in:   "schema: v1alpha1\ncluster:\n  http_port: 8080\ncluster:\n  http_port: 9090\n",
			op:   setPort("http_port", 8081),
			// A malformed doc with duplicate `cluster:` keys: the first block's
			// port is set. (yaml.v3 keeps both keys; the editor targets the first.)
			want:  "schema: v1alpha1\ncluster:\n  http_port: 8081\ncluster:\n  http_port: 9090\n",
			notes: "duplicate cluster keys: first is edited",
		},
	})
}
