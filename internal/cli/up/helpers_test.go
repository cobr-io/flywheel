package up

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
