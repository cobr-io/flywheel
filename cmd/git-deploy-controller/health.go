package main

import (
	"net/http"
	"time"
)

// tickSource is the loop's heartbeat, satisfied by *selfsync.Loop (see
// selfsync.Loop.LastTick). A narrow interface keeps this file testable without
// spinning up git repos or a Kubernetes client.
type tickSource interface {
	LastTick() time.Time
}

// readinessWindow is how many missed polls turn a "between ticks" gap into a
// stalled-loop signal (hung git/Flux call, deadlock, etc.).
const readinessWindow = 3

// newHealthMux builds the process's health-probe handler:
//   - /healthz (liveness): always 200 — the process is up and serving HTTP.
//     A hung Tick does not block this handler, so it never triggers a restart
//     for something a restart wouldn't fix (a stuck git remote, say).
//   - /readyz (readiness): 200 once the loop has completed at least one Tick
//     attempt and the last one wasn't more than readinessWindow polls ago;
//     503 otherwise (loop hasn't started yet, or is stuck mid-tick).
func newHealthMux(loop tickSource, pollInterval time.Duration) *http.ServeMux {
	maxAge := time.Duration(readinessWindow) * pollInterval
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready(loop.LastTick(), maxAge, time.Now()) {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	return mux
}

// ready reports whether last (the loop's last completed-Tick timestamp) is
// recent enough, as of now, to call the loop live. A zero last (no completed
// iteration yet) is never ready.
func ready(last time.Time, maxAge time.Duration, now time.Time) bool {
	return !last.IsZero() && now.Sub(last) <= maxAge
}
