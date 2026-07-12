// Package controller reconciles Flux GitRepository resources by dispatching
// build Jobs whenever the observed revision changes.
//
// Discovery is opt-in: a GitRepository participates if-and-only-if a sibling
// ConfigMap named `<gitrepo-name>-build-config` exists in the same namespace
// with a `builds.yaml` key. The ConfigMap is the source of truth for what to
// build (image, context, dockerfile, optional secrets); we never read TOML
// from the source repo — every build parameter is declarative-in-flux.
//
// One build Job is created per `builds[]` entry per observed commit. Each Job
// is a thin buildctl client that drives the warm buildkitd daemon (see
// manifests/dev-loop/base/buildkitd.yaml and
// docs/designs/2026-06-01-buildkit-builder.md). The Job name is deterministic
// (`build-<repo>-<image>-<ts>-<short-sha>`), so the reconcile loop is
// idempotent: if the Job already exists, we skip.
package controller

import (
	"fmt"
	"strings"

	"github.com/cobr-io/flywheel/internal/naming"
)

// Config carries every client-specific value the controller needs. Every
// field is sourced from the `flywheel-config` ConfigMap (per design § The
// `flywheel-config` ConfigMap). Nothing here is hardcoded.
type Config struct {
	// Namespace is the controller's own namespace. Build Jobs are created
	// here. Sourced from flywheel-config key `namespaces.flywheel`.
	Namespace string

	// BuilderNamespace is the namespace the controller watches for
	// GitRepository resources (and their sibling `*-build-config`
	// ConfigMaps). These are flywheel builder infra, so they live in
	// `namespaces.flywheel` (alongside the controller itself) — the apps
	// namespace holds only the user's application workloads.
	BuilderNamespace string

	// Registry is the k3d registry name (without scheme/port). Sourced
	// from `cluster.registry`.
	Registry string

	// RegistryPort is the host-side port the registry is published on.
	// Sourced from `cluster.registry_port`.
	RegistryPort string

	// ClusterName is the k3d cluster name. Sourced from `cluster.name`.
	// Used in label construction; not in the registry URL (the registry
	// name is independent of the cluster name in v0.1).
	ClusterName string

	// ClientName is the per-client short name. Used as a label prefix on
	// every built artifact. Sourced from `client.name`.
	ClientName string

	// BuildKitAddr is the gRPC address of the warm buildkitd daemon the
	// per-build buildctl client connects to. Optional: empty falls back to
	// defaultBuildKitAddr (the in-cluster Service), which is correct for the
	// standard topology — so the controller Deployment leaves it unset.
	// Overridable for non-default topologies via the `--buildkit-addr` flag
	// or `BUILDKIT_ADDR` env (not currently surfaced through flywheel.yaml /
	// flywheel-config).
	BuildKitAddr string
}

// defaultBuildKitAddr is the in-cluster buildkitd Service address (see
// manifests/dev-loop/base/buildkitd.yaml) used when flywheel-config doesn't
// override it.
// The namespace is fixed (naming.FlywheelNamespace) — buildkitd is flywheel
// infra, so its Service always lives there (task T14).
const defaultBuildKitAddr = "tcp://buildkitd." + naming.FlywheelNamespace + ":1234"

// BuildKitAddrOrDefault returns the configured buildkitd address, or the
// default in-cluster Service address when unset.
func (c Config) BuildKitAddrOrDefault() string {
	if c.BuildKitAddr == "" {
		return defaultBuildKitAddr
	}
	return c.BuildKitAddr
}

// Validate returns nil if every required field is populated. A missing
// field is a configuration bug, not a runtime condition — the controller
// refuses to start.
func (c Config) Validate() error {
	var missing []string
	if c.Namespace == "" {
		missing = append(missing, "FLYWHEEL_NAMESPACE")
	}
	if c.BuilderNamespace == "" {
		missing = append(missing, "BUILDER_NAMESPACE")
	}
	if c.Registry == "" {
		missing = append(missing, "CLUSTER_REGISTRY")
	}
	if c.RegistryPort == "" {
		missing = append(missing, "CLUSTER_REGISTRY_PORT")
	}
	if c.ClusterName == "" {
		missing = append(missing, "CLUSTER_NAME")
	}
	if c.ClientName == "" {
		missing = append(missing, "CLIENT_NAME")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	return nil
}

// RegistryURL is the in-cluster destination host:port the build container pushes to,
// e.g. `k3d-acme-local-registry:5000`. The `k3d-` prefix is the DNS name
// k3d advertises its registries under inside the cluster network. The port is
// naming.InClusterRegistryPort — always 5000 regardless of the host-side port
// mapping (cluster.registry_port, e.g. 50001); see that constant's doc for why
// in-cluster clients standardise on it (task T28: this used to be its own
// InClusterRegistryPort constant, duplicated again in internal/cli/imagepin).
func (c Config) RegistryURL() string {
	return fmt.Sprintf("k3d-%s:%s", c.Registry, naming.InClusterRegistryPort)
}
