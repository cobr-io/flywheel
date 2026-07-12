package fluxpoke

import (
	"context"
	"testing"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/cobr-io/flywheel/internal/naming"
)

var fixedNow = time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

// TestPoke_SetsAnnotationOnTypedObject proves the poke stamps the
// reconcile-request annotation on a vendored (typed) GitRepository, and that
// the merge patch leaves the object's pre-existing annotations intact rather
// than clobbering the whole map.
func TestPoke_SetsAnnotationOnTypedObject(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := sourcev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gr := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "flux-system",
			Namespace:   "flux-system",
			Annotations: map[string]string{"keep": "me"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gr).Build()

	ref := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "flux-system", Namespace: "flux-system"},
	}
	if err := Poke(context.Background(), c, ref, fixedNow); err != nil {
		t.Fatalf("Poke: %v", err)
	}

	var got sourcev1.GitRepository
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gr), &got); err != nil {
		t.Fatal(err)
	}
	if v := got.Annotations[naming.ReconcileRequestAnnotation]; v != fixedNow.Format(time.RFC3339Nano) {
		t.Errorf("reconcile-request annotation = %q, want %q", v, fixedNow.Format(time.RFC3339Nano))
	}
	if got.Annotations["keep"] != "me" {
		t.Errorf("merge patch clobbered pre-existing annotations: %v", got.Annotations)
	}
}

// TestPoke_SetsAnnotationOnUnstructuredObject proves the Unstructured() ref form
// works for a Flux kind flywheel does not vendor a typed API for.
func TestPoke_SetsAnnotationOnUnstructuredObject(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "image.toolkit.fluxcd.io", Version: "v1", Kind: "ImageRepository"}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})

	existing := Unstructured(gvk, "flux-system", "sample-app")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()

	if err := Poke(context.Background(), c, Unstructured(gvk, "flux-system", "sample-app"), fixedNow); err != nil {
		t.Fatalf("Poke: %v", err)
	}

	got := Unstructured(gvk, "flux-system", "sample-app")
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(got), got); err != nil {
		t.Fatal(err)
	}
	if v := got.GetAnnotations()[naming.ReconcileRequestAnnotation]; v != fixedNow.Format(time.RFC3339Nano) {
		t.Errorf("reconcile-request annotation = %q, want %q", v, fixedNow.Format(time.RFC3339Nano))
	}
}

// TestPoke_NotFoundTolerated proves a poke at an absent object is not an error:
// the target simply falls back to its Flux poll interval.
func TestPoke_NotFoundTolerated(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := sourcev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	ref := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "does-not-exist", Namespace: "flux-system"},
	}
	if err := Poke(context.Background(), c, ref, fixedNow); err != nil {
		t.Errorf("Poke on absent object should be nil (NotFound tolerated), got %v", err)
	}
}
