package style

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSpin_OffMode_PrintsLabelAndOutcome(t *testing.T) {
	withEnabled(t, false, func() {
		var buf bytes.Buffer
		err := Spin(&buf, "doing thing", func() error { return nil })
		if err != nil {
			t.Fatalf("Spin: %v", err)
		}
		got := buf.String()
		if !strings.Contains(got, "→ doing thing") {
			t.Errorf("off-mode missing initial step header: %q", got)
		}
		if !strings.Contains(got, "  ok doing thing") {
			t.Errorf("off-mode missing ok outcome: %q", got)
		}
	})
}

func TestSpin_OffMode_PropagatesErr_PrintsWarn(t *testing.T) {
	withEnabled(t, false, func() {
		var buf bytes.Buffer
		boom := errors.New("kaboom")
		err := Spin(&buf, "doing thing", func() error { return boom })
		if err != boom {
			t.Errorf("Spin should propagate fn error verbatim, got %v", err)
		}
		got := buf.String()
		if !strings.Contains(got, "WARN doing thing failed") {
			t.Errorf("off-mode missing failure summary: %q", got)
		}
	})
}

func TestSpin_VerboseMode_NoAnimation_StillSurfacesOutcome(t *testing.T) {
	withEnabled(t, true, func() {
		prev := verbose
		t.Cleanup(func() { verbose = prev })
		verbose = true
		var buf bytes.Buffer
		_ = Spin(&buf, "x", func() error { return nil })
		got := buf.String()
		// Verbose mode prints the step header (so subprocess output
		// can land beneath it) and a final ✓ summary; no cursor-up
		// escape sequence in between.
		if strings.Contains(got, "\033[1A") {
			t.Errorf("verbose mode should not animate, got cursor-up in: %q", got)
		}
		if !strings.Contains(got, "▶") {
			t.Errorf("verbose mode missing step header: %q", got)
		}
		if !strings.Contains(got, "✓") {
			t.Errorf("verbose mode missing final ok summary: %q", got)
		}
	})
}

func TestSpin_TTY_AnimatesThenClearsBeforeSummary(t *testing.T) {
	withEnabled(t, true, func() {
		prev := verbose
		t.Cleanup(func() { verbose = prev })
		verbose = false
		var buf bytes.Buffer

		// fn sleeps long enough for at least one redraw tick.
		err := Spin(&buf, "long thing", func() error {
			time.Sleep(150 * time.Millisecond)
			return nil
		})
		if err != nil {
			t.Fatalf("Spin: %v", err)
		}
		got := buf.String()
		// We expect: at least one frame drawn (no cursor-up) + at
		// least one redraw (one cursor-up before the final clear) +
		// the final clear-before-summary.
		ups := strings.Count(got, "\033[1A\033[J")
		if ups < 2 {
			t.Errorf("expected >=2 cursor-up+clear sequences (redraws + final clear), got %d in:\n%q", ups, got)
		}
		// The final stable summary lands after the last clear.
		if !strings.Contains(got, "✓") {
			t.Errorf("TTY mode missing final ✓ summary: %q", got)
		}
	})
}
