package converge

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/cobr-io/flywheel/internal/cli/applier"
	flywheelSchema "github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/style"
)

// ApplyDevLoop renders the dev-loop manifests with image references
// rewritten for THIS client using the resolved (override-aware) refs
// from imagepin.Resolve. Each `ghcr.io/cobr-io/<name>` slot in the base
// is rewritten to the resolved ref — same as what's already imported
// into the cluster's containerd in step 9. gitServerMemLimit patches the
// git-server container memory limit (see § git-server OOM, issue #4); it
// MUST match the limit the flywheel-dev-loop Flux Kustomization applies
// (builders-kustomization.yaml.tmpl), or the two reconcile paths would
// fight — both are rendered from the same cfg.GitServerMemoryLimit().
func ApplyDevLoop(ctx context.Context, a *applier.Applier, overlayDir string, refs map[string]string, gitServerMemLimit string, out io.Writer) error {
	// Create the transient overlay as a sibling of `base` inside the
	// cache tree, so the resource reference is simply `../base` — no
	// absolute paths (kustomize forbids them) and no `/var`→`/private/var`
	// symlink hazard from os.TempDir on macOS.
	devLoopDir := filepath.Dir(overlayDir)  // .../manifests/dev-loop/overlays
	devLoopRoot := filepath.Dir(devLoopDir) // .../manifests/dev-loop
	tmp, err := os.MkdirTemp(devLoopRoot, ".flywheel-tmp-overlay-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	kustomization := renderDevLoopKustomization(refs, gitServerMemLimit)
	if err := os.WriteFile(filepath.Join(tmp, "kustomization.yaml"), []byte(kustomization), 0o644); err != nil {
		return err
	}
	return a.ApplyKustomize(ctx, tmp, out)
}

// renderDevLoopKustomization builds the transient overlay's kustomization.yaml:
// it rewrites each base ghcr.io image ref to the resolved ref, and patches the
// git-server container's memory limit. Pure (no I/O) so it can be unit-tested.
func renderDevLoopKustomization(refs map[string]string, gitServerMemLimit string) string {
	var images strings.Builder
	for _, name := range flywheelSchema.ImageNames {
		ref := refs[name]
		newName, newTag := splitImageRef(ref)
		fmt.Fprintf(&images, "  - name: ghcr.io/cobr-io/%s\n    newName: %s\n", name, newName)
		if newTag != "" {
			fmt.Fprintf(&images, "    newTag: %s\n", newTag)
		}
	}
	return fmt.Sprintf(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../base
images:
%s%s`, images.String(), gitServerMemoryPatch(gitServerMemLimit))
}

// gitServerMemoryPatch returns a kustomize strategic-merge patch block that
// sets the git-server container's memory limit. Shared shape with the
// flywheel-dev-loop Flux Kustomization (builders-kustomization.yaml.tmpl) so
// the direct-apply path and the Flux reconcile path converge on one value.
func gitServerMemoryPatch(limit string) string {
	return fmt.Sprintf(`patches:
  - patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: git-server
        namespace: flywheel-system
      spec:
        template:
          spec:
            containers:
              - name: git-server
                resources:
                  limits:
                    memory: %s
`, limit)
}

// splitImageRef splits an image reference into newName + newTag. If the
// ref has no tag (rare; only for digest refs or untagged), newTag is
// empty and kustomize will leave the tag at the base's value.
func splitImageRef(ref string) (string, string) {
	// Handle "repo@sha256:..." digest refs by treating the whole thing
	// as newName (kustomize accepts it).
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		return ref, ""
	}
	// Tag is everything after the LAST ":", unless that colon is part
	// of a registry port (i.e. there's a "/" after it).
	i := strings.LastIndex(ref, ":")
	if i < 0 {
		return ref, ""
	}
	if strings.Contains(ref[i:], "/") {
		// The ":" we found is a port, not a tag separator.
		return ref, ""
	}
	return ref[:i], ref[i+1:]
}

// ApplyFlywheelConfig regenerates the flywheel-config ConfigMap from the
// merged flywheel.yaml and applies it to flywheel-system. The keys here
// are the contract from design § The flywheel-config ConfigMap; nothing
// under paths.* or sops.* is included. (Closed material gap O3 / T1.13.)
func ApplyFlywheelConfig(ctx context.Context, a *applier.Applier, cfg *flywheelSchema.File, out io.Writer) error {
	data := map[string]interface{}{
		"client.name":           cfg.Client.Name,
		"cluster.name":          cfg.Cluster.Name,
		"cluster.registry":      cfg.Cluster.Registry,
		"cluster.registry_port": fmt.Sprintf("%d", cfg.Cluster.RegistryPort),
		"namespaces.flywheel":   cfg.Namespaces.Flywheel,
		"namespaces.apps":       cfg.Namespaces.Apps,
		"flux.interval_local":   cfg.Flux.IntervalLocal,
		"local.domain":          cfg.Local.Domain,
	}
	cm := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "flywheel-config",
				"namespace": cfg.Namespaces.Flywheel,
				// Marks this as flywheel-applied machinery for `update --prune`.
				"labels": map[string]interface{}{
					"app.kubernetes.io/managed-by": "flywheel",
				},
			},
			"data": data,
		},
	}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	return a.ApplyObject(ctx, cm, out)
}

// WaitForDeployments polls every named Deployment in `namespace` in
// parallel through a single style.Waiter, so the user sees one
// live-updating block showing each Deployment's current state instead
// of a silent multi-minute pause.
func WaitForDeployments(ctx context.Context, a *applier.Applier, namespace string, names []string, timeout time.Duration, out io.Writer) error {
	header := fmt.Sprintf("waiting for %s deployments", namespace)
	w := style.NewWaiter(out, header)
	for _, n := range names {
		w.Add(n)
	}

	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, n := range names {
			u, err := a.GetUnstructured(ctx, gvr, namespace, n)
			switch {
			case err != nil:
				w.Set(n, style.Pending, "waiting to appear")
			case deploymentReady(u):
				w.Set(n, style.Ready, "ready")
			default:
				w.Set(n, style.Pending, deploymentDetail(u))
			}
		}
		w.Tick()
		if w.AllResolved() {
			w.Done(fmt.Sprintf("%s deployments ready", namespace))
			return nil
		}
		select {
		case <-ctx.Done():
			w.Done("")
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	w.Done("")
	return fmt.Errorf("deployments not Ready before deadline (namespace=%s)", namespace)
}

// deploymentDetail surfaces a short description of why a Deployment
// isn't Ready yet — useful for the wait UI's "what are we waiting on"
// column.
func deploymentDetail(u *unstructured.Unstructured) string {
	if u == nil {
		return "not yet created"
	}
	avail, _, _ := unstructured.NestedInt64(u.Object, "status", "availableReplicas")
	desired, _, _ := unstructured.NestedInt64(u.Object, "spec", "replicas")
	if desired == 0 {
		desired = 1
	}
	// Look for a more specific cause in conditions. Progressing=False
	// usually means an ImagePullBackOff or similar.
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		ctype, _ := m["type"].(string)
		status, _ := m["status"].(string)
		if ctype == "Progressing" && status == "False" {
			if reason, _ := m["reason"].(string); reason != "" {
				return reason
			}
		}
	}
	return fmt.Sprintf("%d/%d available", avail, desired)
}

func deploymentReady(u *unstructured.Unstructured) bool {
	status, found, err := unstructured.NestedMap(u.Object, "status")
	if err != nil || !found {
		return false
	}
	avail, _, _ := unstructured.NestedInt64(status, "availableReplicas")
	desired, _, _ := unstructured.NestedInt64(u.Object, "spec", "replicas")
	if desired == 0 {
		desired = 1
	}
	return avail >= desired
}
