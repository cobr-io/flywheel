package style

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

// Regression for #158: the redraw moves up a single row, so a frame
// wider than the terminal wraps and leaves its earlier rows behind.
// Every frame must fit within the (fallback 80-column) width.
func TestSpin_TTY_LongLabelFitsTerminalWidth(t *testing.T) {
	withEnabled(t, true, func() {
		prev := verbose
		t.Cleanup(func() { verbose = prev })
		verbose = false
		var buf bytes.Buffer

		label := "mirror ghcr.io/cobr-io/image-builder-controller:v0.5.1 → local registry (released image, pulled from ghcr)"
		_ = Spin(&buf, label, func() error {
			time.Sleep(150 * time.Millisecond)
			return nil
		})

		ansi := regexp.MustCompile(`\033\[[0-9;]*[A-Za-z]`)
		frames := 0
		for _, line := range strings.Split(ansi.ReplaceAllString(buf.String(), ""), "\n") {
			// Skip the stable ✓ summary: it lands in scrollback and
			// is never redrawn, so wrapping it is harmless.
			if line == "" || strings.Contains(line, "✓") {
				continue
			}
			frames++
			if n := utf8.RuneCountInString(line); n >= 80 {
				t.Errorf("spinner frame is %d columns, want < 80: %q", n, line)
			}
			if !strings.Contains(line, "…") {
				t.Errorf("long label should be cut with …: %q", line)
			}
		}
		if frames == 0 {
			t.Fatalf("no spinner frames drawn: %q", buf.String())
		}
	})
}
