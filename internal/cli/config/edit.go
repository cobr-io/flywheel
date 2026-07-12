package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/yamledit"
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
//
// The edit is surgical: only the port token on its own line changes, so a file
// the user committed by hand is not reformatted by port-heal (the old whole-doc
// re-encode stripped blank lines and collapsed comment spacing).
func SetClusterPort(path, key string, port int) error {
	return editScalar(path, []string{"cluster", key}, "!!int", fmt.Sprintf("%d", port))
}

// SetFlywheelVersion sets flywheel.version to version in the YAML file at path,
// preserving comments (notably the inline `flywheel.version` tag comment) and
// unrelated content. The flywheel: node is created if somehow absent. Used by
// `flywheel up`'s version-drift gate to roll the pin forward to the installed
// binary's release.
func SetFlywheelVersion(path, version string) error {
	return editScalar(path, []string{"flywheel", "version"}, "!!str", version)
}

// editScalar reads path, applies a single surgical scalar set via yamledit, and
// writes it back — the shared body of SetClusterPort/SetFlywheelVersion.
func editScalar(path string, keyPath []string, tag, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := yamledit.SetScalar(data, keyPath, tag, value)
	if err != nil {
		return fmt.Errorf("edit %s: %w", path, err)
	}
	return os.WriteFile(path, out, 0o644)
}

// editWorkspaceRepos hands fn the workspace.repos sequence node (created if
// absent) within the YAML document at path, then writes back a SURGICAL edit via
// yamledit.EditBlock: only the `workspace:` block's own lines are re-rendered;
// every other byte of the file — blank lines between sections, comments
// (including the commented-out `# workspace:` template the skeleton ships), and
// sibling sections — is left untouched. This is what keeps `flywheel add app`'s
// diff to just the workspace entry instead of reformatting the whole
// flywheel.yaml (issue #37).
func editWorkspaceRepos(path string, fn func(repos *yaml.Node) error) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := yamledit.EditBlock(data, "workspace", func(ws *yaml.Node) error {
		repos := mapEnsure(ws, "repos", yaml.SequenceNode, "!!seq")
		return fn(repos)
	})
	if err != nil {
		return fmt.Errorf("edit %s: %w", path, err)
	}
	return os.WriteFile(path, out, 0o644)
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
