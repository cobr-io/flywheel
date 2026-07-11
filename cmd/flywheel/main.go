// Command flywheel is the local GitOps dev-loop CLI. See
// docs/designs/2026-05-15-harness-template-design.md for the full design.
//
// The command tree is built with cobra (newRootCmd); each subcommand is a thin
// wrapper around the corresponding internal/cli package. Globals (--no-color,
// -v/--verbose) are persistent flags handled in the root's PersistentPreRun.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	"github.com/cobr-io/flywheel/internal/cli/style"
)

func main() {
	// One cancellable root context for the whole run, threaded through every
	// command via cmd.Context(). Ctrl-C (SIGINT) or SIGTERM cancels it so the
	// ctx.Done() branches + exec.CommandContext calls already wired through
	// up/down/clean/use unwind promptly instead of the process hanging until
	// a multi-minute wait finishes. NotifyContext only intercepts the signal
	// once: a second Ctrl-C after cancellation falls through to Go's default
	// disposition (hard kill) — the same "one graceful, two forceful" UX as
	// ctrl.SetupSignalHandler() in the controllers.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		// A blank message means the command already printed everything it
		// needs (help text, a "not implemented" line) — don't double-print.
		if err.Error() != "" {
			style.Err(os.Stderr, "%v", err)
		}
		os.Exit(exitCodeFor(err))
	}
}

// exitCodeFor maps an Execute() error to a process exit code:
//   - exitError carries an explicit code (stubs → 1, usage → 2).
//   - cobra's "unknown command" is a usage error → 2.
//   - everything else (a command's RunE failing) → 1.
func exitCodeFor(err error) int {
	var ee exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	if strings.HasPrefix(err.Error(), "unknown command") {
		return 2
	}
	return 1
}

// exitError attaches an explicit process exit code to an error. errSilent is
// used as the wrapped error when the command has already written its own output
// and main should exit with the code but print nothing further.
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }
func (e exitError) Unwrap() error { return e.err }

var errSilent = errors.New("")

// newRootCmd builds the full command tree. SilenceErrors/SilenceUsage are set
// because main owns error printing and we don't want a usage dump on a runtime
// failure; usage-level errors still surface a clear message.
func newRootCmd() *cobra.Command {
	var noColor, verbose bool

	root := &cobra.Command{
		Use:           "flywheel",
		Short:         "Flywheel — per-client GitOps repo template",
		Long:          "Flywheel — per-client GitOps repo template.\n\nSpin up a client's local k3d cluster with a sub-30-second commit-to-pod dev\nloop, and roll dev-loop improvements forward with a one-line version bump.",
		SilenceErrors: true,
		SilenceUsage:  true,
		// Globals are parsed by the time PersistentPreRun fires, and it runs
		// for every subcommand (only the root defines one), so colour/verbose/
		// klog setup happens exactly once before any command logic.
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			style.Init(noColor)
			style.SetVerbose(verbose)
			silenceKlog(verbose)
		},
		// No subcommand given: print help and exit 2 (matches the legacy
		// no-args behavior). errSilent suppresses a second message in main.
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = cmd.Help()
			return exitError{code: 2, err: errSilent}
		},
	}

	root.PersistentFlags().BoolVar(&noColor, "no-color", false,
		"Disable ANSI colours + Unicode glyphs (also honoured: NO_COLOR env)")
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false,
		"Show k3d / docker / kubectl-klog chatter that's hidden by default")

	// Flag-parse errors are usage errors → exit 2.
	root.SetFlagErrorFunc(func(c *cobra.Command, e error) error {
		return exitError{code: 2, err: e}
	})

	root.AddCommand(
		newDoctorCmd(),
		newInitCmd(),
		newUpCmd(),
		newDownCmd(),
		newCleanCmd(),
		newAddCmd(),
		newPublishAppCmd(),
		newUseCmd(),
		newVersionCmd(),
	)
	return root
}

// silenceKlog mutes klog's stderr output unless --verbose is set. client-go
// discovery + Flux's controller libraries emit `E0000 …` klog warnings on every
// transient apiserver hiccup; users see them as Flywheel errors, even when the
// wider operation succeeds. With verbose, klog goes back to its default stderr
// destination.
//
// klog v2 has TWO output paths and both have to be silenced:
//
//  1. Legacy text path — klog.Errorf / klog.Infof. Killed by
//     klog.SetOutput(io.Discard) + the stderrthreshold flag.
//
//  2. Structured (logr) path — runtime.HandleError / contextual Errors. Goes
//     through whatever logger klog.SetLogger configured; SetOutput does NOT
//     reach it. Killed by replacing the global logger with logr.Discard().
func silenceKlog(verbose bool) {
	if verbose {
		return
	}
	// Path 2: structured logger → no-op sink.
	klog.SetLogger(logr.Discard())

	// Path 1: legacy stream → discard, plus raise the stderr threshold so
	// anything that bypasses SetOutput (FATAL aside) is still suppressed.
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	_ = fs.Set("logtostderr", "false")
	_ = fs.Set("alsologtostderr", "false")
	_ = fs.Set("stderrthreshold", "FATAL")
	klog.SetOutput(io.Discard)
}
