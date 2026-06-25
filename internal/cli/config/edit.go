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

// editWorkspaceRepos loads path as a yaml.Node, hands fn the workspace.repos
// sequence node (created if absent), and writes the document back with 2-space
// indentation (matching the skeleton's flywheel.yaml). Comments survive the
// round-trip; blank lines between top-level sections may be normalised.
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
	ws := mapEnsure(root, "workspace", yaml.MappingNode, "!!map")
	repos := mapEnsure(ws, "repos", yaml.SequenceNode, "!!seq")
	if err := fn(repos); err != nil {
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

func scalar(tag, val string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: val}
}
