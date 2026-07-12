package applier

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery/cached/memory"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/restmapper"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cobr-io/flywheel/internal/naming"
)

// These tests drive the SSA applier against a fake dynamic client
// (k8s.io/client-go/dynamic/fake) plus a REST mapper backed by a fake
// discovery client. Apply paths use a patch reactor to record every SSA
// call (short-circuiting the tracker, whose Apply requires the object to
// pre-exist); List/Delete paths use the default tracker with seeded objects.
// This exercises the real apply/list/delete code without a cluster. It
// extends the seam T01 added (NewForTest + the fake-client pattern in
// internal/cli/clean/run_test.go).

const cmGroupVersion = "v1"

var cmGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

// testMapper builds a REST mapper that knows the core v1 kinds the tests
// apply/list/delete: ConfigMap (namespaced), Namespace (cluster-scoped), and
// PersistentVolumeClaim (namespaced).
func testMapper() *restmapper.DeferredDiscoveryRESTMapper {
	fakeDisco := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{}}
	fakeDisco.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: cmGroupVersion,
			APIResources: []metav1.APIResource{
				{Name: "configmaps", Namespaced: true, Kind: "ConfigMap"},
				{Name: "namespaces", Namespaced: false, Kind: "Namespace"},
				{Name: "persistentvolumeclaims", Namespaced: true, Kind: "PersistentVolumeClaim"},
			},
		},
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(fakeDisco))
}

// applyRecord captures the salient fields of one SSA patch the applier issued.
type applyRecord struct {
	gvr          schema.GroupVersionResource
	namespace    string
	name         string
	patchType    types.PatchType
	fieldManager string
}

// recordingApplyReactor records every apply and short-circuits the tracker.
// Names present in fail return an API Conflict (a realistic SSA failure);
// all others succeed.
func recordingApplyReactor(recs *[]applyRecord, fail map[string]bool) clienttesting.ReactionFunc {
	return func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(clienttesting.PatchActionImpl)
		if !ok {
			return false, nil, nil
		}
		*recs = append(*recs, applyRecord{
			gvr:          pa.GetResource(),
			namespace:    pa.GetNamespace(),
			name:         pa.GetName(),
			patchType:    pa.GetPatchType(),
			fieldManager: pa.PatchOptions.FieldManager,
		})
		if fail[pa.GetName()] {
			return true, nil, apierrors.NewConflict(
				pa.GetResource().GroupResource(), pa.GetName(),
				errConflict)
		}
		return true, nil, nil
	}
}

var errConflict = &staticErr{"field manager conflict"}

type staticErr struct{ s string }

func (e *staticErr) Error() string { return e.s }

// applierWithReactor builds an Applier whose fake dynamic client routes every
// patch through the recording reactor.
func applierWithReactor(recs *[]applyRecord, fail map[string]bool, objects ...runtime.Object) *Applier {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
	dyn.PrependReactor("patch", "*", recordingApplyReactor(recs, fail))
	return NewForTest(dyn, testMapper())
}

// applierWithObjects builds an Applier over the default tracker seeded with
// objects (no reactor) — for the List/Delete/Get paths.
func applierWithObjects(objects ...runtime.Object) *Applier {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
	return NewForTest(dyn, testMapper())
}

func configMap(name, ns string, labels map[string]string) *unstructured.Unstructured {
	meta := map[string]interface{}{
		"name":      name,
		"namespace": ns,
	}
	if labels != nil {
		lm := map[string]interface{}{}
		for k, v := range labels {
			lm[k] = v
		}
		meta["labels"] = lm
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   meta,
	}}
}

func managedLabels() map[string]string {
	return map[string]string{naming.ManagedByLabelKey: naming.ManagedByLabelValue}
}

// TestApplyYAML_MultiDocHappyPath applies a three-document blob (one
// cluster-scoped Namespace + two namespaced ConfigMaps) and asserts every
// object is applied, returned in the ResourceRef keep-set, and issued as an
// SSA patch with the flux-controller field manager. It also locks in the
// namespaced/cluster-scoped split: the ConfigMaps carry their namespace, the
// Namespace carries none.
func TestApplyYAML_MultiDocHappyPath(t *testing.T) {
	const raw = `apiVersion: v1
kind: Namespace
metadata:
  name: apps
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg-a
  namespace: flywheel-system
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg-b
  namespace: flywheel-system
`
	var recs []applyRecord
	a := applierWithReactor(&recs, nil)

	var out bytes.Buffer
	applied, err := a.applyYAML(context.Background(), []byte(raw), &out)
	if err != nil {
		t.Fatalf("applyYAML: %v", err)
	}
	if len(applied) != 3 {
		t.Fatalf("applied refs = %d, want 3 (%v)", len(applied), applied)
	}
	if len(recs) != 3 {
		t.Fatalf("recorded applies = %d, want 3", len(recs))
	}

	byName := map[string]applyRecord{}
	for _, r := range recs {
		byName[r.name] = r
		if r.patchType != types.ApplyPatchType {
			t.Errorf("%s patchType = %q, want ApplyPatchType", r.name, r.patchType)
		}
		if r.fieldManager != FieldManager {
			t.Errorf("%s fieldManager = %q, want %q", r.name, r.fieldManager, FieldManager)
		}
	}
	// Namespaced kinds carry their namespace; the cluster-scoped Namespace
	// object carries none.
	if got := byName["cfg-a"].namespace; got != "flywheel-system" {
		t.Errorf("cfg-a namespace = %q, want flywheel-system", got)
	}
	if got := byName["apps"].namespace; got != "" {
		t.Errorf("Namespace object was applied namespaced (%q), want cluster-scoped", got)
	}
	if got := byName["apps"].gvr.Resource; got != "namespaces" {
		t.Errorf("Namespace GVR resource = %q, want namespaces", got)
	}
}

// TestApplyYAML_PerObjectFailureAggregates is the regression test for the
// bug this task fixes: a multi-document apply that fails on more than one
// object used to fold N failures into a single lastErr, surfacing only the
// last. With errors.Join every failed object surfaces by name, and the
// objects that succeeded still enter the keep-set.
func TestApplyYAML_PerObjectFailureAggregates(t *testing.T) {
	const raw = `apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg-1
  namespace: flywheel-system
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg-2
  namespace: flywheel-system
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg-3
  namespace: flywheel-system
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg-4
  namespace: flywheel-system
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg-5
  namespace: flywheel-system
`
	var recs []applyRecord
	fail := map[string]bool{"cfg-2": true, "cfg-4": true}
	a := applierWithReactor(&recs, fail)

	var out bytes.Buffer
	applied, err := a.applyYAML(context.Background(), []byte(raw), &out)
	if err == nil {
		t.Fatal("applyYAML: want aggregated error, got nil")
	}

	// Both failed objects must be named — the whole point of the fix.
	msg := err.Error()
	for _, name := range []string{"cfg-2", "cfg-4"} {
		if !strings.Contains(msg, name) {
			t.Errorf("aggregated error missing %q; got:\n%s", name, msg)
		}
	}
	// The successes must not appear as failures, and must be tracked.
	if strings.Contains(msg, "cfg-1") || strings.Contains(msg, "cfg-3") || strings.Contains(msg, "cfg-5") {
		t.Errorf("aggregated error names a succeeded object; got:\n%s", msg)
	}
	gotApplied := map[string]bool{}
	for _, r := range applied {
		gotApplied[r.Name] = true
	}
	for _, name := range []string{"cfg-1", "cfg-3", "cfg-5"} {
		if !gotApplied[name] {
			t.Errorf("keep-set missing succeeded object %q (%v)", name, applied)
		}
	}
	if len(applied) != 3 {
		t.Errorf("applied refs = %d, want 3", len(applied))
	}

	// Example two-failure error shape (surfaced in the PR): joined by newline,
	// each line names its object. Assert the join produced two lines.
	if lines := strings.Split(msg, "\n"); len(lines) != 2 {
		t.Errorf("aggregated error = %d lines, want 2:\n%s", len(lines), msg)
	}
	t.Logf("two-failure error shape:\n%s", msg)
}

// TestApplyYAML covers the exported multi-doc entry point (used by
// flux.Install and up's namespace bootstrap), which discards the keep-set
// refs and returns only the aggregated error.
func TestApplyYAML(t *testing.T) {
	const raw = `apiVersion: v1
kind: ConfigMap
metadata:
  name: only
  namespace: flywheel-system
`
	var recs []applyRecord
	a := applierWithReactor(&recs, nil)

	var out bytes.Buffer
	if err := a.ApplyYAML(context.Background(), []byte(raw), &out); err != nil {
		t.Fatalf("ApplyYAML: %v", err)
	}
	if len(recs) != 1 || recs[0].name != "only" {
		t.Fatalf("recorded applies = %v, want the one ConfigMap", recs)
	}
}

// TestApplyObjectAs_FieldManagerOverride covers the ApplyObjectAs seam
// `flywheel use` relies on (issue #17): a distinct field manager must reach
// the API so the deploy-branch annotation survives a later flux-controller
// apply.
func TestApplyObjectAs_FieldManagerOverride(t *testing.T) {
	var recs []applyRecord
	a := applierWithReactor(&recs, nil)

	const deployManager = "flywheel-cli-deploy"
	var out bytes.Buffer
	if err := a.ApplyObjectAs(context.Background(), configMap("cfg", "flywheel-system", nil), deployManager, &out); err != nil {
		t.Fatalf("ApplyObjectAs: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("recorded applies = %d, want 1", len(recs))
	}
	if recs[0].fieldManager != deployManager {
		t.Errorf("fieldManager = %q, want %q", recs[0].fieldManager, deployManager)
	}
}

// TestApplyKustomizeTracked builds a real kustomization on disk and applies
// it, covering buildKustomize + the tracked apply wrappers.
func TestApplyKustomizeTracked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "kustomization.yaml"), `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - cm.yaml
`)
	writeFile(t, filepath.Join(dir, "cm.yaml"), `apiVersion: v1
kind: ConfigMap
metadata:
  name: from-kustomize
  namespace: flywheel-system
`)

	var recs []applyRecord
	a := applierWithReactor(&recs, nil)

	var out bytes.Buffer
	applied, err := a.ApplyKustomizeTracked(context.Background(), dir, &out)
	if err != nil {
		t.Fatalf("ApplyKustomizeTracked: %v", err)
	}
	if len(applied) != 1 || applied[0].Name != "from-kustomize" {
		t.Fatalf("applied = %v, want the one kustomize-built ConfigMap", applied)
	}
	// The wrapper form is also exercised.
	if err := a.ApplyKustomize(context.Background(), dir, &out); err != nil {
		t.Fatalf("ApplyKustomize: %v", err)
	}
}

// TestListSelectors locks in the list selector behavior clean/prune depend
// on (T01): the bare list returns every object in the namespace, while the
// labeled variants return only managed-by=flywheel objects.
func TestListSelectors(t *testing.T) {
	ns := naming.FlywheelNamespace
	managed := configMap("git-server-config", ns, managedLabels())
	unmanaged := configMap("app-config", ns, nil)
	otherNs := configMap("managed-elsewhere", "other", managedLabels())

	a := applierWithObjects(managed, unmanaged, otherNs)
	ctx := context.Background()

	all, err := a.ListUnstructured(ctx, cmGVR, ns)
	if err != nil {
		t.Fatalf("ListUnstructured: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("ListUnstructured in %s = %d, want 2 (both, unscoped)", ns, len(all))
	}

	labeled, err := a.ListUnstructuredLabeled(ctx, cmGVR, ns, naming.ManagedBySelector)
	if err != nil {
		t.Fatalf("ListUnstructuredLabeled: %v", err)
	}
	if len(labeled) != 1 || labeled[0].GetName() != "git-server-config" {
		t.Errorf("ListUnstructuredLabeled = %v, want only the managed ConfigMap", names(labeled))
	}

	// ListByKindLabeled resolves the kind via the mapper and lists across
	// all namespaces: both managed ConfigMaps (this ns + other), not the
	// unmanaged one.
	byKind, err := a.ListByKindLabeled(ctx, schema.GroupKind{Kind: "ConfigMap"}, naming.ManagedBySelector)
	if err != nil {
		t.Fatalf("ListByKindLabeled: %v", err)
	}
	if len(byKind) != 2 {
		t.Errorf("ListByKindLabeled = %v, want the two managed ConfigMaps across namespaces", names(byKind))
	}
}

// TestDeleteResource_NotFoundTolerated proves DeleteResource is idempotent:
// deleting an existing object removes it, and deleting a missing one is not
// an error (clean relies on this).
func TestDeleteResource_NotFoundTolerated(t *testing.T) {
	ns := naming.FlywheelNamespace
	existing := configMap("git-server-data", ns, managedLabels())
	a := applierWithObjects(existing)
	ctx := context.Background()

	var out bytes.Buffer
	// Missing object: tolerated.
	if err := a.DeleteResource(ctx, ResourceRef{Kind: "ConfigMap", Namespace: ns, Name: "never-existed"}, &out); err != nil {
		t.Fatalf("DeleteResource(missing) = %v, want nil (NotFound tolerated)", err)
	}
	// Existing object: deleted.
	if err := a.DeleteResource(ctx, ResourceRef{Kind: "ConfigMap", Namespace: ns, Name: "git-server-data"}, &out); err != nil {
		t.Fatalf("DeleteResource(existing): %v", err)
	}
	remaining, err := a.ListUnstructured(ctx, cmGVR, ns)
	if err != nil {
		t.Fatalf("ListUnstructured: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("after delete, remaining = %v, want none", names(remaining))
	}
}

// TestGetUnstructured covers the single-object get used by up's wait polls.
func TestGetUnstructured(t *testing.T) {
	ns := naming.FlywheelNamespace
	a := applierWithObjects(configMap("cfg", ns, nil))
	ctx := context.Background()

	got, err := a.GetUnstructured(ctx, cmGVR, ns, "cfg")
	if err != nil {
		t.Fatalf("GetUnstructured: %v", err)
	}
	if got.GetName() != "cfg" {
		t.Errorf("GetUnstructured name = %q, want cfg", got.GetName())
	}
	if _, err := a.GetUnstructured(ctx, cmGVR, ns, "missing"); err == nil {
		t.Error("GetUnstructured(missing) = nil error, want NotFound")
	}
}

// TestResetMapper just exercises the discovery-cache reset (no-op against the
// fake, but keeps the seam covered).
func TestResetMapper(t *testing.T) {
	a := applierWithObjects()
	a.ResetMapper()
}

// TestNew_BadKubeconfig covers the config-loading path in New/loadRESTConfig:
// an explicit but nonexistent kubeconfig path must surface as an error rather
// than silently falling back.
func TestNew_BadKubeconfig(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "does-not-exist"), ""); err == nil {
		t.Fatal("New with a nonexistent kubeconfig = nil error, want failure")
	}
}

func names(items []unstructured.Unstructured) []string {
	out := make([]string, len(items))
	for i := range items {
		out[i] = items[i].GetName()
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
