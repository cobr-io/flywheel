package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// newTestGate builds a listGatedRunnable with injected probe/register seams and
// a tiny interval, so the re-probe logic is exercised without a real manager or
// rest.Config.
func newTestGate(probe func(context.Context) (bool, error), register func() error) *listGatedRunnable {
	return &listGatedRunnable{
		name:     "test",
		gvk:      schema.GroupVersionKind{Kind: "Widget"},
		log:      logr.Discard(),
		interval: time.Millisecond,
		probe:    probe,
		register: register,
	}
}

// TestListGate_RegistersWhenListable: the poke controller registers as soon as
// the probe reports the kind listable, then the gate stops.
func TestListGate_RegistersWhenListable(t *testing.T) {
	registered := 0
	g := newTestGate(
		func(context.Context) (bool, error) { return true, nil },
		func() error { registered++; return nil },
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := g.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if registered != 1 {
		t.Errorf("register called %d times, want exactly 1", registered)
	}
}

// TestListGate_RetriesUntilListable: while the kind is unlistable (RBAC not yet
// landed) the gate keeps re-probing, and registers once the permission appears —
// the recover-without-restart behaviour.
func TestListGate_RetriesUntilListable(t *testing.T) {
	probes, registered := 0, 0
	g := newTestGate(
		func(context.Context) (bool, error) {
			probes++
			return probes >= 3, nil // listable only from the 3rd probe on
		},
		func() error { registered++; return nil },
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := g.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if registered != 1 {
		t.Errorf("register called %d times, want exactly 1 (once listable)", registered)
	}
	if probes < 3 {
		t.Errorf("expected at least 3 probes before listable, got %d", probes)
	}
}

// TestListGate_StopsOnContextCancel: a gate that never becomes listable exits
// cleanly (nil) on context cancellation and never registers.
func TestListGate_StopsOnContextCancel(t *testing.T) {
	registered := 0
	g := newTestGate(
		func(context.Context) (bool, error) { return false, errors.New("forbidden") },
		func() error { registered++; return nil },
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Start(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned %v after cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancel")
	}
	if registered != 0 {
		t.Errorf("register called %d times, want 0 (never listable)", registered)
	}
}

// TestListGate_RetriesWhenRegisterFails: a transient registration failure after
// the kind is listable does not wedge the gate — it retries until register
// succeeds.
func TestListGate_RetriesWhenRegisterFails(t *testing.T) {
	registered := 0
	g := newTestGate(
		func(context.Context) (bool, error) { return true, nil },
		func() error {
			registered++
			if registered < 2 {
				return errors.New("transient registration error")
			}
			return nil
		},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := g.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if registered != 2 {
		t.Errorf("register called %d times, want 2 (one failure, one success)", registered)
	}
}
