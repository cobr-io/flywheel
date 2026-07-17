// Package appsync is the per-app worktree<->bare-repo sync engine driving
// cmd/git-auto-sync. It replaces the per-app git-auto-sync sidecar
// (scripts/git-auto-sync/sync.sh)'s bash loop with the race-free tick
// described in docs/designs/2026-07-17-per-app-sync-controller-design.md:
// every decision is made against the checked-out branch's ref
// (refs/heads/<B>) snapshotted once up front, never against a live re-read of
// HEAD, so a `git checkout` landing mid-tick can't apply a stale branch's
// decisions to a worktree that has already moved on.
//
// This file is Phase 1 scaffolding only (see docs/plans/2026-07-17-per-app-sync-
// controller-plan.md): Tick is not yet implemented. Phase 2 replaces its body
// with the full snapshot / branch-follow / fetch / integrate / push / poke
// algorithm plus the deterministic race-injection tests.
//
// Shares selfsync's idioms (explicit refs, push-guard, shell-out runner) but
// is a separate package — internal/selfsync (the gitops/self path) is not
// modified; the per-app product behavior (auto-follow, bidirectional sync)
// needs its own race-free redesign (design § Problem).
package appsync

import (
	"context"
	"fmt"
	"time"

	"github.com/cobr-io/flywheel/internal/execx"
)

// defaultExecTimeout bounds every git exec when Ticker.ExecTimeout is unset,
// so one hung git call against one app's worktree can't wedge the worker
// slot serving it (MAX_CONCURRENT workers serve every app; see design §
// Hygiene / permissions).
const defaultExecTimeout = 30 * time.Second

// FluxPatcher abstracts the Kubernetes-side Flux GitRepository operations a
// Ticker needs. The production implementation patches the GR via a
// controller-runtime client (wired into the Reconciler, Phase 3); tests fake
// it. Mirrors what sync.sh did with kubectl annotate/patch.
type FluxPatcher interface {
	// EnsureBranch makes the GitRepository track branch: it first sets the
	// kustomize.toolkit.fluxcd.io/reconcile=disabled annotation (so Flux's own
	// reconciler can't race the patch by re-applying a stale spec.ref.branch
	// from the source manifest), then patches spec.ref.branch = branch.
	// Called only on branch-follow, i.e. when the tracked branch and the
	// worktree's checked-out branch disagree (design step 2).
	EnsureBranch(ctx context.Context, branch string) error
	// PokeReconcile bumps reconcile.fluxcd.io/requestedAt
	// (naming.ReconcileRequestAnnotation) on the GitRepository so
	// source-controller fetches now instead of waiting out the poll interval.
	// Called whenever the bare repo's head changed this tick — our push, or a
	// fast-forward we integrated (design step 6).
	PokeReconcile(ctx context.Context) error
}

// Ticker performs one race-free sync pass for one app worktree.
type Ticker struct {
	Dir     string      // worktree path, e.g. <WORKSPACES_MOUNT>/<app>
	BareURL string      // in-cluster bare repo URL (push/fetch target)
	Flux    FluxPatcher // branch patch + reconcile poke

	// ExecTimeout bounds every git exec this Ticker runs (see
	// defaultExecTimeout); zero uses the default.
	ExecTimeout time.Duration

	Logf func(string, ...any) // optional
}

// TickResult reports what a single Tick did. Phase 1 stub: Tick always
// returns the zero value. Phase 2 populates it per the design's tick
// algorithm (the checked-out branch observed, whether the branch-follow patch
// ran, whether the worktree was pushed or fast-forwarded, and whether the
// tick stalled on a rebase conflict).
type TickResult struct {
	Branch string // checked-out branch B this tick observed, or "" if detached/empty (tick skipped)
}

// Tick performs one race-free sync pass against trackedBranch, the GR's
// current spec.ref.branch. Phase 1 stub: not yet implemented — Phase 2
// replaces this body with the design's snapshot / branch-follow / fetch /
// integrate / push / poke algorithm.
func (t *Ticker) Tick(ctx context.Context, trackedBranch string) (TickResult, error) {
	return TickResult{}, fmt.Errorf("appsync: Tick not implemented (phase 2)")
}

// run/output duplicate internal/selfsync's Worktree helpers of the same name
// (~40 lines) rather than sharing them: extraction would touch selfsync in a
// PR that otherwise doesn't (design open question Q2). Unlike selfsync's
// (which run under the caller's own context), every exec here is wrapped in
// its own ExecTimeout-bounded context — see defaultExecTimeout.

// run executes `git args...` in t.Dir under execx's automation identity
// (GitAuto: prompts disabled, pinned committer), discarding stdout.
func (t *Ticker) run(ctx context.Context, args ...string) error {
	_, err := t.output(ctx, args...)
	return err
}

// output executes `git args...` in t.Dir and returns stdout, bounded by
// t.ExecTimeout (default defaultExecTimeout).
func (t *Ticker) output(ctx context.Context, args ...string) (string, error) {
	timeout := t.ExecTimeout
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return execx.GitAuto(cctx, t.Dir, args...)
}

func (t *Ticker) logf(format string, args ...any) {
	if t.Logf != nil {
		t.Logf(format, args...)
	}
}
