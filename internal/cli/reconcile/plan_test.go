package reconcile

import "testing"

// T1.15 — PVCs (and CRDs) are NEVER auto-deleted: they always classify
// to an orphan tier, so they never appear in DeletableDestructive.
func TestClassifyRemoval_PVCsAndCRDsAreOrphaned(t *testing.T) {
	cases := []struct {
		group, kind string
		want        Tier
	}{
		{"", "PersistentVolumeClaim", OrphanPVC},
		{"apiextensions.k8s.io", "CustomResourceDefinition", OrphanCRD},
		{"helm.toolkit.fluxcd.io", "HelmRelease", Normal},
		{"apps", "Deployment", Normal},
		{"", "Service", Normal},
		{"cert-manager.io", "ClusterIssuer", Normal},
	}
	for _, tc := range cases {
		if got := ClassifyRemoval(tc.group, tc.kind); got != tc.want {
			t.Errorf("ClassifyRemoval(%q,%q)=%v want %v", tc.group, tc.kind, got, tc.want)
		}
	}
}

func TestPlan_TiersDestructivesOnAdd(t *testing.T) {
	p := &Plan{}
	p.Add(Change{Group: "", Kind: "PersistentVolumeClaim", Namespace: "flywheel-system", Name: "kaniko-cache", Op: Destructive})
	p.Add(Change{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition", Name: "certificates.cert-manager.io", Op: Destructive})
	p.Add(Change{Group: "helm.toolkit.fluxcd.io", Kind: "HelmRelease", Namespace: "cert-manager", Name: "cert-manager", Op: Destructive})

	if d := p.DeletableDestructive(); len(d) != 1 || d[0].Kind != "HelmRelease" {
		t.Fatalf("DeletableDestructive = %v, want exactly the HelmRelease", d)
	}
	orphans := p.Orphaned()
	if len(orphans) != 2 {
		t.Fatalf("Orphaned = %v, want 2 (PVC + CRD)", orphans)
	}
}

// T1.15 explicit: a PVC destined for removal is NEVER deletable even when
// the user passes --yes.
func TestPlan_PVCNeverDeletableEvenWithYes(t *testing.T) {
	p := &Plan{}
	p.Add(Change{Kind: "PersistentVolumeClaim", Namespace: "flywheel-system", Name: "git-server-data", Op: Destructive})
	if p.HasDeletableDestructive() {
		t.Error("a PVC-only destructive plan must have no deletable-destructive changes")
	}
	if p.NeedsConfirmation(Approval{Yes: true}) {
		t.Error("a PVC-only plan needs no confirmation (nothing auto-deletable)")
	}
	if got := p.DeletableDestructive(); len(got) != 0 {
		t.Errorf("PVC must never be auto-deletable; got %v", got)
	}
	if got := p.Orphaned(); len(got) != 1 {
		t.Errorf("PVC must be reported orphaned; got %v", got)
	}
}

// T1.4 — --yes-additive approves +additive but refuses -destructive;
// --yes approves destructive; no flag → needs confirmation when
// destructive present.
func TestPlan_ApprovalGating(t *testing.T) {
	additiveOnly := &Plan{}
	additiveOnly.Add(Change{Kind: "Deployment", Namespace: "flywheel-system", Name: "git-server", Op: Additive})

	if additiveOnly.NeedsConfirmation(Approval{}) {
		t.Error("additive-only plan should never need confirmation")
	}
	if additiveOnly.NeedsConfirmation(Approval{YesAdditive: true}) {
		t.Error("additive-only plan with --yes-additive should proceed")
	}

	withDestructive := &Plan{}
	withDestructive.Add(Change{Kind: "Deployment", Name: "git-server", Op: Additive})
	withDestructive.Add(Change{Group: "helm.toolkit.fluxcd.io", Kind: "HelmRelease", Namespace: "cert-manager", Name: "cert-manager", Op: Destructive})

	if !withDestructive.NeedsConfirmation(Approval{}) {
		t.Error("plan with destructive change needs confirmation when no flag set")
	}
	if !withDestructive.NeedsConfirmation(Approval{YesAdditive: true}) {
		t.Error("--yes-additive must NOT cover destructive ops")
	}
	if withDestructive.NeedsConfirmation(Approval{Yes: true}) {
		t.Error("--yes must cover destructive ops")
	}
}

func TestPlan_RenderCounts(t *testing.T) {
	p := &Plan{}
	p.Add(Change{Kind: "Deployment", Name: "a", Op: Additive})
	p.Add(Change{Kind: "Service", Name: "b", Op: Mutating})
	p.Add(Change{Group: "helm.toolkit.fluxcd.io", Kind: "HelmRelease", Name: "c", Op: Destructive})
	p.Add(Change{Kind: "PersistentVolumeClaim", Name: "d", Op: Destructive})
	out := p.Render()
	if !contains(out, "1 to add, 1 to change, 1 to destroy, 1 orphaned") {
		t.Errorf("render summary wrong:\n%s", out)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
