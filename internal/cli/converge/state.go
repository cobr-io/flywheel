package converge

import (
	"sigs.k8s.io/yaml"
)

// State is the on-disk shape of .flywheel-state.yaml. **Committed** (not
// gitignored) so every developer and CI share one view of what the running
// cluster last received.
//
// It records only the cluster baseline (design § state model):
// cluster.converged_sha is the embedded-asset SHA last pushed to the
// in-cluster mirror.
type State struct {
	Cluster ClusterState `json:"cluster"`

	// Deprecated legacy flat field — read-only, for migrating pre-split
	// state files. Migrate() folds it into Cluster and zeroes it, so it is
	// never written back. Do not read directly; use Cluster.
	FlywheelSHA string `json:"flywheel_sha,omitempty"`
}

// ClusterState is the cluster baseline: what the running cluster last
// received.
type ClusterState struct {
	// ConvergedSHA is the embedded-asset SHA last pushed to the in-cluster
	// mirror with images rolled.
	ConvergedSHA string `json:"converged_sha,omitempty"`
	// ImageTags is the content tag last delivered per image, for drift
	// comparison (name → tag, e.g. git-server → dogfood-9f4e1a2).
	ImageTags map[string]string `json:"image_tags,omitempty"`
}

func (s *State) Marshal() ([]byte, error) {
	return yaml.Marshal(s)
}

// Migrate folds a legacy flat state file into the nested cluster shape. A
// pre-split file carried a single flywheel_sha (plus now-dropped template
// fields); the cluster baseline starts equal to that SHA. Idempotent: a file
// already in nested form, or an empty one, is left untouched.
func (s *State) Migrate() {
	if s.Cluster.ConvergedSHA != "" {
		return // already nested
	}
	if s.FlywheelSHA == "" {
		return // nothing to migrate
	}
	s.Cluster.ConvergedSHA = s.FlywheelSHA
	s.FlywheelSHA = ""
}

func ParseState(raw []byte) (*State, error) {
	var s State
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	s.Migrate()
	return &s, nil
}
