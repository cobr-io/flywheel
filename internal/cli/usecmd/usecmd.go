// Package usecmd implements `flywheel use <branch>`: explicitly choose which
// AUTHORED branch the dev loop deploys.
//
// Flux's self GitRepository now tracks a constant DEPLOY branch
// (flywheel/local-deploy); it is never repointed. Instead, `use` records the
// chosen AUTHORED branch in the deploy-branch annotation (naming.DeployBranchAnnotation)
// on the self GitRepository, and git-deploy-controller feeds that branch into DEPLOY
// (= AUTHORED + the IUA's image bumps). The controller polls the annotation, so
// no reconcile trigger is needed here — selecting a branch takes effect on the
// controller's next tick, which rebuilds DEPLOY and pokes Flux.
//
// This stays deliberate (not auto-following worktree checkouts, issue #17): a
// transient checkout during a rebase never changes the deployed branch.
package usecmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/cobr-io/flywheel/internal/cli/applier"
	"github.com/cobr-io/flywheel/internal/cli/config"
	"github.com/cobr-io/flywheel/internal/cli/k3d"
	flywheelSchema "github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/style"
	"github.com/cobr-io/flywheel/internal/execx"
	"github.com/cobr-io/flywheel/internal/naming"
)

// Default identity of the self/gitops GitRepository (matches the
// self-source.yaml.tmpl manifest and the sync deployment's env).
const (
	DefaultGitRepoName      = "flux-system"
	DefaultGitRepoNamespace = naming.FluxNamespace
)

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

	// Warn (don't fail) on the deliberate AUTHORED/worktree split: git-deploy-
	// controller feeds the *selected* branch into DEPLOY by pushing the worktree's
	// copy of it, so commits you make on a different checkout won't deploy until
	// you switch. Easy to get wrong, hence the nudge (design open question #3).
	if cur := currentBranch(opts.RepoDir); cur != "" && cur != opts.Branch {
		style.Warn(opts.Stdout, "your worktree is on %q, not %q — commits you make now won't deploy until you `git switch %s`", cur, opts.Branch, opts.Branch)
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

	obj := BranchPatch(opts.GitRepoName, opts.GitRepoNamespace, opts.Branch)
	if err := apply(ctx, obj); err != nil {
		return fmt.Errorf("set deploy-branch on GitRepository %s/%s to %q: %w", opts.GitRepoNamespace, opts.GitRepoName, opts.Branch, err)
	}

	style.Summary(opts.Stdout, "deploying branch '%s'", opts.Branch)
	style.Detail(opts.Stdout, "git-deploy-controller will build it into the deploy branch on its next tick")
	return nil
}

// BranchPatch builds the server-side-apply object that records the chosen
// AUTHORED branch on the self GitRepository. It carries only the
// DeployBranchAnnotation — git-deploy-controller reads it each tick and feeds
// the branch into DEPLOY. Flux's spec.ref is the constant DEPLOY branch and is
// deliberately NOT touched here.
//
// Split out as a pure function so the object shape is unit-testable without a
// cluster.
func BranchPatch(name, namespace, branch string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": gitRepoGVK.GroupVersion().String(),
			"kind":       gitRepoGVK.Kind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
				"annotations": map[string]any{
					naming.DeployBranchAnnotation: branch,
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
	// TODO: thread a context once callers carry one; this is a best-effort
	// completion helper whose callers don't have a ctx in hand.
	out, err := execx.Git(context.Background(), repoDir, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, err
	}
	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			branches = append(branches, line)
		}
	}
	return branches, nil
}

// currentBranch returns the worktree's checked-out branch, or "" when it can't
// be determined (detached HEAD, unborn branch, git error) — in which case the
// AUTHORED/worktree-mismatch warning is skipped.
func currentBranch(repoDir string) string {
	// TODO: thread a context once callers carry one.
	out, err := execx.Git(context.Background(), repoDir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// readConfig parses flywheel.yaml merged with flywheel.yaml.local (for a
// per-developer cluster-name override, unlikely but consistent with the other
// commands). Only cluster.name is needed to resolve the k3d context, so it
// loads with RequireCluster and no full validation.
func readConfig(repoDir string) (*flywheelSchema.File, error) {
	return config.Load(repoDir, config.LoadOptions{RequireCluster: true})
}
