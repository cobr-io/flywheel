package selfsync

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cobr-io/flywheel/internal/naming"
)

var (
	gitRepoGVK = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"}
	iuaGVK     = schema.GroupVersionKind{Group: "image.toolkit.fluxcd.io", Version: "v1", Kind: "ImageUpdateAutomation"}
	kustGVK    = schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}
)

// K8sFlux implements Flux against a Kubernetes API via a controller-runtime
// client, patching Flux objects as unstructured (there is no typed image-
// automation API vendored). Mirrors the patch shapes used by
// internal/controller/imagepolicy_iua_controller.go and what sync.sh did with
// kubectl.
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

// ConfiguredAuthored reads the deploy-branch annotation off the self
// GitRepository, or "" when unset/absent.
func (k *K8sFlux) ConfiguredAuthored(ctx context.Context) (string, error) {
	u := obj(gitRepoGVK, k.GitRepoNamespace, k.GitRepoName)
	if err := k.Client.Get(ctx, client.ObjectKeyFromObject(u), u); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return u.GetAnnotations()[naming.DeployBranchAnnotation], nil
}

// SuspendIUA sets spec.suspend on the ImageUpdateAutomation. A missing IUA is
// not an error (nothing to suspend).
func (k *K8sFlux) SuspendIUA(ctx context.Context, suspend bool) error {
	if k.IUAName == "" {
		return nil
	}
	u := obj(iuaGVK, k.IUANamespace, k.IUAName)
	return k.patch(ctx, u, fmt.Appendf(nil, `{"spec":{"suspend":%t}}`, suspend))
}

// PokeGitRepository bumps the self GitRepository's reconcile-request annotation.
func (k *K8sFlux) PokeGitRepository(ctx context.Context) error {
	return k.pokeReconcile(ctx, gitRepoGVK, k.GitRepoNamespace, k.GitRepoName)
}

// PokeKustomization bumps the apps Kustomization's reconcile-request annotation.
func (k *K8sFlux) PokeKustomization(ctx context.Context) error {
	if k.KustomizationName == "" {
		return nil
	}
	return k.pokeReconcile(ctx, kustGVK, k.KustomizationNamespace, k.KustomizationName)
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
		u := obj(gitRepoGVK, k.GitRepoNamespace, k.GitRepoName)
		if err := k.Client.Get(ctx, client.ObjectKeyFromObject(u), u); err == nil {
			rev, _, _ := unstructured.NestedString(u.Object, "status", "artifact", "revision")
			if strings.Contains(rev, targetSHA) {
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

func (k *K8sFlux) pokeReconcile(ctx context.Context, gvk schema.GroupVersionKind, ns, name string) error {
	u := obj(gvk, ns, name)
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, naming.ReconcileRequestAnnotation, k.now())
	return k.patch(ctx, u, patch)
}

// patch applies a JSON merge patch, treating a missing object as success.
func (k *K8sFlux) patch(ctx context.Context, u *unstructured.Unstructured, p []byte) error {
	err := k.Client.Patch(ctx, u, client.RawPatch(types.MergePatchType, p))
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (k *K8sFlux) now() string {
	n := time.Now
	if k.Now != nil {
		n = k.Now
	}
	return n().UTC().Format(time.RFC3339Nano)
}

func obj(gvk schema.GroupVersionKind, ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}
