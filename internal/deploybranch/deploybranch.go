// Package deploybranch maintains the dev-loop DEPLOY branch in the in-cluster
// bare gitops repo, where DEPLOY = AUTHORED + the image-update-automation's
// ephemeral image-bump commits.
//
// It is the parity-preserving alternative to letting Flux's IUA commit bumps
// onto the developer's own (pushable) branch: the developer authors on AUTHORED;
// the IUA commits bumps onto DEPLOY; this package keeps DEPLOY = AUTHORED + bumps
// as AUTHORED advances, so authored history stays clean while Flux still rolls
// from a real, git-committed image tag (same git-commit IUA path as prod).
//
// All operations run against the in-cluster bare repo via an ordinary working
// clone; nothing here touches Kubernetes. The controller that wraps this
// (suspending the IUA around a rebuild, poking Flux to reconcile) owns those
// concerns. See docs/designs/2026-06-28-isolate-dev-loop-image-bumps-design.md.
package deploybranch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Maintainer keeps Deploy = Authored + bumps in the bare repo at RemoteURL,
// using a persistent working clone at WorkDir (created on first Reconcile).
type Maintainer struct {
	WorkDir   string // persistent working clone; created if absent
	RemoteURL string // the in-cluster bare repo (becomes git remote "origin")
	Authored  string // authored branch name (e.g. "main", "feat-x")
	Deploy    string // deploy branch name (e.g. "flywheel/local-deploy")
}

// Result describes what a single Reconcile did.
type Result struct {
	Seeded         bool   // Deploy created for the first time (= Authored tip)
	RebasedForward bool   // the bump layer was carried onto an advanced Authored
	ResetFallback  bool   // rebase conflicted → Deploy reset to Authored (IUA re-bumps)
	Changed        bool   // the Deploy tip moved as a result
	DeploySHA      string // the resulting Deploy tip
}

// committer identity used for rebased bump commits. Mirrors the IUA's author so
// the deploy branch reads as machine-generated. Only the committer is rewritten
// on rebase; original bump authorship is preserved.
const (
	committerName  = "flywheel-deploy"
	committerEmail = "image-automation@dev.local"
)

// Reconcile brings Deploy into the invariant Deploy = Authored + bumps:
//   - seeds Deploy = Authored when Deploy does not exist yet;
//   - no-ops when Deploy already sits directly on the current Authored tip;
//   - otherwise (Authored advanced) rebases the bump layer forward onto the new
//     Authored tip — and on conflict resets Deploy to Authored, leaving the IUA
//     to re-bump (the reset-and-rebump fallback).
//
// The caller is responsible for suspending the IUA around Reconcile so a bump
// can't land mid-rebuild.
func (m *Maintainer) Reconcile(ctx context.Context) (Result, error) {
	if err := m.ensureClone(ctx); err != nil {
		return Result{}, err
	}
	// Refresh every remote-tracking ref in one shot.
	if err := m.git(ctx, "fetch", "--force", "origin"); err != nil {
		return Result{}, fmt.Errorf("fetch origin: %w", err)
	}

	authoredSHA, ok, err := m.resolve(ctx, m.originRef(m.Authored))
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, fmt.Errorf("authored branch %q does not exist in %s", m.Authored, m.RemoteURL)
	}

	deploySHA, deployExists, err := m.resolve(ctx, m.originRef(m.Deploy))
	if err != nil {
		return Result{}, err
	}

	// Seed: no Deploy yet → Deploy = Authored.
	if !deployExists {
		if err := m.pushDeploy(ctx, authoredSHA); err != nil {
			return Result{}, err
		}
		return Result{Seeded: true, Changed: true, DeploySHA: authoredSHA}, nil
	}

	// If Authored is already the merge-base, Deploy sits directly on top of the
	// current Authored (Deploy = Authored + bumps) — nothing to do.
	base, err := m.mergeBase(ctx, authoredSHA, deploySHA)
	if err != nil {
		return Result{}, err
	}
	if base == authoredSHA {
		return Result{DeploySHA: deploySHA}, nil
	}

	// Authored advanced: carry the bump layer (base..Deploy) onto Authored.
	return m.rebaseForward(ctx, authoredSHA, base, deploySHA)
}

// rebaseForward replays the commits in base..deploySHA (the bump layer) onto
// authoredSHA. On success it force-updates Deploy to the rebased tip; on
// conflict it aborts and resets Deploy to authoredSHA (reset-and-rebump).
func (m *Maintainer) rebaseForward(ctx context.Context, authoredSHA, base, deploySHA string) (Result, error) {
	// Park a working branch at the current Deploy tip, then rebase its bump
	// layer onto Authored. --force discards any leftover working-tree state.
	if err := m.git(ctx, "checkout", "--force", "-B", workBranch, deploySHA); err != nil {
		return Result{}, fmt.Errorf("checkout deploy tip: %w", err)
	}
	if err := m.gitQuiet(ctx, "rebase", "--onto", authoredSHA, base, workBranch); err == nil {
		newSHA, _, err := m.resolve(ctx, "HEAD")
		if err != nil {
			return Result{}, err
		}
		if err := m.pushDeploy(ctx, newSHA); err != nil {
			return Result{}, err
		}
		return Result{RebasedForward: true, Changed: newSHA != deploySHA, DeploySHA: newSHA}, nil
	}
	// Conflict (the authored change touched the setter-line region) → fall back
	// to resetting Deploy onto Authored; the IUA re-bumps to the current image.
	_ = m.gitQuiet(ctx, "rebase", "--abort")
	if err := m.pushDeploy(ctx, authoredSHA); err != nil {
		return Result{}, err
	}
	return Result{ResetFallback: true, Changed: authoredSHA != deploySHA, DeploySHA: authoredSHA}, nil
}

// workBranch is the ephemeral local branch the rebuild operates on.
const workBranch = "flywheel-deploy-rebuild"

// ensureClone makes WorkDir a working clone of RemoteURL (origin), with a
// committer identity set for rebased commits. Idempotent.
func (m *Maintainer) ensureClone(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(m.WorkDir, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.WorkDir), 0o755); err != nil {
		return fmt.Errorf("mkdir for clone: %w", err)
	}
	if err := m.runGit(ctx, "", "clone", m.RemoteURL, m.WorkDir); err != nil {
		return fmt.Errorf("clone %s: %w", m.RemoteURL, err)
	}
	if err := m.git(ctx, "config", "user.name", committerName); err != nil {
		return err
	}
	return m.git(ctx, "config", "user.email", committerEmail)
}

// pushDeploy force-updates the Deploy branch on origin to sha.
func (m *Maintainer) pushDeploy(ctx context.Context, sha string) error {
	// The IUA is suspended by the caller during a rebuild, so this branch has a
	// single writer and a plain --force is safe (no lease needed).
	if err := m.git(ctx, "push", "--force", "origin", sha+":refs/heads/"+m.Deploy); err != nil {
		return fmt.Errorf("push deploy %q: %w", m.Deploy, err)
	}
	return nil
}

// resolve returns the commit SHA a ref points at, ok=false when the ref does not
// exist (not an error).
func (m *Maintainer) resolve(ctx context.Context, ref string) (sha string, ok bool, err error) {
	out, runErr := m.gitOutput(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	out = strings.TrimSpace(out)
	if out == "" {
		// --quiet makes a missing ref exit non-zero with empty output.
		return "", false, nil
	}
	if runErr != nil {
		return "", false, runErr
	}
	return out, true, nil
}

// mergeBase returns the best common ancestor of a and b.
func (m *Maintainer) mergeBase(ctx context.Context, a, b string) (string, error) {
	out, err := m.gitOutput(ctx, "merge-base", a, b)
	if err != nil {
		return "", fmt.Errorf("merge-base: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func (m *Maintainer) originRef(branch string) string { return "refs/remotes/origin/" + branch }

// --- git plumbing -----------------------------------------------------------

// git runs a git subcommand in WorkDir, surfacing combined output on failure.
func (m *Maintainer) git(ctx context.Context, args ...string) error {
	return m.runGit(ctx, m.WorkDir, args...)
}

// gitQuiet runs a git subcommand in WorkDir and returns only the error, without
// wrapping the (expected, e.g. rebase-conflict) output into it.
func (m *Maintainer) gitQuiet(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", m.WorkDir}, args...)...)
	cmd.Env = gitEnv()
	return cmd.Run()
}

// gitOutput runs a git subcommand in WorkDir and returns its stdout.
func (m *Maintainer) gitOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", m.WorkDir}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	return string(out), err
}

// runGit runs a git subcommand in dir (empty = process cwd), surfacing combined
// output on failure.
func (m *Maintainer) runGit(ctx context.Context, dir string, args ...string) error {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitEnv disables interactive prompts (the in-cluster bare repo is unauthent-
// icated http) and pins a committer identity so rebases never fail on a missing
// ident in a bare container.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_COMMITTER_NAME="+committerName,
		"GIT_COMMITTER_EMAIL="+committerEmail,
	)
}
