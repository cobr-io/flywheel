package clean

import (
	"bytes"
	"context"
	"sort"
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

// TestRun_DeletesOnlyUndeclaredLabeledPVCs locks both PVC-safety fixes:
//   - an unlabeled PVC (standing in for one an app or Flux created) is never
//     touched — the original unscoped-list bug;
//   - a labeled PVC the current manifests still declare (git-server-data) is
//     live state and is kept — issue #172, where `clean` deleted the in-cluster
//     git mirror and build cache on a healthy cluster.
//
// Only the labeled, no-longer-declared PVC (left by an older version) goes.
func TestRun_DeletesOnlyUndeclaredLabeledPVCs(t *testing.T) {
	const ns = "flywheel-system"
	a := newTestApplier(
		pvc("git-server-data", ns, true),
		pvc("old-kaniko-cache", ns, true),
		pvc("app-data", ns, false),
	)

	var out bytes.Buffer
	err := Run(context.Background(), a, Options{
		FlywheelNamespace: ns,
		Orphaned:          true,
		Declared: []applier.ResourceRef{
			{Kind: "PersistentVolumeClaim", Namespace: ns, Name: "git-server-data"},
		},
	}, &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	gvr := schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	remaining, err := a.ListUnstructured(context.Background(), gvr, ns)
	if err != nil {
		t.Fatalf("ListUnstructured: %v", err)
	}
	var names []string
	for _, r := range remaining {
		names = append(names, r.GetName())
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "app-data,git-server-data" {
		t.Fatalf("remaining PVCs = %s, want app-data,git-server-data (only old-kaniko-cache is orphaned)\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "cleaned 1 PVC(s) in flywheel-system") {
		t.Errorf("summary missing or wrong:\n%s", out.String())
	}
}
