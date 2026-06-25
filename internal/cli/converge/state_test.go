package converge

import (
	"reflect"
	"testing"
)

// TestState_RoundTrip marshals a nested-shape State and parses it back,
// asserting the cluster baseline survives verbatim.
func TestState_RoundTrip(t *testing.T) {
	in := &State{
		Cluster: ClusterState{
			ConvergedSHA: "9f4e1a2",
			ImageTags:    map[string]string{"git-server": "dogfood-9f4e1a2"},
		},
	}
	raw, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, in)
	}
	// A nested file must not resurrect the deprecated flat field.
	if got.FlywheelSHA != "" {
		t.Errorf("flat field should be empty after parse, got sha=%q", got.FlywheelSHA)
	}
}

// TestState_MigrateLegacyFlat parses a pre-split flat state file and asserts
// the cluster baseline is seeded from flywheel_sha and the flat field is
// zeroed. Now-dropped template fields (answers/files) are simply ignored.
func TestState_MigrateLegacyFlat(t *testing.T) {
	legacy := []byte(`flywheel_sha: 0123456789abcdef
answers:
  ClientName: acme
files:
  flywheel.yaml: deadbeef
`)
	s, err := ParseState(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if s.Cluster.ConvergedSHA != "0123456789abcdef" {
		t.Errorf("cluster.converged_sha = %q, want 0123456789abcdef", s.Cluster.ConvergedSHA)
	}
	if s.FlywheelSHA != "" {
		t.Errorf("flat field not zeroed after migrate: sha=%q", s.FlywheelSHA)
	}
}

// TestState_MigrateIdempotentOnNested ensures an already-nested file is left
// untouched (no clobbering a real baseline with an empty flat field).
func TestState_MigrateIdempotentOnNested(t *testing.T) {
	s := &State{Cluster: ClusterState{ConvergedSHA: "newsha"}}
	s.Migrate()
	if s.Cluster.ConvergedSHA != "newsha" {
		t.Errorf("Migrate clobbered nested baseline: %+v", s)
	}
}
