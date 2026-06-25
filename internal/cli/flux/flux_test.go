package flux

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

// decodeAll splits a multi-doc YAML blob into unstructured objects.
func decodeAll(t *testing.T, raw []byte) []*unstructured.Unstructured {
	t.Helper()
	dec := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var objs []*unstructured.Unstructured
	for {
		o := &unstructured.Unstructured{}
		if err := dec.Decode(o); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode: %v", err)
		}
		if o.Object != nil {
			objs = append(objs, o)
		}
	}
	return objs
}

func managerArgs(t *testing.T, objs []*unstructured.Unstructured, deployment string) []string {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() != "Deployment" || o.GetName() != deployment {
			continue
		}
		cs, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
		for _, c := range cs {
			cm := c.(map[string]any)
			if cm["name"] != "manager" {
				continue
			}
			args, _, _ := unstructured.NestedStringSlice(cm, "args")
			return args
		}
	}
	t.Fatalf("deployment %q manager container not found", deployment)
	return nil
}

func countFlag(args []string, prefix string) int {
	n := 0
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			n++
		}
	}
	return n
}

// TestTransformInjectsRequeueDependency is the guard the design relies on:
// the embedded (pristine) manifest must come out with exactly one
// --requeue-dependency flag at our value on kustomize-controller, and it
// must be absent from the pristine input (proving the transform, not the
// vendored file, is what sets it).
func TestTransformInjectsRequeueDependency(t *testing.T) {
	pristine := decodeAll(t, installManifest)
	if got := countFlag(managerArgs(t, pristine, "kustomize-controller"), "--requeue-dependency"); got != 0 {
		t.Fatalf("pristine install.yaml already has --requeue-dependency (%d); it must stay stock — set the flag via the transform, not the file", got)
	}

	out, err := transformedManifest()
	if err != nil {
		t.Fatalf("transformedManifest: %v", err)
	}
	args := managerArgs(t, decodeAll(t, out), "kustomize-controller")
	if got := countFlag(args, "--requeue-dependency"); got != 1 {
		t.Fatalf("transformed manifest has %d --requeue-dependency flags, want exactly 1: %v", got, args)
	}
	want := "--requeue-dependency=" + requeueDependency
	found := false
	for _, a := range args {
		if a == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("transformed args missing %q: %v", want, args)
	}
}

// TestTransformIdempotent: running the transform on already-transformed
// output yields the same single flag (no duplicate), so a re-apply is safe.
func TestTransformIdempotent(t *testing.T) {
	once, err := transformedManifest()
	if err != nil {
		t.Fatal(err)
	}
	// Feed the transformed output back through by swapping it in as the
	// source for one more pass via the same logic path.
	saved := installManifest
	installManifest = once
	defer func() { installManifest = saved }()

	twice, err := transformedManifest()
	if err != nil {
		t.Fatal(err)
	}
	args := managerArgs(t, decodeAll(t, twice), "kustomize-controller")
	if got := countFlag(args, "--requeue-dependency"); got != 1 {
		t.Fatalf("re-transform produced %d --requeue-dependency flags, want 1: %v", got, args)
	}
}

// TestTransformPreservesOtherDeployments: a non-target Deployment
// (source-controller) must pass through unmodified — in particular we must
// NOT inject the flag into anything but kustomize-controller.
func TestTransformPreservesOtherDeployments(t *testing.T) {
	out, err := transformedManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range []string{"source-controller", "image-automation-controller"} {
		if got := countFlag(managerArgs(t, decodeAll(t, out), dep), "--requeue-dependency"); got != 0 {
			t.Errorf("%s wrongly got --requeue-dependency (%d)", dep, got)
		}
	}
}
