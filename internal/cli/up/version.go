package up

import (
	"fmt"
	"io"
	"path/filepath"

	"golang.org/x/mod/semver"

	flywheel "github.com/cobr-io/flywheel"
	"github.com/cobr-io/flywheel/internal/cli/config"
	"github.com/cobr-io/flywheel/internal/cli/style"
	"github.com/cobr-io/flywheel/internal/naming"
)

// checkVersionDrift enforces the binary↔pin agreement invariant before `up`
// does any work: up only proceeds when the installed flywheel binary and the
// repo's pinned flywheel.version agree, or after the user accepts a forward
// bump that makes them agree. It returns the version up should proceed with
// (the bumped value on an accepted upgrade, otherwise the original pin) and a
// non-nil error when up must abort.
//
// The comparison is intentionally directional so an outdated binary can never
// roll the pin backward:
//
//   - binary == pin → proceed unchanged.
//   - binary  > pin → the repo is behind a newer installed binary; warn and
//     prompt to roll flywheel.version forward (the "one-line version bump").
//     Accept → write the pin + continue; decline (or a non-TTY where we can't
//     ask) → abort. We never run new manifests against old pinned image tags.
//   - binary  < pin → the binary is behind the repo's pin; hard stop. The fix
//     is to upgrade the binary, never to downgrade the repo, so this branch
//     has no path that writes flywheel.version.
//
// Dev/unreleased binaries (the unstamped v0.0.0-dev sentinel, or git-describe
// builds like v0.1.0-5-gabc123) carry pre-release/build metadata and can't
// assert meaningful drift — the check is skipped for them entirely.
func checkVersionDrift(out io.Writer, stdin io.Reader, repoDir, pin string) (string, error) {
	build := flywheel.BuildVersion

	if !isReleaseVersion(build) {
		return pin, nil // dev/unreleased binary — nothing to assert
	}
	if !semver.IsValid(pin) {
		style.Warn(out, "flywheel.version %q is not valid semver; skipping version check", pin)
		return pin, nil
	}

	switch semver.Compare(build, pin) {
	case 0:
		return pin, nil // in sync
	case -1:
		// Binary behind the repo's pin → hard stop. Upgrading the binary is the
		// only fix; we never offer to downgrade the pin.
		return pin, fmt.Errorf(
			"installed flywheel %s is older than this repo's flywheel.version %s\n"+
				"upgrade your flywheel binary and re-run — flywheel up will not run an "+
				"older binary against a newer pin (it would deploy stale manifests)",
			build, pin)
	default: // +1: binary ahead → offer to roll the pin forward
		return promptBumpVersion(out, stdin, repoDir, pin, build)
	}
}

// confirmBump reports whether the user accepts rolling flywheel.version forward
// to build, and whether the question could be asked at all (asked=false on a
// non-TTY, where up must abort rather than silently mutate the file). A package
// var so tests can drive the accept/decline paths without a real terminal.
var confirmBump = func(stdin io.Reader, out io.Writer, build string) (accepted, asked bool) {
	if !isTTY(stdin) {
		return false, false
	}
	return promptYesNo(stdin, out, fmt.Sprintf("update flywheel.version to %s?", build), true), true
}

// promptBumpVersion handles the binary > pin case: warn, then (on a TTY) ask to
// roll flywheel.version forward to the installed release. On accept it rewrites
// flywheel.yaml and returns the new version; on decline, or on a non-TTY where
// we can't ask, it returns an abort error rather than silently mutating the
// file or proceeding under version skew.
func promptBumpVersion(out io.Writer, stdin io.Reader, repoDir, pin, build string) (string, error) {
	style.Warn(out, "flywheel.version (%s) is behind your installed flywheel (%s)", pin, build)

	accepted, asked := confirmBump(stdin, out, build)
	if !asked {
		return pin, fmt.Errorf(
			"flywheel.version %s is behind installed flywheel %s; set flywheel.version "+
				"to %s (or run flywheel up on a terminal to update it interactively)",
			pin, build, build)
	}
	if !accepted {
		return pin, fmt.Errorf(
			"aborted: flywheel.version (%s) is behind installed flywheel (%s) — bump it "+
				"to %s or re-run and accept the update", pin, build, build)
	}

	path := filepath.Join(repoDir, naming.ConfigFile)
	if err := config.SetFlywheelVersion(path, build); err != nil {
		return pin, fmt.Errorf("update flywheel.version: %w", err)
	}
	style.OK(out, "flywheel.version → %s", build)
	return build, nil
}

// isReleaseVersion reports whether v is an exact release tag: valid semver with
// no pre-release or build metadata. The unstamped v0.0.0-dev sentinel and
// git-describe dev builds (v0.1.0-5-gabc123) carry a pre-release segment and
// return false, so the drift gate ignores them.
func isReleaseVersion(v string) bool {
	return semver.IsValid(v) && semver.Prerelease(v) == "" && semver.Build(v) == ""
}
