package style

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestWaiter_OffTTY_HeartbeatOnStatusChange(t *testing.T) {
	withEnabled(t, false, func() {
		var buf bytes.Buffer
		w := NewWaiter(&buf, "waiting")
		w.tty = false // explicit; matches what NewWaiter captured
		w.Add("a")
		w.Add("b")

		// Flip a to Ready; off-TTY should emit a heartbeat line now,
		// not wait for `heartbeatEvery` Ticks.
		w.Set("a", Ready, "ready")
		got := buf.String()
		if !strings.Contains(got, "1/2 ready") {
			t.Errorf("expected '1/2 ready' heartbeat after status change, got:\n%s", got)
		}
	})
}

func TestWaiter_OffTTY_HeartbeatSuppressesDuplicates(t *testing.T) {
	withEnabled(t, false, func() {
		var buf bytes.Buffer
		w := NewWaiter(&buf, "waiting")
		w.tty = false
		w.heartbeatEvery = 1 // tick every call
		w.Add("a")

		// Two ticks with no change → only the first heartbeat emits.
		w.Tick()
		w.Tick()
		hb := strings.Count(buf.String(), "0/1 ready")
		if hb != 1 {
			t.Errorf("expected 1 deduped heartbeat, got %d in:\n%s", hb, buf.String())
		}
	})
}

func TestWaiter_AllResolved_RespectsFailed(t *testing.T) {
	withEnabled(t, false, func() {
		var buf bytes.Buffer
		w := NewWaiter(&buf, "x")
		w.Add("a")
		w.Add("b")
		if w.AllResolved() {
			t.Fatal("AllResolved should be false with only pending items")
		}
		w.Set("a", Ready, "")
		w.Set("b", Failed, "")
		if !w.AllResolved() {
			t.Fatal("AllResolved should be true when items are Ready or Failed")
		}
	})
}

func TestWaiter_TTY_FirstTickPrintsBlock_SecondReusesCursor(t *testing.T) {
	withEnabled(t, true, func() {
		var buf bytes.Buffer
		w := NewWaiter(&buf, "x")
		w.tty = true
		w.Add("acme-1")
		w.Add("acme-2")

		w.Tick()
		first := buf.String()
		// First Tick: header + 2 rows; no cursor-up sequence yet.
		if strings.Contains(first, "\033[2A") {
			t.Errorf("first Tick should not emit cursor-up, got:\n%q", first)
		}

		w.Tick()
		// Second Tick: cursor-up over 2 rows, then redraw 2 rows.
		after := strings.TrimPrefix(buf.String(), first)
		if !strings.HasPrefix(after, "\033[2A\033[J") {
			t.Errorf("second Tick should start with cursor-up + clear, got:\n%q", after)
		}
	})
}

func TestWaiter_TTY_Done_ClearsBlockAndPrintsSummary(t *testing.T) {
	withEnabled(t, true, func() {
		var buf bytes.Buffer
		w := NewWaiter(&buf, "x")
		w.tty = true
		w.Add("a")
		w.Tick()
		buf.Reset()

		w.Done("a is ready (12s)")
		got := buf.String()
		if !strings.HasPrefix(got, "\033[1A\033[J") {
			t.Errorf("Done should clear the prior block first, got:\n%q", got)
		}
		if !strings.Contains(got, "a is ready (12s)") {
			t.Errorf("Done summary missing from output:\n%q", got)
		}
	})
}

func TestDurStr_Formats(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{500 * time.Millisecond, "1s"}, // rounds up to nearest second
		{12 * time.Second, "12s"},
		{60 * time.Second, "1m"},
		{84 * time.Second, "1m24s"},
		{2*time.Minute + 2*time.Second, "2m02s"},
	}
	for _, c := range cases {
		if got := durStr(c.in); got != c.want {
			t.Errorf("durStr(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTruncate_Ellipsis(t *testing.T) {
	if got := truncate("client-builders", 26); got != "client-builders" {
		t.Errorf("short label shouldn't truncate, got %q", got)
	}
	if got := truncate("a-very-long-deployment-name-that-overflows", 26); got != "a-very-long-deployment-na…" {
		t.Errorf("truncate w/ ellipsis: got %q", got)
	}
}
