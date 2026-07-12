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
