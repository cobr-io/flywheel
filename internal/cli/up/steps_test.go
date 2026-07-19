package up

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// recordStep returns a critical/non-critical step whose run appends its name to
// *log (so tests can assert what ran, in order) and returns err.
func recordStep(log *[]string, name string, critical bool, err error) step {
	return step{
		name:     name,
		critical: critical,
		run: func(*upState) error {
			*log = append(*log, name)
			return err
		},
	}
}

// TestRunSteps_OrderAndSuccess: steps execute in declaration order and a
// clean run returns nil.
func TestRunSteps_OrderAndSuccess(t *testing.T) {
	var log []string
	s := &upState{out: &bytes.Buffer{}}
	if err := runSteps(s, []step{
		recordStep(&log, "a", true, nil),
		recordStep(&log, "b", true, nil),
		recordStep(&log, "c", true, nil),
	}); err != nil {
		t.Fatalf("runSteps returned error: %v", err)
	}
	if got := strings.Join(log, ","); got != "a,b,c" {
		t.Fatalf("steps ran out of order: %q", got)
	}
}

// TestRunSteps_AbortOnCritical: a critical step's error aborts the pipeline,
// is wrapped with the step name, and preserves errors.Is on the cause.
func TestRunSteps_AbortOnCritical(t *testing.T) {
	var log []string
	s := &upState{out: &bytes.Buffer{}}
	boom := errors.New("boom")
	err := runSteps(s, []step{
		recordStep(&log, "first", true, nil),
		recordStep(&log, "explode", true, boom),
		recordStep(&log, "never", true, nil),
	})
	if err == nil {
		t.Fatal("expected error from critical step")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error should wrap the step's cause: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "explode: ") {
		t.Fatalf("error should be named by the failing step, got: %v", err)
	}
	if got := strings.Join(log, ","); got != "first,explode" {
		t.Fatalf("critical failure must abort the pipeline; ran %q", got)
	}
}

// TestRunSteps_WarnAndContinue: a non-critical step's error is warned (named)
// and the pipeline continues; runSteps returns nil.
func TestRunSteps_WarnAndContinue(t *testing.T) {
	var log []string
	buf := &bytes.Buffer{}
	s := &upState{out: buf}
	if err := runSteps(s, []step{
		recordStep(&log, "soft", false, errors.New("degraded")),
		recordStep(&log, "after", true, nil),
	}); err != nil {
		t.Fatalf("non-critical failure must not abort: %v", err)
	}
	if got := strings.Join(log, ","); got != "soft,after" {
		t.Fatalf("pipeline must continue past a non-critical failure; ran %q", got)
	}
	if !strings.Contains(buf.String(), "soft: degraded") {
		t.Fatalf("expected a warning naming the step; got %q", buf.String())
	}
}

// TestRunSteps_Skip: a step whose skip reports true is elided entirely (its run,
// including a would-be error, never fires).
func TestRunSteps_Skip(t *testing.T) {
	var log []string
	ran := false
	s := &upState{out: &bytes.Buffer{}}
	if err := runSteps(s, []step{
		{name: "skipped", critical: true, skip: func(*upState) bool { return true },
			run: func(*upState) error { ran = true; return errors.New("must not run") }},
		recordStep(&log, "ran", true, nil),
	}); err != nil {
		t.Fatalf("a skipped step must not surface its error: %v", err)
	}
	if ran {
		t.Fatal("skip=true must elide the step's run")
	}
	if got := strings.Join(log, ","); got != "ran" {
		t.Fatalf("later steps must still run; ran %q", got)
	}
}

// TestRunSteps_PruneSkippedWhenBootstrapFailed locks the load-bearing
// apply-flux-system → prune-machinery dependency: an apply-flux-system
// failure clears bootstrapOK, and skipPrune must then elide prune-machinery
// so a resource that failed to apply is never mistaken for a superseded
// orphan. In production apply-flux-system is critical (issue #117), so this
// path is normally unreachable — runSteps aborts before prune-machinery is
// even considered — but the bootstrapOK gate stays as belt-and-braces, and
// this test exercises it directly with a locally-built (non-critical) step.
func TestRunSteps_PruneSkippedWhenBootstrapFailed(t *testing.T) {
	pruned := false
	s := &upState{out: &bytes.Buffer{}, bootstrapOK: true}
	if err := runSteps(s, []step{
		{name: "apply-flux-system", run: func(s *upState) error {
			s.bootstrapOK = false // mirrors applyFluxSystem's failure path
			return errors.New("apply failed")
		}},
		{name: "prune-machinery", skip: skipPrune, run: func(*upState) error {
			pruned = true
			return nil
		}},
	}); err != nil {
		t.Fatalf("runSteps returned error: %v", err)
	}
	if pruned {
		t.Fatal("prune-machinery must be skipped when bootstrapOK is false")
	}
}

// The inverse: a healthy bootstrap leaves bootstrapOK true and prune runs.
func TestRunSteps_PruneRunsWhenBootstrapOK(t *testing.T) {
	pruned := false
	s := &upState{out: &bytes.Buffer{}, bootstrapOK: true}
	if err := runSteps(s, []step{
		{name: "apply-flux-system", run: func(*upState) error { return nil }},
		{name: "prune-machinery", skip: skipPrune, run: func(*upState) error {
			pruned = true
			return nil
		}},
	}); err != nil {
		t.Fatalf("runSteps returned error: %v", err)
	}
	if !pruned {
		t.Fatal("prune-machinery must run when bootstrapOK is true")
	}
}

// TestUpSteps_ApplyFluxSystemIsCritical locks issue #117's fix: a failed
// bootstrap apply must abort `up` with the real error, not degrade to a WARN
// that the Ready-wait's found-set derivation could then silently paper over
// (the wait only ever waits for whatever Kustomizations the API server
// actually holds, so a dropped one shrank the success criterion instead of
// failing the run).
func TestUpSteps_ApplyFluxSystemIsCritical(t *testing.T) {
	for _, st := range upSteps() {
		if st.name == "apply-flux-system" {
			if !st.critical {
				t.Fatal("apply-flux-system must be critical: a failed bootstrap apply should abort `up`, not warn and continue")
			}
			return
		}
	}
	t.Fatal("apply-flux-system step not found in upSteps()")
}

// TestStepSkipPredicates locks the skip functions the table wires in: the two
// Options-driven seams (SkipFluxInstall, --wait) and the state-driven prune gate.
func TestStepSkipPredicates(t *testing.T) {
	if !skipFluxInstall(&upState{opts: Options{SkipFluxInstall: true}}) {
		t.Error("skipFluxInstall should be true when Options.SkipFluxInstall is set")
	}
	if skipFluxInstall(&upState{opts: Options{}}) {
		t.Error("skipFluxInstall should be false by default")
	}
	if !skipWait(&upState{opts: Options{Wait: boolPtr(false)}}) {
		t.Error("skipWait should be true when --wait=false")
	}
	if skipWait(&upState{opts: Options{Wait: boolPtr(true)}}) {
		t.Error("skipWait should be false when --wait=true")
	}
	if skipWait(&upState{opts: Options{}}) {
		t.Error("skipWait should be false when Wait is unset (default: wait)")
	}
	if !skipPrune(&upState{bootstrapOK: false}) {
		t.Error("skipPrune should be true when bootstrapOK is false")
	}
	if skipPrune(&upState{bootstrapOK: true}) {
		t.Error("skipPrune should be false when bootstrapOK is true")
	}
}
