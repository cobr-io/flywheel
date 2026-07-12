// Package gitcheckout classifies the git layout `flywheel` is run from — a
// normal clone, a sibling linked worktree (supported), or a nested worktree
// (unsupported) — and resolves the extra host bind-mount a worktree needs so
// the in-cluster git-deploy-controller can read the checkout.
//
// A git linked worktree's `.git` is a *file* holding an absolute host path to
// the shared git dir (`gitdir: /Users/…/<mainrepo>/.git/worktrees/<id>`). Only
// `workspaces_root` is bind-mounted into k3d, so that path doesn't exist inside
// the container and every `git` operation the controller runs against
// `/workspaces/<repo>` fails — leaving the client-* Kustomizations stuck on
// "Source artifact not found" (issue #62). The fix is to also bind-mount the
// shared git dir at its host-absolute path so `git` resolves; this package
// resolves that path and flags the layouts flywheel can't support.
//
// Note: flywheel already uses "worktree" for an app's sibling checkout under
// workspaces_root (internal/cli/worktree). Here "worktree" means a *git linked
// worktree* — a different concept kept in a separate package on purpose.
package gitcheckout

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cobr-io/flywheel/internal/execx"
)

// AllowNestedEnv, when set to a non-empty value, bypasses the nested-worktree
// guard — an escape hatch for advanced setups whose apps live in-repo (so the
// broken sibling model doesn't matter). See Layout.Nested.
const AllowNestedEnv = "FLYWHEEL_ALLOW_NESTED_WORKTREE"

// Layout classifies the git layout a directory represents.
type Layout struct {
	Dir          string // the inspected dir (absolute)
	IsWorktree   bool   // .git is a file → a git linked worktree (not a normal clone)
	CommonDir    string // absolute host path of the shared git dir; "" for a normal clone
	MainWorktree string // absolute host path of the main working tree; "" for a normal clone
	Nested       bool   // the checkout lives INSIDE the main working tree (e.g. <repo>/.claude/worktrees/x)
}

// Inspect classifies dir's git layout. A normal clone (`.git` is a directory)
// returns IsWorktree=false and empty resolution fields. A linked worktree
// (`.git` is a file) resolves the shared git dir and the main working tree via
// `git`, and flags whether it is nested inside the main working tree.
//
// A dir with no `.git`, or one git can't resolve, is reported as a non-worktree
// with a nil error where possible (callers treat that as a clone) — resolution
// failures on a `.git` *file* return an error so callers can warn.
func Inspect(dir string) (Layout, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	l := Layout{Dir: abs}

	info, err := os.Lstat(filepath.Join(abs, ".git"))
	if err != nil || info.IsDir() {
		// No .git (not a repo / unreadable), or a real .git directory: a normal
		// clone as far as flywheel's mount model is concerned.
		return l, nil
	}

	// `.git` is a file (or symlink) → a linked worktree. Resolve via git.
	l.IsWorktree = true
	common, err := resolveCommonDir(abs)
	if err != nil {
		return l, fmt.Errorf("resolve git common-dir for %s: %w", abs, err)
	}
	l.CommonDir = common

	main, err := mainWorktree(abs)
	if err != nil {
		return l, fmt.Errorf("resolve main worktree for %s: %w", abs, err)
	}
	l.MainWorktree = main
	if main != "" {
		l.Nested = isNested(abs, main)
	}
	return l, nil
}

// isNested reports whether child lives at or under parent. Both sides are
// symlink-resolved first so a macOS /var → /private/var difference (or a
// symlinked checkout path) between filepath.Abs and git's recorded path doesn't
// hide the nesting.
func isNested(child, parent string) bool {
	c, p := resolveSymlinks(child), resolveSymlinks(parent)
	return c == p || strings.HasPrefix(c, p+string(filepath.Separator))
}

func resolveSymlinks(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// NestedRemediation is the guidance shown when flywheel is run from a nested
// git worktree.
func NestedRemediation(l Layout) string {
	return fmt.Sprintf(
		"%s is a git worktree nested inside %s.\n"+
			"flywheel bind-mounts the checkout's parent dir (%s) into the cluster as the "+
			"workspace root; for a nested worktree that lands inside another repo and "+
			"breaks flywheel's sibling-repo model.\n"+
			"Create the worktree at sibling level under your workspaces_root instead "+
			"(e.g. `git worktree add ../<repo>-<feature> <branch>`), or use a full clone.\n"+
			"To override this guard (advanced; only safe when your apps live in-repo), set %s=1.",
		l.Dir, l.MainWorktree, filepath.Dir(l.Dir), AllowNestedEnv)
}

// UnreachableCommonDirRemediation is the guidance shown when a worktree's shared
// git dir did not bind-mount into the cluster (e.g. it lives on a host path the
// cluster VM doesn't share).
func UnreachableCommonDirRemediation(l Layout) string {
	return fmt.Sprintf(
		"the git worktree's shared git dir (%s) did not bind-mount into k3d, so the "+
			"in-cluster git-deploy-controller can't read your checkout — the client-* "+
			"Kustomizations would never find their source.\n"+
			"This happens when the main repo lives on a path the cluster VM doesn't share "+
			"(e.g. /tmp, /var/folders). Move the main repo under your home directory, or run "+
			"flywheel from a full clone instead of a worktree.",
		l.CommonDir)
}

// resolveCommonDir returns the absolute host path of the shared git dir for a
// worktree checkout. Prefers `--path-format=absolute` (git ≥ 2.31); falls back
// to joining a relative `--git-common-dir` onto dir for older git.
func resolveCommonDir(dir string) (string, error) {
	if out, err := gitOutput(dir, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
		if p := strings.TrimSpace(out); filepath.IsAbs(p) {
			return filepath.Clean(p), nil
		}
	}
	out, err := gitOutput(dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(out)
	if p == "" {
		return "", fmt.Errorf("git returned an empty common-dir")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p), nil
}

// mainWorktree returns the absolute path of the main working tree — the first
// entry of `git worktree list --porcelain`.
func mainWorktree(dir string) (string, error) {
	out, err := gitOutput(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "worktree "); ok {
			return filepath.Clean(strings.TrimSpace(rest)), nil
		}
	}
	return "", nil
}

func gitOutput(dir string, args ...string) (string, error) {
	// TODO: thread a context once Inspect takes one (it is called from `up`'s
	// preflight, which would cascade the signature change beyond this package).
	return execx.Git(context.Background(), dir, args...)
}
