package converge

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	flywheel "github.com/cobr-io/flywheel"
	"github.com/cobr-io/flywheel/internal/cli/render"
	flywheelSchema "github.com/cobr-io/flywheel/internal/cli/schema"
)

// RenderBootstrap materialises the in-cluster Flux entrypoint
// (clusters/local/flux-system/*) from the binary's embedded templates
// into a fresh tmpdir, using values resolved at this `up`'s runtime.
//
// The output is bootstrap-only: `flywheel up` step 11d applies these
// resources directly via kustomize/kubectl, and Flux thereafter
// reconciles only their *sourceRef* targets (builders/, apps/,
// infra/, the Flywheel mirror) — never this directory. So we
// intentionally keep these files out of the client's committed
// gitops repo: it eliminates the git-auto-sync ↔ refresh-overlay
// race (the bare repo never sees these files, so the host worktree
// can't be reset over uncommitted runtime values), and edits to
// `flywheel.yaml.local` flow through on the next `up` without any
// extra "refresh" step.
//
// Caller owns the returned path and must os.RemoveAll it.
func RenderBootstrap(cfg *flywheelSchema.File, refs map[string]string, flywheelSHA, repoBaseName, branch string) (string, error) {
	tmp, err := os.MkdirTemp("", "flywheel-bootstrap-")
	if err != nil {
		return "", fmt.Errorf("mkdir tmp bootstrap dir: %w", err)
	}
	sub, err := fs.Sub(flywheel.Assets, "templates/bootstrap/clusters/local/flux-system")
	if err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("embed bootstrap missing: %w", err)
	}
	values, err := bootstrapValues(cfg, refs, flywheelSHA, repoBaseName, branch)
	if err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	if err := render.Tree(sub, ".", tmp, values); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("render bootstrap tree: %w", err)
	}
	return tmp, nil
}

// bootstrapValues maps cfg + resolved image refs onto the value names
// the embedded templates expect. Resolved refs are split into
// (name, tag) pairs for builders-kustomization.yaml's `spec.images`
// block, and surfaced verbatim for self-git-auto-sync.yaml's
// container `image:` field.
func bootstrapValues(cfg *flywheelSchema.File, refs map[string]string, flywheelSHA, repoBaseName, branch string) (map[string]any, error) {
	gsName, gsTag := splitImageRef(refs["git-server"])
	// git-auto-sync is surfaced both verbatim (GitAutoSyncImageRef, for the
	// self sidecar's container image: field) AND split into name/tag, which the
	// client-builders Kustomization uses to rewrite the per-app sidecars'
	// canonical ghcr.io ref to whatever `up` mirrored into the local registry.
	gasName, gasTag := splitImageRef(refs["git-auto-sync"])
	ibcName, ibcTag := splitImageRef(refs["image-builder-controller"])

	// The client-infra tier reconciles at flux.iac_interval; infra changes
	// less often than app code, so it can poll slower than interval_local.
	// Optional — fall back to interval_local when unset (older repos).
	iacInterval := cfg.Flux.IacInterval
	if iacInterval == "" {
		iacInterval = cfg.Flux.IntervalLocal
	}

	// All three need a tag (kustomize requires it); empty newTag would
	// otherwise leave the base's value. Default refs always have one;
	// reject overrides that don't (matches what `up` step 9 expects).
	for name, val := range map[string]string{
		"git-server:               (newTag)": gsTag,
		"git-auto-sync:            (newTag)": gasTag,
		"image-builder-controller: (newTag)": ibcTag,
	} {
		if val == "" {
			return nil, fmt.Errorf("bootstrap: %s missing — flywheel.images overrides must include an explicit `:tag`", strings.TrimSuffix(name, " (newTag)"))
		}
	}

	return map[string]any{
		"ClientName":                      cfg.Client.Name,
		"RepoBaseName":                    repoBaseName,
		"Domain":                          cfg.Local.Domain,
		"ClusterName":                     cfg.Cluster.Name,
		"Registry":                        cfg.Cluster.Registry,
		"RegistryPort":                    cfg.Cluster.RegistryPort,
		"Branch":                          branch,
		"FluxIntervalLocal":               cfg.Flux.IntervalLocal,
		"FluxIacInterval":                 iacInterval,
		"FlywheelSHA":                     flywheelSHA,
		"GitServerImageName":              gsName,
		"GitServerImageTag":               gsTag,
		"GitServerMemoryLimit":            cfg.GitServerMemoryLimit(),
		"GitAutoSyncImageRef":             refs["git-auto-sync"],
		"GitAutoSyncImageName":            gasName,
		"GitAutoSyncImageTag":             gasTag,
		"ImageBuilderControllerImageName": ibcName,
		"ImageBuilderControllerImageTag":  ibcTag,
	}, nil
}

// ResolveRepoBaseName returns the basename of the client repo path —
// what /workspaces/<this> resolves to inside the cluster and what
// the in-cluster bare repo is named (<this>.git).
func ResolveRepoBaseName(repoDir string) string {
	return filepath.Base(repoDir)
}

// CurrentBranch returns the branch the client worktree is on, so the
// bootstrap flux-system GitRepository tracks the branch the developer is
// actually working on rather than a hardcoded `main`. Every `flywheel up`
// thus asserts the current branch — agreeing with git-auto-sync-self
// (which patches spec.ref.branch on switches) instead of clobbering it.
//
// Falls back to "main" on a detached HEAD or any git error (fresh repo
// with no commits, git absent): the safe default that matches the
// pre-existing behaviour.
func CurrentBranch(repoDir string) string {
	cmd := exec.Command("git", "-C", repoDir, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "main"
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" || branch == "HEAD" { // empty or detached HEAD
		return "main"
	}
	return branch
}
