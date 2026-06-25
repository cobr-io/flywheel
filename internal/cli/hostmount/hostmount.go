// Package hostmount guards against putting a flywheel gitops repo (and its
// workspaces_root) on a host path Docker Desktop won't bind-mount into the k3d
// cluster. On macOS the cluster runs in a Linux VM, and macOS temp dirs aren't
// reliably shared: `/tmp` is a symlink to `/private/tmp` that the VM's own
// `/tmp` shadows, and `/var/folders` (the per-user temp dir) is unshared. A
// repo there comes up with empty `/workspaces`, so git-auto-sync can't push it
// and Flux's client-* Kustomizations fail with a cryptic "Source artifact not
// found". This package fails fast with remediation instead.
package hostmount

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// AllowEnv, when set to a non-empty value, bypasses Guard — an escape hatch for
// advanced setups (e.g. a Docker Desktop that genuinely shares a temp dir).
const AllowEnv = "FLYWHEEL_ALLOW_EPHEMERAL_WORKSPACE"

// tempDirPrefixes are macOS host paths Docker Desktop does not reliably
// bind-mount into a k3d cluster. Both the symlink and resolved forms are listed
// so a path reported either way is caught.
var tempDirPrefixes = []string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}

// UnshareableTempDir reports whether p is at or under a macOS temp dir that
// won't bind-mount into k3d, returning the matched prefix. Pure path logic with
// no OS gating, so it's deterministically testable on any platform; callers
// apply the darwin gate (see Guard).
func UnshareableTempDir(p string) (string, bool) {
	abs := filepath.Clean(p)
	for _, pre := range tempDirPrefixes {
		if abs == pre || strings.HasPrefix(abs, pre+string(filepath.Separator)) {
			return pre, true
		}
	}
	return "", false
}

// Remediation is the shared guidance shown when a workspace path can't reach
// the cluster.
func Remediation() string {
	return "Docker Desktop does not reliably share macOS temp dirs (/tmp, /var/folders) into " +
		"k3d, so the cluster can't see your gitops repo or its sibling worktrees.\n" +
		"Clone your gitops repo under your home directory (e.g. ~/src/...) and run from there — " +
		"Docker Desktop shares $HOME by default.\n" +
		"To override this guard (advanced), set " + AllowEnv + "=1."
}

// Guard refuses (on macOS) to let `action` proceed from a host path Docker
// Desktop won't bind-mount into k3d, unless AllowEnv is set. `dir` is the path
// being checked (the repo dir, or an `init` scaffold target). It returns nil on
// non-darwin platforms, on a shareable path, or when overridden — so Linux
// (where any host path mounts) and CI are unaffected.
func Guard(action, dir string) error {
	if os.Getenv(AllowEnv) != "" {
		return nil
	}
	if runtime.GOOS != "darwin" {
		return nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	if matched, bad := UnshareableTempDir(abs); bad {
		return fmt.Errorf("refusing to %s in %s (under %s).\n%s", action, abs, matched, Remediation())
	}
	return nil
}
