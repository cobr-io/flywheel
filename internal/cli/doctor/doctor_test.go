package doctor

import (
	"context"
	"errors"
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
