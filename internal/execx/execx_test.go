package execx

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
