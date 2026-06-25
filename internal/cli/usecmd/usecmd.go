// Package usecmd implements `flywheel use <branch>`: explicitly choose which
// branch of the client's gitops repo Flux deploys, by repointing the self
// GitRepository (flux-system/flux-system) at it.
//
// This is the opt-in counterpart to git-auto-sync's branch following. The
// gitops/self sync no longer auto-follows worktree checkouts (issue #17) —
// auto-following is dangerous on the repo that carries infra, because a
// transient checkout (e.g. the one `git rebase` does) would repoint Flux at an
// infra-less branch tip and, with prune:true, tear that infra down. So the
// deployed branch is chosen deliberately here instead.
//
// The patch mirrors what the sync script's patch_gitrepository() does: disable
// kustomize-controller reconcile on the GitRepository (so the static
// `branch:` in the source manifest can't revert our change) and set
// spec.ref.branch, plus a reconcile trigger so Flux converges immediately.
package usecmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/cobr-io/flywheel/internal/cli/applier"
	"github.com/cobr-io/flywheel/internal/cli/config"
	"github.com/cobr-io/flywheel/internal/cli/k3d"
	flywheelSchema "github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/style"
)

// Default identity of the self/gitops GitRepository (matches the
// self-source.yaml.tmpl manifest and the sync deployment's env).
const (
	DefaultGitRepoName      = "flux-system"
	DefaultGitRepoNamespace = "flux-system"
)

// DeployBranchAnnotation is the durable record of the branch the operator
// selected with `flywheel use`. git-auto-sync-self reconciles
// spec.ref.branch to it, so an external clobber (notably a `flywheel up`
// re-apply of the self-source manifest) is corrected instead of silently
// changing the deployed branch (issue #17).
const DeployBranchAnnotation = "flywheel.cobr.io/deploy-branch"

// deployFieldManager is a DISTINCT SSA field manager (not applier.FieldManager
// = "flux-controller"). It must differ so the deploy-branch annotation written
// here survives `flywheel up`, whose flux-controller apply omits the annotation
// — SSA only strips fields owned by the same manager that omits them.
const deployFieldManager = "flywheel-deploy"

// gitRepoGVK is the Flux source GitRepository kind.
var gitRepoGVK = schema.GroupVersionKind{
	Group:   "source.toolkit.fluxcd.io",
	Version: "v1",
	Kind:    "GitRepository",
}

// Options are the inputs to Run.
type Options struct {
	RepoDir          string // client repo root; defaults to cwd
	Branch           string // branch to deploy; required
	GitRepoName      string // self GitRepository name; defaults to flux-system
	GitRepoNamespace string // self GitRepository namespace; defaults to flux-system
	Stdout           io.Writer

	// applyObject overrides the cluster apply (tests). Defaults to applying
	// via a real Applier bound to the client's k3d context.
	applyObject func(ctx context.Context, obj *unstructured.Unstructured) error
}

// Run repoints the self GitRepository at opts.Branch.
func Run(ctx context.Context, opts Options) error {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Branch == "" {
		return fmt.Errorf("branch is required: flywheel use <branch>")
	}
	if opts.RepoDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts.RepoDir = wd
	}
	if opts.GitRepoName == "" {
		opts.GitRepoName = DefaultGitRepoName
	}
	if opts.GitRepoNamespace == "" {
		opts.GitRepoNamespace = DefaultGitRepoNamespace
	}

	cfg, err := readConfig(opts.RepoDir)
	if err != nil {
		return err
	}

	// Warn (don't fail) if the branch isn't a local branch: Flux reads the
	// in-cluster bare repo, which git-auto-sync populates from the worktree, so
	// a branch you've never checked out won't be there yet. Naming a local
	// branch is the common, working case; anything else is the caller's call.
	if branches, berr := LocalBranches(opts.RepoDir); berr == nil && !slices.Contains(branches, opts.Branch) {
		style.Warn(opts.Stdout, "branch %q is not a local branch in %s; Flux can only deploy it once it exists in the cluster's bare repo (git-auto-sync pushes branches you check out)", opts.Branch, opts.RepoDir)
	}

	apply := opts.applyObject
	if apply == nil {
		a, aerr := applier.New("", k3d.KubeContext(cfg.Cluster.Name))
		if aerr != nil {
			return fmt.Errorf("connect to cluster %q: %w", cfg.Cluster.Name, aerr)
		}
		apply = func(ctx context.Context, obj *unstructured.Unstructured) error {
			// Distinct field manager so the deploy-branch annotation survives a
			// later `flywheel up` (flux-controller) apply — see issue #17.
			return a.ApplyObjectAs(ctx, obj, deployFieldManager, io.Discard)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	obj := BranchPatch(opts.GitRepoName, opts.GitRepoNamespace, opts.Branch, now)
	if err := apply(ctx, obj); err != nil {
		return fmt.Errorf("repoint GitRepository %s/%s to %q: %w", opts.GitRepoNamespace, opts.GitRepoName, opts.Branch, err)
	}

	style.Summary(opts.Stdout, "deploying branch '%s'", opts.Branch)
	style.Detail(opts.Stdout, "Flux GitRepository %s/%s repointed; reconcile triggered", opts.GitRepoNamespace, opts.GitRepoName)
	return nil
}

// BranchPatch builds the server-side-apply object that repoints the self
// GitRepository at `branch`. It carries only the fields we own:
//   - the durable DeployBranchAnnotation (=branch), the record
//     git-auto-sync-self reconciles spec.ref.branch to, so a later clobber
//     (e.g. `flywheel up`) is drift-corrected instead of silently changing the
//     deployed branch;
//   - the `kustomize.toolkit.fluxcd.io/reconcile=disabled` annotation, so
//     kustomize-controller stops re-applying the static `branch:` from the
//     source manifest and racing this change (same reason the sync script adds
//     it imperatively);
//   - a `reconcile.fluxcd.io/requestedAt` trigger so Flux fetches now;
//   - spec.ref.branch.
//
// Split out as a pure function so the object shape is unit-testable without a
// cluster.
func BranchPatch(name, namespace, branch, now string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": gitRepoGVK.GroupVersion().String(),
			"kind":       gitRepoGVK.Kind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
				"annotations": map[string]any{
					// Durable record of intent — git-auto-sync-self reconciles
					// spec.ref.branch to this, so it survives clobbers.
					DeployBranchAnnotation: branch,
					// Stop kustomize-controller re-applying the static branch.
					"kustomize.toolkit.fluxcd.io/reconcile": "disabled",
					// Trigger an immediate fetch.
					"reconcile.fluxcd.io/requestedAt": now,
				},
			},
			"spec": map[string]any{
				"ref": map[string]any{
					"branch": branch,
				},
			},
		},
	}
	obj.SetGroupVersionKind(gitRepoGVK)
	return obj
}

// LocalBranches lists the worktree's local branch names (refs/heads), for
// shell completion of the <branch> argument and the not-a-local-branch warning.
// Best-effort: callers treat an error as "no candidates".
func LocalBranches(repoDir string) ([]string, error) {
	cmd := exec.Command("git", "-C", repoDir, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			branches = append(branches, line)
		}
	}
	return branches, nil
}

// readConfig parses flywheel.yaml merged with flywheel.yaml.local (for a
// per-developer cluster-name override, unlikely but consistent with the other
// commands). Only cluster.name is needed to resolve the k3d context.
func readConfig(repoDir string) (*flywheelSchema.File, error) {
	committed, err := os.ReadFile(filepath.Join(repoDir, "flywheel.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read flywheel.yaml: %w", err)
	}
	var local []byte
	if data, err := os.ReadFile(filepath.Join(repoDir, "flywheel.yaml.local")); err == nil {
		local = data
	}
	merged, err := config.MergeYAML(committed, local)
	if err != nil {
		return nil, err
	}
	cfg, err := flywheelSchema.Parse(merged)
	if err != nil {
		return nil, err
	}
	if cfg.Cluster.Name == "" {
		return nil, fmt.Errorf("flywheel.yaml: cluster.name is required")
	}
	return cfg, nil
}
