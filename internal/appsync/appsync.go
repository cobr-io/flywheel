// Package appsync is the per-app worktree->bare-repo sync engine driving
// cmd/git-auto-sync. It replaces the per-app git-auto-sync sidecar
// (scripts/git-auto-sync/sync.sh)'s bash loop with the race-free tick
// described in docs/designs/2026-07-17-per-app-sync-controller-design.md:
// every decision is made against the checked-out branch's ref
// (refs/heads/<B>) snapshotted once up front, never against a live re-read of
// HEAD, so a `git checkout` landing mid-tick can't apply a stale branch's
// decisions to a worktree that has already moved on.
//
// Sync is one-directional, worktree -> bare (issue #161). Nothing but this
// controller writes an app's bare repo, so a bare head the worktree doesn't
// contain never means new work to pull: it means the developer rewrote
// history (reset, rebase, amend). The tick pushes that rewrite to the bare
// repo and never mutates the worktree. The design's integrate (reset --hard)
// and divergence-rebase paths silently undid such rewrites and are gone.
//
// Shares selfsync's idioms (explicit refs, push-guard, shell-out runner) but
// is a separate package — internal/selfsync (the gitops/self path) is not
// modified; the per-app product behavior (auto-follow) needs its own race-free
// redesign (design § Problem).
package appsync

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cobr-io/flywheel/internal/execx"
)

// defaultExecTimeout bounds every git exec when Ticker.ExecTimeout is unset,
// so one hung git call against one app's worktree can't wedge the worker
// slot serving it (MAX_CONCURRENT workers serve every app; see design §
// Hygiene / permissions).
const defaultExecTimeout = 30 * time.Second

// bareRefPrefix namespaces the worktree-local copy of each bare branch that a
// tick fetches (step 3). Outside refs/heads, so it is never a branch, never in
// the step-1 snapshot.
const bareRefPrefix = "refs/flywheel/bare/"

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
	// Called whenever this tick pushed, i.e. the bare repo's head changed
	// (design step 6).
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

	// testHook is a test-only injection point (nil in production), invoked at
	// the "post-snapshot" stage so race tests can land a `git checkout` after
	// the snapshot and before any decision.
	testHook func(stage string)
}

// TickResult reports what a single Tick did: the checked-out branch observed,
// whether the branch-follow patch ran, and whether the worktree was pushed.
type TickResult struct {
	// Branch is the checked-out branch B this tick observed, or "" when the
	// worktree is detached / on an unborn branch (tick skipped, no error).
	Branch string
	// Followed is set when trackedBranch != B and the branch-follow patch was
	// attempted this tick. FollowErr carries a patch failure — the tick still
	// continues (sync.sh parity) so the bare repo converges and the reconciler
	// requeues normally; it is surfaced here for observability, not returned.
	Followed  bool
	FollowErr error
	// Pushed is set when the worktree's branch was pushed to the bare repo:
	// worktree-ahead, a local history rewrite, or a brand-new-branch create.
	Pushed bool
	// Rewrote is set when the push replaced bare history the worktree no
	// longer contains (a local reset, rebase or amend), forced with a lease.
	Rewrote bool
	// Poked is set when PokeReconcile ran because this tick pushed. Never set
	// on an idle or skip tick.
	Poked bool
}

// Tick performs one race-free sync pass against trackedBranch, the GR's current
// spec.ref.branch (design "The race-free tick"). Every decision is made against
// L (the checked-out branch's ref, snapshotted once up front) and the step-1 ref
// snapshot, never a live re-read of HEAD — so a `git checkout` landing mid-tick
// can't redirect a decision onto a worktree that has already moved on.
func (t *Ticker) Tick(ctx context.Context, trackedBranch string) (TickResult, error) {
	var res TickResult

	// Step 1: snapshot. B is the checked-out branch; refsSnapshot is every
	// refs/heads/* name->sha at this instant; L is B's sha. A detached/empty
	// HEAD (symbolic-ref errors, or resolves to the literal "HEAD") or an unborn
	// B (no commit yet, so absent from refsSnapshot) has nothing to sync: skip
	// the tick with no error so the reconciler just requeues on the poll
	// interval — sync.sh's "detached or empty branch, skipping".
	B, err := t.symbolicRef(ctx)
	if err != nil || B == "" || B == "HEAD" {
		return res, nil
	}
	res.Branch = B

	refsSnapshot, err := t.snapshotHeads(ctx)
	if err != nil {
		return res, fmt.Errorf("snapshot refs/heads: %w", err)
	}
	L, ok := refsSnapshot[B]
	if !ok || L == "" {
		return res, nil
	}

	t.hook("post-snapshot")

	// Step 2: branch-follow. trackedBranch is the GR's live spec.ref.branch,
	// reconciled fresh each tick, so this single comparison subsumes sync.sh's
	// switch-detection AND its drift-correction (an external re-apply that
	// clobbered spec.ref.branch also lands here as trackedBranch != B — no
	// cached LAST_BRANCH state). A patch failure does NOT abort: sync.sh keeps
	// syncing and retries the patch next loop, so we log it, record it, and
	// press on — the reconciler still requeues normally.
	if trackedBranch != B {
		res.Followed = true
		if err := t.Flux.EnsureBranch(ctx, B); err != nil {
			t.logf("branch-follow: EnsureBranch(%s) failed; continuing sync, will retry: %v", B, err)
			res.FollowErr = err
		}
	}

	// Step 3: fetch the bare repo's view of B into a private ref outside
	// refs/heads, so no branch ref is rewritten out from under the snapshot.
	// Not FETCH_HEAD: this .git is the developer's, and git truncates
	// FETCH_HEAD when a fetch starts and appends when it ends, so a tick fetch
	// overlapping the developer's `git pull` left both entries in it — pull
	// failed ("Cannot rebase onto multiple branches") or this tick read the
	// developer's fetch as the bare head.
	bareRef := bareRefPrefix + B
	if err := t.run(ctx, "fetch", "--no-tags", "--no-write-fetch-head", t.BareURL, "+refs/heads/"+B+":"+bareRef); err != nil {
		// The branch is not in the bare repo yet (first push): a plain push of
		// the explicit sha L creates it. sync.sh parity, plus a poke (tail) so
		// Flux sees the branch's first appearance now.
		if perr := t.pushExplicit(ctx, B, L, ""); perr != nil {
			t.logf("create branch %s in bare: %v", B, perr)
			return res, nil
		}
		res.Pushed = true
		return t.finish(ctx, res)
	}
	R, err := t.revParse(ctx, bareRef)
	if err != nil {
		return res, fmt.Errorf("resolve %s: %w", bareRef, err)
	}

	// Step 4: compare the bare head R against L (the step-1 snapshot of B),
	// NEVER a re-read of HEAD. This is where sync.sh sampled HEAD and could
	// apply a stale branch's decision to a worktree a checkout had already moved.
	// The worktree is never mutated: this controller is the bare repo's only
	// writer, so R is always something the worktree once had (issue #161).
	switch {
	case L == R:
		// idle push-guard — already in sync, nothing to do (issue #6).
	case t.isAncestor(ctx, R, L):
		// worktree ahead of bare — push the explicit sha L, leasing against R.
		if err := t.pushExplicit(ctx, B, L, R); err != nil {
			t.logf("push %s: %v", B, err) // warn-and-continue, sync.sh parity
		} else {
			res.Pushed = true
		}
	default:
		// L behind R, or diverged from it: the developer rewrote history
		// (reset, rebase, amend). Force the bare repo to L, leasing against R.
		if err := t.pushExplicit(ctx, B, L, R); err != nil {
			t.logf("push rewritten %s: %v", B, err) // warn-and-continue
		} else {
			t.logf("%s rewritten locally (%s → %s); bare updated", B, short(R), short(L))
			res.Pushed = true
			res.Rewrote = true
		}
	}

	return t.finish(ctx, res)
}

// pushExplicit pushes the explicit sha (NOT a ref name) to the bare repo's
// branch, so a state change after the step-1 snapshot cannot substitute content
// (design step 5 — the source refspec is the sha L captured up front, not
// whatever refs/heads/B happens to be by the time the push runs). expect is the
// bare head the force-with-lease guards against; "" pushes plain (a brand-new
// branch has no remote ref to lease). A lease miss or other failure retries
// plain, matching sync.sh's fallback. Hook neutralization (pre-push must not
// run in the container) is inherited from output(), like every git exec here.
func (t *Ticker) pushExplicit(ctx context.Context, branch, sha, expect string) error {
	dst := sha + ":refs/heads/" + branch
	args := []string{"push"}
	if expect != "" {
		args = append(args, "--force-with-lease="+branch+":"+expect)
	}
	args = append(args, t.BareURL, dst)
	if err := t.run(ctx, args...); err == nil {
		return nil
	} else if expect == "" {
		return err // already a plain push — nothing to fall back to
	}
	// Lease miss / brand-new remote ref (nothing for the lease to compare
	// against): retry plain. A plain push only creates the branch or
	// fast-forwards it — a non-fast-forward is rejected by the remote, so this
	// fallback can never clobber the commits the lease was meant to protect.
	return t.run(ctx, "push", t.BareURL, dst)
}

// finish applies the poke rule and returns the tick result. Flux is poked
// exactly when this tick pushed, and never on an idle or skip tick (design
// step 6). Genuine errors from earlier steps return directly and bypass this,
// so a failed tick never pokes.
func (t *Ticker) finish(ctx context.Context, res TickResult) (TickResult, error) {
	if res.Pushed {
		t.poke(ctx, &res)
	}
	return res, nil
}

// poke asks Flux to reconcile now (the production FluxPatcher bumps
// reconcile.fluxcd.io/requestedAt — naming.ReconcileRequestAnnotation — on the
// GR). Best-effort: a failure just falls back to the poll interval, so it warns
// but never fails the tick (sync.sh's trigger_reconcile).
func (t *Ticker) poke(ctx context.Context, res *TickResult) {
	if err := t.Flux.PokeReconcile(ctx); err != nil {
		t.logf("poke reconcile: %v", err)
		return
	}
	res.Poked = true
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
// t.ExecTimeout (default defaultExecTimeout). Every invocation neutralizes the
// developer's git hooks (`-c core.hooksPath=/dev/null`): the worktree's .git is
// host-owned and this process runs as root in the container, so a hook firing
// here (pre-push on pushes)
// would execute host-authored code as root. sync.sh disabled hooks globally
// (`git config --global core.hooksPath /dev/null`); this is the same guarantee,
// applied per-exec.
func (t *Ticker) output(ctx context.Context, args ...string) (string, error) {
	timeout := t.ExecTimeout
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return execx.GitAuto(cctx, t.Dir, append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
}

func (t *Ticker) logf(format string, args ...any) {
	if t.Logf != nil {
		t.Logf(format, args...)
	}
}

// hook invokes the test-only injection point (nil in production) at the named
// stage. Race tests use it to land a `git checkout` at "post-snapshot"
// (snapshot taken, no decision yet).
func (t *Ticker) hook(stage string) {
	if t.testHook != nil {
		t.testHook(stage)
	}
}

// symbolicRef returns the checked-out branch via `symbolic-ref --short HEAD`.
// A detached HEAD makes the command exit non-zero (there is no symbolic ref);
// any failure returns an error and the caller skips the tick. Unlike a re-read
// of HEAD later in the tick, this is the ONE authoritative read of the branch
// name — every decision is made against the ref it names, snapshotted once.
func (t *Ticker) symbolicRef(ctx context.Context) (string, error) {
	out, err := t.output(ctx, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// snapshotHeads returns every refs/heads/* as name->sha. The tick decides
// against this snapshot, so a checkout that moves HEAD mid-tick can neither
// redirect a decision (design step 1).
func (t *Ticker) snapshotHeads(ctx context.Context) (map[string]string, error) {
	out, err := t.output(ctx, "for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	m := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue // empty (no branches) or malformed line
		}
		m[fields[0]] = fields[1]
	}
	return m, nil
}

// revParse resolves rev (e.g. refs/flywheel/bare/<B>, refs/heads/<B>) to its full sha.
func (t *Ticker) revParse(ctx context.Context, rev string) (string, error) {
	out, err := t.output(ctx, "rev-parse", rev)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// isAncestor reports whether a is an ancestor of b (fast-forward test). A git
// error (bad sha) is treated as "not an ancestor", matching sync.sh's plain
// `if git merge-base --is-ancestor ...`.
func (t *Ticker) isAncestor(ctx context.Context, a, b string) bool {
	return t.run(ctx, "merge-base", "--is-ancestor", a, b) == nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
