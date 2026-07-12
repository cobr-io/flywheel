package execx

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
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
