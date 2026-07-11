package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cobr-io/flywheel/internal/cli/config"
	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/worktree"
	"github.com/cobr-io/flywheel/internal/naming"
)

// workspaceCheck reports the state of the workspace block (flywheel.yaml) versus
// what is actually on disk under workspaces_root. Read-only: it never clones,
// prompts, or mutates anything. The doctor framework is pass/fail (no warn
// level), so every actionable finding surfaces as one informational failure —
// full `flywheel doctor` does not gate `up`, and `up`'s own reconcile is what
// fixes missing siblings. Outside a flywheel repo it is a no-op (passes).
func workspaceCheck(repoDir string) Check {
	return Check{
		Name:        "workspace",
		Description: "declared sibling repos are present under workspaces_root",
		Run: func(ctx context.Context) error {
			cfg, err := loadConfigForWorkspace(repoDir)
			if err != nil {
				return nil // not in a flywheel repo / unreadable — nothing to report
			}
			wsRoot := cfg.Paths.WorkspacesRoot
			if wsRoot == "" {
				wsRoot = filepath.Dir(repoDir)
			}

			declared := make(map[string]bool, len(cfg.Workspace.Repos))
			var missingRemote, missingLocal, occupied []string
			for _, r := range cfg.Workspace.Repos {
				declared[r.Name] = true
				p := filepath.Join(wsRoot, r.Name)
				info, statErr := os.Stat(p)
				switch {
				case statErr != nil:
					if r.LocalOnly {
						missingLocal = append(missingLocal, r.Name)
					} else {
						missingRemote = append(missingRemote, r.Name)
					}
				case !info.IsDir() || !isGitRepo(p):
					occupied = append(occupied, r.Name)
				}
			}

			var undeclared []string
			if apps, aerr := worktree.DeclaredApps(repoDir); aerr == nil {
				for _, a := range apps {
					if a.Worktree != "" && !declared[a.Worktree] {
						undeclared = append(undeclared, fmt.Sprintf("%s→%s", a.Name, a.Worktree))
					}
				}
			}

			var msgs []string
			if len(missingRemote) > 0 {
				msgs = append(msgs, fmt.Sprintf("missing (clone with `flywheel up --clone`): %s", strings.Join(missingRemote, ", ")))
			}
			if len(missingLocal) > 0 {
				msgs = append(msgs, fmt.Sprintf("missing local-only (publish their source first): %s", strings.Join(missingLocal, ", ")))
			}
			if len(occupied) > 0 {
				msgs = append(msgs, fmt.Sprintf("path occupied by a non-git directory: %s", strings.Join(occupied, ", ")))
			}
			if len(undeclared) > 0 {
				msgs = append(msgs, fmt.Sprintf("app(s) referencing an undeclared worktree: %s", strings.Join(undeclared, ", ")))
			}
			if len(msgs) == 0 {
				return nil
			}
			return errors.New(strings.Join(msgs, "; "))
		},
	}
}

// isGitRepo reports whether dir looks like a git checkout (has a .git entry).
func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// loadConfigForWorkspace reads flywheel.yaml merged with flywheel.yaml.local and
// parses it (no Validate — doctor must work on an in-progress config too).
func loadConfigForWorkspace(repoDir string) (*schema.File, error) {
	committed, err := os.ReadFile(filepath.Join(repoDir, naming.ConfigFile))
	if err != nil {
		return nil, err
	}
	var local []byte
	if data, err := os.ReadFile(filepath.Join(repoDir, naming.ConfigFileLocal)); err == nil {
		local = data
	}
	merged, err := config.MergeYAML(committed, local)
	if err != nil {
		return nil, err
	}
	return schema.Parse(merged)
}
