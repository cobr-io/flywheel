package style

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Spin runs `fn` while displaying a single-row braille spinner with
// `label`. On TTY (and non-verbose), the line redraws every 100ms
// showing elapsed time. When fn returns, the spinner line is replaced
// with a stable dim `✓ label (12s)` summary — or a `⚠ label failed
// (12s)` line if fn errored.
//
// In non-TTY mode (logs, CI) or verbose mode (where subprocess output
// would otherwise corrupt the in-place redraw), Spin degrades
// gracefully: it prints the label up front, runs fn, and prints the
// outcome. The error from fn is always returned verbatim; Spin is
// purely a display layer.
//
// Lifecycle / concurrency: Spin starts an internal goroutine that
// owns `w` for the duration of fn. Callers MUST NOT write to `w`
// from inside fn during a non-verbose TTY Spin (the writes would
// collide with the redraw). In practice, fn is a shell-out whose
// stdout/stderr is routed through VerboseWriter, so non-verbose
// runs produce no output from fn.
func Spin(w io.Writer, label string, fn func() error) error {
	start := time.Now()

	// Degraded modes: non-TTY (cursor-up sequences would corrupt
	// the log) or verbose (where fn's own output would clash with
	// the redraw). Print a marker, run, print outcome.
	if !enabled || verbose {
		Step(w, "%s", label)
		err := fn()
		if err != nil {
			Warn(w, "%s failed (%s)", label, durStr(time.Since(start)))
			return err
		}
		OK(w, "%s (%s)", label, durStr(time.Since(start)))
		return nil
	}

	// TTY + non-verbose: animated spinner.
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go renderSpin(w, label, start, done, &wg)

	err := fn()
	close(done)
	wg.Wait()

	// Spinner goroutine cleared its line on exit. Now emit a stable
	// summary that lands in scrollback.
	if err != nil {
		Warn(w, "%s failed (%s)", label, durStr(time.Since(start)))
		return err
	}
	OK(w, "%s (%s)", label, durStr(time.Since(start)))
	return nil
}

// renderSpin owns the spinner's drawn line until `done` closes.
// First draw is immediate so the user sees the label without delay
// even if fn is fast; subsequent frames tick every 100ms.
func renderSpin(w io.Writer, label string, start time.Time, done <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()

	const frameRate = 100 * time.Millisecond
	ticker := time.NewTicker(frameRate)
	defer ticker.Stop()

	frame := 0
	draw := func(drawn bool) {
		// If we've already drawn a line, move cursor up + clear before
		// re-rendering — that's the in-place redraw.
		if drawn {
			fmt.Fprintf(w, "\033[1A\033[J")
		}
		// Spinner glyph is bold cyan (matches the Step header colour);
		// label + elapsed are dim (this is a transient line, not
		// scrollback-worthy).
		fmt.Fprintf(w, "%s%s%s %s%s  %s%s\n",
			boldCyan, string(spinFrames[frame]), reset,
			dim, label,
			durStr(time.Since(start)), reset,
		)
	}
	draw(false)
	for {
		select {
		case <-done:
			// Clear the line so the caller's OK/Warn summary lands
			// without leftover spinner state.
			fmt.Fprintf(w, "\033[1A\033[J")
			return
		case <-ticker.C:
			frame = (frame + 1) % len(spinFrames)
			draw(true)
		}
	}
}
