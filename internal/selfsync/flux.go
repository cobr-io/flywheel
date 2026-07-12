package selfsync

import (
	"context"
	"fmt"
	"strings"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cobr-io/flywheel/internal/fluxpoke"
	"github.com/cobr-io/flywheel/internal/naming"
)

// iuaGVK / kustGVK are the Flux image-automation and kustomize kinds. Flywheel
// vendors neither, so K8sFlux addresses them as unstructured (via
// fluxpoke.Unstructured). GitRepository IS vendored (sourcev1), so it is
// addressed as a typed object below.
var (
	iuaGVK  = schema.GroupVersionKind{Group: "image.toolkit.fluxcd.io", Version: "v1", Kind: "ImageUpdateAutomation"}
	kustGVK = schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}
)

// K8sFlux implements Flux against a Kubernetes API via a controller-runtime
// client. Reconcile pokes go through internal/fluxpoke — the single poke
// implementation shared with the build controllers. Mirrors what sync.sh did
// with kubectl.
type K8sFlux struct {
	Client client.Client

	GitRepoName, GitRepoNamespace             string
	IUAName, IUANamespace                     string
	KustomizationName, KustomizationNamespace string

	// WaitTimeout bounds WaitArtifact; ArtifactPoll is its poll cadence.
	WaitTimeout  time.Duration
	ArtifactPoll time.Duration

	// Now is a clock seam for tests; defaults to time.Now.
	Now func() time.Time
}

// gitRepoRef is a typed, name/namespace-only reference to the self
// GitRepository — enough to Get into or Patch, without carrying a re-declared
// unstructured GVK.
func (k *K8sFlux) gitRepoRef() *sourcev1.GitRepository {
	return &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Namespace: k.GitRepoNamespace, Name: k.GitRepoName},
	}
}

// ConfiguredAuthored reads the deploy-branch annotation off the self
// GitRepository, or "" when unset/absent.
func (k *K8sFlux) ConfiguredAuthored(ctx context.Context) (string, error) {
	gr := k.gitRepoRef()
	if err := k.Client.Get(ctx, client.ObjectKeyFromObject(gr), gr); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return gr.GetAnnotations()[naming.DeployBranchAnnotation], nil
}

// SuspendIUA sets spec.suspend on the ImageUpdateAutomation. A missing IUA is
// not an error (nothing to suspend).
func (k *K8sFlux) SuspendIUA(ctx context.Context, suspend bool) error {
	if k.IUAName == "" {
		return nil
	}
	u := fluxpoke.Unstructured(iuaGVK, k.IUANamespace, k.IUAName)
	return k.patch(ctx, u, fmt.Appendf(nil, `{"spec":{"suspend":%t}}`, suspend))
}

// PokeGitRepository bumps the self GitRepository's reconcile-request annotation.
func (k *K8sFlux) PokeGitRepository(ctx context.Context) error {
	return fluxpoke.Poke(ctx, k.Client, k.gitRepoRef(), k.nowTime())
}

// PokeKustomization bumps the apps Kustomization's reconcile-request annotation.
func (k *K8sFlux) PokeKustomization(ctx context.Context) error {
	if k.KustomizationName == "" {
		return nil
	}
	return fluxpoke.Poke(ctx, k.Client,
		fluxpoke.Unstructured(kustGVK, k.KustomizationNamespace, k.KustomizationName),
		k.nowTime())
}

// WaitArtifact blocks until the GitRepository's stored artifact revision contains
// targetSHA, or WaitTimeout elapses (so the Kustomization poke applies the fresh
// source, not a stale artifact). A timeout returns an error the caller treats as
// non-fatal.
func (k *K8sFlux) WaitArtifact(ctx context.Context, targetSHA string) error {
	timeout := k.WaitTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	poll := k.ArtifactPoll
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		gr := k.gitRepoRef()
		if err := k.Client.Get(ctx, client.ObjectKeyFromObject(gr), gr); err == nil {
			if a := gr.Status.Artifact; a != nil && strings.Contains(a.Revision, targetSHA) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("artifact did not reach %s within %s", short(targetSHA), timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// patch applies a JSON merge patch, treating a missing object as success. Used
// for the non-poke mutation (SuspendIUA); the reconcile pokes go through
// fluxpoke.
func (k *K8sFlux) patch(ctx context.Context, u client.Object, p []byte) error {
	err := k.Client.Patch(ctx, u, client.RawPatch(types.MergePatchType, p))
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (k *K8sFlux) nowTime() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}
