// Package appsync is the per-app worktree<->bare-repo sync engine driving
// cmd/git-auto-sync. It replaces the per-app git-auto-sync sidecar
// (scripts/git-auto-sync/sync.sh)'s bash loop with the race-free tick
// described in docs/designs/2026-07-17-per-app-sync-controller-design.md:
// every decision is made against the checked-out branch's ref
// (refs/heads/<B>) snapshotted once up front, never against a live re-read of
// HEAD, so a `git checkout` landing mid-tick can't apply a stale branch's
// decisions to a worktree that has already moved on.
//
// Phase 2 (see docs/plans/2026-07-17-per-app-sync-controller-plan.md) implements
// Tick as the design's snapshot / branch-follow / fetch / integrate / push /
// poke algorithm, proven by deterministic race-injection tests. The Reconciler
// that resolves GitRepositories to Tickers is Phase 3.
//
// Shares selfsync's idioms (explicit refs, push-guard, shell-out runner) but
// is a separate package — internal/selfsync (the gitops/self path) is not
// modified; the per-app product behavior (auto-follow, bidirectional sync)
// needs its own race-free redesign (design § Problem).
package appsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
	// Branch is the checked-out branch B this tick observed, or "" when the
	// worktree is detached / on an unborn branch (tick skipped, no error).
	Branch string
	// Healed is set when heal_index_if_corrupt rebuilt a corrupt .git/index this
	// tick (issue #4).
	Healed bool
	// Followed is set when trackedBranch != B and the branch-follow patch was
	// attempted this tick. FollowErr carries a patch failure — the tick still
	// continues (sync.sh parity) so the bare repo converges and the reconciler
	// requeues normally; it is surfaced here for observability, not returned.
	Followed  bool
	FollowErr error
	// Pushed is set when the worktree's commits were pushed to the bare repo:
	// worktree-ahead, a successful divergence rebase, or a brand-new-branch
	// create.
	Pushed bool
	// Integrated is set when the bare repo was strictly ahead and the worktree
	// was fast-forwarded onto it (reset --hard R).
	Integrated bool
	// RolledBack is set when post-verify caught a checkout that won the
	// microsecond window before reset --hard and moved the wrong branch's ref:
	// that ref was restored from the step-1 snapshot and the tick aborted (no
	// push, no poke). Near-never in production; logged at WARNING.
	RolledBack bool
	// Stalled is set when a genuine divergence hit a rebase conflict: the rebase
	// was aborted, the worktree left pristine, and the reconciler should requeue
	// on the long stall interval (design step 7 / plan Q4). No push, no poke.
	Stalled bool
	// Poked is set when PokeReconcile ran because the bare head for B changed
	// this tick (our push, or a fast-forward we integrated). Never set on an
	// idle / skip / stall tick.
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

	// Rebuild a corrupt .git/index before any index-reading op (the integrate
	// path's dirty classification), so a transient corruption self-heals instead
	// of wedging the loop — run early, matching sync.sh's placement before the
	// branch-compare block (issue #4).
	if t.healIndexIfCorrupt(ctx) {
		res.Healed = true
	}

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

	// Step 3: fetch the bare repo's view of B. Objects only — never a refspec
	// that updates a local ref — so FETCH_HEAD is the only thing that moves and
	// no branch ref is rewritten out from under the snapshot.
	if err := t.run(ctx, "fetch", "--no-tags", t.BareURL, B); err != nil {
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
	R, err := t.revParse(ctx, "FETCH_HEAD")
	if err != nil {
		return res, fmt.Errorf("resolve FETCH_HEAD: %w", err)
	}

	// Step 4: compare the bare head R against L (the step-1 snapshot of B),
	// NEVER a re-read of HEAD. This is where sync.sh sampled HEAD and could
	// apply a stale branch's decision to a worktree a checkout had already moved.
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
	case t.isAncestor(ctx, L, R):
		// bare strictly ahead — integrate (the only worktree-mutating path).
		res, err = t.integrate(ctx, B, L, R, refsSnapshot, res)
		if err != nil {
			return res, err
		}
	default:
		// genuine divergence (neither ancestor) — rebase worktree on R.
		res, err = t.rebaseDivergence(ctx, B, R, res)
		if err != nil {
			return res, err
		}
	}

	return t.finish(ctx, res)
}

// integrate fast-forwards the worktree onto the strictly-ahead bare head R.
// This is the ONLY worktree-mutating path — the one where sync.sh's HEAD-sampled
// reset --hard poisoned the bare repo (issue #86) — so it is the guarded one:
// dirty-guard, re-verify the checked-out branch is still B, reset, then
// post-verify that the reset moved only refs/heads/B and roll back anything else.
// It never pushes and never pokes; success is reported via res.Integrated and
// the caller's poke rule fires. heal_index_if_corrupt already ran early in Tick.
func (t *Ticker) integrate(ctx context.Context, B, L, R string, snap map[string]string, res TickResult) (TickResult, error) {
	// Dirty classification. diff --quiet exits 0 clean / 1 changes / >1 index
	// error (issue #4).
	unstaged, err := t.diffQuietCode(ctx, "diff", "--quiet")
	if err != nil {
		return res, fmt.Errorf("classify worktree (unstaged): %w", err)
	}
	staged, err := t.diffQuietCode(ctx, "diff", "--cached", "--quiet")
	if err != nil {
		return res, fmt.Errorf("classify worktree (staged): %w", err)
	}
	if unstaged > 1 || staged > 1 {
		// Index unreadable this round — skip integrate; the next tick's heal
		// rebuilds it. Not a stall, just a retry (issue #4).
		t.logf("integrate %s: worktree index unreadable (git diff errored); skipping this tick", B)
		return res, nil
	}
	if unstaged != 0 || staged != 0 {
		// DATA-LOSS GUARD: reset --hard would silently discard the uncommitted
		// work. Skip the integrate AND everything downstream — were we to fall
		// through and push, --force-with-lease=B:R would rewind the bare repo
		// (at R) back over R's commits to L. Once the developer commits, the
		// worktree has a local commit ahead and the next tick integrates via the
		// dirty-safe divergence rebase instead. sync.sh parity (its `continue`).
		t.logf("integrate %s: bare advanced to %s but worktree has uncommitted changes; NOT hard-resetting (commit to integrate)", B, short(R))
		return res, nil
	}

	// Re-verify the checked-out branch is STILL B immediately before mutating.
	// A checkout that landed between the snapshot and here is caught now → no
	// reset, so the wrong branch is never touched (design step 5c).
	if cur, err := t.symbolicRef(ctx); err != nil || cur != B {
		t.logf("integrate %s: worktree switched to %q before reset; skipping integrate", B, cur)
		return res, nil
	}

	// Fast-forward the worktree onto the bare head.
	if err := t.run(ctx, "reset", "--hard", R); err != nil {
		return res, fmt.Errorf("reset --hard %s: %w", short(R), err)
	}

	// POST-VERIFY. reset --hard moves whatever branch HEAD points at and rewrites
	// the worktree; if a checkout won the microsecond window after the re-verify,
	// it moved the WRONG branch. The only ref allowed to differ from the step-1
	// snapshot is refs/heads/B (now == R). Restore any other moved ref from the
	// snapshot and abort — this is the guard that makes the issue-#86 bare poison
	// impossible, so it logs loudly if it ever fires.
	cur, curErr := t.symbolicRef(ctx)
	after, err := t.snapshotHeads(ctx)
	if err != nil {
		return res, fmt.Errorf("post-verify snapshot: %w", err)
	}
	var rolled []string
	for name, before := range snap {
		if name == B {
			continue // the one ref allowed to have moved (to R)
		}
		// reset --hard only MOVES an existing ref, never deletes one, so a
		// vanished ref is an unrelated external delete we must not fight.
		if now, ok := after[name]; ok && now != before {
			if uerr := t.run(ctx, "update-ref", "refs/heads/"+name, before); uerr != nil {
				t.logf("post-verify: WARNING failed to roll back refs/heads/%s %s->%s: %v", name, short(now), short(before), uerr)
			} else {
				t.logf("post-verify: WARNING refs/heads/%s moved to %s during reset (checkout race); rolled back to %s", name, short(now), short(before))
			}
			rolled = append(rolled, name)
		}
	}
	if curErr != nil || cur != B || len(rolled) > 0 {
		// A checkout raced the reset. Abort: no push, no poke. The next tick
		// re-runs cleanly on whatever branch is now checked out.
		res.RolledBack = len(rolled) > 0
		t.logf("integrate %s: aborted after reset — checkout raced (now on %q); no push/poke", B, cur)
		return res, nil
	}

	// The bare advanced without us pushing (a teammate/CI push straight to bare):
	// the fast-forward integrated cleanly.
	res.Integrated = true
	return res, nil
}

// pushExplicit pushes the explicit sha (NOT a ref name) to the bare repo's
// branch, so a state change after the step-1 snapshot cannot substitute content
// (design step 5 — the source refspec is the sha L captured up front, not
// whatever refs/heads/B happens to be by the time the push runs). expect is the
// bare head the force-with-lease guards against; "" pushes plain (a brand-new
// branch has no remote ref to lease). A lease miss or other failure retries
// plain, matching sync.sh's fallback. The developer's git hooks are disabled —
// they must not run in the container.
func (t *Ticker) pushExplicit(ctx context.Context, branch, sha, expect string) error {
	dst := sha + ":refs/heads/" + branch
	args := []string{"-c", "core.hooksPath=/dev/null", "push"}
	if expect != "" {
		args = append(args, "--force-with-lease="+branch+":"+expect)
	}
	args = append(args, t.BareURL, dst)
	if err := t.run(ctx, args...); err == nil {
		return nil
	} else if expect == "" {
		return err // already a plain push — nothing to fall back to
	}
	// Lease miss / brand-new remote ref (nothing to compare the lease against):
	// retry plain, which creates or force-updates the branch.
	return t.run(ctx, "-c", "core.hooksPath=/dev/null", "push", t.BareURL, dst)
}

// rebaseDivergence handles genuine divergence (neither L nor R is an ancestor of
// the other): replay the worktree's unique commits on top of R. A conflict means
// the developer touched the same lines an out-of-band bare update did — abort,
// log the resolve-manually instructions, and mark the tick stalled (the
// reconciler requeues on the long interval; sync.sh's 30s sleep). On success the
// worktree advanced to a new head on top of R; push that explicit sha (a
// fast-forward from R, leased against R). Never pokes — the caller's poke rule
// fires on res.Pushed.
func (t *Ticker) rebaseDivergence(ctx context.Context, B, R string, res TickResult) (TickResult, error) {
	// A checkout landing between the compare and here would make `git rebase`
	// replay onto the wrong branch; re-verify first (once it starts, git itself
	// refuses a checkout while a rebase is in progress).
	if cur, err := t.symbolicRef(ctx); err != nil || cur != B {
		t.logf("divergence %s: worktree switched to %q before rebase; skipping", B, cur)
		return res, nil
	}
	t.logf("rebasing %s on %s", B, short(R))
	if err := t.run(ctx, "rebase", R); err != nil {
		_ = t.run(ctx, "rebase", "--abort")
		t.logf("REBASE CONFLICT: %s and the bare repo diverge.", B)
		t.logf("  Resolve manually: cd %s && git pull --rebase %s %s", t.Dir, t.BareURL, B)
		res.Stalled = true
		return res, nil
	}
	// The rebase advanced refs/heads/B to a new head; read the NAMED ref (never
	// HEAD) and push that explicit sha — a fast-forward from R.
	head, err := t.revParse(ctx, "refs/heads/"+B)
	if err != nil {
		return res, fmt.Errorf("resolve %s after rebase: %w", B, err)
	}
	if err := t.pushExplicit(ctx, B, head, R); err != nil {
		t.logf("push %s after rebase: %v", B, err) // warn-and-continue, sync.sh parity
		return res, nil
	}
	res.Pushed = true
	return res, nil
}

// finish applies the poke rule and returns the tick result. Flux is poked
// exactly when the bare repo's head for B changed this tick — our push, or a
// fast-forward we integrated — and never on an idle, dirty-skip,
// re-verify-skip, rollback-abort or stall tick (design step 6). Genuine errors
// from earlier steps return directly and bypass this, so a failed tick never
// pokes.
func (t *Ticker) finish(ctx context.Context, res TickResult) (TickResult, error) {
	if res.Pushed || res.Integrated {
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
// redirect a decision nor hide a ref the post-verify must protect (design
// step 1 / step 5e).
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

// revParse resolves rev (e.g. FETCH_HEAD, refs/heads/<B>) to its full sha.
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

// healIndexIfCorrupt rebuilds a corrupt/unreadable .git/index from HEAD, port
// of sync.sh's heal_index_if_corrupt (issue #4). The worktree's .git is a host
// bind-mount written by both this container (root) and the developer; a
// concurrent or interrupted index write can truncate/garble .git/index, and git
// then aborts every index-reading op with "index file corrupt". Left unhealed
// that wedges the loop (the dirty guard misreads the error as uncommitted work).
// Rebuilding only rewrites .git/index from the committed tree — working-tree
// file *contents* are untouched. Best-effort: it never errors, so the tick
// continues and retries. Returns whether a rebuild was attempted.
func (t *Ticker) healIndexIfCorrupt(ctx context.Context) bool {
	// ls-files reads the index and nothing else; a clean index (nil err)
	// short-circuits. Only the specific corruption messages trigger a rebuild —
	// any other failure (e.g. the worktree isn't mounted yet) is left for the
	// tick's normal retry paths, exactly as sync.sh's case default.
	if _, err := t.output(ctx, "ls-files"); err == nil || !isIndexCorrupt(err) {
		return false
	}
	// Resolve the real index path (`--git-path` is correct even for linked
	// worktrees, where .git is a file) and delete it, then rebuild from HEAD.
	if idx, err := t.output(ctx, "rev-parse", "--git-path", "index"); err == nil {
		_ = os.Remove(strings.TrimSpace(idx))
	}
	if err := t.run(ctx, "reset", "-q"); err != nil {
		t.logf("heal index: rebuild failed (no commits yet?); will retry next tick: %v", err)
	} else {
		t.logf("heal index: .git/index was corrupt; rebuilt from HEAD (working-tree files preserved)")
	}
	return true
}

// isIndexCorrupt matches the exact git error strings sync.sh keyed on, so a
// corrupt index is healed but an unrelated ls-files failure is not.
func isIndexCorrupt(err error) bool {
	m := err.Error()
	return strings.Contains(m, "index file corrupt") ||
		strings.Contains(m, "index file smaller than expected") ||
		strings.Contains(m, "bad index file") ||
		strings.Contains(m, "unknown index")
}

// diffQuietCode runs a `git diff [--cached] --quiet` and returns its exit code:
// 0 (clean), 1 (real changes), or >1 (git error, e.g. an index too corrupt for
// this round's heal to have rebuilt). Preserving the >1 distinction is the
// issue-#4 fix: the old `! git diff --quiet` collapsed >1 into "dirty" and
// stalled forever on a transient corruption. A non-exit failure (context
// cancelled, git not runnable) is returned as an error for the caller to surface.
func (t *Ticker) diffQuietCode(ctx context.Context, args ...string) (int, error) {
	_, err := t.output(ctx, args...)
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
