package style

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// WaitStatus is the lifecycle state of a single watched item.
type WaitStatus int

const (
	// Pending means we haven't observed the item Ready or Failed yet.
	// Default zero-value, so a freshly registered item starts pending.
	Pending WaitStatus = iota
	// Ready means the item reached its terminal success condition
	// (Deployment status.availableReplicas met spec, Flux Kustomization
	// status.conditions[type=Ready].status==True, etc.).
	Ready
	// Failed means the item reached a terminal failure (the wait
	// function gives up and reports it; Failed items don't keep the
	// Waiter going).
	Failed
)

// WaitItem is the per-row state in a Waiter's block. Callers create
// items via Waiter.Add, then mutate them via Waiter.Set on each poll.
//
// Detail is free-form text rendered to the right of Label — typically
// the current condition ("pulling image", "blocked on: client-infra",
// "ready"). Empty is fine for items where the status alone is the
// whole story.
type WaitItem struct {
	Label  string
	Status WaitStatus
	Detail string
	Since  time.Time // when the item was first registered; drives the elapsed-time column
}

// Waiter draws an in-place updating block of WaitItems on a TTY, with
// a graceful fallback to periodic heartbeat lines off-TTY. One Waiter
// per long-running wait; lifecycle:
//
//	w := style.NewWaiter(out, "waiting for Flux Kustomizations")
//	w.Add("flywheel-dev-loop")
//	w.Add("client-infra")
//	for !done {
//	    w.Set("flywheel-dev-loop", style.Ready, "ready")
//	    w.Set("client-infra", style.Pending, "blocked on: flywheel-dev-loop")
//	    w.Tick()                  // redraws (TTY) or maybe-heartbeats (non-TTY)
//	    time.Sleep(2 * time.Second)
//	}
//	w.Done("Flux Kustomizations Ready (2m12s)")
//
// Waiter is NOT goroutine-safe — the typical wait pollers in
// `flywheel up` are sequential anyway, and serialising avoids the
// scrollback corruption you'd get from concurrent writers to the
// in-place block.
type Waiter struct {
	out      io.Writer
	header   string
	items    map[string]*WaitItem
	order    []string // insertion order, so the row layout is deterministic across redraws
	tty      bool     // captured at NewWaiter time so the test path can override
	lastRows int      // number of rows last drawn — used to compute the cursor-up jump
	spin     int      // current braille-spinner frame; advances on every Tick

	// Off-TTY pacing: emit a heartbeat every `heartbeatEvery` Ticks
	// when there's been no status change. (When a status changes off-
	// TTY, we emit immediately so the log captures the transition.)
	heartbeatEvery int
	heartbeatTicks int
	lastSnapshot   string
	started        time.Time
}

// braille spinner frames; 10-frame cycle reads as smooth motion at
// ~5–10 fps. We advance one frame per Tick regardless of poll rate,
// so a 2s poll renders ~30 spinner positions per minute — visible
// motion without being distracting.
var spinFrames = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}

// NewWaiter creates a Waiter writing to `out`. `header` is the step
// header printed once above the block (e.g. "waiting for Flux
// Kustomizations Ready"). Header is rendered via Step, so it gets
// the bold-cyan ▶ treatment in TTY mode.
func NewWaiter(out io.Writer, header string) *Waiter {
	w := &Waiter{
		out:            out,
		header:         header,
		items:          map[string]*WaitItem{},
		tty:            enabled, // ride the same flag as colour: TTY ⇒ redraw, plain ⇒ heartbeat
		heartbeatEvery: 10,      // ~20s at a 2s poll
		started:        time.Now(),
	}
	Step(out, "%s", header)
	return w
}

// Add registers a new row. Idempotent — calling Add for an existing
// label is a no-op. The row starts Pending with empty Detail and is
// rendered on the next Tick.
func (w *Waiter) Add(label string) {
	if _, ok := w.items[label]; ok {
		return
	}
	w.items[label] = &WaitItem{Label: label, Since: time.Now()}
	w.order = append(w.order, label)
}

// Set updates the status + detail of a row (creating it if new).
// Status changes trigger an off-TTY immediate flush so logs capture
// the transition; status-unchanged calls just update the detail and
// elapsed time for the next Tick.
func (w *Waiter) Set(label string, status WaitStatus, detail string) {
	it, ok := w.items[label]
	if !ok {
		w.Add(label)
		it = w.items[label]
	}
	changed := it.Status != status
	it.Status = status
	it.Detail = detail
	if !w.tty && changed {
		w.flushHeartbeat()
	}
}

// AllResolved returns true iff every registered item is Ready or
// Failed — the wait can exit. With zero items it's vacuously true,
// which matches "nothing to wait for".
func (w *Waiter) AllResolved() bool {
	for _, it := range w.items {
		if it.Status == Pending {
			return false
		}
	}
	return true
}

// Tick advances the spinner and redraws the block. Callers invoke
// Tick once per poll cycle, regardless of whether anything changed —
// the spinner animation depends on it.
func (w *Waiter) Tick() {
	w.spin = (w.spin + 1) % len(spinFrames)
	if w.tty {
		w.redrawTTY()
		return
	}
	w.heartbeatTicks++
	if w.heartbeatTicks >= w.heartbeatEvery {
		w.heartbeatTicks = 0
		w.flushHeartbeat()
	}
}

// Done collapses the live block (TTY) or stops the heartbeat (off-
// TTY) and emits a final stable line via Summary. Use the summary
// to record the outcome in scrollback (e.g. "Flux Kustomizations
// Ready (2m12s)"). Done is a no-op if Tick was never called.
func (w *Waiter) Done(summary string) {
	if w.tty && w.lastRows > 0 {
		fmt.Fprintf(w.out, "\033[%dA\033[J", w.lastRows)
		w.lastRows = 0
	}
	if summary != "" {
		// We render the summary as an OK line so it stays consistent
		// with the rest of the bootstrap output and respects the
		// dim-when-routine convention.
		OK(w.out, "%s", summary)
	}
}

// redrawTTY moves the cursor up over the previously drawn block,
// clears to end-of-screen, and re-emits the rows with current state.
// First call has lastRows=0 so the cursor stays in place; subsequent
// calls replace the prior render.
func (w *Waiter) redrawTTY() {
	if w.lastRows > 0 {
		fmt.Fprintf(w.out, "\033[%dA\033[J", w.lastRows)
	}
	rows := w.sortedRows()
	for _, it := range rows {
		fmt.Fprintln(w.out, w.formatRow(it))
	}
	w.lastRows = len(rows)
}

// flushHeartbeat emits a single plain-text line summarising current
// state. Off-TTY mode uses it on status changes AND on a fixed cadence
// (every `heartbeatEvery` Ticks) so a hung wait still produces log
// entries.
func (w *Waiter) flushHeartbeat() {
	snap := w.snapshotLine()
	if snap == w.lastSnapshot {
		return // suppress duplicate consecutive heartbeats
	}
	w.lastSnapshot = snap
	Detail(w.out, "%s", snap)
}

func (w *Waiter) snapshotLine() string {
	ready, pending, failed := w.counts()
	parts := []string{
		fmt.Sprintf("%d/%d ready", ready, ready+pending+failed),
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	if pending > 0 {
		oldest := w.oldestPending()
		parts = append(parts, fmt.Sprintf("oldest pending: %s (%s)", oldest.Label, durStr(time.Since(oldest.Since))))
	}
	return strings.Join(parts, ", ")
}

func (w *Waiter) counts() (ready, pending, failed int) {
	for _, it := range w.items {
		switch it.Status {
		case Ready:
			ready++
		case Failed:
			failed++
		default:
			pending++
		}
	}
	return
}

func (w *Waiter) oldestPending() *WaitItem {
	var oldest *WaitItem
	for _, label := range w.order {
		it := w.items[label]
		if it.Status != Pending {
			continue
		}
		if oldest == nil || it.Since.Before(oldest.Since) {
			oldest = it
		}
	}
	return oldest
}

func (w *Waiter) sortedRows() []*WaitItem {
	rows := make([]*WaitItem, 0, len(w.order))
	for _, label := range w.order {
		rows = append(rows, w.items[label])
	}
	// Pending rows sorted by registration time (already in order);
	// resolved rows bubble up so the "still waiting" rows stay
	// visually anchored near the bottom of the block. This matches
	// what most CI/installer UIs do.
	sort.SliceStable(rows, func(i, j int) bool {
		return statusRank(rows[i].Status) < statusRank(rows[j].Status)
	})
	return rows
}

func statusRank(s WaitStatus) int {
	switch s {
	case Ready:
		return 0
	case Failed:
		return 1
	default: // Pending
		return 2
	}
}

// formatRow lays out a single row: `<glyph> <label-26> <detail-32> <elapsed>`.
// Widths chosen to fit typical k8s resource names without wrapping in
// an 80-col terminal. Long names/details are truncated with `…` (we
// favour terminal width over perfect fidelity).
func (w *Waiter) formatRow(it *WaitItem) string {
	const (
		labelW  = 26
		detailW = 32
	)
	label := truncate(it.Label, labelW)
	detail := truncate(it.Detail, detailW)

	if !w.tty {
		// Plain mode never reaches here in normal use (Tick routes
		// off-TTY to flushHeartbeat); keep the format defensive for
		// callers that bypass the lifecycle.
		return fmt.Sprintf("  %s %-*s %-*s %s",
			plainGlyph(it.Status), labelW, label, detailW, detail,
			durStr(time.Since(it.Since)))
	}
	g, gColor := w.glyphFor(it)
	return fmt.Sprintf("  %s%s%s %s%-*s  %-*s  %s%s",
		gColor, g, reset,
		dim, labelW, label,
		detailW, detail,
		durStr(time.Since(it.Since)), reset,
	)
}

func plainGlyph(s WaitStatus) string {
	switch s {
	case Ready:
		return "ok"
	case Failed:
		return "FAIL"
	default:
		return "..."
	}
}

func (w *Waiter) glyphFor(it *WaitItem) (string, string) {
	switch it.Status {
	case Ready:
		return glyphOK, green
	case Failed:
		return glyphFail, red
	default:
		return string(spinFrames[w.spin]), cyan
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	return s[:max-1] + "…"
}

// durStr renders a time.Duration in a compact "12s" / "1m24s" form
// without trailing zero units. Used in the elapsed column.
func durStr(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d / time.Minute)
	s := int((d % time.Minute) / time.Second)
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm%02ds", m, s)
}
