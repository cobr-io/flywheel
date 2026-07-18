package appsync

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cobr-io/flywheel/internal/testgit"
)

// These tests exercise the tick against real on-disk git repos (the selfsync /
// deploybranch test style): a bare repo served as a plain filesystem path (git
// treats a path as a remote, so no HTTP server is needed) and a worktree cloned
// from it. A fake FluxPatcher records EnsureBranch / PokeReconcile. Nothing
// touches the network.

// --- fake FluxPatcher --------------------------------------------------------

type fakeFlux struct {
	ensured   []string // branches passed to EnsureBranch, in order
	ensureErr error    // returned by EnsureBranch when set
	pokes     int      // PokeReconcile call count
}

func (f *fakeFlux) EnsureBranch(_ context.Context, b string) error {
	f.ensured = append(f.ensured, b)
	return f.ensureErr
}

func (f *fakeFlux) PokeReconcile(context.Context) error { f.pokes++; return nil }

// --- git fixtures ------------------------------------------------------------

// bareRepo builds a bare repo with one commit on main carrying app.txt=content.
func bareRepo(t *testing.T, content string) string {
	t.Helper()
	seed := filepath.Join(t.TempDir(), "seed")
	testgit.Init(t, seed)
	writeFile(t, seed, "app.txt", content)
	testgit.Git(t, seed, "add", "-A")
	testgit.Git(t, seed, "commit", "-q", "-m", "init")
	bare := filepath.Join(t.TempDir(), "bare.git")
	testgit.Git(t, filepath.Dir(bare), "clone", "-q", "--bare", seed, bare)
	return bare
}

// cloneWorktree clones bare into a worktree with a committer identity, the way
// the container mounts a developer's checkout.
func cloneWorktree(t *testing.T, bare string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt")
	testgit.Git(t, filepath.Dir(wt), "clone", "-q", bare, wt)
	testgit.Git(t, wt, "config", "user.email", "dev@dev")
	testgit.Git(t, wt, "config", "user.name", "dev")
	return wt
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// advanceBare pushes a new commit to bare's <branch> via a throwaway clone,
// simulating a teammate or CI pushing straight to the bare repo. Returns the
// new sha.
func advanceBare(t *testing.T, bare, branch string, edit func(dir string)) string {
	t.Helper()
	c := filepath.Join(t.TempDir(), "adv")
	testgit.Git(t, filepath.Dir(c), "clone", "-q", bare, c)
	testgit.Git(t, c, "config", "user.email", "ci@ci")
	testgit.Git(t, c, "config", "user.name", "ci")
	testgit.Git(t, c, "checkout", "-q", "-B", branch, "origin/"+branch)
	edit(c)
	testgit.Git(t, c, "add", "-A") // stage new files too (the modes test adds one)
	testgit.Git(t, c, "commit", "-q", "-m", "ci change")
	testgit.Git(t, c, "push", "-q", "origin", branch+":refs/heads/"+branch)
	return testgit.Out(t, c, "rev-parse", "HEAD")
}

func bareRef(t *testing.T, bare, branch string) string {
	t.Helper()
	return testgit.Out(t, ".", "--git-dir="+bare, "rev-parse", "refs/heads/"+branch)
}

func localRef(t *testing.T, wt, branch string) string {
	t.Helper()
	return testgit.Out(t, wt, "rev-parse", "refs/heads/"+branch)
}

// localRefs snapshots every refs/heads/* in the worktree, for asserting that a
// tick moved only the refs it was allowed to.
func localRefs(t *testing.T, wt string) map[string]string {
	t.Helper()
	out := testgit.Out(t, wt, "for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads/")
	m := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			m[f[0]] = f[1]
		}
	}
	return m
}

func newTicker(t *testing.T, bare, wt string, flux FluxPatcher) *Ticker {
	t.Helper()
	return &Ticker{Dir: wt, BareURL: bare, Flux: flux, ExecTimeout: 30 * time.Second, Logf: t.Logf}
}

// --- happy-path tests --------------------------------------------------------

// A worktree on a branch the GR is not yet tracking triggers the branch-follow
// patch; feat/x is already in the bare repo, so the tick is otherwise idle.
func TestTick_FollowPatchesBranch(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	testgit.Git(t, wt, "switch", "-q", "-c", "feat/x")
	testgit.Git(t, wt, "push", "-q", bare, "feat/x:refs/heads/feat/x")

	flux := &fakeFlux{}
	res, err := newTicker(t, bare, wt, flux).Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Branch != "feat/x" || !res.Followed {
		t.Fatalf("expected follow of feat/x, got %+v", res)
	}
	if len(flux.ensured) != 1 || flux.ensured[0] != "feat/x" {
		t.Fatalf("EnsureBranch calls = %v, want [feat/x]", flux.ensured)
	}
	if res.Pushed || res.Poked || flux.pokes != 0 {
		t.Errorf("feat/x already in sync — no push/poke expected: res=%+v pokes=%d", res, flux.pokes)
	}
}

// A branch absent from the bare repo is created by a plain push and poked.
func TestTick_BrandNewBranchCreates(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	testgit.Git(t, wt, "switch", "-q", "-c", "feat/new")
	writeFile(t, wt, "app.txt", "feat\n")
	testgit.Git(t, wt, "commit", "-q", "-am", "feat change")
	local := localRef(t, wt, "feat/new")

	flux := &fakeFlux{}
	res, err := newTicker(t, bare, wt, flux).Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.Followed || len(flux.ensured) != 1 {
		t.Errorf("expected branch-follow of feat/new, got %+v ensured=%v", res, flux.ensured)
	}
	if !res.Pushed || !res.Poked || flux.pokes != 1 {
		t.Fatalf("expected create-push + poke, got %+v pokes=%d", res, flux.pokes)
	}
	if got := bareRef(t, bare, "feat/new"); got != local {
		t.Errorf("bare feat/new = %s, want the worktree sha %s", got, local)
	}
}

// An in-sync tick pushes nothing and pokes nothing (the issue-#6 push-guard).
func TestTick_IdleNoOp(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	before := bareRef(t, bare, "main")

	flux := &fakeFlux{}
	res, err := newTicker(t, bare, wt, flux).Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Pushed || res.Integrated || res.Poked || res.Followed {
		t.Errorf("idle tick should be a pure no-op, got %+v", res)
	}
	if flux.pokes != 0 {
		t.Errorf("idle tick poked Flux (%d)", flux.pokes)
	}
	if after := bareRef(t, bare, "main"); after != before {
		t.Errorf("idle tick moved bare main %s -> %s", before, after)
	}
}

// A worktree ahead of bare is pushed with the explicit sha (lease against R).
func TestTick_WorktreeAheadPush(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	writeFile(t, wt, "app.txt", "v1\n")
	testgit.Git(t, wt, "commit", "-q", "-am", "dev change")
	local := localRef(t, wt, "main")

	flux := &fakeFlux{}
	res, err := newTicker(t, bare, wt, flux).Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.Pushed || !res.Poked || flux.pokes != 1 {
		t.Fatalf("expected push + poke, got %+v pokes=%d", res, flux.pokes)
	}
	if got := bareRef(t, bare, "main"); got != local {
		t.Errorf("bare main = %s, want the pushed worktree sha %s", got, local)
	}
}

// A bare repo strictly ahead fast-forwards the worktree (reset --hard R): the
// worktree branch and files move to R, Flux is poked, and the tick does NOT push.
func TestTick_BareAheadFastForward(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	R := advanceBare(t, bare, "main", func(dir string) { writeFile(t, dir, "app.txt", "ci\n") })

	flux := &fakeFlux{}
	res, err := newTicker(t, bare, wt, flux).Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.Integrated || res.Pushed {
		t.Fatalf("expected integrate (no push), got %+v", res)
	}
	if !res.Poked || flux.pokes != 1 {
		t.Errorf("expected one poke, got %+v pokes=%d", res, flux.pokes)
	}
	if got := localRef(t, wt, "main"); got != R {
		t.Errorf("worktree main = %s, want fast-forwarded to R %s", got, R)
	}
	if got := readFile(t, wt, "app.txt"); got != "ci\n" {
		t.Errorf("worktree file not updated to R content: %q", got)
	}
	if got := bareRef(t, bare, "main"); got != R {
		t.Errorf("integrate must not move bare main (was R %s, now %s)", R, got)
	}
}

// Genuine divergence (both sides moved, non-conflicting) rebases then pushes.
// The two edits are 8 lines apart so they land in separate diff hunks (a clean
// rebase) rather than the same 3-line context window.
func TestTick_DivergenceRebase(t *testing.T) {
	const base = "top\na\nb\nc\nd\ne\nf\ng\nbottom\n"
	bare := bareRepo(t, base)
	wt := cloneWorktree(t, bare)
	writeFile(t, wt, "app.txt", strings.Replace(base, "top\n", "dev-top\n", 1))
	testgit.Git(t, wt, "commit", "-q", "-am", "dev edits top")
	advanceBare(t, bare, "main", func(dir string) {
		writeFile(t, dir, "app.txt", strings.Replace(base, "bottom\n", "ci-bottom\n", 1))
	})

	flux := &fakeFlux{}
	res, err := newTicker(t, bare, wt, flux).Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.Pushed || !res.Poked || res.Stalled {
		t.Fatalf("expected rebase push (no stall), got %+v", res)
	}
	if got, want := bareRef(t, bare, "main"), localRef(t, wt, "main"); got != want {
		t.Errorf("bare main %s != worktree main %s after rebase-push", got, want)
	}
	got := readFile(t, wt, "app.txt")
	if !strings.Contains(got, "dev-top") || !strings.Contains(got, "ci-bottom") {
		t.Errorf("rebased content should carry both edits, got %q", got)
	}
}

// Divergence where both sides touched the same line conflicts: the rebase is
// aborted, the tick stalls, and the worktree is left pristine — no push/poke.
func TestTick_ConflictStall(t *testing.T) {
	bare := bareRepo(t, "shared\n")
	wt := cloneWorktree(t, bare)
	writeFile(t, wt, "app.txt", "dev\n")
	testgit.Git(t, wt, "commit", "-q", "-am", "dev edits shared line")
	local := localRef(t, wt, "main")
	R := advanceBare(t, bare, "main", func(dir string) { writeFile(t, dir, "app.txt", "ci\n") })

	flux := &fakeFlux{}
	res, err := newTicker(t, bare, wt, flux).Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.Stalled || res.Pushed || res.Poked {
		t.Fatalf("expected stall (no push/poke), got %+v", res)
	}
	if got := localRef(t, wt, "main"); got != local {
		t.Errorf("worktree main moved on stall: %s -> %s", local, got)
	}
	if got := readFile(t, wt, "app.txt"); got != "dev\n" {
		t.Errorf("worktree content changed on stall: %q", got)
	}
	if got := bareRef(t, bare, "main"); got != R {
		t.Errorf("bare main moved on stall: want %s got %s", R, got)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git", "rebase-merge")); !os.IsNotExist(err) {
		t.Errorf("rebase state left behind (rebase --abort should have cleaned it up)")
	}
}

// A corrupt .git/index self-heals before the branch-compare block: the tick
// rebuilds the index and proceeds (here to an idle no-op) instead of wedging.
func TestTick_CorruptIndexHeal(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	// Truncate the valid index below its header so git reports it corrupt.
	if err := os.Truncate(filepath.Join(wt, ".git", "index"), 8); err != nil {
		t.Fatal(err)
	}

	flux := &fakeFlux{}
	res, err := newTicker(t, bare, wt, flux).Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.Healed {
		t.Fatalf("expected the corrupt index to be healed, got %+v", res)
	}
	// The rebuilt index is usable again — a status read would fail if not.
	testgit.Git(t, wt, "status", "--porcelain")
}

// A bare advance with an uncommitted local edit refuses to hard-reset (would
// lose work) AND skips the push (would rewind bare over R): nothing moves.
func TestTick_DirtyGuardRefusal(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	local := localRef(t, wt, "main")
	R := advanceBare(t, bare, "main", func(dir string) { writeFile(t, dir, "app.txt", "ci\n") })
	writeFile(t, wt, "app.txt", "dirty-local\n") // uncommitted

	flux := &fakeFlux{}
	res, err := newTicker(t, bare, wt, flux).Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Integrated || res.Pushed || res.Poked {
		t.Fatalf("dirty-guard must skip integrate AND push, got %+v", res)
	}
	if got := localRef(t, wt, "main"); got != local {
		t.Errorf("worktree main was reset despite the dirty guard: %s -> %s", local, got)
	}
	if got := readFile(t, wt, "app.txt"); got != "dirty-local\n" {
		t.Errorf("uncommitted edit was clobbered: %q", got)
	}
	if got := bareRef(t, bare, "main"); got != R {
		t.Errorf("bare main changed under the dirty guard: want %s got %s", R, got)
	}
}

// --- race-injection tests ----------------------------------------------------
//
// The testHook lands a `git checkout` at one of the tick's guard points,
// deterministically simulating the developer racing the sync loop. These are
// the acceptance core: they prove the issue-#86 bare-repo poison is impossible.

// addOtherBranch creates a second branch `other` at a distinct commit and
// leaves the worktree back on main. Returns other's sha.
func addOtherBranch(t *testing.T, wt string) string {
	t.Helper()
	testgit.Git(t, wt, "switch", "-q", "-c", "other")
	writeFile(t, wt, "app.txt", "other-content\n")
	testgit.Git(t, wt, "commit", "-q", "-am", "other commit")
	o := localRef(t, wt, "other")
	testgit.Git(t, wt, "switch", "-q", "main")
	return o
}

// (a) A checkout landing between the snapshot and the fetch cannot make an idle
// tick mutate anything: every decision was computed against refs/heads/main
// (L == R here), so the tick no-ops even though HEAD is now on `other`. The
// pre-fix sync.sh would re-read HEAD, see other != bare-main, and misfire.
func TestTick_RaceCheckoutAtSnapshotNoOp(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	addOtherBranch(t, wt)
	beforeRefs := localRefs(t, wt)
	beforeBare := bareRef(t, bare, "main")

	flux := &fakeFlux{}
	tk := newTicker(t, bare, wt, flux)
	tk.testHook = func(stage string) {
		if stage == "post-snapshot" {
			testgit.Git(t, wt, "checkout", "-q", "other")
		}
	}
	res, err := tk.Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Pushed || res.Integrated || res.Poked {
		t.Fatalf("race no-op tick mutated something, got %+v", res)
	}
	if flux.pokes != 0 {
		t.Errorf("idle race tick poked Flux (%d)", flux.pokes)
	}
	if after := localRefs(t, wt); !reflect.DeepEqual(beforeRefs, after) {
		t.Errorf("no local ref may move: %v -> %v", beforeRefs, after)
	}
	if got := bareRef(t, bare, "main"); got != beforeBare {
		t.Errorf("bare main moved: %s -> %s", beforeBare, got)
	}
}

// (b) In a bare-ahead setup, a checkout at post-snapshot flips HEAD to `other`
// before the integrate re-verify (step 5c) runs; the re-verify sees HEAD != B
// and skips the reset entirely. No ref moves, the bare is untouched.
func TestTick_RaceCheckoutBeforeResetReverifySkips(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	addOtherBranch(t, wt)
	R := advanceBare(t, bare, "main", func(dir string) { writeFile(t, dir, "app.txt", "ci\n") })
	beforeRefs := localRefs(t, wt)

	flux := &fakeFlux{}
	tk := newTicker(t, bare, wt, flux)
	tk.testHook = func(stage string) {
		if stage == "post-snapshot" {
			testgit.Git(t, wt, "checkout", "-q", "other")
		}
	}
	res, err := tk.Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Integrated || res.Pushed || res.Poked {
		t.Fatalf("re-verify should have skipped integrate, got %+v", res)
	}
	if flux.pokes != 0 {
		t.Errorf("skipped tick poked Flux (%d)", flux.pokes)
	}
	if after := localRefs(t, wt); !reflect.DeepEqual(beforeRefs, after) {
		t.Errorf("no local ref may move: %v -> %v", beforeRefs, after)
	}
	if got := bareRef(t, bare, "main"); got != R {
		t.Errorf("bare main moved: want %s got %s", R, got)
	}
}

// (c) THE issue-#86 test. The checkout lands at pre-reset — AFTER the re-verify
// passed — so reset --hard R fires while HEAD is on `other`, moving the WRONG
// branch's ref to R. Post-verify detects that refs/heads/other moved, rolls it
// back to its pre-tick sha, and aborts the tick: no push, bare untouched. Absent
// the rollback, `other` would point at main's content and the next push would
// poison the bare repo.
func TestTick_RaceCheckoutAtPreResetRollsBack(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	other := addOtherBranch(t, wt)
	mainSHA := localRef(t, wt, "main")
	R := advanceBare(t, bare, "main", func(dir string) { writeFile(t, dir, "app.txt", "ci\n") })

	flux := &fakeFlux{}
	tk := newTicker(t, bare, wt, flux)
	tk.testHook = func(stage string) {
		if stage == "pre-reset" {
			testgit.Git(t, wt, "checkout", "-q", "other")
		}
	}
	res, err := tk.Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.RolledBack || res.Integrated || res.Pushed || res.Poked {
		t.Fatalf("expected post-verify rollback + abort, got %+v", res)
	}
	if flux.pokes != 0 {
		t.Errorf("poisoned tick poked Flux (%d)", flux.pokes)
	}
	if got := localRef(t, wt, "other"); got != other {
		t.Errorf("the wrong branch was NOT rolled back: other = %s, want its pre-tick sha %s", got, other)
	}
	if got := localRef(t, wt, "main"); got != mainSHA {
		t.Errorf("refs/heads/main moved unexpectedly: %s -> %s", mainSHA, got)
	}
	if got := bareRef(t, bare, "main"); got != R {
		t.Errorf("bare main was poisoned: %s (want untouched %s)", got, R)
	}
}

// (e) The rebase path's analogue of (c). Divergence sends the tick into
// rebaseDivergence; a checkout to `other` (positioned at the pre-divergence base
// commit, an ancestor of R) lands at pre-rebase, so `git rebase R` fast-forwards
// the WRONG branch (other) onto R. Post-verify detects it, rolls other back, and
// aborts. Absent the guard the push would read the unchanged refs/heads/main and
// force-push it over R with a matching lease — rewinding the bare repo (#86).
func TestTick_RaceCheckoutAtPreRebaseRollsBack(t *testing.T) {
	const base = "top\na\nb\nc\nd\ne\nf\ng\nbottom\n"
	bare := bareRepo(t, base)
	wt := cloneWorktree(t, bare)
	testgit.Git(t, wt, "branch", "other") // at the base commit, an ancestor of R
	writeFile(t, wt, "app.txt", strings.Replace(base, "top\n", "dev-top\n", 1))
	testgit.Git(t, wt, "commit", "-q", "-am", "dev edits top")
	R := advanceBare(t, bare, "main", func(dir string) {
		writeFile(t, dir, "app.txt", strings.Replace(base, "bottom\n", "ci-bottom\n", 1))
	})
	otherSHA := localRef(t, wt, "other")
	mainSHA := localRef(t, wt, "main")

	flux := &fakeFlux{}
	tk := newTicker(t, bare, wt, flux)
	tk.testHook = func(stage string) {
		if stage == "pre-rebase" {
			testgit.Git(t, wt, "checkout", "-q", "other")
		}
	}
	res, err := tk.Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.RolledBack || res.Pushed || res.Poked || res.Stalled {
		t.Fatalf("expected post-verify rollback + abort on the rebase path, got %+v", res)
	}
	if flux.pokes != 0 {
		t.Errorf("poisoned rebase tick poked Flux (%d)", flux.pokes)
	}
	if got := localRef(t, wt, "other"); got != otherSHA {
		t.Errorf("the wrong branch was NOT rolled back: other = %s, want its pre-tick sha %s", got, otherSHA)
	}
	if got := localRef(t, wt, "main"); got != mainSHA {
		t.Errorf("refs/heads/main moved unexpectedly: %s -> %s", mainSHA, got)
	}
	if got := bareRef(t, bare, "main"); got != R {
		t.Errorf("bare main was poisoned via the rebase path: %s (want untouched %s)", got, R)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git", "rebase-merge")); !os.IsNotExist(err) {
		t.Errorf("rebase state left behind after the aborted rebase tick")
	}
}

// (d) Root-written files stay host-writable. Under umask 0 (what cmd main sets,
// applied here around the tick since the test binary doesn't run that main), an
// integrate's reset --hard recreates a file group+other-writable (0666), so the
// host user can edit and commit it afterward — the EACCES class this controller
// exists to fix.
func TestTick_IntegrateModesRootWritable(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	advanceBare(t, bare, "main", func(dir string) { writeFile(t, dir, "newfile.txt", "created-by-integrate\n") })

	flux := &fakeFlux{}
	old := syscall.Umask(0)
	res, err := newTicker(t, bare, wt, flux).Tick(context.Background(), "main")
	syscall.Umask(old)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.Integrated {
		t.Fatalf("expected integrate, got %+v", res)
	}
	info, err := os.Stat(filepath.Join(wt, "newfile.txt"))
	if err != nil {
		t.Fatalf("reset --hard did not create the file: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0o022 {
		t.Errorf("integrate-recreated file mode = %o, want group+other writable (0666 under umask 0)", perm)
	}
}

// The developer's git hooks must never run inside the container: the worktree's
// .git is host-owned and the process runs as root, so a pre-rebase (or pre-push)
// hook firing here would execute host-authored code as root. sync.sh disabled
// hooks globally; the port neutralizes them on every exec via output(). The
// divergence rebase is the op that would otherwise fire pre-rebase — prove it
// doesn't, and that the rebase still completes.
func TestTick_DeveloperHooksDoNotRun(t *testing.T) {
	const base = "top\na\nb\nc\nd\ne\nf\ng\nbottom\n"
	bare := bareRepo(t, base)
	wt := cloneWorktree(t, bare)
	writeFile(t, wt, "app.txt", strings.Replace(base, "top\n", "dev-top\n", 1))
	testgit.Git(t, wt, "commit", "-q", "-am", "dev edits top")
	advanceBare(t, bare, "main", func(dir string) {
		writeFile(t, dir, "app.txt", strings.Replace(base, "bottom\n", "ci-bottom\n", 1))
	})

	// A host-authored pre-rebase hook that would both leave a marker and FAIL
	// the rebase if executed. exit 1 makes hook execution unmissable: the
	// divergence rebase would abort into a stall instead of pushing.
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hookDir := filepath.Join(wt, ".git", "hooks")
	hook := filepath.Join(hookDir, "pre-rebase")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	flux := &fakeFlux{}
	res, err := newTicker(t, bare, wt, flux).Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.Pushed || res.Stalled {
		t.Fatalf("rebase should have completed and pushed despite the failing hook, got %+v", res)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("pre-rebase hook RAN in the container (marker %s exists)", marker)
	}
}

// A checkout landing at post-reset — after a LEGITIMATE integrate's reset
// already moved refs/heads/B to R — must abort the tick (no Integrated, no
// poke) without rolling anything back: the checkout moved only HEAD, no ref
// was wrongly moved, and B's move to R was the integrate doing its job. The
// pure cur != B abort path of postVerify.
func TestTick_RaceCheckoutAtPostResetAbortsWithoutRollback(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	other := addOtherBranch(t, wt)
	R := advanceBare(t, bare, "main", func(dir string) { writeFile(t, dir, "app.txt", "ci\n") })

	flux := &fakeFlux{}
	tk := newTicker(t, bare, wt, flux)
	tk.testHook = func(stage string) {
		if stage == "post-reset" {
			testgit.Git(t, wt, "checkout", "-q", "other")
		}
	}
	res, err := tk.Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Integrated || res.RolledBack || res.Pushed || res.Poked {
		t.Fatalf("expected a pure abort (no rollback, no integrate), got %+v", res)
	}
	if flux.pokes != 0 {
		t.Errorf("aborted tick poked Flux (%d)", flux.pokes)
	}
	// The reset legitimately fast-forwarded main to R before the checkout
	// raced in; the abort must not undo that, and other must be untouched.
	if got := localRef(t, wt, "main"); got != R {
		t.Errorf("refs/heads/main = %s, want the integrated R %s", got, R)
	}
	if got := localRef(t, wt, "other"); got != other {
		t.Errorf("refs/heads/other moved: %s -> %s", other, got)
	}
	if got := bareRef(t, bare, "main"); got != R {
		t.Errorf("bare main moved: want %s got %s", R, got)
	}
}
