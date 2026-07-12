// Package doctor runs the host-prerequisite checks documented in design
// § Prerequisites. `doctor --quick` runs the minimal set wired into
// `flywheel up` step 2; `doctor` (full mode) adds profile-specific
// checks and live-port-collision validation against the allocator.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Check is one host-prerequisite probe.
type Check struct {
	Name        string
	Description string
	// Timeout overrides the default per-check budget in Run. Zero means
	// use the default. Most probes are instantaneous; the docker daemon
	// ping wants a larger budget because it retries under load.
	Timeout time.Duration
	Run     func(context.Context) error
}

// Severity classifies a failing Result for the renderer and the command's
// exit-code decision. It lives on Result, not Check, because severity is a
// property of what a given run actually found — the same Check type (a bare
// `func(context.Context) error`) is reused by both host-prereq probes and
// workspace advisories; only the returned error says which kind of finding
// this particular failure is (see Warnf/Infof).
type Severity string

const (
	// SeverityError fails the run: `flywheel doctor`'s exit code is
	// non-zero iff at least one Result has this severity. Host-prereq
	// checks (git/k3d/docker/mkcert, live port collisions, unsupported
	// worktree/mount layouts) return plain errors and get this severity
	// by default — no per-check opt-in needed.
	SeverityError Severity = "error"
	// SeverityWarn is an advisory finding: printed distinctly but never
	// fails the run. Use Warnf for checks documented as "never gates
	// `up`" (workspace/local-only siblings, dev-convenience tooling like
	// pre-commit/yq/certutil).
	SeverityWarn Severity = "warn"
	// SeverityInfo is purely descriptive — no action implied.
	SeverityInfo Severity = "info"
)

// severityError tags an error returned by Check.Run with a Severity below
// SeverityError. Build one with Warnf or Infof; a plain error (anything
// that doesn't unwrap to *severityError) defaults to SeverityError.
type severityError struct {
	severity Severity
	err      error
}

func (e *severityError) Error() string { return e.err.Error() }
func (e *severityError) Unwrap() error { return e.err }

// Warnf returns an advisory finding: Run tags the Result SeverityWarn, the
// renderer prints it distinctly (style.Warn), and it never fails the run.
func Warnf(format string, a ...any) error {
	return &severityError{severity: SeverityWarn, err: fmt.Errorf(format, a...)}
}

// Infof is Warnf's purely-informational sibling.
func Infof(format string, a ...any) error {
	return &severityError{severity: SeverityInfo, err: fmt.Errorf(format, a...)}
}

// severityOf extracts the Severity tagged onto err (via Warnf/Infof),
// defaulting to SeverityError for anything else — including nil, though
// callers only consult Severity after checking Result.OK().
func severityOf(err error) Severity {
	var se *severityError
	if errors.As(err, &se) {
		return se.severity
	}
	return SeverityError
}

// Result aggregates a single Check's outcome.
type Result struct {
	Check Check
	Err   error
	// Severity is meaningful only when Err != nil (see OK). It is derived
	// from Err by Run — SeverityError unless the Check tagged its error
	// via Warnf/Infof.
	Severity Severity
}

func (r Result) OK() bool { return r.Err == nil }

// QuickChecks are the minimum set `flywheel up` step 2 runs before any
// network call.
func QuickChecks() []Check {
	return []Check{
		binaryCheck("git", "the user's own GitOps repo workflow"),
		binaryCheck("k3d", "the local cluster runtime"),
		dockerCheck(),
		mkcertCheck(),
	}
}

// mkcertCheck verifies mkcert is on PATH — Flywheel's local TLS depends
// on its OS trust-store integration.
func mkcertCheck() Check {
	return binaryCheck("mkcert", "OS trust-store integration for local TLS")
}

// defaultCheckTimeout is the per-check budget for probes that don't set
// their own Timeout — host prereq probes should be near-instantaneous.
const defaultCheckTimeout = 5 * time.Second

// Run executes every Check sequentially and returns the results in the
// same order. Each Check gets its Timeout (or defaultCheckTimeout when
// unset) as a context deadline.
func Run(checks []Check) []Result {
	results := make([]Result, len(checks))
	for i, c := range checks {
		timeout := c.Timeout
		if timeout == 0 {
			timeout = defaultCheckTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := c.Run(ctx)
		results[i] = Result{Check: c, Err: err, Severity: severityOf(err)}
		cancel()
	}
	return results
}

func binaryCheck(name, why string) Check {
	return Check{
		Name:        name,
		Description: why,
		Run: func(ctx context.Context) error {
			if _, err := exec.LookPath(name); err != nil {
				return fmt.Errorf("%s not on PATH: %w", name, err)
			}
			return nil
		},
	}
}

// advisoryBinaryCheck is like binaryCheck but a missing binary only
// produces a SeverityWarn finding — for dev-convenience tooling
// documented as never gating `up` (pre-commit hooks, NSS certutil).
func advisoryBinaryCheck(name, why string) Check {
	return Check{
		Name:        name,
		Description: why,
		Run: func(ctx context.Context) error {
			if _, err := exec.LookPath(name); err != nil {
				return Warnf("%s not on PATH: %v", name, err)
			}
			return nil
		},
	}
}

func dockerCheck() Check {
	return Check{
		Name:        "docker",
		Description: "k3d daemon + Flywheel's image-mirror step",
		// The daemon can be briefly unresponsive under load — a burst of
		// `docker build`s in CI, or a busy laptop — so the ping retries
		// with backoff rather than hard-failing on one slow probe. That
		// needs a larger budget than the default.
		Timeout: 15 * time.Second,
		Run: func(ctx context.Context) error {
			if _, err := exec.LookPath("docker"); err != nil {
				return fmt.Errorf("docker CLI not on PATH: %w", err)
			}
			return pingDocker(ctx)
		},
	}
}

// pingDocker probes `docker info` with bounded retries and a 1s backoff,
// returning nil on the first success or the last error once ctx's budget
// is spent. `docker info` exits non-zero when no daemon is reachable, so
// a single slow/transient failure shouldn't condemn the host.
func pingDocker(ctx context.Context) error {
	var lastErr error
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		out, err := exec.CommandContext(probeCtx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput()
		cancel()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("docker daemon unreachable: %w (%s)", err, strings.TrimSpace(string(out)))
		// Back off, but stop as soon as the overall budget is spent —
		// returning the real docker error, not a bare deadline.
		select {
		case <-ctx.Done():
			return lastErr
		case <-time.After(time.Second):
		}
	}
}
