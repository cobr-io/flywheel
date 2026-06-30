package config

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/cobr-io/flywheel/internal/cli/schema"
)

// UpsertWorkspaceRepo inserts or replaces (by name) the workspace.repos entry
// for repo in the YAML file at path, preserving comments and unrelated content.
// The workspace:/repos: nodes are created if absent. Used by `flywheel add app`.
func UpsertWorkspaceRepo(path string, repo schema.WorkspaceRepo) error {
	return editWorkspaceRepos(path, func(repos *yaml.Node) error {
		entry := repoNode(repo)
		for i, child := range repos.Content {
			if nodeMapValue(child, "name") == repo.Name {
				repos.Content[i] = entry // replace in place (preserves position)
				return nil
			}
		}
		repos.Content = append(repos.Content, entry)
		return nil
	})
}

// SetWorkspaceRepoURL flips the workspace.repos entry for worktree from
// local_only to a remote URL, preserving its position. It errors when no entry
// declares worktree. Used by `flywheel publish-app`.
func SetWorkspaceRepoURL(path, worktree, url string) error {
	return editWorkspaceRepos(path, func(repos *yaml.Node) error {
		for _, child := range repos.Content {
			if nodeMapValue(child, "name") == worktree {
				setEntryURL(child, url)
				return nil
			}
		}
		return fmt.Errorf("no workspace.repos entry for worktree %q in %s", worktree, path)
	})
}

// SetClusterPort sets cluster.<key> to port in the YAML file at path,
// preserving comments and unrelated content. key is one of "registry_port",
// "http_port", "https_port". The cluster: node is created if somehow absent.
// Used by `flywheel up`'s host-port healing to persist a reallocated port
// back to flywheel.yaml so it stays stable on the next up.
func SetClusterPort(path, key string, port int) error {
	return editRoot(path, func(root *yaml.Node) error {
		cluster := mapEnsure(root, "cluster", yaml.MappingNode, "!!map")
		mapSetScalar(cluster, key, "!!int", fmt.Sprintf("%d", port))
		return nil
	})
}

// SetFlywheelVersion sets flywheel.version to version in the YAML file at path,
// preserving comments (notably the inline `flywheel.version` tag comment) and
// unrelated content. The flywheel: node is created if somehow absent. Used by
// `flywheel up`'s version-drift gate to roll the pin forward to the installed
// binary's release.
func SetFlywheelVersion(path, version string) error {
	return editRoot(path, func(root *yaml.Node) error {
		fw := mapEnsure(root, "flywheel", yaml.MappingNode, "!!map")
		mapSetScalar(fw, "version", "!!str", version)
		return nil
	})
}

// editWorkspaceRepos hands fn the workspace.repos sequence node (created if
// absent) within the YAML document at path, then writes back a SURGICAL edit:
// only the `workspace:` block's own lines are re-rendered; every other byte of
// the file — blank lines between sections, comments (including the commented-out
// `# workspace:` template the skeleton ships), and sibling sections — is left
// untouched. This is what keeps `flywheel add app`'s diff to just the workspace
// entry instead of reformatting the whole flywheel.yaml (issue #37). It cannot
// reuse editRoot, whose whole-document re-encode strips blank lines and re-wraps
// comments.
func editWorkspaceRepos(path string, fn func(repos *yaml.Node) error) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("%s: not a YAML document", path)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: top level is not a mapping", path)
	}

	// Find an existing real `workspace:` key (a commented-out template block is
	// not a node, so it never matches), or build a fresh mapping to append.
	wsIdx := -1
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "workspace" {
			wsIdx = i
			break
		}
	}
	var ws *yaml.Node
	var startLine, endLine int
	if wsIdx >= 0 {
		ws = root.Content[wsIdx+1]
		if ws.Kind != yaml.MappingNode {
			return fmt.Errorf("%s: workspace is not a mapping", path)
		}
		// Capture the existing block's source line span NOW, before fn mutates
		// the tree: replaced/appended nodes carry no source position (Line == 0),
		// so the span must be read off the freshly parsed nodes.
		startLine = root.Content[wsIdx].Line
		endLine = maxNodeLine(root.Content[wsIdx], ws)
	} else {
		ws = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	repos := mapEnsure(ws, "repos", yaml.SequenceNode, "!!seq")
	if err := fn(repos); err != nil {
		return err
	}

	block, err := marshalBlock("workspace", ws)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}

	var out []byte
	if wsIdx >= 0 {
		// Replace exactly the existing block's line span; everything before the
		// `workspace:` line and after the block's last content line is preserved
		// byte-for-byte (including the blank line that separates it from siblings).
		out = spliceLines(data, startLine, endLine, block)
	} else {
		out = appendBlock(data, block)
	}
	return os.WriteFile(path, out, 0o644)
}

// marshalBlock renders a single top-level `key:` mapping (key + val) to YAML
// text with the skeleton's 2-space indentation, producing exactly the bytes the
// surgical splice writes for the workspace block. It carries no document
// wrapper, so the output is just the block's lines with a trailing newline.
func marshalBlock(key string, val *yaml.Node) ([]byte, error) {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = []*yaml.Node{scalar("!!str", key), val}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// maxNodeLine returns the largest source line (1-based) reached by walking the
// given nodes and all their descendants — i.e. the last line the block occupies.
func maxNodeLine(nodes ...*yaml.Node) int {
	max := 0
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		if n.Line > max {
			max = n.Line
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	return max
}

// lineStarts returns the byte offset at which each line begins; index k holds
// the start of line k+1, so the start of line L is lineStarts(data)[L-1].
func lineStarts(data []byte) []int {
	starts := []int{0}
	for i, b := range data {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// spliceLines replaces the inclusive 1-based line range [startLine, endLine] of
// data with block (which ends in a newline), leaving all other bytes intact.
func spliceLines(data []byte, startLine, endLine int, block []byte) []byte {
	starts := lineStarts(data)
	startOff := starts[startLine-1]
	endOff := len(data)
	if endLine < len(starts) {
		endOff = starts[endLine] // first byte of the line after endLine
	}
	out := make([]byte, 0, startOff+len(block)+(len(data)-endOff))
	out = append(out, data[:startOff]...)
	out = append(out, block...)
	out = append(out, data[endOff:]...)
	return out
}

// appendBlock adds a fresh block to the end of data, separated from the existing
// content by exactly one blank line (the skeleton's inter-section spacing).
func appendBlock(data, block []byte) []byte {
	out := append([]byte(nil), data...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	if len(out) > 0 && !(len(out) >= 2 && out[len(out)-2] == '\n') {
		out = append(out, '\n') // ensure a blank line precedes the new section
	}
	return append(out, block...)
}

// editRoot loads path as a yaml.Node, hands fn the root mapping node, and
// writes the document back with 2-space indentation (matching the skeleton's
// flywheel.yaml). Comments survive the round-trip; blank lines between
// top-level sections may be normalised.
func editRoot(path string, fn func(root *yaml.Node) error) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("%s: not a YAML document", path)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: top level is not a mapping", path)
	}
	if err := fn(root); err != nil {
		return err
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// repoNode builds a mapping node for one workspace.repos entry: name, then
// exactly one of url / local_only (matching schema.Validate's invariant), then
// an optional branch (the clone-time checkout directive).
func repoNode(r schema.WorkspaceRepo) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	appendScalar(n, "name", "!!str", r.Name)
	if r.LocalOnly {
		appendScalar(n, "local_only", "!!bool", "true")
	} else {
		appendScalar(n, "url", "!!str", r.URL)
	}
	if r.Branch != "" {
		appendScalar(n, "branch", "!!str", r.Branch)
	}
	return n
}

// setEntryURL flips an entry to remote-backed: replace a local_only key in
// place with url (keeping its position and any line comment), or set url
// directly if the entry already has one, else append url.
func setEntryURL(entry *yaml.Node, url string) {
	for i := 0; i+1 < len(entry.Content); i += 2 {
		switch entry.Content[i].Value {
		case "local_only":
			entry.Content[i].Value = "url"
			entry.Content[i].Tag = "!!str"
			entry.Content[i+1] = scalar("!!str", url)
			return
		case "url":
			entry.Content[i+1] = scalar("!!str", url)
			return
		}
	}
	appendScalar(entry, "url", "!!str", url)
}

// mapEnsure returns the value node for key in mapping m, creating an empty node
// of the given kind/tag (and the key) when absent.
func mapEnsure(m *yaml.Node, key string, kind yaml.Kind, tag string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	v := &yaml.Node{Kind: kind, Tag: tag}
	m.Content = append(m.Content, scalar("!!str", key), v)
	return v
}

// nodeMapValue returns the scalar value of key in mapping m, or "" if absent.
func nodeMapValue(m *yaml.Node, key string) string {
	if m.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1].Value
		}
	}
	return ""
}

func appendScalar(m *yaml.Node, key, valTag, val string) {
	m.Content = append(m.Content, scalar("!!str", key), scalar(valTag, val))
}

// mapSetScalar sets key=val (with the given value tag) in mapping m. When key
// already exists its value node is mutated in place — preserving any line/head
// comment attached to it — rather than replaced. Appends a fresh pair when
// absent.
func mapSetScalar(m *yaml.Node, key, valTag, val string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1].Tag = valTag
			m.Content[i+1].Value = val
			return
		}
	}
	appendScalar(m, key, valTag, val)
}

func scalar(tag, val string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: val}
}
