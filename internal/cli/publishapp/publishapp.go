// Package publishapp implements `flywheel publish-app <name>`: it promotes a
// local-only app to remote-backed once its worktree has been pushed to an
// external remote, by flipping the worktree's workspace entry in flywheel.yaml
// from local_only to the origin URL. After that the local-only guard no longer
// fires and the app may be merged to the integration branch.
package publishapp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cobr-io/flywheel/internal/cli/config"
	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/style"
	"github.com/cobr-io/flywheel/internal/cli/worktree"
	"github.com/cobr-io/flywheel/internal/naming"
)

// Options are the inputs to Run.
type Options struct {
	RepoDir string // client gitops repo; defaults to cwd
	Name    string // registered app name (builders/base/<name>)
	Stdout  io.Writer
}

func Run(opts Options) error {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.RepoDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts.RepoDir = wd
	}
	if opts.Name == "" {
		return errors.New("app name is required")
	}

	grPath := filepath.Join(opts.RepoDir, "builders", "base", opts.Name, "gitrepository.yaml")
	if _, err := os.Stat(grPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no app %q (expected %s)", opts.Name, filepath.Join("builders", "base", opts.Name))
		}
		return err
	}
	app, err := worktree.ParseAppGitRepository(grPath)
	if err != nil {
		return err
	}

	cfg, err := readConfig(opts.RepoDir)
	if err != nil {
		return err
	}

	// Idempotent: only a worktree declared local-only in the workspace block
	// has anything to publish.
	entry, ok := cfg.WorkspaceRepo(app.Worktree)
	switch {
	case !ok:
		style.Detail(opts.Stdout, "%s (worktree %q) is not declared in the workspace block; nothing to publish (run 'flywheel add app' to register it)", opts.Name, app.Worktree)
		return nil
	case !entry.LocalOnly:
		style.Detail(opts.Stdout, "%s is already remote-backed (source: %s); nothing to publish", opts.Name, entry.URL)
		return nil
	}

	wsRoot := cfg.Paths.WorkspacesRoot
	if wsRoot == "" {
		wsRoot = filepath.Dir(opts.RepoDir)
	}
	wtPath := filepath.Join(wsRoot, app.Worktree)
	if _, err := os.Stat(wtPath); err != nil {
		return fmt.Errorf("worktree %s not found; cannot verify it before publishing", wtPath)
	}

	// Publishing means "this app is now recoverable by others", so require a
	// reachable origin AND that the worktree's branch is actually pushed — a
	// reachable origin with unpushed commits would not make it recoverable.
	url, ok := worktree.GitRemoteURL(wtPath)
	if !ok {
		return fmt.Errorf("worktree %s has no 'origin' remote; add one and push the branch, then re-run", wtPath)
	}
	if !worktree.OriginReachable(wtPath) {
		return fmt.Errorf("origin (%s) is not reachable; check your access/network, then re-run", url)
	}
	pushed, err := worktree.BranchPushed(wtPath)
	if err != nil {
		return fmt.Errorf("verify the branch is pushed: %w", err)
	}
	if !pushed {
		return fmt.Errorf("the worktree's current branch isn't pushed to origin (%s); run 'git push' first, then re-run", url)
	}

	// Flip the workspace entry local_only → the origin URL.
	flywheelYAML := filepath.Join(opts.RepoDir, naming.ConfigFile)
	if err := config.SetWorkspaceRepoURL(flywheelYAML, app.Worktree, url); err != nil {
		return err
	}

	style.Summary(opts.Stdout, "published %s: source is now %s", opts.Name, url)
	return nil
}

// readConfig loads the merged flywheel.yaml(+.local) — we only need
// paths.workspaces_root, which lives in the .local overlay.
func readConfig(repoDir string) (*schema.File, error) {
	committed, err := os.ReadFile(filepath.Join(repoDir, naming.ConfigFile))
	if err != nil {
		return nil, fmt.Errorf("read flywheel.yaml: %w", err)
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
