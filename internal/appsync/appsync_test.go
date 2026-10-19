package appsync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	if res.Pushed || res.Poked || res.Followed {
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

// syncedCommits makes n empty commits on the worktree's current branch and
// ticks once so the bare repo holds them all, the state every rewrite test
// starts from. Returns the shas oldest first.
func syncedCommits(t *testing.T, bare, wt string, n int) []string {
	t.Helper()
	var shas []string
	for i := range n {
		testgit.Git(t, wt, "commit", "-q", "--allow-empty", "-m", fmt.Sprintf("c%d", i))
		shas = append(shas, testgit.Out(t, wt, "rev-parse", "HEAD"))
	}
	if _, err := newTicker(t, bare, wt, &fakeFlux{}).Tick(context.Background(), "main"); err != nil {
		t.Fatalf("seed Tick: %v", err)
	}
	if got := bareRef(t, bare, "main"); got != shas[n-1] {
		t.Fatalf("seed tick did not sync: bare main = %s, want %s", got, shas[n-1])
	}
	return shas
}

// A local `git reset` drops a commit the bare repo already has (L is an
// ancestor of R). The tick must push the rewrite and leave the worktree
// alone. It used to `reset --hard R`, putting the dropped commit back: an
// untracked file alone didn't trip its dirty guard (issue #161).
func TestTick_LocalResetPushesRewrite(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	shas := syncedCommits(t, bare, wt, 2)
	testgit.Git(t, wt, "reset", "-q", "--mixed", "HEAD~1")
	writeFile(t, wt, "untracked.txt", "new\n")

	flux := &fakeFlux{}
	res, err := newTicker(t, bare, wt, flux).Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.Pushed || !res.Rewrote || !res.Poked || flux.pokes != 1 {
		t.Fatalf("expected a rewrite push + poke, got %+v pokes=%d", res, flux.pokes)
	}
	if got := localRef(t, wt, "main"); got != shas[0] {
		t.Errorf("worktree main = %s, want the reset target %s (the dropped commit came back)", got, shas[0])
	}
	if got := bareRef(t, bare, "main"); got != shas[0] {
		t.Errorf("bare main = %s, want the rewritten worktree sha %s", got, shas[0])
	}
	if got := readFile(t, wt, "untracked.txt"); got != "new\n" {
		t.Errorf("untracked file was clobbered: %q", got)
	}
}

// A local rebase that drops a middle commit diverges from the bare repo
// (neither L nor R is an ancestor of the other). The tick must force the bare
// repo to L. It used to `git rebase R`, which replayed nothing new and put the
// branch back on its old tip (issue #161).
func TestTick_LocalRebaseDropPushesRewrite(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	shas := syncedCommits(t, bare, wt, 3)
	testgit.Git(t, wt, "rebase", "-q", "--onto", shas[0], shas[1], "main")
	L := localRef(t, wt, "main")
	if L == shas[2] {
		t.Fatal("fixture: rebase did not rewrite main")
	}

	res, err := newTicker(t, bare, wt, &fakeFlux{}).Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.Pushed || !res.Rewrote {
		t.Fatalf("expected a rewrite push, got %+v", res)
	}
	if got := localRef(t, wt, "main"); got != L {
		t.Errorf("worktree main = %s, want the rebased %s (the rewrite was undone)", got, L)
	}
	if got := bareRef(t, bare, "main"); got != L {
		t.Errorf("bare main = %s, want the rebased worktree sha %s", got, L)
	}
}

// The tick shares the developer's .git, so it must not write FETCH_HEAD: git
// truncates it when a fetch starts and appends when it ends, so a tick fetch
// overlapping the developer's `git pull` left both entries in it, and pull
// then failed with "Cannot rebase onto multiple branches". The developer's
// FETCH_HEAD must survive a tick byte-for-byte, and the tick must still find
// the bare head without it.
func TestTick_LeavesDeveloperFetchHeadAlone(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	testgit.Git(t, wt, "fetch", "-q", "origin")
	gitDir := filepath.Join(wt, ".git")
	before := readFile(t, gitDir, "FETCH_HEAD")
	writeFile(t, wt, "app.txt", "v1\n")
	testgit.Git(t, wt, "commit", "-q", "-am", "dev change")

	res, err := newTicker(t, bare, wt, &fakeFlux{}).Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.Pushed || res.Rewrote {
		t.Fatalf("expected a fast-forward push, got %+v", res)
	}
	if after := readFile(t, gitDir, "FETCH_HEAD"); after != before {
		t.Errorf("tick rewrote the developer's FETCH_HEAD:\nbefore: %q\nafter:  %q", before, after)
	}
}

// The developer's git hooks must never run inside the container: the worktree's
// .git is host-owned and the process runs as root, so a pre-push hook firing
// here would execute host-authored code as root. sync.sh disabled hooks
// globally; the port neutralizes them on every exec via output().
func TestTick_DeveloperHooksDoNotRun(t *testing.T) {
	bare := bareRepo(t, "v0\n")
	wt := cloneWorktree(t, bare)
	writeFile(t, wt, "app.txt", "v1\n")
	testgit.Git(t, wt, "commit", "-q", "-am", "dev change")

	// A host-authored pre-push hook that would both leave a marker and FAIL
	// the push if executed.
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(wt, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := newTicker(t, bare, wt, &fakeFlux{}).Tick(context.Background(), "main")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.Pushed {
		t.Fatalf("push should have succeeded despite the failing hook, got %+v", res)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("pre-push hook RAN in the container (marker %s exists)", marker)
	}
}

// --- race-injection tests ----------------------------------------------------
//
// The testHook lands a `git checkout` after the snapshot, deterministically
// simulating the developer racing the sync loop.

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
	if res.Pushed || res.Poked {
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
