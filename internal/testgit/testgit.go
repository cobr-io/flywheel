// Package testgit is the shared git-repo bootstrap for tests that exercise the
// real-git-in-a-tempdir style (the deliberate alternative to mocking git). It
// replaces the byte-identical git/gitOut/init-config helpers that were
// copy-pasted across the deploybranch, selfsync, gitcheckout and doctor test
// suites. It is intentionally tiny — no framework, just the three primitives
// those suites share.
package testgit

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Git runs `git -C dir args...`, failing the test on error. It pins a
// deterministic author+committer identity so commits never depend on the
// developer's ambient git config (or its absence in CI).
func Git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// Out runs `git -C dir args...` and returns its trimmed stdout, failing the
// test on error.
func Out(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// Init creates dir (mkdir -p) and initialises an empty git repo on branch main
// with the deterministic test identity configured. Callers add files and
// commit with Git.
func Init(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	Git(t, dir, "init", "-q", "-b", "main")
	Git(t, dir, "config", "user.email", "t@t")
	Git(t, dir, "config", "user.name", "t")
}
