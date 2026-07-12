// Package applier server-side-applies kustomize-built manifests via
// client-go. Every apply uses fieldManager="flux-controller" so that
// when Flux takes over the same resource later it silently adopts
// ownership without conflict warnings or drift-restomp loops (per
// design § up step 10 and the closed material gap on field-manager
// strategy).
package applier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/kustomize/api/krusty"
	ktypes "sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"github.com/cobr-io/flywheel/internal/cli/style"
)

// FieldManager is the SSA field manager used by every Flywheel apply.
// Matches what Flux's kustomize-controller uses, so subsequent Flux
// reconciles silently adopt the same fields without ownership warnings.
const FieldManager = "flux-controller"

// Applier is reusable — build it once with a kubeconfig context, apply
// many manifests over its lifetime.
type Applier struct {
	dyn    dynamic.Interface
	mapper *restmapper.DeferredDiscoveryRESTMapper
}

// ResetMapper invalidates the cached discovery data so newly-installed
// CRDs (e.g. Flux's ImageUpdateAutomation, installed in `up` step 10)
// become mappable. Call after applying CRDs and before applying the
// custom resources that use them.
func (a *Applier) ResetMapper() {
	a.mapper.Reset()
}

// New constructs an Applier bound to the given kubeconfig context.
// `kubeconfigPath` may be empty (uses the default loading rules — env
// KUBECONFIG, then ~/.kube/config). `contextName` may be empty (uses
// the current-context).
func New(kubeconfigPath, contextName string) (*Applier, error) {
	cfg, err := loadRESTConfig(kubeconfigPath, contextName)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(
		memory.NewMemCacheClient(disc),
	)
	return &Applier{dyn: dyn, mapper: mapper}, nil
}

// NewForTest builds an Applier around an already-constructed dynamic client
// and REST mapper, bypassing kubeconfig/discovery. Lets tests inject a fake
// dynamic client (k8s.io/client-go/dynamic/fake) and a mapper backed by a
// fake discovery client, to exercise a full List+Delete flow without a real
// cluster. Production code uses New.
func NewForTest(dyn dynamic.Interface, mapper *restmapper.DeferredDiscoveryRESTMapper) *Applier {
	return &Applier{dyn: dyn, mapper: mapper}
}

// ApplyKustomize builds the kustomization at `dir` and applies every
// resource with SSA fieldManager=flux-controller.
func (a *Applier) ApplyKustomize(ctx context.Context, dir string, out io.Writer) error {
	_, err := a.ApplyKustomizeTracked(ctx, dir, out)
	return err
}

// ApplyKustomizeTracked is ApplyKustomize that also returns a ResourceRef for
// every object it SUCCESSFULLY applied. `flywheel up` uses this to build the
// "keep set" for its orphan prune (converge.PruneOrphanedMachinery): only a
// resource that this run actually applied is spared, so a manifest that stops
// being rendered between versions can be reaped. The error is non-nil if any
// object failed (the returned refs then cover only the ones that landed).
func (a *Applier) ApplyKustomizeTracked(ctx context.Context, dir string, out io.Writer) ([]ResourceRef, error) {
	yamlBytes, err := buildKustomize(dir)
	if err != nil {
		return nil, fmt.Errorf("kustomize build %s: %w", dir, err)
	}
	return a.applyYAML(ctx, yamlBytes, out)
}

// ResourceRef identifies a single resource (no spec) — used by
// DeleteResource and `flywheel clean` to address a resource for deletion.
type ResourceRef struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

// ApplyYAML applies a (possibly multi-document) YAML blob. Each
// document becomes one SSA patch.
func (a *Applier) ApplyYAML(ctx context.Context, raw []byte, out io.Writer) error {
	_, err := a.applyYAML(ctx, raw, out)
	return err
}

// applyYAML is the shared apply engine: it SSA-patches every document and
// returns a ResourceRef for each one that succeeded (the tracking the
// orphan prune relies on). On a decode error it returns immediately; per-
// object apply failures are warned and skipped (so they don't enter the keep
// set), and every one is aggregated into the returned error via errors.Join
// — so a multi-doc apply that fails on more than one object reports all of
// them by name, not just the last. Each joined error wraps the underlying
// apply error with %w, so callers' errors.Is/As still reach it.
func (a *Applier) applyYAML(ctx context.Context, raw []byte, out io.Writer) ([]ResourceRef, error) {
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(raw)), 4096)
	var applied []ResourceRef
	var applyErrs []error
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(obj); err != nil {
			if err == io.EOF {
				break
			}
			return applied, fmt.Errorf("decode yaml: %w", err)
		}
		if obj.Object == nil {
			continue
		}
		if err := a.ApplyObject(ctx, obj, out); err != nil {
			ident := fmt.Sprintf("%s %s/%s",
				obj.GroupVersionKind().GroupKind().String(),
				obj.GetNamespace(),
				obj.GetName())
			style.Warn(out, "apply %s: %v", ident, err)
			applyErrs = append(applyErrs, fmt.Errorf("apply %s: %w", ident, err))
			continue
		}
		gvk := obj.GroupVersionKind()
		applied = append(applied, ResourceRef{
			Group:     gvk.Group,
			Kind:      gvk.Kind,
			Namespace: obj.GetNamespace(),
			Name:      obj.GetName(),
		})
	}
	return applied, errors.Join(applyErrs...)
}

// ApplyObject does one SSA Patch for the given unstructured object.
func (a *Applier) ApplyObject(ctx context.Context, obj *unstructured.Unstructured, out io.Writer) error {
	return a.ApplyObjectAs(ctx, obj, FieldManager, out)
}

// ApplyObjectAs is ApplyObject with an explicit SSA field manager. Use a
// manager OTHER than FieldManager when a field must SURVIVE a later
// `flux-controller` apply (SSA only strips fields owned by the same manager
// that omits them). `flywheel use` relies on this so its deploy-branch
// annotation isn't erased when `flywheel up` re-applies the self-source
// manifest (issue #17).
func (a *Applier) ApplyObjectAs(ctx context.Context, obj *unstructured.Unstructured, fieldManager string, out io.Writer) error {
	gvk := obj.GroupVersionKind()
	mapping, err := a.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		// CRDs that don't exist yet show up as no-match. Caller should
		// re-run after CRDs install; surface as a soft error.
		return fmt.Errorf("REST mapping: %w", err)
	}

	var resource dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := obj.GetNamespace()
		if ns == "" {
			ns = "default"
		}
		resource = a.dyn.Resource(mapping.Resource).Namespace(ns)
	} else {
		resource = a.dyn.Resource(mapping.Resource)
	}

	data, err := obj.MarshalJSON()
	if err != nil {
		return err
	}
	force := true
	_, err = resource.Patch(ctx, obj.GetName(), types.ApplyPatchType, data,
		metav1.PatchOptions{
			FieldManager: fieldManager,
			Force:        &force,
		})
	if err != nil {
		return err
	}
	style.OKv(out, "%s/%s %s",
		schemaGVKLabel(gvk),
		obj.GetNamespace(),
		obj.GetName())
	return nil
}

// DeleteResource deletes a single resource by ref (used by `flywheel clean`).
// Missing resources are not an error (idempotent).
func (a *Applier) DeleteResource(ctx context.Context, ref ResourceRef, out io.Writer) error {
	mapping, err := a.mapper.RESTMapping(schema.GroupKind{Group: ref.Group, Kind: ref.Kind})
	if err != nil {
		return fmt.Errorf("REST mapping %s/%s: %w", ref.Group, ref.Kind, err)
	}
	var resource dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := ref.Namespace
		if ns == "" {
			ns = "default"
		}
		resource = a.dyn.Resource(mapping.Resource).Namespace(ns)
	} else {
		resource = a.dyn.Resource(mapping.Resource)
	}
	err = resource.Delete(ctx, ref.Name, metav1.DeleteOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	fmt.Fprintf(out, "  deleted %s %s/%s\n", refLabel(ref), ref.Namespace, ref.Name)
	return nil
}

func refLabel(r ResourceRef) string {
	if r.Group == "" {
		return r.Kind
	}
	return r.Group + "/" + r.Kind
}

// GetUnstructured fetches a single object by GVR + name. Used by
// step-completion polls in `flywheel up` (waitForDeployments,
// waitForFluxKustomizations).
func (a *Applier) GetUnstructured(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	var resource dynamic.ResourceInterface
	if namespace == "" {
		resource = a.dyn.Resource(gvr)
	} else {
		resource = a.dyn.Resource(gvr).Namespace(namespace)
	}
	return resource.Get(ctx, name, metav1.GetOptions{})
}

// ListUnstructured lists objects of a GVR in `namespace`. Empty
// namespace = all-namespaces. Returns the items slice (not the wrapping
// List object) for convenience.
func (a *Applier) ListUnstructured(ctx context.Context, gvr schema.GroupVersionResource, namespace string) ([]unstructured.Unstructured, error) {
	var resource dynamic.ResourceInterface
	if namespace == "" {
		resource = a.dyn.Resource(gvr)
	} else {
		resource = a.dyn.Resource(gvr).Namespace(namespace)
	}
	list, err := resource.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ListUnstructuredLabeled is ListUnstructured scoped additionally by
// labelSelector. Used where the caller must not operate on every object of
// a kind in the namespace — e.g. `flywheel clean`, which must only delete
// PVCs it applied (managed-by=flywheel), not ones an app or Flux created.
func (a *Applier) ListUnstructuredLabeled(ctx context.Context, gvr schema.GroupVersionResource, namespace, labelSelector string) ([]unstructured.Unstructured, error) {
	var resource dynamic.ResourceInterface
	if namespace == "" {
		resource = a.dyn.Resource(gvr)
	} else {
		resource = a.dyn.Resource(gvr).Namespace(namespace)
	}
	list, err := resource.List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ListByKindLabeled lists every object of the given GroupKind (across all
// namespaces for a namespaced kind, cluster-wide for a cluster-scoped one)
// that matches labelSelector. The orphan prune uses it to find flywheel's
// own applied machinery (label app.kubernetes.io/managed-by=flywheel) without
// having to enumerate namespaces — the label, not the namespace, bounds the
// candidate set. The GroupKind is resolved to a resource via the REST mapper,
// so the caller passes kinds (Deployment, DaemonSet, ...), not GVRs.
func (a *Applier) ListByKindLabeled(ctx context.Context, gk schema.GroupKind, labelSelector string) ([]unstructured.Unstructured, error) {
	mapping, err := a.mapper.RESTMapping(gk)
	if err != nil {
		return nil, fmt.Errorf("REST mapping %s: %w", gk.String(), err)
	}
	// Resource(gvr) with no .Namespace() lists across all namespaces for a
	// namespaced kind and cluster-wide for a cluster-scoped one.
	list, err := a.dyn.Resource(mapping.Resource).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func schemaGVKLabel(gvk schema.GroupVersionKind) string {
	if gvk.Group == "" {
		return gvk.Kind
	}
	return gvk.Group + "/" + gvk.Kind
}

func buildKustomize(dir string) ([]byte, error) {
	opts := krusty.MakeDefaultOptions()
	// `flywheel up` generates transient overlays in a temp dir that
	// reference the cached base via an absolute path (outside the
	// overlay root). Default kustomize forbids that; relax the
	// restrictor so cross-tree resource references resolve.
	opts.LoadRestrictions = ktypes.LoadRestrictionsNone
	k := krusty.MakeKustomizer(opts)
	fs := filesys.MakeFsOnDisk()
	rm, err := k.Run(fs, dir)
	if err != nil {
		return nil, err
	}
	return rm.AsYaml()
}

func loadRESTConfig(kubeconfigPath, contextName string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		rules.ExplicitPath = kubeconfigPath
	}
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	return cc.ClientConfig()
}
