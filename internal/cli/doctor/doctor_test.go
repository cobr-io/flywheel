package doctor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Run must honor a Check's per-check Timeout: a probe that blocks until its
// context is cancelled should return promptly with the deadline error,
// bounded by the Check's Timeout rather than the default.
func TestRunHonorsPerCheckTimeout(t *testing.T) {
	c := Check{
		Name:    "blocks-until-ctx-done",
		Timeout: 50 * time.Millisecond,
		Run: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}

	start := time.Now()
	res := Run([]Check{c})
	elapsed := time.Since(start)

	if res[0].OK() {
		t.Fatal("expected a timeout error, got OK")
	}
	if !errors.Is(res[0].Err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", res[0].Err)
	}
	if elapsed > defaultCheckTimeout/2 {
		t.Fatalf("per-check Timeout not honored: took %s (default is %s)", elapsed, defaultCheckTimeout)
	}
}

// An unset Timeout falls back to the default budget.
func TestRunDefaultTimeoutForZero(t *testing.T) {
	var gotDeadline bool
	c := Check{
		Name: "inspects-deadline",
		Run: func(ctx context.Context) error {
			dl, ok := ctx.Deadline()
			gotDeadline = ok && time.Until(dl) <= defaultCheckTimeout
			return nil
		},
	}
	if res := Run([]Check{c}); !res[0].OK() {
		t.Fatalf("unexpected error: %v", res[0].Err)
	}
	if !gotDeadline {
		t.Fatal("zero Timeout should fall back to defaultCheckTimeout")
	}
}

// A Check that returns a plain error (the pre-existing pattern used by every
// host-prereq probe) must default to SeverityError — this is the backward-
// compatibility guarantee that let T25 land without touching git/k3d/docker/
// mkcert/ports/worktree/mount.
func TestRun_PlainErrorDefaultsToSeverityError(t *testing.T) {
	c := Check{
		Name: "plain-error",
		Run:  func(ctx context.Context) error { return errors.New("boom") },
	}
	res := Run([]Check{c})[0]
	if res.OK() {
		t.Fatal("expected a failure")
	}
	if res.Severity != SeverityError {
		t.Errorf("Severity = %q, want %q", res.Severity, SeverityError)
	}
}

// A Check that opts in via Warnf must surface as SeverityWarn, and the
// original message must still be reachable via Err.Error() (renderers and
// tests both format Err directly).
func TestRun_WarnfTagsSeverityWarn(t *testing.T) {
	c := Check{
		Name: "advisory",
		Run:  func(ctx context.Context) error { return Warnf("missing sibling: %s", "web") },
	}
	res := Run([]Check{c})[0]
	if res.OK() {
		t.Fatal("expected a failure")
	}
	if res.Severity != SeverityWarn {
		t.Errorf("Severity = %q, want %q", res.Severity, SeverityWarn)
	}
	if res.Err.Error() != "missing sibling: web" {
		t.Errorf("Err.Error() = %q, want the formatted message verbatim", res.Err.Error())
	}
}

// Infof mirrors Warnf at SeverityInfo.
func TestRun_InfofTagsSeverityInfo(t *testing.T) {
	c := Check{
		Name: "info",
		Run:  func(ctx context.Context) error { return Infof("fyi") },
	}
	res := Run([]Check{c})[0]
	if res.Severity != SeverityInfo {
		t.Errorf("Severity = %q, want %q", res.Severity, SeverityInfo)
	}
}

// A Warnf error wrapped by another layer (e.g. fmt.Errorf("...: %w", ...))
// must still resolve to SeverityWarn via errors.As/Unwrap.
func TestRun_WrappedWarnfStillResolves(t *testing.T) {
	c := Check{
		Name: "wrapped-advisory",
		Run: func(ctx context.Context) error {
			return fmt.Errorf("context: %w", Warnf("inner"))
		},
	}
	res := Run([]Check{c})[0]
	if res.Severity != SeverityWarn {
		t.Errorf("Severity = %q, want %q", res.Severity, SeverityWarn)
	}
}
