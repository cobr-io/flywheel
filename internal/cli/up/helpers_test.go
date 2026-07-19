package up

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/cobr-io/flywheel/internal/cli/applier"
	"github.com/cobr-io/flywheel/internal/naming"
)

// A real, parseable age private key — loadAgeKey itself doesn't parse it, but
// using a realistic value keeps the test honest about what gets installed.
const testAgeKey = "AGE-SECRET-KEY-1QYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQ8"

func writeKey(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// The committed repo key (clusters/local/age.key) is canonical: it's returned
// even when a host key also exists, and a 0644 checkout (not 0600) is fine.
func TestLoadAgeKey_RepoKeyWins(t *testing.T) {
	repo := t.TempDir()
	home := t.TempDir()
	writeKey(t, filepath.Join(repo, "clusters", "local", "age.key"), testAgeKey+"\n", 0o644)
	writeKey(t, filepath.Join(home, ".config", "flywheel", "acme", "age.key"), "AGE-SECRET-KEY-HOSTCOPY\n", 0o600)

	content, path, err := loadAgeKey(repo, "acme", home)
	if err != nil {
		t.Fatalf("loadAgeKey: %v", err)
	}
	if want := filepath.Join(repo, "clusters", "local", "age.key"); path != want {
		t.Errorf("path = %q, want repo key %q", path, want)
	}
	if strings.TrimSpace(content) != testAgeKey {
		t.Errorf("content = %q, want repo key", content)
	}
}

// The clone case: a teammate clones, has no host key, and `up` reads the
// committed repo key with no key handoff.
func TestLoadAgeKey_CloneNoHostKey(t *testing.T) {
	repo := t.TempDir()
	home := t.TempDir() // empty: no host key here
	writeKey(t, filepath.Join(repo, "clusters", "local", "age.key"), testAgeKey+"\n", 0o644)

	content, _, err := loadAgeKey(repo, "acme", home)
	if err != nil {
		t.Fatalf("loadAgeKey (clone): %v", err)
	}
	if strings.TrimSpace(content) != testAgeKey {
		t.Errorf("content = %q, want repo key", content)
	}
}

// Existing repos with no committed key fall back to the host key (0600).
func TestLoadAgeKey_FallsBackToHostKey(t *testing.T) {
	repo := t.TempDir() // no clusters/local/age.key
	home := t.TempDir()
	writeKey(t, filepath.Join(home, ".config", "flywheel", "acme", "age.key"), testAgeKey+"\n", 0o600)

	content, path, err := loadAgeKey(repo, "acme", home)
	if err != nil {
		t.Fatalf("loadAgeKey (fallback): %v", err)
	}
	if !strings.Contains(path, filepath.Join(".config", "flywheel", "acme")) {
		t.Errorf("path = %q, want host key", path)
	}
	if strings.TrimSpace(content) != testAgeKey {
		t.Errorf("content = %q, want host key", content)
	}
}

// The host-key 0600 invariant still bites when no repo key shadows it.
func TestLoadAgeKey_HostKeyWrongModeRejected(t *testing.T) {
	repo := t.TempDir()
	home := t.TempDir()
	writeKey(t, filepath.Join(home, ".config", "flywheel", "acme", "age.key"), testAgeKey+"\n", 0o644)

	if _, _, err := loadAgeKey(repo, "acme", home); err == nil {
		t.Fatal("want error for 0644 host key, got nil")
	}
}

func TestParseDependencyBlock(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		// The two shapes Flux emits in steady-state IUA churn.
		{"dependency 'flux-system/client-infra' is not ready", "client-infra"},
		{"dependency 'flux-system/flywheel-dev-loop' revision is not up to date", "flywheel-dev-loop"},
		// Non-flux-system namespace still parses (and we strip the ns).
		{"dependency 'other/foo' is not ready", "foo"},
		// Unrelated message shouldn't match.
		{"kustomize build failed: …", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := parseDependencyBlock(c.msg); got != c.want {
			t.Errorf("parseDependencyBlock(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
}

func TestKustomizationDetail_BlockedOn(t *testing.T) {
	u := newKust("client-apps", []map[string]any{
		{
			"type":    "Ready",
			"status":  "False",
			"message": "dependency 'flux-system/client-infra' is not ready",
		},
	})
	if got := kustomizationDetail(u); got != "blocked on: client-infra" {
		t.Errorf("kustomizationDetail = %q, want 'blocked on: client-infra'", got)
	}
}

func TestKustomizationDetail_FallbackReconciling(t *testing.T) {
	// No Ready condition at all → "reconciling".
	u := newKust("freshly-created", nil)
	if got := kustomizationDetail(u); got != "reconciling" {
		t.Errorf("kustomizationDetail with no conditions = %q, want 'reconciling'", got)
	}
}

func TestKustomizationDetail_RawMessagePassesThrough(t *testing.T) {
	u := newKust("broken", []map[string]any{
		{
			"type":    "Ready",
			"status":  "False",
			"message": "kustomize build failed: missing kustomization.yaml",
		},
	})
	got := kustomizationDetail(u)
	if got != "kustomize build failed: missing kustomization.yaml" {
		t.Errorf("kustomizationDetail raw passthrough = %q", got)
	}
}

func newKust(name string, conditions []map[string]any) *unstructured.Unstructured {
	condIfaces := make([]any, 0, len(conditions))
	for _, c := range conditions {
		condIfaces = append(condIfaces, c)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"status":   map[string]any{"conditions": condIfaces},
	}}
}

// waitForFluxKustomizations' CRD-retry branch used to `time.Sleep(2s)`
// uncancellably (issue: Ctrl-C during `up`'s multi-minute wait hung until the
// sleep elapsed on its own). waitTick is the extracted, ctx-aware replacement;
// an already-canceled ctx must return promptly with ctx.Err() rather than
// block for the full duration.
func TestWaitTick_CanceledContextReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before waitTick is ever called

	start := time.Now()
	err := waitTick(ctx, 2*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitTick(canceled ctx) error = %v, want context.Canceled", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("waitTick(canceled ctx) took %v, want a prompt return (<500ms), not the full 2s duration", elapsed)
	}
}

// The uncanceled path still waits out the full duration and returns nil —
// guards against a fix that short-circuits unconditionally.
func TestWaitTick_ElapsesNormallyWhenNotCanceled(t *testing.T) {
	start := time.Now()
	err := waitTick(context.Background(), 20*time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("waitTick(live ctx) error = %v, want nil", err)
	}
	if elapsed < 20*time.Millisecond {
		t.Fatalf("waitTick(live ctx) returned after %v, want it to wait out the full duration", elapsed)
	}
}

func TestMkcertRootSecret(t *testing.T) {
	const caPEM = "-----BEGIN CERTIFICATE-----\nMOCK\n-----END CERTIFICATE-----\n"
	s := mkcertRootSecret(caPEM)

	if got := s.GetName(); got != "mkcert-ca" {
		t.Errorf("name = %q, want mkcert-ca", got)
	}
	if got := s.GetNamespace(); got != "kube-system" {
		t.Errorf("namespace = %q, want kube-system", got)
	}
	if got := s.GetKind(); got != "Secret" {
		t.Errorf("kind = %q, want Secret", got)
	}
	if got, _, _ := unstructured.NestedString(s.Object, "type"); got != "Opaque" {
		t.Errorf("type = %q, want Opaque", got)
	}
	if got, _, _ := unstructured.NestedString(s.Object, "stringData", "ca.crt"); got != caPEM {
		t.Errorf("ca.crt = %q, want the root CA PEM", got)
	}
}

// TestKustomizationNames locks kustomizationNames' filter: only Kustomization
// refs contribute a name (issue #117, Tier 2) — the bootstrap keep set also
// carries GitRepository/ConfigMap/Namespace refs that have nothing to
// Ready-wait on.
func TestKustomizationNames(t *testing.T) {
	refs := []applier.ResourceRef{
		{Kind: "Kustomization", Name: "client-infra"},
		{Kind: "GitRepository", Name: "flux-system"},
		{Kind: "Kustomization", Name: "client-apps"},
		{Kind: "ConfigMap", Name: "flywheel-config"},
	}
	got := kustomizationNames(refs)
	want := []string{"client-infra", "client-apps"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("kustomizationNames = %v, want %v", got, want)
	}
}

var kustomizationGVR = schema.GroupVersionResource{
	Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations",
}

// readyKustomization builds a Kustomization unstructured object reporting
// Ready — enough for the fake dynamic client to serve back through
// GetUnstructured.
func readyKustomization(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": naming.FluxNamespace,
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True"},
			},
		},
	}}
}

// TestWaitForFluxKustomizations_MissingNameNeverResolves is Tier 2's core
// property (issue #117): waitForFluxKustomizations must wait on the names it
// was TOLD to expect, not whatever the API server happens to hold. Here
// "client-infra" is Ready in the (fake) cluster but "client-apps" — part of
// the expected set, e.g. because apply-flux-system applied it — never shows
// up. The old found-set implementation would have listed one Kustomization,
// seen it Ready, and reported "1/1 ready" success; seeding the expected set
// up front must instead leave the wait pending on the missing name. Proven
// here by canceling ctx quickly and asserting the wait was still going
// (returned the context's error) instead of having already declared success.
func TestWaitForFluxKustomizations_MissingNameNeverResolves(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{kustomizationGVR: "KustomizationList"},
		readyKustomization("client-infra"))
	a := applier.NewForTest(dyn, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := waitForFluxKustomizations(ctx, a, []string{"client-infra", "client-apps"}, time.Minute, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForFluxKustomizations = %v, want context.DeadlineExceeded (must still be waiting on the missing name, not declaring success)", err)
	}
}

// The inverse: once every expected name is present and Ready, the wait
// returns nil — Tier 2 must not regress the happy path.
func TestWaitForFluxKustomizations_AllPresentAndReadySucceeds(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{kustomizationGVR: "KustomizationList"},
		readyKustomization("client-infra"), readyKustomization("client-apps"))
	a := applier.NewForTest(dyn, nil)

	err := waitForFluxKustomizations(context.Background(), a, []string{"client-infra", "client-apps"}, time.Minute, io.Discard)
	if err != nil {
		t.Fatalf("waitForFluxKustomizations = %v, want nil once every expected name is Ready", err)
	}
}
