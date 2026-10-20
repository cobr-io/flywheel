package converge

import (
	"testing"

	flywheel "github.com/cobr-io/flywheel"
	"github.com/cobr-io/flywheel/internal/cli/applier"
)

// TestDeclaredDevLoopPVCs_IncludesLivePVCs locks issue #172 from the other
// side: the declared set `flywheel clean` keeps must contain the dev-loop's
// live PVCs, built from the binary's real embedded manifests. If it ever came
// back without them, clean would delete the git mirror and build cache.
func TestDeclaredDevLoopPVCs_IncludesLivePVCs(t *testing.T) {
	got, err := DeclaredDevLoopPVCs(flywheel.Assets)
	if err != nil {
		t.Fatalf("DeclaredDevLoopPVCs: %v", err)
	}
	have := map[applier.ResourceRef]bool{}
	for _, r := range got {
		have[r] = true
	}
	for _, name := range []string{"git-server-data", "buildkit-cache"} {
		want := applier.ResourceRef{Kind: "PersistentVolumeClaim", Namespace: "flywheel-system", Name: name}
		if !have[want] {
			t.Errorf("declared PVCs %v missing %s/%s", got, want.Namespace, want.Name)
		}
	}
}
