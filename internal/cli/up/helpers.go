package up

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/cobr-io/flywheel/internal/cli/applier"
	"github.com/cobr-io/flywheel/internal/cli/style"
	"github.com/cobr-io/flywheel/internal/naming"
)

// waitForFluxKustomizations polls every Flux Kustomization across the
// cluster until each reports Ready or `timeout` elapses. Surfaces
// "blocked on: <dep>" inline when a Kustomization's Not-Ready reason
// is a dependency lag, so the user sees which link in the chain is
// holding the rest up.
func waitForFluxKustomizations(ctx context.Context, a *applier.Applier, timeout time.Duration, out io.Writer) error {
	gvr := schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	w := style.NewWaiter(out, "waiting for Flux Kustomizations Ready")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		items, err := a.ListUnstructured(ctx, gvr, "")
		if err != nil {
			// CRD may not exist yet; retry (Tick the spinner so the
			// user sees we're alive).
			w.Tick()
			if err := waitTick(ctx, 2*time.Second); err != nil {
				w.Done("")
				return err
			}
			continue
		}
		// On the first list, seed all items so the layout is stable
		// across redraws even if a Kustomization transiently disappears.
		for _, it := range items {
			w.Add(it.GetName())
		}
		for _, it := range items {
			name := it.GetName()
			if kustomizationReady(&it) {
				w.Set(name, style.Ready, "ready")
			} else {
				w.Set(name, style.Pending, kustomizationDetail(&it))
			}
		}
		w.Tick()
		if len(items) > 0 && w.AllResolved() {
			w.Done(fmt.Sprintf("%d Flux Kustomization(s) Ready", len(items)))
			return nil
		}
		if err := waitTick(ctx, 2*time.Second); err != nil {
			w.Done("")
			return err
		}
	}
	w.Done("")
	return fmt.Errorf("Flux Kustomizations not all Ready before deadline")
}

// waitTick pauses for d, or returns ctx.Err() immediately if ctx is canceled
// first. Shared by both retry branches of waitForFluxKustomizations (the
// CRD-not-registered-yet path and the steady poll) so Ctrl-C during a
// multi-minute `up` wait unwinds the loop instead of blocking up to 2s per
// iteration until the deadline finally trips.
func waitTick(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// kustomizationDetail returns a short description of the current
// Not-Ready reason, with special handling for dependsOn lag (the
// most common reason during steady-state IUA churn).
//
// Flux Kustomization message formats we parse:
//   - "dependency 'flux-system/<name>' is not ready"
//   - "dependency 'flux-system/<name>' revision is not up to date"
//
// Anything else falls through to the raw message (truncated by the
// Waiter to fit the detail column).
func kustomizationDetail(u *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] != "Ready" {
			continue
		}
		msg, _ := m["message"].(string)
		if msg == "" {
			return "reconciling"
		}
		if dep := parseDependencyBlock(msg); dep != "" {
			return "blocked on: " + dep
		}
		return msg
	}
	return "reconciling"
}

// parseDependencyBlock pulls the dependency Name out of Flux's
// "dependency 'flux-system/<name>' is not ready" / "...revision is
// not up to date" message. Returns empty if the message isn't one
// of those shapes.
func parseDependencyBlock(msg string) string {
	const marker = "dependency '"
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(marker):]
	end := strings.Index(rest, "'")
	if end < 0 {
		return ""
	}
	dep := rest[:end]
	// Strip "<namespace>/" prefix — the namespace is always flux-system
	// in our topology, and the name is enough to identify the blocker.
	if slash := strings.Index(dep, "/"); slash >= 0 {
		dep = dep[slash+1:]
	}
	return dep
}

func kustomizationReady(u *unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] == "Ready" && m["status"] == "True" {
			return true
		}
	}
	return false
}

// ensureMkcert runs `mkcert -install` (idempotent: it just verifies the
// trust-store root is present) and generates cert/{cert,key}.pem for
// the configured domain if not already present.
func ensureMkcert(ctx context.Context, repoDir, domain string, out io.Writer) error {
	if domain == "" {
		domain = "localdev.me"
	}
	certDir := filepath.Join(repoDir, "cert")
	certPath := filepath.Join(certDir, "cert.pem")
	keyPath := filepath.Join(certDir, "key.pem")
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			style.OK(out, "mkcert: cert/{cert,key}.pem already present")
			return nil
		}
	}
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		return err
	}
	// `mkcert -install` is fast and idempotent — no harm running it
	// each `up`.
	cmd := exec.CommandContext(ctx, "mkcert", "-install")
	cmd.Stdout = style.VerboseWriter(out)
	cmd.Stderr = style.VerboseWriter(out)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mkcert -install: %w", err)
	}
	cmd = exec.CommandContext(ctx, "mkcert",
		"-cert-file", certPath,
		"-key-file", keyPath,
		domain, "*."+domain)
	cmd.Stdout = style.VerboseWriter(out)
	cmd.Stderr = style.VerboseWriter(out)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mkcert generate: %w", err)
	}
	style.OK(out, "mkcert: generated cert/{cert,key}.pem for %s, *.%s", domain, domain)
	return nil
}

// managedByFlywheel returns the label set every resource `flywheel up` applies
// imperatively carries (app.kubernetes.io/managed-by=flywheel). On Secrets it's
// provenance only — the orphan prune denylists Secret — but it keeps the marker
// uniform across everything `up` creates (issue #27), so future tooling can
// identify flywheel's own objects by one label. Returned as a fresh map per
// call since unstructured stores it by reference.
func managedByFlywheel() map[string]interface{} {
	return map[string]interface{}{naming.ManagedByLabelKey: naming.ManagedByLabelValue}
}

// createAgeSecret creates the `sops-age` Secret in `flux-system`. Flux's
// SOPS decryption looks for a key named `age.agekey`. Run by `up`'s
// create-secrets step.
func createAgeSecret(ctx context.Context, a *applier.Applier, ageContent string, out io.Writer) error {
	secret := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name":      "sops-age",
				"namespace": naming.FluxNamespace,
				"labels":    managedByFlywheel(),
			},
			"type": "Opaque",
			"stringData": map[string]interface{}{
				"age.agekey": ageContent,
			},
		},
	}
	return a.ApplyObject(ctx, secret, out)
}

// createMkcertSecret loads cert/{cert,key}.pem and creates the
// `local-cert` TLS Secret in `kube-system` (where k3d's bundled Traefik
// looks for it via the TLSStore from manifests/infra).
func createMkcertSecret(ctx context.Context, a *applier.Applier, repoDir string, out io.Writer) error {
	cert, err := os.ReadFile(filepath.Join(repoDir, "cert", "cert.pem"))
	if err != nil {
		return err
	}
	key, err := os.ReadFile(filepath.Join(repoDir, "cert", "key.pem"))
	if err != nil {
		return err
	}
	secret := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       string(corev1.ResourceSecrets),
			"metadata": map[string]interface{}{
				"name":      "local-cert",
				"namespace": "kube-system",
				"labels":    managedByFlywheel(),
			},
			"type": "kubernetes.io/tls",
			"stringData": map[string]interface{}{
				"tls.crt": string(cert),
				"tls.key": string(key),
			},
		},
	}
	secret.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
	return a.ApplyObject(ctx, secret, out)
}

// createMkcertRootSecret resolves the mkcert root CA
// (`$(mkcert -CAROOT)/rootCA.pem`) and creates the `mkcert-ca` Secret in
// `kube-system`. This is the symmetric other half of createMkcertSecret:
// the leaf (`local-cert`) lets services *serve* `*.<domain>` TLS, this root
// lets in-cluster clients *verify* it. Pods don't inherit host trust, so a
// client calling a sibling by its public `*.<domain>` hostname over TLS
// would otherwise fail with "unable to get local issuer certificate".
// Idempotent apply, mirroring createMkcertSecret.
func createMkcertRootSecret(ctx context.Context, a *applier.Applier, out io.Writer) error {
	caRoot, err := mkcertCARoot(ctx)
	if err != nil {
		return err
	}
	caPEM, err := os.ReadFile(filepath.Join(caRoot, "rootCA.pem"))
	if err != nil {
		return err
	}
	return a.ApplyObject(ctx, mkcertRootSecret(string(caPEM)), out)
}

// mkcertCARoot returns the directory mkcert stores its root CA in, resolved
// via `mkcert -CAROOT`. Don't hardcode `~/Library/...` — the location is
// OS-specific and mkcert is the source of truth.
func mkcertCARoot(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "mkcert", "-CAROOT").Output()
	if err != nil {
		return "", fmt.Errorf("mkcert -CAROOT: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// mkcertRootSecret builds the `mkcert-ca` Opaque Secret carrying the root CA
// under `ca.crt`. Split out from createMkcertRootSecret so the object shape
// is unit-testable without a cluster or mkcert on PATH.
func mkcertRootSecret(caPEM string) *unstructured.Unstructured {
	secret := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       string(corev1.ResourceSecrets),
			"metadata": map[string]interface{}{
				"name":      "mkcert-ca",
				"namespace": "kube-system",
				"labels":    managedByFlywheel(),
			},
			"type": "Opaque",
			"stringData": map[string]interface{}{
				"ca.crt": caPEM,
			},
		},
	}
	secret.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
	return secret
}
