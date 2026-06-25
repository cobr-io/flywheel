// Package doctor runs the host-prerequisite checks documented in design
// § Prerequisites. `doctor --quick` runs the minimal set wired into
// `flywheel up` step 2; `doctor` (full mode) adds profile-specific
// checks and live-port-collision validation against the allocator.
package doctor

import (
	"context"
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

// Result aggregates a single Check's outcome.
type Result struct {
	Check Check
	Err   error
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
		results[i] = Result{Check: c, Err: c.Run(ctx)}
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
