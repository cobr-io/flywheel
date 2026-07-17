// Package naming centralizes flywheel's identity strings — the label keys,
// annotations, branch names, namespaces, config-file names, and in-cluster URLs
// that must agree across the CLI (internal/cli/*), the controllers
// (internal/controller), the self-sync loop (internal/selfsync), the commands
// (cmd/*), and the embedded templates/manifests.
//
// Consolidating them here means a rename is a one-line change, and — for the
// strings that ALSO live in a template or static manifest — drift is caught by
// `go test` (the agreement tests in internal/cli/converge render/read the
// template and assert the embedded literal equals the constant here) rather
// than on a live cluster.
//
// This package is deliberately dependency-free (stdlib only) so every layer can
// import it without introducing an import cycle. scripts/git-auto-sync/sync.sh
// keeps its own bash copies of a few of these strings; agreement between the
// bash and Go copies is enforced Go-side only (see the pointer comment there).
package naming

// Managed-by marker. Every resource `flywheel up` applies imperatively (the
// dev-loop machinery, the flywheel-config ConfigMap, the bootstrap flux-system
// tree, the secrets) carries this label; resources Flux reconciles from the
// gitops repo (app/infra workloads, per-app git-auto-sync sidecars) are
// deliberately NOT labeled. `up`'s orphan prune scopes its scan to this label,
// so the safety of that prune depends on every writer spelling it identically —
// which is exactly why it lives in one place here.
const (
	ManagedByLabelKey   = "app.kubernetes.io/managed-by"
	ManagedByLabelValue = "flywheel"

	// ManagedBySelector is the managed-by label as a `key=value` label
	// selector string (built from the constants above so it can never drift
	// from them).
	ManagedBySelector = ManagedByLabelKey + "=" + ManagedByLabelValue
)

// DeployBranchAnnotation is the durable record of the AUTHORED branch the
// operator selected with `flywheel use`, stored on the self GitRepository.
// git-deploy-controller reads it each tick to decide which branch to feed into
// DEPLOY. Writer: internal/cli/usecmd. Reader: internal/selfsync.
const DeployBranchAnnotation = "flywheel.cobr.io/deploy-branch"

// DeployBranch is the constant DEPLOY branch Flux always tracks. It is never
// repointed per developer-branch; git-deploy-controller maintains it as
// AUTHORED + the IUA's image-bump commits. Also embedded in
// self-source.yaml.tmpl and manifests/dev-loop/base/image-update-automation.yaml
// (agreement-tested).
const DeployBranch = "flywheel/local-deploy"

// ReconcileRequestAnnotation is Flux's "reconcile now" trigger. The value is
// opaque to Flux — it only has to differ from the last handled value. Also used
// by scripts/git-auto-sync/sync.sh (bash copy).
const ReconcileRequestAnnotation = "reconcile.fluxcd.io/requestedAt"

// KustomizeReconcileDisabledAnnotation, set to KustomizeReconcileDisabledValue,
// stops kustomize-controller from reconciling (and so re-applying a stale
// spec.ref.branch from the source manifest over) a GitRepository that is
// patched imperatively instead — the per-app branch-follow race (design doc
// 2026-07-17-per-app-sync-controller-design.md, Open Issue #11). Present in
// the source manifest it also blocks Flux's own creation/prune of the
// resource, so it must only ever be added after first apply. Also used by
// scripts/git-auto-sync/sync.sh (bash copy).
const (
	KustomizeReconcileDisabledAnnotation = "kustomize.toolkit.fluxcd.io/reconcile"
	KustomizeReconcileDisabledValue      = "disabled"
)

// FluxNamespace is the Flux install namespace. It is fixed by convention,
// independent of the client-configurable flywheel/apps namespaces.
const FluxNamespace = "flux-system"

// FlywheelNamespace is THE namespace for flywheel's in-cluster machinery
// (git-server, buildkitd, the build jobs, the controllers). It is fixed, not
// client-configurable: this constant is the single global definition every Go
// call site, rendered template (via the render value builders), and static
// manifest (via the agreement tests in internal/cli/converge) derives from or
// is checked against. The `namespaces.flywheel` flywheel.yaml key is
// deprecated — schema.Validate accepts it only when it equals this value (task
// T14).
const FlywheelNamespace = "flywheel-system"

// Config + state file names, relative to a client repo root.
const (
	// ConfigFile is the committed cluster config.
	ConfigFile = "flywheel.yaml"
	// ConfigFileLocal is the git-ignored per-developer override that merges
	// over ConfigFile.
	ConfigFileLocal = "flywheel.yaml.local"
	// StateFile records which flywheel release a repo was scaffolded against.
	StateFile = ".flywheel-state.yaml"
)

// ImageOrg is the public container registry org every default Flywheel image
// reference resolves under: `ghcr.io/cobr-io/<name>:<version>`. Read by
// imagepin.DefaultRef; `<name>` is one of schema.ImageNames.
const ImageOrg = "ghcr.io/cobr-io"

// InClusterRegistryPort is the port the k3d registry *container* listens on
// inside the cluster network — always 5000, regardless of the host-side
// published port (`cluster.registry_port`). In-cluster pulls, the build
// container's push, and Flux's ImageRepository scan all hit this port; the
// host port is only for the `docker push` mirror step run from the
// developer's machine. Canonical definition for what used to be duplicated as
// internal/controller.Config's InClusterRegistryPort and imagepin's own copy
// (task T28).
const InClusterRegistryPort = "5000"

// BuildKitClientImage is the upstream image for the thin buildkit CLIENT that
// each build Job runs (the build itself executes in the shared warm buildkitd
// daemon). `up` mirrors this image into the cluster's local registry and hands
// the in-cluster ref to the controller via the flywheel-config key
// `images.buildkit_client`, so the first build Job scheduled on each node
// pulls it from the LAN registry (~2s) instead of Docker Hub (~15-30s) — the
// per-node cold pull behind the bimodal early-bump latency (issue #107). The
// controller falls back to this Hub ref when the key is absent or the mirror
// was skipped (offline host). Bump this in lockstep with the buildkitd
// daemon's image in manifests/dev-loop/base/buildkitd.yaml.
const BuildKitClientImage = "moby/buildkit:v0.16.0-rootless"

// ManagedByLabels returns the managed-by label set as a fresh map (a new map
// per call, since callers that hand it to unstructured objects store it by
// reference).
func ManagedByLabels() map[string]string {
	return map[string]string{ManagedByLabelKey: ManagedByLabelValue}
}

// GitServerURL returns the in-cluster base URL of the git-server Service in the
// given namespace (no trailing slash). Bare-repo URLs are formed by appending
// "/<name>.git". Also embedded in self-source.yaml.tmpl and
// flywheel-source.yaml.tmpl (agreement-tested).
func GitServerURL(namespace string) string {
	return "http://git-server." + namespace + ".svc.cluster.local:8080"
}
