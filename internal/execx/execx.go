// Package execx runs external commands with one uniform error format and a
// single shared git environment, so the ~dozen packages that shell out don't
// each re-roll the plumbing.
//
// Two git entry points exist on purpose:
//
//   - Git inherits the developer's environment — the CLI paths (init, worktree,
//     use, checkout inspection, branch reads) that must honour the user's git
//     config and ambient credentials.
//   - GitAuto pins the in-cluster automation identity used by the bare-repo
//     image-bump loop (deploybranch, selfsync): interactive prompts disabled
//     and a committer identity so a container without git config can still
//     rebase and commit. It also runs git in its own process group and kills
//     that group on ctx cancellation, so a network remote helper (e.g.
//     git-remote-http) dies with git instead of lingering past WaitDelay
//     (#150).
//
// It lives outside internal/cli so both the CLI packages and the in-cluster
// controllers (deploybranch, selfsync) can import it.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CommitterName and CommitterEmail identify the in-cluster image-bump
// automation. They are the committer of DEPLOY-branch commits and are pinned in
// gitEnv so a bare container without git config can still rebase and commit.
const (
	CommitterName  = "flywheel-deploy"
	CommitterEmail = "image-automation@dev.local"
)

// Run executes name with args in dir (dir=="" → the process working directory),
// inheriting the current environment, and returns the command's stdout. On
// failure the returned error wraps the underlying error (usually an
// *exec.ExitError, so errors.Is / errors.As keep working) and carries the
// command's trimmed stderr.
func Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	return runEnv(ctx, dir, nil, false, name, args...)
}

// Git runs `git args...` in dir with the developer's inherited environment.
func Git(ctx context.Context, dir string, args ...string) (string, error) {
	return runEnv(ctx, dir, nil, false, "git", args...)
}

// GitAuto runs `git args...` in dir with gitEnv(): interactive prompts disabled
// and the pinned committer identity, for the in-cluster bare-repo automation.
// It runs in its own process group, killed as a whole on ctx cancellation —
// see runEnv's killProcessGroup and #150.
func GitAuto(ctx context.Context, dir string, args ...string) (string, error) {
	return runEnv(ctx, dir, gitEnv(), true, "git", args...)
}

// waitDelay bounds how long Wait keeps blocking on a killed command's stdout
// and stderr pipes once the command itself has exited or been killed.
// Without it, a grandchild that inherited those pipes (e.g. git's
// git-remote-http helper for an http(s) remote) can hold Wait open long after
// ctx's deadline or cancellation fired (#150). A few seconds is generous
// enough for a well-behaved command's own I/O to drain, so anything still
// open past it is exactly the leaked-pipe case this bounds.
const waitDelay = 5 * time.Second

// runEnv is the single implementation behind Run/Git/GitAuto. A nil env
// inherits the parent environment. killProcessGroup additionally starts name
// in its own process group and kills that whole group on ctx cancellation,
// instead of just the process exec.CommandContext would kill by default —
// GitAuto sets it so a spawned remote helper can't outlive git; Run and Git
// never do, since the CLI's developer-environment paths need the process in
// the caller's terminal's process group to receive interactive prompts (a
// background process group can't read the terminal — SIGTTIN).
func runEnv(ctx context.Context, dir string, env []string, killProcessGroup bool, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env // nil → inherit the parent environment
	cmd.WaitDelay = waitDelay
	if killProcessGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return stdout.String(), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

// gitEnv disables interactive prompts (the in-cluster bare repo is
// unauthenticated http), pins a committer identity so rebases and commits
// never fail on a missing ident in a bare container, and turns off git's
// background auto-maintenance.
//
// Since git 2.47, an everyday command like `fetch` backgrounds `git
// maintenance run --auto --quiet --detach`. The detached process outlives its
// parent and is reparented to PID 1, which normally is an init that reaps it;
// the in-cluster controllers ARE PID 1, with no init, and never reap it — a
// zombie `git` per fetch (#144). Root also has no business
// running gc/maintenance on a repo it doesn't own (the bare automation repo,
// or a developer's host worktree), so this stays off regardless of the PID-1
// backstop (shareProcessNamespace on the Deployments).
//
// Set via GIT_CONFIG_COUNT/_KEY_N/_VALUE_N rather than a `-c` flag at each
// GitAuto call site, so gitEnv stays the one place this is guaranteed.
// Appended at the next free index (nextGitConfigIndex) instead of hard-coding
// index 0, in case the inherited environment already carries some — nothing
// under our control sets GIT_CONFIG_COUNT today, but clobbering a
// pre-existing one would silently drop whatever config it carried.
func gitEnv() []string {
	env := os.Environ()
	i := nextGitConfigIndex(env)
	return append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_COMMITTER_NAME="+CommitterName,
		"GIT_COMMITTER_EMAIL="+CommitterEmail,
		fmt.Sprintf("GIT_CONFIG_COUNT=%d", i+1),
		fmt.Sprintf("GIT_CONFIG_KEY_%d=maintenance.auto", i),
		fmt.Sprintf("GIT_CONFIG_VALUE_%d=false", i),
	)
}

// nextGitConfigIndex returns the first unused GIT_CONFIG_KEY_N/VALUE_N index:
// the inherited GIT_CONFIG_COUNT if env already sets one and it parses as a
// non-negative integer, else 0. An unparsable count is left for git itself to
// reject; nextGitConfigIndex just doesn't compound the error by clobbering it.
func nextGitConfigIndex(env []string) int {
	for _, e := range env {
		v, ok := strings.CutPrefix(e, "GIT_CONFIG_COUNT=")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
		return 0
	}
	return 0
}
