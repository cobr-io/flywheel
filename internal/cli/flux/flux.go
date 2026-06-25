// Package flux embeds the Flux install manifest at a pinned version
// and applies it via the SSA applier with fieldManager=flux-controller.
// `flywheel up` step 10.
//
// install.yaml is kept as PRISTINE upstream Flux — regenerating it is a
// clean overwrite with no manual re-patching. Flywheel's one deviation
// (a faster --requeue-dependency on kustomize-controller) is applied as a
// programmatic transform at apply time, see requeueDependency below.
package flux

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/cobr-io/flywheel/internal/cli/applier"
)

//go:embed install.yaml
var installManifest []byte

// Version is the pinned Flux version embedded above. Bumped when the
// manifest is regenerated.
const Version = "v2.8.7"

// requeueDependency is the value Flywheel sets for kustomize-controller's
// --requeue-dependency flag, overriding upstream Flux's 30s default.
//
// Why: a dependent Kustomization that evaluates its dependsOn while the
// target is mid-reconcile (a ~260ms Ready=Unknown blink that happens on
// every routine interval reconcile) requeues by this constant. At the 30s
// default that turned a 260ms race into a ~30s commit-to-pod outlier
// (~1 run in 4, measured). 2s bounds the worst case while the longer
// mirror-tier intervals make the race rare in the first place.
const requeueDependency = "2s"

// Install applies the embedded Flux manifest (with the requeue-dependency
// transform) via SSA.
func Install(ctx context.Context, a *applier.Applier, out io.Writer) error {
	manifest, err := transformedManifest()
	if err != nil {
		return err
	}
	return a.ApplyYAML(ctx, manifest, out)
}

// transformedManifest decodes the pristine embedded manifest, injects the
// --requeue-dependency flag into the kustomize-controller Deployment's
// `manager` container, and re-encodes the full multi-document stream.
// Documents other than that one Deployment pass through byte-for-byte
// (modulo YAML round-trip normalisation, which ApplyYAML tolerates since
// it re-decodes anyway).
func transformedManifest() ([]byte, error) {
	dec := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(installManifest), 4096)
	var out bytes.Buffer
	patched := false
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(obj); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decode flux manifest: %w", err)
		}
		if obj.Object == nil {
			continue
		}
		if isKustomizeControllerDeployment(obj) {
			if err := setRequeueDependency(obj); err != nil {
				return nil, err
			}
			patched = true
		}
		doc, err := yaml.Marshal(obj.Object)
		if err != nil {
			return nil, fmt.Errorf("re-encode flux doc: %w", err)
		}
		out.WriteString("---\n")
		out.Write(doc)
	}
	if !patched {
		// The kustomize-controller Deployment vanishing means the embedded
		// manifest changed shape out from under us — fail loud rather than
		// silently ship the 30s default and reintroduce the outlier.
		return nil, fmt.Errorf("flux transform: kustomize-controller Deployment not found in install.yaml")
	}
	return out.Bytes(), nil
}

func isKustomizeControllerDeployment(obj *unstructured.Unstructured) bool {
	return obj.GetKind() == "Deployment" && obj.GetName() == "kustomize-controller"
}

// setRequeueDependency sets/replaces --requeue-dependency=<requeueDependency>
// in the args of the Deployment's `manager` container. Idempotent: an
// existing --requeue-dependency flag (any value) is replaced, not duplicated.
func setRequeueDependency(obj *unstructured.Unstructured) error {
	containers, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		return fmt.Errorf("flux transform: kustomize-controller has no containers: %w", err)
	}
	flag := "--requeue-dependency=" + requeueDependency
	for i, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok || cm["name"] != "manager" {
			continue
		}
		args, _, _ := unstructured.NestedStringSlice(cm, "args")
		replaced := false
		for j, a := range args {
			if len(a) >= len("--requeue-dependency=") && a[:len("--requeue-dependency=")] == "--requeue-dependency=" {
				args[j] = flag
				replaced = true
				break
			}
		}
		if !replaced {
			args = append(args, flag)
		}
		argsAny := make([]any, len(args))
		for j, a := range args {
			argsAny[j] = a
		}
		cm["args"] = argsAny
		containers[i] = cm
		if err := unstructured.SetNestedSlice(obj.Object, containers, "spec", "template", "spec", "containers"); err != nil {
			return fmt.Errorf("flux transform: set containers: %w", err)
		}
		return nil
	}
	return fmt.Errorf("flux transform: kustomize-controller has no `manager` container")
}
