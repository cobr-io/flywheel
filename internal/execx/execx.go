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
//     rebase and commit.
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
	"strings"
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
	return runEnv(ctx, dir, nil, name, args...)
}

// Git runs `git args...` in dir with the developer's inherited environment.
func Git(ctx context.Context, dir string, args ...string) (string, error) {
	return runEnv(ctx, dir, nil, "git", args...)
}

// GitAuto runs `git args...` in dir with gitEnv(): interactive prompts disabled
// and the pinned committer identity, for the in-cluster bare-repo automation.
func GitAuto(ctx context.Context, dir string, args ...string) (string, error) {
	return runEnv(ctx, dir, gitEnv(), "git", args...)
}

// runEnv is the single implementation behind Run/Git/GitAuto. A nil env
// inherits the parent environment.
func runEnv(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env // nil → inherit the parent environment
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
// unauthenticated http) and pins a committer identity so rebases and commits
// never fail on a missing ident in a bare container.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_COMMITTER_NAME="+CommitterName,
		"GIT_COMMITTER_EMAIL="+CommitterEmail,
	)
}
