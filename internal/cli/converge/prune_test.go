package converge

import (
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/cobr-io/flywheel/internal/cli/applier"
)

func keepSetOf(refs ...applier.ResourceRef) map[string]bool {
	m := make(map[string]bool, len(refs))
	for _, r := range refs {
		m[refKey(r)] = true
	}
	return m
}

// The issue #27 scenario: this run re-applied the current dev-loop Deployments
// (git-server, git-deploy-controller, ...) but NOT the old git-auto-sync-self.
// The orphan — present in the cluster (found) but absent from keep — is the
// only thing prunePlan schedules for deletion.
func TestPrunePlan_ReapsSupersededSelfSync(t *testing.T) {
	dep := func(name string) applier.ResourceRef {
		return applier.ResourceRef{Kind: "Deployment", Namespace: "flywheel-system", Name: name}
	}
	keep := keepSetOf(dep("git-server"), dep("git-deploy-controller"), dep("image-builder-controller"), dep("buildkitd"))
	found := []applier.ResourceRef{
		dep("git-server"), dep("git-deploy-controller"), dep("image-builder-controller"), dep("buildkitd"),
		dep("git-auto-sync-self"), // the superseded orphan
	}

	got := prunePlan(keep, found)
	if len(got) != 1 || got[0].Name != "git-auto-sync-self" {
		t.Fatalf("prunePlan = %v, want exactly [git-auto-sync-self]", got)
	}
}

// A resource this run applied is never pruned, even if it also shows up in the
// cluster scan (the common steady-state where keep == found).
func TestPrunePlan_KeepsEverythingReapplied(t *testing.T) {
	refs := []applier.ResourceRef{
		{Kind: "Deployment", Namespace: "flywheel-system", Name: "git-server"},
		{Kind: "Service", Namespace: "flywheel-system", Name: "git-server"},
		{Group: "image.toolkit.fluxcd.io", Kind: "ImageUpdateAutomation", Namespace: "flux-system", Name: "flywheel-self"},
	}
	if got := prunePlan(keepSetOf(refs...), refs); len(got) != 0 {
		t.Fatalf("prunePlan = %v, want nothing pruned when keep == found", got)
	}
}

// Same name + kind in a different namespace is a different resource: a keep
// entry must not spare an orphan that merely shares the name.
func TestPrunePlan_NamespaceScopedIdentity(t *testing.T) {
	keep := keepSetOf(applier.ResourceRef{Kind: "ConfigMap", Namespace: "flywheel-system", Name: "flywheel-config"})
	found := []applier.ResourceRef{
		{Kind: "ConfigMap", Namespace: "flywheel-system", Name: "flywheel-config"}, // kept
		{Kind: "ConfigMap", Namespace: "other", Name: "flywheel-config"},           // orphan, different ns
	}
	got := prunePlan(keep, found)
	if len(got) != 1 || got[0].Namespace != "other" {
		t.Fatalf("prunePlan = %v, want only the 'other' namespace ConfigMap", got)
	}
}

func TestPrunePlan_EmptyFound(t *testing.T) {
	keep := keepSetOf(applier.ResourceRef{Kind: "Deployment", Namespace: "flywheel-system", Name: "git-server"})
	if got := prunePlan(keep, nil); len(got) != 0 {
		t.Fatalf("prunePlan(empty found) = %v, want nil", got)
	}
}

// prunableGroupKinds derives the scan universe from the applied set, dedupes
// it, and drops the safety denylist — so the prune never even lists a kind
// whose deletion would cascade or destroy state.
func TestPrunableGroupKinds_DedupesAndDenylists(t *testing.T) {
	keep := []applier.ResourceRef{
		{Kind: "Deployment", Namespace: "flywheel-system", Name: "git-server"},
		{Kind: "Deployment", Namespace: "flywheel-system", Name: "git-deploy-controller"}, // dup kind
		{Kind: "Service", Namespace: "flywheel-system", Name: "git-server"},
		{Kind: "DaemonSet", Namespace: "flywheel-system", Name: "inotify-bump"},
		{Group: "image.toolkit.fluxcd.io", Kind: "ImageUpdateAutomation", Namespace: "flux-system", Name: "flywheel-self"},
		// All of these must be filtered out by the denylist:
		{Kind: "Namespace", Name: "flywheel-system"},
		{Kind: "PersistentVolumeClaim", Namespace: "flywheel-system", Name: "git-server-data"},
		{Kind: "Secret", Namespace: "flux-system", Name: "sops-age"},
		{Group: "kustomize.toolkit.fluxcd.io", Kind: "Kustomization", Namespace: "flux-system", Name: "client-apps"},
		{Group: "source.toolkit.fluxcd.io", Kind: "GitRepository", Namespace: "flux-system", Name: "flux-system"},
	}

	got := prunableGroupKinds(keep)

	want := []schema.GroupKind{
		{Kind: "Deployment"},
		{Kind: "Service"},
		{Kind: "DaemonSet"},
		{Group: "image.toolkit.fluxcd.io", Kind: "ImageUpdateAutomation"},
	}
	norm := func(gks []schema.GroupKind) []string {
		s := make([]string, len(gks))
		for i, gk := range gks {
			s[i] = gk.String()
		}
		sort.Strings(s)
		return s
	}
	gotN, wantN := norm(got), norm(want)
	if len(gotN) != len(wantN) {
		t.Fatalf("prunableGroupKinds = %v, want %v", gotN, wantN)
	}
	for i := range wantN {
		if gotN[i] != wantN[i] {
			t.Fatalf("prunableGroupKinds = %v, want %v", gotN, wantN)
		}
	}
}

// Guard the denylist contents explicitly — these are the kinds whose deletion
// would cascade-tear-down app/infra or destroy state (the safety contract from
// the user's "never remove app/infra" requirement). A regression that drops
// one of these from the map should fail loudly.
func TestPruneDenylist_GuardsDangerousKinds(t *testing.T) {
	for _, gk := range []schema.GroupKind{
		{Kind: "Namespace"},
		{Kind: "PersistentVolumeClaim"},
		{Kind: "Secret"},
		{Group: "kustomize.toolkit.fluxcd.io", Kind: "Kustomization"},
		{Group: "source.toolkit.fluxcd.io", Kind: "GitRepository"},
	} {
		if !pruneDenylist[gk] {
			t.Errorf("pruneDenylist missing %s — deleting it could cascade or destroy state", gk.String())
		}
	}
}
