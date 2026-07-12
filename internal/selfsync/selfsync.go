// Package selfsync is the dev-loop's self/gitops sync controller: it owns the
// git-auto-sync-self path end-to-end, replacing the sync.sh sidecar for the
// gitops repo only (per-app git-auto-sync sidecars keep running sync.sh).
//
// Each tick it:
//  1. pushes the host worktree's AUTHORED branch → the in-cluster bare repo
//     (guarded: only when the worktree advanced);
//  2. when AUTHORED advanced (or on the first tick), rebuilds the DEPLOY branch
//     = AUTHORED + the IUA's image-bump commits via deploybranch.Maintainer,
//     with the ImageUpdateAutomation suspended around the rebuild so a bump can't
//     land mid-rebase;
//  3. pokes Flux (GitRepository, then — once the artifact advances — the apps
//     Kustomization) so the change rolls promptly.
//
// Idle ticks (worktree unchanged) do no Kubernetes work, preserving the
// idle-CPU behaviour the dev loop already tuned (issue #6). The IUA's own bumps
// are left for Flux to pick up on its interval; this loop only acts when AUTHORED
// moves, carrying any accumulated bumps forward on the next rebuild.
//
// See docs/designs/2026-06-28-isolate-dev-loop-image-bumps-design.md.
package selfsync

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cobr-io/flywheel/internal/deploybranch"
	"github.com/cobr-io/flywheel/internal/execx"
)

// Flux abstracts the Kubernetes-side operations the loop needs. The production
// implementation patches Flux objects via a controller-runtime client; tests
// fake it. Mirrors what sync.sh did with kubectl.
type Flux interface {
	// ConfiguredAuthored returns the AUTHORED branch the operator selected with
	// `flywheel use` (the deploy-branch annotation, naming.DeployBranchAnnotation,
	// on the self GitRepository), or "" when unset.
	ConfiguredAuthored(ctx context.Context) (string, error)
	// SuspendIUA sets spec.suspend on the ImageUpdateAutomation.
	SuspendIUA(ctx context.Context, suspend bool) error
	// PokeGitRepository bumps the self GitRepository's reconcile-request
	// annotation so source-controller fetches now.
	PokeGitRepository(ctx context.Context) error
	// WaitArtifact blocks (bounded) until the GitRepository's stored artifact
	// revision contains targetSHA, so the Kustomization doesn't reconcile a stale
	// source. Best-effort: a timeout is not fatal.
	WaitArtifact(ctx context.Context, targetSHA string) error
	// PokeKustomization bumps the apps Kustomization's reconcile-request
	// annotation so it applies the new revision now.
	PokeKustomization(ctx context.Context) error
}

// Loop is the self-sync control loop. Construct with a worktree, a DEPLOY
// maintainer bound to the same bare repo, and a Flux implementation.
type Loop struct {
	Worktree      *Worktree
	Deploy        *deploybranch.Maintainer
	Flux          Flux
	DefaultBranch string // AUTHORED fallback when no branch is configured (the integration branch, from flywheel-config)
	PollInterval  time.Duration
	Logf          func(string, ...any) // optional

	seeded         bool   // DEPLOY has been established at least once this process lifetime
	deployedBranch string // the AUTHORED branch DEPLOY was last built from (to detect `flywheel use` switches)
}

// TickResult reports what a single Tick did.
type TickResult struct {
	Authored string              // the branch treated as AUTHORED this tick
	Pushed   bool                // the worktree's AUTHORED advanced and was pushed to bare
	Rebuilt  bool                // the DEPLOY rebuild path ran (seed or AUTHORED-advance)
	Deploy   deploybranch.Result // the maintainer result when Rebuilt
}

// Run ticks every PollInterval until ctx is cancelled. Tick errors are logged
// and retried on the next tick rather than aborting the loop.
func (l *Loop) Run(ctx context.Context) error {
	t := time.NewTicker(l.PollInterval)
	defer t.Stop()
	for {
		if _, err := l.Tick(ctx); err != nil {
			l.logf("tick error: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Tick performs one synchronisation pass.
func (l *Loop) Tick(ctx context.Context) (TickResult, error) {
	authored, err := l.authoredBranch(ctx)
	if err != nil {
		return TickResult{}, err
	}

	pushed, _, err := l.Worktree.PushAuthored(ctx, authored)
	if err != nil {
		return TickResult{}, fmt.Errorf("push authored %q: %w", authored, err)
	}

	res := TickResult{Authored: authored, Pushed: pushed}

	// Rebuild DEPLOY when there's something to rebuild from: the first tick
	// (seed), after the worktree advanced (rebase-forward), or after a `flywheel
	// use` switched the selected branch (reset — the old bumps belong to a
	// different branch). An idle tick with the IUA's own bumps needs no rebuild —
	// they already satisfy DEPLOY = AUTHORED + bumps; the next advance carries
	// them forward.
	switched := l.seeded && authored != l.deployedBranch
	if !l.seeded || pushed || switched {
		l.Deploy.Authored = authored

		// Suspend the IUA so a concurrent bump can't be clobbered by the rebuild.
		if err := l.Flux.SuspendIUA(ctx, true); err != nil {
			l.logf("suspend IUA: %v", err)
		}
		// A branch switch resets DEPLOY to the new AUTHORED (discarding the old
		// branch's bumps); a seed/advance rebases the bump layer forward.
		var dres deploybranch.Result
		var rerr error
		if switched {
			dres, rerr = l.Deploy.ResetToAuthored(ctx)
		} else {
			dres, rerr = l.Deploy.Reconcile(ctx)
		}
		// Always attempt to resume, even if the rebuild failed.
		if err := l.Flux.SuspendIUA(ctx, false); err != nil {
			l.logf("resume IUA: %v", err)
		}
		if rerr != nil {
			return res, fmt.Errorf("rebuild deploy branch: %w", rerr)
		}
		l.seeded = true
		l.deployedBranch = authored
		res.Rebuilt = true
		res.Deploy = dres

		if dres.Changed {
			switch {
			case dres.Seeded:
				l.logf("seeded DEPLOY %q = %s @ %s", l.Deploy.Deploy, authored, short(dres.DeploySHA))
			case dres.Reset:
				l.logf("switched DEPLOY %q to %s @ %s (IUA will re-bump)", l.Deploy.Deploy, authored, short(dres.DeploySHA))
			case dres.ResetFallback:
				l.logf("reset DEPLOY %q onto %s @ %s (rebase conflict; IUA will re-bump)", l.Deploy.Deploy, authored, short(dres.DeploySHA))
			default:
				l.logf("rebuilt DEPLOY %q = %s + bumps @ %s", l.Deploy.Deploy, authored, short(dres.DeploySHA))
			}
			if err := l.Flux.PokeGitRepository(ctx); err != nil {
				return res, fmt.Errorf("poke gitrepository: %w", err)
			}
			if err := l.Flux.WaitArtifact(ctx, dres.DeploySHA); err != nil {
				l.logf("wait artifact %s: %v", short(dres.DeploySHA), err) // non-fatal
			}
			if err := l.Flux.PokeKustomization(ctx); err != nil {
				return res, fmt.Errorf("poke kustomization: %w", err)
			}
		}
	}
	return res, nil
}

// authoredBranch resolves the branch to treat as AUTHORED: the configured
// (flywheel use) branch when it exists in the worktree, else the default. This
// is the AUTO_FOLLOW_BRANCH=false model — the loop tracks the deliberately
// chosen branch, not whatever the worktree transiently has checked out.
func (l *Loop) authoredBranch(ctx context.Context) (string, error) {
	cfg, err := l.Flux.ConfiguredAuthored(ctx)
	if err != nil {
		return "", fmt.Errorf("read configured authored branch: %w", err)
	}
	if cfg != "" && cfg != l.DefaultBranch {
		if l.Worktree.HasLocalBranch(ctx, cfg) {
			return cfg, nil
		}
		l.logf("configured branch %q absent in worktree; using default %q", cfg, l.DefaultBranch)
	}
	return l.DefaultBranch, nil
}

func (l *Loop) logf(format string, args ...any) {
	if l.Logf != nil {
		l.Logf(format, args...)
	}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// Worktree wraps the git operations on the host worktree (a hostPath mount of
// the developer's gitops checkout) needed to mirror AUTHORED into the bare repo.
type Worktree struct {
	Dir     string // worktree path in the container (e.g. /workspaces/<repo>)
	BareURL string // in-cluster bare repo URL (push target)
}

// HasLocalBranch reports whether refs/heads/<branch> exists in the worktree.
func (w *Worktree) HasLocalBranch(ctx context.Context, branch string) bool {
	return w.run(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+branch) == nil
}

// PushAuthored pushes the worktree's local <branch> to the bare repo's <branch>,
// but only when it has advanced past what the bare repo already has (the
// push-guard, to avoid a smart-HTTP negotiation every idle tick). Returns
// changed=true only when a push happened. Uses --force-with-lease (with a plain
// fallback for a brand-new branch or a lease miss); git hooks are disabled.
func (w *Worktree) PushAuthored(ctx context.Context, branch string) (changed bool, head string, err error) {
	local, err := w.output(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	local = strings.TrimSpace(local)
	if local == "" {
		return false, "", fmt.Errorf("authored branch %q is not present in worktree %s", branch, w.Dir)
	}
	bare := w.remoteHead(ctx, branch)
	if local == bare {
		return false, local, nil // already in sync — push-guard skips
	}
	if bare == "" {
		// brand-new branch on the bare repo — plain create.
		if perr := w.push(ctx, branch, ""); perr != nil {
			return false, local, perr
		}
		return true, local, nil
	}
	if perr := w.push(ctx, branch, bare); perr != nil {
		// lease miss / non-ff the developer rebased — retry without the lease.
		if perr := w.push(ctx, branch, ""); perr != nil {
			return false, local, perr
		}
	}
	return true, local, nil
}

// push runs `git push [--force-with-lease=branch:expect] BareURL branch:branch`
// with the developer's git hooks disabled.
func (w *Worktree) push(ctx context.Context, branch, expect string) error {
	args := []string{"-c", "core.hooksPath=/dev/null", "push"}
	if expect != "" {
		args = append(args, "--force-with-lease="+branch+":"+expect)
	}
	args = append(args, w.BareURL, branch+":"+branch)
	return w.run(ctx, args...)
}

// remoteHead returns the bare repo's current head for branch, or "" if absent.
func (w *Worktree) remoteHead(ctx context.Context, branch string) string {
	out, err := w.output(ctx, "ls-remote", w.BareURL, "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func (w *Worktree) run(ctx context.Context, args ...string) error {
	_, err := execx.GitAuto(ctx, w.Dir, args...)
	return err
}

func (w *Worktree) output(ctx context.Context, args ...string) (string, error) {
	return execx.GitAuto(ctx, w.Dir, args...)
}
