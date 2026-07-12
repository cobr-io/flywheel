package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mutableTick lets a test move the clock forward on an otherwise-fixed
// tickSource, simulating a loop that stalls mid-run.
type mutableTick struct{ t time.Time }

func (m *mutableTick) LastTick() time.Time { return m.t }

func TestReady(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		last   time.Time
		maxAge time.Duration
		want   bool
	}{
		{"never ticked", time.Time{}, time.Second, false},
		{"fresh tick", now, 3 * time.Second, true},
		{"tick exactly at the edge", now.Add(-3 * time.Second), 3 * time.Second, true},
		{"stale tick", now.Add(-10 * time.Second), 3 * time.Second, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ready(tt.last, tt.maxAge, now); got != tt.want {
				t.Errorf("ready(%v, %v, %v) = %v, want %v", tt.last, tt.maxAge, now, got, tt.want)
			}
		})
	}
}

// TestHealthMux_HealthzAlwaysOK proves liveness never depends on the loop's
// tick state — a stalled loop must not flip liveness (only a restart-worthy
// process failure should).
func TestHealthMux_HealthzAlwaysOK(t *testing.T) {
	m := &mutableTick{} // never ticked
	srv := httptest.NewServer(newHealthMux(m, time.Second))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", resp.StatusCode)
	}
}

// TestHealthMux_ReadyzFlipsOnStall is the plan's required test: a loop that
// stops ticking turns readiness false, and a fresh tick turns it back true.
func TestHealthMux_ReadyzFlipsOnStall(t *testing.T) {
	const poll = 100 * time.Millisecond
	m := &mutableTick{} // never ticked yet
	mux := newHealthMux(m, poll)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func() int {
		resp, err := http.Get(srv.URL + "/readyz")
		if err != nil {
			t.Fatalf("GET /readyz: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := get(); got != http.StatusServiceUnavailable {
		t.Fatalf("readyz before any tick = %d, want 503", got)
	}

	m.t = time.Now()
	if got := get(); got != http.StatusOK {
		t.Fatalf("readyz right after a tick = %d, want 200", got)
	}

	// Simulate a stalled loop: the last tick is now well past readinessWindow polls ago.
	m.t = time.Now().Add(-time.Duration(readinessWindow+1) * poll)
	if got := get(); got != http.StatusServiceUnavailable {
		t.Fatalf("readyz after a stall = %d, want 503", got)
	}

	// A fresh tick recovers readiness.
	m.t = time.Now()
	if got := get(); got != http.StatusOK {
		t.Fatalf("readyz after recovery = %d, want 200", got)
	}
}
