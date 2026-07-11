package clean

import (
	"bytes"
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/cached/memory"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/restmapper"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cobr-io/flywheel/internal/cli/applier"
	"github.com/cobr-io/flywheel/internal/naming"
)

// newTestApplier builds an *applier.Applier around a fake dynamic client
// (k8s.io/client-go/dynamic/fake) and a REST mapper backed by a fake
// discovery client that knows only about v1/PersistentVolumeClaim — enough
// to exercise the real List + Delete code paths in applier.Applier without
// a cluster.
func newTestApplier(objects ...runtime.Object) *applier.Applier {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)

	fakeDisco := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{}}
	fakeDisco.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "persistentvolumeclaims", Namespaced: true, Kind: "PersistentVolumeClaim"},
			},
		},
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(fakeDisco))

	return applier.NewForTest(dyn, mapper)
}

// pvc builds an unstructured PersistentVolumeClaim fixture. When labeled is
// true it carries naming.ManagedBySelector, the same label `flywheel up`
// stamps on every PVC it applies.
func pvc(name, ns string, labeled bool) *unstructured.Unstructured {
	labels := map[string]interface{}{}
	if labeled {
		k, v, _ := strings.Cut(naming.ManagedBySelector, "=")
		labels[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
			"labels":    labels,
		},
	}}
}

// TestCleanOrphanedPVCs_OnlyDeletesLabeled is the regression test for the
// bug where cleanOrphanedPVCs listed PVCs with a bare, unscoped list (no
// label selector) and deleted every PVC in the namespace — including ones
// an app or Flux created, not just the ones flywheel itself applied. It
// must delete only the PVC carrying naming.ManagedBySelector and leave
// the unlabeled one (standing in for an app-owned PVC) untouched.
func TestCleanOrphanedPVCs_OnlyDeletesLabeled(t *testing.T) {
	const ns = "flywheel-system"
	labeled := pvc("git-server-data", ns, true)
	unlabeled := pvc("app-data", ns, false)

	a := newTestApplier(labeled, unlabeled)

	var out bytes.Buffer
	if err := cleanOrphanedPVCs(context.Background(), a, ns, &out); err != nil {
		t.Fatalf("cleanOrphanedPVCs: %v", err)
	}

	gvr := schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	remaining, err := a.ListUnstructured(context.Background(), gvr, ns)
	if err != nil {
		t.Fatalf("ListUnstructured: %v", err)
	}
	if len(remaining) != 1 || remaining[0].GetName() != "app-data" {
		t.Fatalf("remaining PVCs = %v, want only the unlabeled app-data PVC to survive", remaining)
	}
}
