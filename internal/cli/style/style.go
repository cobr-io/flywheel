// Package style is the CLI's output styler: ANSI colours + Unicode
// glyphs for TTY callers, plain ASCII for everything else.
//
// Discovery flow at process start:
//
//  1. Init(forceOff) sets the global Enabled state once, honoring:
//     - explicit `forceOff` (e.g. a --no-color flag) → off;
//     - NO_COLOR env (any value)                    → off;
//     - CLICOLOR_FORCE=1 env                        → on, even off-TTY;
//     - else: on iff stdout is a TTY.
//
//  2. Every Step/OK/Warn/Err/Detail/Summary call branches on Enabled.
//     Off → ASCII-only prefix (`→`, `ok`, `WARN`, `FAIL`) — backwards
//     compatible with anything that greps the previous output.
//
//     On → Unicode glyph + ANSI 8-color, with `Reset` after each line
//     so any downstream tool that ate part of our output keeps its
//     own colour state intact.
//
// The package has no third-party dependencies beyond golang.org/x/term
// (already an indirect dep). Hand-rolled rather than via fatih/color
// because the surface is small enough that owning the bytes is worth
// it; see CHANGELOG for the design rationale.
package style

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// VerboseWriter returns `w` when --verbose is on, else io.Discard.
// Used to gate noisy subprocess output (k3d, docker, mkcert) and
// kubectl/Flux klog chatter so the default UX is clean, and -v
// surfaces everything for diagnosis.
func VerboseWriter(w io.Writer) io.Writer {
	if verbose {
		return w
	}
	return io.Discard
}

// ANSI control sequences — 8-color basic set, terminal-safe.
const (
	reset      = "\033[0m"
	bold       = "\033[1m"
	dim        = "\033[2m"
	red        = "\033[31m"
	green      = "\033[32m"
	yellow     = "\033[33m"
	cyan       = "\033[36m"
	boldCyan   = "\033[1;36m"
	boldYellow = "\033[1;33m"
	boldRed    = "\033[1;31m"
)

// Unicode glyphs (single grapheme each in any modern terminal font).
const (
	glyphStep = "▶"
	glyphOK   = "✓"
	glyphWarn = "⚠"
	glyphFail = "✗"
)

// enabled is the global flag set once by Init. Default false so any
// caller that forgets to Init (mostly tests) still gets plain output.
var enabled bool

// verbose is the global -v/--verbose flag captured once by SetVerbose.
// When false, third-party tool output (k3d, docker, mkcert, klog,
// Flux's apply per-resource chatter) is routed to io.Discard via
// VerboseWriter; user-facing Step/OK/Warn lines are unaffected.
var verbose bool

// SetVerbose toggles the verbose flag. Call once from main with the
// parsed -v/--verbose value.
func SetVerbose(v bool) { verbose = v }

// Verbose reports whether the -v/--verbose flag was set.
func Verbose() bool { return verbose }

// Init configures the package for the rest of this process. Pass
// forceOff=true to honor an explicit `--no-color` style flag from the
// CLI front door; otherwise it's the env + TTY logic described in
// the package doc.
func Init(forceOff bool) {
	switch {
	case forceOff:
		enabled = false
	case envSet("NO_COLOR"):
		enabled = false
	case os.Getenv("CLICOLOR_FORCE") == "1":
		enabled = true
	default:
		enabled = term.IsTerminal(int(os.Stdout.Fd()))
	}
}

// Enabled reports whether colour + glyph output is on. Callers that
// need to lay out tabular output should branch on this so widths
// match the rendered glyph width.
func Enabled() bool { return enabled }

func envSet(name string) bool {
	_, ok := os.LookupEnv(name)
	return ok
}

// wrap applies an ANSI code (and reset) around text when colour is on.
// When off it returns text untouched — callers don't have to branch.
func wrap(code, text string) string {
	if !enabled {
		return text
	}
	return code + text + reset
}

// Step prints a top-level step header. Bold cyan + ▶ glyph in TTY mode,
// plain `→ <line>` otherwise. Always newline-terminated.
func Step(w io.Writer, format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	if enabled {
		fmt.Fprintf(w, "%s%s %s%s\n", boldCyan, glyphStep, line, reset)
	} else {
		fmt.Fprintf(w, "→ %s\n", line)
	}
}

// OK prints a sub-step success line. Dim + ✓ in TTY mode, plain
// `  ok <line>` otherwise. The whole line is dimmed (incl. the glyph)
// because these lines are routine noise during long runs — the user
// scans for the absence of warnings/errors, not for individual oks.
func OK(w io.Writer, format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	if enabled {
		fmt.Fprintf(w, "%s  %s %s%s\n", dim, glyphOK, line, reset)
	} else {
		fmt.Fprintf(w, "  ok %s\n", line)
	}
}

// OKv is OK that only prints when --verbose is on. Use it for the
// per-resource apply chatter that's useful for debugging but pure
// noise in normal interactive use (Flux install lists ~30 CRDs +
// ServiceAccounts + Deployments; bootstrap-apply another ~13).
func OKv(w io.Writer, format string, a ...any) {
	if !verbose {
		return
	}
	OK(w, format, a...)
}

// Warn prints a warning. Bold yellow + ⚠ in TTY mode, plain `WARN <line>`
// otherwise. Intentionally NOT indented like OK — warnings should be
// flush-left so they pop out of a sea of indented OKs.
func Warn(w io.Writer, format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	if enabled {
		fmt.Fprintf(w, "%s%s %s%s\n", boldYellow, glyphWarn, line, reset)
	} else {
		fmt.Fprintf(w, "WARN %s\n", line)
	}
}

// Err prints a fatal-but-recoverable error condition. Bold red + ✗.
// (Process-fatal errors still go through `cmd/flywheel/main.go`'s
// `fmt.Fprintln(os.Stderr, "error:", err)` path so they appear on
// stderr with whatever the user's shell does with stderr.)
func Err(w io.Writer, format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	if enabled {
		fmt.Fprintf(w, "%s%s %s%s\n", boldRed, glyphFail, line, reset)
	} else {
		fmt.Fprintf(w, "FAIL %s\n", line)
	}
}

// Detail prints supplementary information (paths, image refs, SHAs).
// Dimmed but no glyph; meant to read as "additional context for the
// nearest step header above".
func Detail(w io.Writer, format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	if enabled {
		fmt.Fprintf(w, "%s  %s%s\n", dim, line, reset)
	} else {
		fmt.Fprintf(w, "  %s\n", line)
	}
}

// Summary prints a terminal one-line outcome (e.g. "Cluster up. …").
// Bold no-colour so it pops without competing with the success-green
// scattered through the run.
func Summary(w io.Writer, format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	if enabled {
		fmt.Fprintf(w, "%s%s%s\n", bold, line, reset)
	} else {
		fmt.Fprintf(w, "%s\n", line)
	}
}

// Highlight returns `text` wrapped in bold (no colour) when enabled.
// Useful for embedding inside a longer line without taking ownership
// of the whole print, e.g. style.Step("cluster %s up", style.Highlight(name)).
func Highlight(text string) string { return wrap(bold, text) }

// Dim returns `text` wrapped in dim (no colour) when enabled. Same
// embedding intent as Highlight; used for inlining paths inside a
// summary line.
func Dim(text string) string { return wrap(dim, text) }
