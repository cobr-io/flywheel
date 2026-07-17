package appsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	testgit.Git(t, c, "commit", "-q", "-am", "ci change")
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
