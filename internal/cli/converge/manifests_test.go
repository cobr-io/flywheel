package converge

import (
	"strings"
	"testing"

	flywheel "github.com/cobr-io/flywheel"
)

// The IUA must NOT pin a branch (no git.checkout / git.push): it commits to the
// GitRepository's tracked branch, which is the constant DEPLOY branch
// (flywheel/local-deploy). A push.branch / checkout.ref would defeat the
// deploy-ref model by sending bumps somewhere else. Guards the design's test-plan
// invariant "the IUA still has no push.branch".
func TestIUAManifest_HasNoBranchPin(t *testing.T) {
	raw, err := flywheel.Assets.ReadFile("manifests/dev-loop/base/image-update-automation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	// These are YAML keys (with a colon); the comments use the dotted form
	// ("push.branch", "checkout.ref"), so they won't false-match.
	for _, key := range []string{"push:", "checkout:"} {
		if strings.Contains(s, key) {
			t.Errorf("IUA manifest must not set git.%s — it would pin a branch and defeat the deploy-ref model:\n%s", strings.TrimSuffix(key, ":"), s)
		}
	}
}
