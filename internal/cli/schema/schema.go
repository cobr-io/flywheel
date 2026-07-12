// Package schema validates flywheel.yaml against the v1alpha1 shape (per
// design § flywheel.yaml schema). During the v0.x phase the shape may
// break between minor releases; the migration framework (Phase 3.4) will
// walk older schemas forward when introduced. v0.1 only knows v1alpha1.
package schema

import (
	"fmt"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/cobr-io/flywheel/internal/naming"
)

// NativeLabel is the schema label the CLI natively understands.
const NativeLabel = "v1alpha1"

// File is the in-memory shape of flywheel.yaml. JSON tags are used because
// sigs.k8s.io/yaml converts YAML → JSON → struct. Optional fields use
// pointer or omitempty so unset values stay zero.
type File struct {
	Schema     string     `json:"schema"`
	Flywheel   Flywheel   `json:"flywheel"`
	Client     Client     `json:"client"`
	Cluster    Cluster    `json:"cluster"`
	Namespaces Namespaces `json:"namespaces"`
	Flux       Flux       `json:"flux"`
	Local      Local      `json:"local"`
	Sops       Sops       `json:"sops"`

	// Optional.
	Git Git `json:"git,omitempty"`

	// Optional. Tunables for the in-cluster git-server (flywheel-system).
	GitServer GitServer `json:"git_server,omitempty"`

	// Optional. Declares the sibling app repos this cluster's dev loop needs,
	// one entry per worktree under paths.workspaces_root. The single source of
	// truth for `up`-clone and the local-only guard (see the 2026-06-17
	// addendum to docs/designs/2026-06-16-add-app-source-modes-and-local-only-guard-design.md).
	Workspace Workspace `json:"workspace,omitempty"`

	// Optional .local-only fields.
	Paths Paths `json:"paths,omitempty"`
}

type Flywheel struct {
	Version string `json:"version"`
	// Images is an optional per-image override map. Keys MUST be a subset
	// of {git-server, git-auto-sync, image-builder-controller,
	// git-deploy-controller}. An unset or omitted key falls back to
	// `ghcr.io/cobr-io/<name>:<version>` at `flywheel up` time. The natural
	// home for per-developer dogfood overrides is flywheel.yaml.local
	// (gitignored + deep-merged).
	Images map[string]string `json:"images,omitempty"`
}

// ImageNames are the known image keys for flywheel.images. Any other key in
// the map is a schema violation. Adding image N+1 touches several
// non-derivable sites (Dockerfiles, goreleaser, dependabot, …) plus the
// bootstrap wiring that IS derived from this slice — the ordered checklist is
// docs/dev/add-controller-image.md.
var ImageNames = []string{"git-server", "git-auto-sync", "image-builder-controller", "git-deploy-controller"}

type Client struct {
	Name string `json:"name"`
	Org  string `json:"org,omitempty"`
}

type Cluster struct {
	Name         string `json:"name"`
	Registry     string `json:"registry"`
	RegistryPort int    `json:"registry_port"`
	HttpPort     int    `json:"http_port"`
	HttpsPort    int    `json:"https_port"`
	Servers      int    `json:"servers,omitempty"`
	Agents       int    `json:"agents,omitempty"`
	K3sImage     string `json:"k3s_image,omitempty"`
}

type Namespaces struct {
	// Flywheel is DEPRECATED and no longer read: flywheel's namespace is fixed
	// at naming.FlywheelNamespace (task T14). The field is retained so strict
	// parsing still accepts older client files that carry it; Validate rejects a
	// value that differs from the fixed namespace.
	Flywheel string `json:"flywheel"`
	Apps     string `json:"apps"`
}

type Flux struct {
	IntervalLocal string `json:"interval_local"`
	IacInterval   string `json:"iac_interval,omitempty"`
}

type Local struct {
	Domain string `json:"domain,omitempty"`
}

type Sops struct {
	AgeRecipientsLocal []string `json:"age_recipients_local,omitempty"`
}

type Git struct {
	// IntegrationBranch is the protected branch the local-only guard refuses
	// to let local-only apps reach. Optional; defaults to "main" via
	// File.IntegrationBranch().
	IntegrationBranch string `json:"integration_branch,omitempty"`
}

// GitServer holds tunables for the in-cluster git-server Deployment that serves
// app worktrees to the buildkit build jobs (flywheel-system).
type GitServer struct {
	// MemoryLimit is the container memory limit. The default (128Mi) is fine
	// for small repos, but git's pack/compression on `git-upload-pack` of a
	// large monorepo can spike past it and get the pod OOMKilled mid-build
	// (issue #4) — raise it (e.g. 512Mi) when building from sizeable repos.
	// Optional; defaults to DefaultGitServerMemoryLimit via
	// File.GitServerMemoryLimit(). A Kubernetes memory quantity (128Mi, 1Gi…).
	MemoryLimit string `json:"memory_limit,omitempty"`
}

// DefaultGitServerMemoryLimit is the git-server container memory limit when
// git_server.memory_limit is unset — the historical baked-in value, kept so
// existing repos behave identically.
const DefaultGitServerMemoryLimit = "128Mi"

// GitServerMemoryLimit returns the configured git-server memory limit, or the
// default ("128Mi") when unset.
func (f *File) GitServerMemoryLimit() string {
	if l := strings.TrimSpace(f.GitServer.MemoryLimit); l != "" {
		return l
	}
	return DefaultGitServerMemoryLimit
}

// Workspace declares the sibling app repos this cluster depends on, keyed by
// the worktree directory each lives in under paths.workspaces_root.
type Workspace struct {
	Repos []WorkspaceRepo `json:"repos,omitempty"`
}

// WorkspaceRepo is one sibling repo / worktree. Name is the worktree directory
// basename (== the git-auto-sync worktree under workspaces_root, and the
// basename of every sharing app's GitRepository spec.url minus ".git").
// Exactly one of URL (remote-backed) or LocalOnly (no remote yet) is set.
//
// Branch, when set, is the branch to check out on a fresh clone (by `up`'s
// worktree reconciliation or `add-app` clone-mode). It is a clone-time
// directive only: a worktree that already exists on disk is left on whatever
// branch it is on. Empty means the remote's default branch.
type WorkspaceRepo struct {
	Name      string `json:"name"`
	URL       string `json:"url,omitempty"`
	LocalOnly bool   `json:"local_only,omitempty"`
	Branch    string `json:"branch,omitempty"`
}

type Paths struct {
	WorkspacesRoot string `json:"workspaces_root,omitempty"`
}

// DefaultIntegrationBranch is the protected branch when git.integration_branch
// is unset.
const DefaultIntegrationBranch = "main"

// IntegrationBranch returns the configured integration branch, or the default
// ("main") when unset. The shell guard (scripts/ci/check-local-only.sh) reads
// the same field via yq with the same default.
func (f *File) IntegrationBranch() string {
	if b := strings.TrimSpace(f.Git.IntegrationBranch); b != "" {
		return b
	}
	return DefaultIntegrationBranch
}

// WorkspaceRepo returns the workspace entry declaring the given worktree, and
// ok=false when no entry does.
func (f *File) WorkspaceRepo(worktree string) (WorkspaceRepo, bool) {
	for _, r := range f.Workspace.Repos {
		if r.Name == worktree {
			return r, true
		}
	}
	return WorkspaceRepo{}, false
}

// LocalOnlyWorktrees returns the names of every workspace entry flagged
// local_only — the worktrees whose source exists only on one machine. The
// local-only guard keys on this set.
func (f *File) LocalOnlyWorktrees() []string {
	var names []string
	for _, r := range f.Workspace.Repos {
		if r.LocalOnly {
			names = append(names, r.Name)
		}
	}
	return names
}

// worktreeNameRe validates a workspace.repos entry name: a path-free directory
// basename (no slashes or spaces). The flat /workspaces mount supports only a
// single directory level, so a name with a separator could never resolve.
var worktreeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// branchNameRe is a deliberately loose plausibility check: a single token of
// branch-name characters. It exists to catch a typo that would silently
// disable the local-only guard (e.g. an embedded space), not to reproduce
// git-check-ref-format exactly.
var branchNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// memoryQuantityRe is a loose plausibility check for a Kubernetes memory
// quantity (e.g. 128Mi, 512Mi, 1Gi, 256M). The unit suffix is required: a bare
// number is a valid quantity (bytes) but for a memory limit it's always a typo
// for the Mi/Gi form, so we reject it. This catches the likely mistakes (no
// unit, a stray space) before the value reaches kustomize/the API server; it
// does not reproduce resource.Quantity parsing exactly.
var memoryQuantityRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(Ei|Pi|Ti|Gi|Mi|Ki|E|P|T|G|M|k)$`)

// Parse unmarshals YAML into a File without semantic validation. Callers
// run Validate (or ValidateLocal) afterwards depending on whether the
// document is the committed file or `.local` overlay.
//
// Unmarshaling is strict (unknown fields rejected): a typo'd key (e.g.
// `gitserver:` instead of `git_server:`) would otherwise silently parse as
// all-defaults instead of failing loud — the exact failure class
// Validate's git.integration_branch comment below warns about ("a typo here
// would silently disable the local-only guard").
func Parse(raw []byte) (*File, error) {
	var f File
	if err := yaml.UnmarshalStrict(raw, &f); err != nil {
		return nil, fmt.Errorf("parse flywheel.yaml: %w", err)
	}
	return &f, nil
}

// ValidateError aggregates field-level violations so the CLI can report
// every problem at once rather than one per run.
type ValidateError struct {
	Field   string
	Message string
}

func (e ValidateError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Errors is a flat list of ValidateError. Print one per line so the user
// sees the whole picture.
type Errors []ValidateError

func (es Errors) Error() string {
	var b strings.Builder
	for i, e := range es {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(e.Error())
	}
	return b.String()
}

// HasErrors returns nil when the slice is empty (convenience so callers
// can `return es.HasErrors()` after collecting).
func (es Errors) HasErrors() error {
	if len(es) == 0 {
		return nil
	}
	return es
}

// Validate checks a *committed* flywheel.yaml against v1alpha1. The
// `Paths` field is rejected here — it belongs only in `flywheel.yaml.local`
// per design § flywheel.yaml.local ("The committed file MUST NOT contain
// absolute host paths or per-developer values").
func Validate(f *File) error {
	var es Errors

	if f.Schema == "" {
		es = append(es, ValidateError{"schema", "required"})
	} else if f.Schema != NativeLabel {
		es = append(es, ValidateError{"schema", fmt.Sprintf("CLI knows %q; got %q", NativeLabel, f.Schema)})
	}

	if f.Flywheel.Version == "" {
		es = append(es, ValidateError{"flywheel.version", "required"})
	}
	// flywheel.images is optional; if present, keys must be a subset of
	// the known image names. Unknown keys are a typo/misconfig — fail loud.
	known := map[string]bool{}
	for _, n := range ImageNames {
		known[n] = true
	}
	for k, v := range f.Flywheel.Images {
		if !known[k] {
			es = append(es, ValidateError{
				"flywheel.images." + k,
				fmt.Sprintf("unknown image key; valid keys: %v", ImageNames),
			})
		}
		if v == "" {
			es = append(es, ValidateError{"flywheel.images." + k, "value cannot be empty"})
		}
	}
	if f.Client.Name == "" {
		es = append(es, ValidateError{"client.name", "required"})
	}
	if f.Cluster.Name == "" {
		es = append(es, ValidateError{"cluster.name", "required"})
	}
	if f.Cluster.Registry == "" {
		es = append(es, ValidateError{"cluster.registry", "required"})
	}
	if f.Cluster.RegistryPort == 0 {
		es = append(es, ValidateError{"cluster.registry_port", "required and non-zero"})
	}
	if f.Cluster.HttpPort == 0 {
		es = append(es, ValidateError{"cluster.http_port", "required and non-zero"})
	}
	if f.Cluster.HttpsPort == 0 {
		es = append(es, ValidateError{"cluster.https_port", "required and non-zero"})
	}
	// namespaces.flywheel is NOT client-configurable: flywheel's own machinery
	// always lives in naming.FlywheelNamespace. The key is optional — older
	// client files that still carry `flywheel: flywheel-system` keep validating
	// (strict parsing rejects unknown keys, not deprecated ones) — but a value
	// that DIFFERS from the fixed namespace is a silent no-op that would mislead,
	// so it's a hard error with a fix-it message. (Task T14.)
	if ns := f.Namespaces.Flywheel; ns != "" && ns != naming.FlywheelNamespace {
		es = append(es, ValidateError{
			"namespaces.flywheel",
			fmt.Sprintf("flywheel's namespace is fixed at %s; remove `namespaces.flywheel` from flywheel.yaml (got %q)", naming.FlywheelNamespace, ns),
		})
	}
	if f.Namespaces.Apps == "" {
		es = append(es, ValidateError{"namespaces.apps", "required"})
	}
	if f.Flux.IntervalLocal == "" {
		es = append(es, ValidateError{"flux.interval_local", "required"})
	}
	if f.Paths.WorkspacesRoot != "" {
		es = append(es, ValidateError{"paths.workspaces_root", "belongs in flywheel.yaml.local, not the committed file"})
	}
	// git.integration_branch is optional, but a present value must be a
	// plausible branch name — a typo here would silently disable the
	// local-only guard (it would compare against a branch nobody uses).
	if b := f.Git.IntegrationBranch; b != "" && !branchNameRe.MatchString(b) {
		es = append(es, ValidateError{"git.integration_branch", fmt.Sprintf("%q is not a valid branch name", b)})
	}
	// git_server.memory_limit is optional; a present value must look like a
	// Kubernetes memory quantity (a typo would fail the Deployment apply later).
	if l := strings.TrimSpace(f.GitServer.MemoryLimit); l != "" && !memoryQuantityRe.MatchString(l) {
		es = append(es, ValidateError{"git_server.memory_limit", fmt.Sprintf("%q is not a valid memory quantity (e.g. 128Mi, 512Mi, 1Gi)", l)})
	}

	// workspace.repos: each entry needs a dir-safe name, exactly one of
	// url/local_only, and names must be unique (the block is keyed by worktree).
	seen := map[string]bool{}
	for i, r := range f.Workspace.Repos {
		field := fmt.Sprintf("workspace.repos[%d]", i)
		switch {
		case r.Name == "":
			es = append(es, ValidateError{field + ".name", "required"})
		case !worktreeNameRe.MatchString(r.Name):
			es = append(es, ValidateError{field + ".name", fmt.Sprintf("%q is not a valid worktree directory name", r.Name)})
		case seen[r.Name]:
			es = append(es, ValidateError{field + ".name", fmt.Sprintf("duplicate worktree %q (the block is keyed by worktree)", r.Name)})
		}
		seen[r.Name] = true
		// Exactly one of url / local_only. hasURL == LocalOnly catches both
		// "both set" and "neither set".
		if hasURL := strings.TrimSpace(r.URL) != ""; hasURL == r.LocalOnly {
			es = append(es, ValidateError{field, "set exactly one of url or local_only"})
		}
		// branch is optional; a present value must be a plausible branch name
		// (a typo would silently fall back to the remote default on clone).
		if r.Branch != "" && !branchNameRe.MatchString(r.Branch) {
			es = append(es, ValidateError{field + ".branch", fmt.Sprintf("%q is not a valid branch name", r.Branch)})
		}
	}

	return es.HasErrors()
}

// ValidateLocal validates a `flywheel.yaml.local` overlay. Most fields
// are optional here; the only one we positively check is paths.workspaces_root
// (must be an absolute path when present).
func ValidateLocal(f *File) error {
	var es Errors
	if f.Paths.WorkspacesRoot != "" && !strings.HasPrefix(f.Paths.WorkspacesRoot, "/") {
		es = append(es, ValidateError{"paths.workspaces_root", "must be an absolute path"})
	}
	return es.HasErrors()
}
