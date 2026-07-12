package selfsync

import (
	"context"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/cobr-io/flywheel/internal/naming"
)

var fluxFixedNow = time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

// fluxScheme registers the typed GitRepository plus the unstructured Flux kinds
// (IUA, Kustomization) K8sFlux addresses.
func fluxScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := sourcev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, gvk := range []schema.GroupVersionKind{iuaGVK, kustGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

func newK8sFlux(c client.Client) *K8sFlux {
	return &K8sFlux{
		Client:                 c,
		GitRepoName:            "flux-system",
		GitRepoNamespace:       naming.FluxNamespace,
		IUAName:                "flywheel-self",
		IUANamespace:           naming.FluxNamespace,
		KustomizationName:      "client-apps",
		KustomizationNamespace: naming.FluxNamespace,
		WaitTimeout:            50 * time.Millisecond,
		ArtifactPoll:           5 * time.Millisecond,
		Now:                    func() time.Time { return fluxFixedNow },
	}
}

func selfGitRepo() *sourcev1.GitRepository {
	return &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "flux-system", Namespace: naming.FluxNamespace},
	}
}

// TestK8sFlux_ConfiguredAuthored reads the deploy-branch annotation off the
// typed self GitRepository, and returns "" when the GR is absent.
func TestK8sFlux_ConfiguredAuthored(t *testing.T) {
	gr := selfGitRepo()
	gr.Annotations = map[string]string{naming.DeployBranchAnnotation: "feat-x"}
	c := fake.NewClientBuilder().WithScheme(fluxScheme(t)).WithObjects(gr).Build()

	got, err := newK8sFlux(c).ConfiguredAuthored(context.Background())
	if err != nil {
		t.Fatalf("ConfiguredAuthored: %v", err)
	}
	if got != "feat-x" {
		t.Errorf("ConfiguredAuthored = %q, want %q", got, "feat-x")
	}

	// Absent GR -> "" (not an error): a fresh cluster before `flywheel use`.
	empty := fake.NewClientBuilder().WithScheme(fluxScheme(t)).Build()
	got, err = newK8sFlux(empty).ConfiguredAuthored(context.Background())
	if err != nil || got != "" {
		t.Errorf("ConfiguredAuthored(absent) = %q, %v; want \"\", nil", got, err)
	}
}

// TestK8sFlux_PokeGitRepository stamps the reconcile-request annotation on the
// typed GitRepository.
func TestK8sFlux_PokeGitRepository(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(fluxScheme(t)).WithObjects(selfGitRepo()).Build()

	if err := newK8sFlux(c).PokeGitRepository(context.Background()); err != nil {
		t.Fatalf("PokeGitRepository: %v", err)
	}
	var got sourcev1.GitRepository
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: naming.FluxNamespace, Name: "flux-system"}, &got); err != nil {
		t.Fatal(err)
	}
	if v := got.Annotations[naming.ReconcileRequestAnnotation]; v != fluxFixedNow.Format(time.RFC3339Nano) {
		t.Errorf("reconcile-request annotation = %q, want %q", v, fluxFixedNow.Format(time.RFC3339Nano))
	}
}

// TestK8sFlux_PokeNotFoundTolerated: poking absent Flux objects is not an error
// (they fall back to their poll interval).
func TestK8sFlux_PokeNotFoundTolerated(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(fluxScheme(t)).Build()
	k := newK8sFlux(c)
	if err := k.PokeGitRepository(context.Background()); err != nil {
		t.Errorf("PokeGitRepository(absent) = %v, want nil", err)
	}
	if err := k.PokeKustomization(context.Background()); err != nil {
		t.Errorf("PokeKustomization(absent) = %v, want nil", err)
	}
}

// TestK8sFlux_SuspendIUA sets spec.suspend on the unstructured IUA, and is a
// no-op (nil) when no IUA name is configured.
func TestK8sFlux_SuspendIUA(t *testing.T) {
	iua := &unstructured.Unstructured{}
	iua.SetGroupVersionKind(iuaGVK)
	iua.SetNamespace(naming.FluxNamespace)
	iua.SetName("flywheel-self")
	c := fake.NewClientBuilder().WithScheme(fluxScheme(t)).WithObjects(iua).Build()

	if err := newK8sFlux(c).SuspendIUA(context.Background(), true); err != nil {
		t.Fatalf("SuspendIUA: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(iuaGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: naming.FluxNamespace, Name: "flywheel-self"}, got); err != nil {
		t.Fatal(err)
	}
	suspend, found, _ := unstructured.NestedBool(got.Object, "spec", "suspend")
	if !found || !suspend {
		t.Errorf("spec.suspend = %v (found=%v), want true", suspend, found)
	}

	// No IUA configured -> no-op.
	k := newK8sFlux(c)
	k.IUAName = ""
	if err := k.SuspendIUA(context.Background(), true); err != nil {
		t.Errorf("SuspendIUA with empty IUAName = %v, want nil", err)
	}
}

// TestK8sFlux_PokeKustomization stamps the annotation on the unstructured
// Kustomization, and is a no-op when unnamed.
func TestK8sFlux_PokeKustomization(t *testing.T) {
	kust := &unstructured.Unstructured{}
	kust.SetGroupVersionKind(kustGVK)
	kust.SetNamespace(naming.FluxNamespace)
	kust.SetName("client-apps")
	c := fake.NewClientBuilder().WithScheme(fluxScheme(t)).WithObjects(kust).Build()

	if err := newK8sFlux(c).PokeKustomization(context.Background()); err != nil {
		t.Fatalf("PokeKustomization: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(kustGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: naming.FluxNamespace, Name: "client-apps"}, got); err != nil {
		t.Fatal(err)
	}
	if v := got.GetAnnotations()[naming.ReconcileRequestAnnotation]; v != fluxFixedNow.Format(time.RFC3339Nano) {
		t.Errorf("reconcile-request annotation = %q, want %q", v, fluxFixedNow.Format(time.RFC3339Nano))
	}

	k := newK8sFlux(c)
	k.KustomizationName = ""
	if err := k.PokeKustomization(context.Background()); err != nil {
		t.Errorf("PokeKustomization with empty name = %v, want nil", err)
	}
}

// TestK8sFlux_WaitArtifact returns nil once the GitRepository's artifact
// revision contains the target SHA, and errors when it never does.
func TestK8sFlux_WaitArtifact(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"

	ready := selfGitRepo()
	ready.Status = sourcev1.GitRepositoryStatus{Artifact: &meta.Artifact{Revision: "flywheel/local-deploy@sha1:" + sha}}
	c := fake.NewClientBuilder().WithScheme(fluxScheme(t)).WithObjects(ready).Build()
	if err := newK8sFlux(c).WaitArtifact(context.Background(), sha); err != nil {
		t.Errorf("WaitArtifact(present) = %v, want nil", err)
	}

	// Artifact never advances to the target SHA -> bounded wait then error.
	stale := selfGitRepo()
	stale.Status = sourcev1.GitRepositoryStatus{Artifact: &meta.Artifact{Revision: "main@sha1:deadbeef"}}
	c2 := fake.NewClientBuilder().WithScheme(fluxScheme(t)).WithObjects(stale).Build()
	if err := newK8sFlux(c2).WaitArtifact(context.Background(), sha); err == nil {
		t.Error("WaitArtifact(absent) = nil, want timeout error")
	}
}
