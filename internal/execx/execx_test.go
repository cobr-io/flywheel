package execx

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cobr-io/flywheel/internal/testgit"
)

func TestRun_StdoutAndDir(t *testing.T) {
	dir := t.TempDir()
	// `pwd` prints the working directory, proving dir is honoured.
	out, err := Run(context.Background(), dir, "pwd")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(out) != dir {
		t.Fatalf("Run stdout = %q, want %q", strings.TrimSpace(out), dir)
	}
}

func TestRun_FailureWrapsExitErrorAndStderr(t *testing.T) {
	// `sh -c 'echo boom >&2; exit 3'` fails with stderr and a non-zero code.
	_, err := Run(context.Background(), "", "sh", "-c", "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("expected an error from a failing command")
	}
	// The one error format must wrap the underlying *exec.ExitError so
	// errors.Is / errors.As keep working through it.
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("error %v does not unwrap to *exec.ExitError", err)
	}
	if ee.ExitCode() != 3 {
		t.Errorf("exit code = %d, want 3", ee.ExitCode())
	}
	// Trimmed stderr is included.
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q should carry trimmed stderr", err.Error())
	}
}

func TestGit_InheritsEnv_NoCommitter(t *testing.T) {
	// Git must NOT force the automation committer, so an unconfigured repo's
	// commit fails rather than being silently attributed to flywheel-deploy.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	env := gitEnv()
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_COMMITTER_NAME=") {
			found = true
			if e != "GIT_COMMITTER_NAME="+CommitterName {
				t.Errorf("gitEnv committer = %q, want %q", e, CommitterName)
			}
		}
	}
	if !found {
		t.Error("gitEnv must pin GIT_COMMITTER_NAME")
	}
}

func TestGitAuto_CommitterIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	ctx := context.Background()
	if _, err := GitAuto(ctx, dir, "init", "-q", "-b", "main"); err != nil {
		t.Fatalf("init: %v", err)
	}
	// No `git config user.*` set: only GitAuto's env provides an identity.
	// The author still needs one, so pin it inline; the committer must come
	// from GitAuto's gitEnv.
	if _, err := GitAuto(ctx, dir,
		"-c", "user.name=author", "-c", "user.email=author@x",
		"commit", "--allow-empty", "-q", "-m", "t"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	out, err := GitAuto(ctx, dir, "log", "-1", "--format=%cn <%ce>")
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	want := CommitterName + " <" + CommitterEmail + ">"
	if strings.TrimSpace(out) != want {
		t.Fatalf("committer = %q, want %q", strings.TrimSpace(out), want)
	}
}

// TestGitAuto_NoAutoMaintenanceChild is the regression test for #144: since
// git 2.47, an everyday `git fetch` backgrounds `git maintenance run --auto
// --quiet --detach` unless maintenance.auto is off. A detached process
// outlives its parent `git`, so under the in-cluster controllers (PID 1, no
// init) it gets reparented to PID 1 and never reaped — one zombie per fetch.
// gitEnv must turn maintenance.auto off for every GitAuto call.
//
// Proof is via a real fetch's GIT_TRACE2_EVENT log rather than a fake git
// binary, so this asserts against git's actual behaviour instead of our
// assumption about it: a fetch with something new to pull is the one case
// that reliably triggers the auto-maintenance hook (a no-op fetch doesn't).
func TestGitAuto_NoAutoMaintenanceChild(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()

	// origin: a bare "remote" with one commit — mirrors the bare-repo
	// image-bump loop's origin (deploybranch/selfsync fixtures use the same
	// work -> --bare shape).
	work := filepath.Join(t.TempDir(), "work")
	testgit.Init(t, work)
	testgit.Git(t, work, "commit", "-q", "--allow-empty", "-m", "A")
	origin := filepath.Join(t.TempDir(), "origin.git")
	testgit.Git(t, work, "clone", "-q", "--bare", work, origin)

	// clone: what GitAuto fetches into. Starts behind origin.
	clone := filepath.Join(t.TempDir(), "clone")
	testgit.Git(t, filepath.Dir(clone), "clone", "-q", origin, clone)

	// Advance origin past what clone has, so the fetch below is a real
	// protocol exchange (transfers an object, triggering the auto-maintenance
	// hook) rather than a no-op that git may skip it for.
	testgit.Git(t, work, "commit", "-q", "--allow-empty", "-m", "B")
	testgit.Git(t, work, "push", "-q", origin, "main")

	trace := filepath.Join(t.TempDir(), "trace2.jsonl")
	t.Setenv("GIT_TRACE2_EVENT", trace) // gitEnv() inherits os.Environ()

	if _, err := GitAuto(ctx, clone, "fetch", "origin"); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("read trace2 log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var ev struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("parse trace2 line %q: %v", line, err)
		}
		if ev.Event != "child_start" {
			continue
		}
		for _, a := range ev.Argv {
			if strings.Contains(a, "maintenance") {
				t.Fatalf("GitAuto fetch spawned a maintenance child (argv=%v) — it will outlive its parent and, run as PID 1 with no init, never get reaped", ev.Argv)
			}
		}
	}
}

// TestGitAuto_CtxTimeoutBoundsWaitOnRemoteHelper is the regression test for
// #150: git's remote helper for an http(s) remote (git-remote-http) inherits
// the stdout/stderr pipes runEnv gives the parent git process. Without
// cmd.WaitDelay, a ctx that fires only kills git itself — Wait then blocks on
// those pipes until the helper exits on its own, so neither ExecTimeout nor
// cancellation actually bounds the call (see the issue's real-world repro: a
// 2s ctx returned only after 23.7s). A TCP listener that accepts and never
// replies reproduces that without a real remote: git blocks reading the
// response, ctx fires, git itself dies, and — pre-fix — GitAuto returns only
// once the listener force-closes the connection out from under the orphaned
// helper.
func TestGitAuto_CtxTimeoutBoundsWaitOnRemoteHelper(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const ctxTimeout = 1 * time.Second
	// Comfortably longer than ctxTimeout+waitDelay+margin below, so a failure
	// to bound the wait shows up as a timeout against that margin rather than
	// coincidentally finishing whenever the listener happens to close.
	addr := hangingListener(t, 15*time.Second)

	dir := t.TempDir()
	testgit.Init(t, dir)

	ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
	defer cancel()

	start := time.Now()
	_, err := GitAuto(ctx, dir, "fetch", "http://"+addr+"/x.git", "main")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error fetching from a server that never replies")
	}
	// ctxTimeout to hit the deadline, waitDelay for Wait to give up on the
	// leaked pipes, and margin for process start/teardown.
	bound := ctxTimeout + waitDelay + 3*time.Second
	if elapsed > bound {
		t.Fatalf("GitAuto returned after %s (ctx timeout %s), want <= %s: %v", elapsed, ctxTimeout, bound, err)
	}
}

// TestGitAuto_KillsRemoteHelperGroupOnCancel is the regression test for the
// second half of #150: WaitDelay alone bounds runEnv's own wait, but leaves
// the orphaned git-remote-http helper running in the background after
// GitAuto has already returned. GitAuto additionally starts git in its own
// process group and kills that whole group on cancellation, so the helper
// dies with git.
//
// The helper is a grandchild, not GitAuto's direct child, and pgrep isn't
// portable, so its pid isn't otherwise observable from the test. This
// recovers it the way #145's TestGitAuto_NoAutoMaintenanceChild recovers
// spawned children: from git's own GIT_TRACE2_EVENT log. Every process
// trace2-logs a "start" event as the first thing it does, and that event's
// session id (sid) ends in "-P<hex pid>" — confirmed by cross-checking it
// against the "pid" field a corresponding child_exit event reports for the
// same process.
func TestGitAuto_KillsRemoteHelperGroupOnCancel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const ctxTimeout = 1 * time.Second
	addr := hangingListener(t, 15*time.Second)

	dir := t.TempDir()
	testgit.Init(t, dir)

	trace := filepath.Join(t.TempDir(), "trace2.jsonl")
	t.Setenv("GIT_TRACE2_EVENT", trace) // gitEnv() inherits os.Environ()

	ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
	defer cancel()
	if _, err := GitAuto(ctx, dir, "fetch", "http://"+addr+"/x.git", "main"); err == nil {
		t.Fatal("expected an error fetching from a server that never replies")
	}

	pid := remoteHelperPID(t, trace)
	if pid == 0 {
		t.Fatal("trace2 log never recorded a git-remote-http start event")
	}
	// Grace period: SIGKILL delivery and reparent/reap aren't instantaneous.
	time.Sleep(300 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("git-remote-http (pid %d) is still alive after GitAuto returned", pid)
	}
}

// remoteHelperPID scans a GIT_TRACE2_EVENT log for the git-remote-http
// helper's own "start" event — the actual dashed binary that holds the
// network connection, not the "git remote-http" wrapper that execs it — and
// recovers its pid from the trailing "-P<hex>" of that event's sid. Returns 0
// if the log never recorded one.
func remoteHelperPID(t *testing.T, traceFile string) int {
	t.Helper()
	data, err := os.ReadFile(traceFile)
	if err != nil {
		t.Fatalf("read trace2 log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var ev struct {
			Event string   `json:"event"`
			SID   string   `json:"sid"`
			Argv  []string `json:"argv"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("parse trace2 line %q: %v", line, err)
		}
		if ev.Event != "start" || len(ev.Argv) == 0 || !strings.HasSuffix(ev.Argv[0], "git-remote-http") {
			continue
		}
		seg := ev.SID
		if i := strings.LastIndex(seg, "/"); i >= 0 {
			seg = seg[i+1:]
		}
		i := strings.LastIndex(seg, "-P")
		if i < 0 {
			continue
		}
		pid, err := strconv.ParseInt(seg[i+2:], 16, 64)
		if err != nil {
			continue
		}
		return int(pid)
	}
	return 0
}

// hangingListener starts a TCP listener that accepts connections and never
// replies, so a fetch against it blocks until ctx kills git — reproducing the
// leaked-pipe hang (#150) without a real remote. It force-closes every
// connection after closeAfter so a run against the unfixed runEnv, which has
// no bound of its own, still returns instead of hanging the suite.
func hangingListener(t *testing.T, closeAfter time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	closeAll := func() {
		mu.Lock()
		defer mu.Unlock()
		ln.Close()
		for _, c := range conns {
			c.Close()
		}
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	timer := time.AfterFunc(closeAfter, closeAll)
	t.Cleanup(func() {
		timer.Stop()
		closeAll()
	})
	return ln.Addr().String()
}
