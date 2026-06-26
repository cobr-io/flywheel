package style

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// unsetEnv removes the var for the test duration. (`t.Setenv("X", "")`
// SETS the var to empty, which the NO_COLOR spec still treats as
// "color disabled" — see https://no-color.org. To test
// CLICOLOR_FORCE we need NO_COLOR genuinely absent.)
func unsetEnv(t *testing.T, name string) {
	t.Helper()
	prev, had := os.LookupEnv(name)
	t.Cleanup(func() {
		if had {
			os.Setenv(name, prev)
		} else {
			os.Unsetenv(name)
		}
	})
	os.Unsetenv(name)
}

// withEnabled temporarily flips the global enabled flag and restores
// it after the test body runs. Lets each test pick its own mode
// without poking the package state from outside.
func withEnabled(t *testing.T, on bool, body func()) {
	t.Helper()
	prev := enabled
	t.Cleanup(func() { enabled = prev })
	enabled = on
	body()
}

func TestStep_OffMode_PlainArrow(t *testing.T) {
	withEnabled(t, false, func() {
		var buf bytes.Buffer
		Step(&buf, "installing Flux v%s", "2.8.7")
		got := buf.String()
		if got != "→ installing Flux v2.8.7\n" {
			t.Errorf("Step off-mode = %q, want plain arrow form", got)
		}
	})
}

func TestStep_OnMode_BoldCyanWithGlyph(t *testing.T) {
	withEnabled(t, true, func() {
		var buf bytes.Buffer
		Step(&buf, "installing Flux v2.8.7")
		got := buf.String()
		if !strings.Contains(got, "\033[1;36m") {
			t.Errorf("Step on-mode missing bold-cyan ANSI: %q", got)
		}
		if !strings.Contains(got, "▶") {
			t.Errorf("Step on-mode missing ▶ glyph: %q", got)
		}
		if !strings.HasSuffix(got, "\033[0m\n") {
			t.Errorf("Step on-mode not reset-terminated: %q", got)
		}
	})
}

func TestOK_OffMode_PlainOK(t *testing.T) {
	withEnabled(t, false, func() {
		var buf bytes.Buffer
		OK(&buf, "deployment/%s/%s", "ns", "name")
		if got := buf.String(); got != "  ok deployment/ns/name\n" {
			t.Errorf("OK off-mode = %q", got)
		}
	})
}

func TestOK_OnMode_DimWithCheck(t *testing.T) {
	withEnabled(t, true, func() {
		var buf bytes.Buffer
		OK(&buf, "x")
		got := buf.String()
		if !strings.Contains(got, "\033[2m") {
			t.Errorf("OK on-mode missing dim ANSI: %q", got)
		}
		if !strings.Contains(got, "✓") {
			t.Errorf("OK on-mode missing ✓ glyph: %q", got)
		}
	})
}

func TestWarn_OffMode_PlainWARN(t *testing.T) {
	withEnabled(t, false, func() {
		var buf bytes.Buffer
		Warn(&buf, "step %d failed", 14)
		if got := buf.String(); got != "WARN step 14 failed\n" {
			t.Errorf("Warn off-mode = %q", got)
		}
	})
}

func TestErr_OffMode_PlainFAIL(t *testing.T) {
	withEnabled(t, false, func() {
		var buf bytes.Buffer
		Err(&buf, "thing %s", "broke")
		if got := buf.String(); got != "FAIL thing broke\n" {
			t.Errorf("Err off-mode = %q", got)
		}
	})
}

func TestDetail_TwoSpaceIndent(t *testing.T) {
	withEnabled(t, false, func() {
		var buf bytes.Buffer
		Detail(&buf, "ports: %d/%d/%d", 50001, 8080, 8540)
		if got := buf.String(); got != "  ports: 50001/8080/8540\n" {
			t.Errorf("Detail off-mode = %q", got)
		}
	})
}

func TestSummary_OffMode_NoPrefix(t *testing.T) {
	withEnabled(t, false, func() {
		var buf bytes.Buffer
		Summary(&buf, "Cluster up. Visit %s", "https://hello.localdev.me")
		if got := buf.String(); got != "Cluster up. Visit https://hello.localdev.me\n" {
			t.Errorf("Summary off-mode = %q", got)
		}
	})
}

func TestInit_NoColorEnvForcesOff(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("CLICOLOR_FORCE", "")
	Init(false)
	if enabled {
		t.Error("Init with NO_COLOR set should leave enabled=false")
	}
}

func TestInit_CliColorForceTrumpsTTYDetection(t *testing.T) {
	unsetEnv(t, "NO_COLOR")
	t.Setenv("CLICOLOR_FORCE", "1")
	Init(false)
	if !enabled {
		t.Error("Init with CLICOLOR_FORCE=1 should force enabled=true even off-TTY")
	}
}

func TestInit_ForceOffWins(t *testing.T) {
	unsetEnv(t, "NO_COLOR")
	t.Setenv("CLICOLOR_FORCE", "1")
	Init(true)
	if enabled {
		t.Error("Init(true) should force enabled=false regardless of CLICOLOR_FORCE")
	}
}
