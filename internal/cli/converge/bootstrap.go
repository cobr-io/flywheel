package converge

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	flywheel "github.com/cobr-io/flywheel"
	"github.com/cobr-io/flywheel/internal/cli/render"
	flywheelSchema "github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/execx"
)

// RenderBootstrap materialises the in-cluster Flux entrypoint
// (clusters/local/flux-system/*) from the binary's embedded templates
// into a fresh tmpdir, using values resolved at this `up`'s runtime.
//
// The output is bootstrap-only: `flywheel up`'s apply-flux-system step applies these
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
func RenderBootstrap(cfg *flywheelSchema.File, refs map[string]string, flywheelSHA, repoBaseName, buildKitClientRef string) (string, error) {
	tmp, err := os.MkdirTemp("", "flywheel-bootstrap-")
	if err != nil {
		return "", fmt.Errorf("mkdir tmp bootstrap dir: %w", err)
	}
	sub, err := fs.Sub(flywheel.Assets, "templates/bootstrap/clusters/local/flux-system")
	if err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("embed bootstrap missing: %w", err)
	}
	values, err := bootstrapValues(cfg, refs, flywheelSHA, repoBaseName, buildKitClientRef)
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

// bootstrapImage is one resolved runtime image, split into a kustomize
// spec.images entry: Name is the schema image key (schema.ImageNames) used to
// build the `ghcr.io/cobr-io/<Name>` match, ImageName/ImageTag are the resolved
// newName/newTag `flywheel up` mirrored into the cluster. The two bootstrap
// *builders-kustomization.yaml.tmpl templates range over slices of these.
type bootstrapImage struct {
	Name      string
	ImageName string
	ImageTag  string
}

// The two Flux Kustomizations that rewrite runtime image refs on the bootstrap
// path. bootstrapImageOwners assigns each schema.ImageNames entry to exactly
// one of them.
const (
	imgOwnerDevLoop        = "dev-loop"        // flywheel-dev-loop Kustomization (builders-kustomization.yaml)
	imgOwnerClientBuilders = "client-builders" // client-builders Kustomization (client-builders-kustomization.yaml)
)

// bootstrapImageOwners is the single source of truth for the split between
// the two bootstrap Kustomizations' `images:` blocks — the split rationale that
// used to be duplicated as prose in both templates lives here instead.
//
// The two Kustomizations reconcile different trees, so an image's ref must be
// rewritten in whichever one owns its Deployment:
//   - imgOwnerDevLoop: the flywheel-dev-loop Kustomization reconciles
//     manifests/dev-loop/overlays/local. git-server, image-builder-controller,
//     git-deploy-controller and git-auto-sync all have their (single, shared)
//     Deployment under that overlay, so their refs are rewritten there to
//     match the step-11a direct apply (renderDevLoopKustomization) —
//     otherwise Flux re-applies the base ghcr.io ref and the pod
//     ErrImagePulls. git-auto-sync joined this bucket in the Go-port design
//     (docs/designs/2026-07-17-per-app-sync-controller-design.md): it used to
//     be the odd one out (imgOwnerClientBuilders, below) because its only
//     Deployments were per-app builder sidecars; it is now a single
//     controller alongside the other three.
//   - imgOwnerClientBuilders: the client-builders Kustomization reconciles the
//     client repo's per-app builders/ tree. No current image's Deployment
//     lives there (schema.ImageNames images all run in dev-loop/base now), so
//     this bucket — and the client-builders-kustomization.yaml.tmpl `images:`
//     block it feeds — is presently empty. It stays defined for the next
//     image whose only Deployment is per-app-rendered.
//
// Every schema.ImageNames entry MUST appear here; an image with no owner is
// rendered into NEITHER block, which the image agreement test
// (TestBootstrapImages_TemplateUnionMatchesSchema) turns into a CI failure
// instead of a runtime ImagePullBackOff. See docs/dev/add-controller-image.md.
var bootstrapImageOwners = map[string]string{
	"git-server":               imgOwnerDevLoop,
	"image-builder-controller": imgOwnerDevLoop,
	"git-deploy-controller":    imgOwnerDevLoop,
	"git-auto-sync":            imgOwnerDevLoop,
}

// bootstrapContext is the typed render context for the bootstrap tree
// (templates/bootstrap/clusters/local/flux-system/*). It embeds the shared
// schema.Core projection and adds the bootstrap-only extras: the ConfigMap
// key/value map, the two image slices the `images:` blocks range over, and the
// infra cadence / git-server tunable / SHA that only these templates read.
//
// Only three Core fields are actually referenced by the bootstrap templates
// (AppsNamespace, FlywheelNamespace, FluxIntervalLocal); the rest ride along
// unused via the embed, which is harmless (a struct field a template never
// names is simply ignored). The previous map form also carried ClientName,
// Domain, ClusterName, Registry, RegistryPort and IntegrationBranch that NO
// bootstrap template used — those dead keys are gone by construction.
type bootstrapContext struct {
	flywheelSchema.Core
	// RepoBaseName is the client repo basename — not derivable from cfg (it is
	// a filesystem fact), so it is a bootstrap extra rather than a Core field.
	RepoBaseName string
	// FluxIacInterval is the client-infra reconcile cadence (flux.iac_interval,
	// falling back to interval_local).
	FluxIacInterval      string
	FlywheelSHA          string
	GitServerMemoryLimit string
	// FlywheelConfigData is the flywheel-config ConfigMap's full key/value map
	// from the single producer (FlywheelConfigData). flywheel-config.yaml.tmpl
	// ranges over it (text/template visits map keys in sorted order, so the
	// rendered ConfigMap is deterministic) instead of hardcoding keys — so this
	// Flux-owned copy and the step-11 prelude direct apply can't diverge.
	FlywheelConfigData map[string]string
	// DevLoopImages / ClientBuilderImages are the resolved image refs the two
	// `images:` blocks range over, bucketed per bootstrapImageOwners.
	DevLoopImages       []bootstrapImage
	ClientBuilderImages []bootstrapImage
}

// bootstrapValues maps cfg + resolved image refs onto the bootstrap render
// context. It loops over schema.ImageNames (no hand-unrolled per-image keys),
// splitting each resolved ref into a bootstrapImage and bucketing it into
// DevLoopImages or ClientBuilderImages per bootstrapImageOwners. The two
// `images:` template blocks range over their respective slice.
func bootstrapValues(cfg *flywheelSchema.File, refs map[string]string, flywheelSHA, repoBaseName, buildKitClientRef string) (bootstrapContext, error) {
	// The client-infra tier reconciles at flux.iac_interval; infra changes
	// less often than app code, so it can poll slower than interval_local.
	// Optional — fall back to interval_local when unset (older repos).
	iacInterval := cfg.Flux.IacInterval
	if iacInterval == "" {
		iacInterval = cfg.Flux.IntervalLocal
	}

	var devLoopImages, clientBuilderImages []bootstrapImage
	for _, name := range flywheelSchema.ImageNames {
		newName, newTag := splitImageRef(refs[name])
		// Every image needs a tag (kustomize requires it); an empty newTag
		// would otherwise leave the base's value. Default refs always have
		// one; reject overrides that don't (matches what `up`'s mirror-images step expects).
		if newTag == "" {
			return bootstrapContext{}, fmt.Errorf("bootstrap: %s missing — flywheel.images overrides must include an explicit `:tag`", name)
		}
		img := bootstrapImage{Name: name, ImageName: newName, ImageTag: newTag}
		switch bootstrapImageOwners[name] {
		case imgOwnerDevLoop:
			devLoopImages = append(devLoopImages, img)
		case imgOwnerClientBuilders:
			clientBuilderImages = append(clientBuilderImages, img)
		}
		// An image with no owner entry is intentionally rendered into NEITHER
		// block; the image agreement test catches that omission in CI.
	}

	// Core supplies FlywheelNamespace (fixed at naming.FlywheelNamespace) and
	// AppsNamespace (the configured default — a real client knob); cfg is always
	// loader-defaulted (config.Load → applyLoadDefaults) by the time up reaches
	// RenderBootstrap, so AppsNamespace is never empty in production.
	return bootstrapContext{
		Core:                 flywheelSchema.NewCore(cfg),
		RepoBaseName:         repoBaseName,
		FluxIacInterval:      iacInterval,
		FlywheelSHA:          flywheelSHA,
		GitServerMemoryLimit: cfg.GitServerMemoryLimit(),
		FlywheelConfigData:   FlywheelConfigData(cfg, repoBaseName, buildKitClientRef),
		DevLoopImages:        devLoopImages,
		ClientBuilderImages:  clientBuilderImages,
	}, nil
}

// ResolveRepoBaseName returns the basename of the client repo path —
// what /workspaces/<this> resolves to inside the cluster and what
// the in-cluster bare repo is named (<this>.git).
func ResolveRepoBaseName(repoDir string) string {
	return filepath.Base(repoDir)
}

// CurrentBranch returns the branch the client worktree is on. Used by the
// add-app local-only guard (refuse a local-only app on the integration branch)
// and by `up`'s worktree-reconcile reporting. (The bootstrap no longer derives
// Flux's deployed branch from it — Flux tracks the constant flywheel/local-deploy
// DEPLOY branch; see the deploy-ref design.)
//
// Falls back to "main" on a detached HEAD or any git error (fresh repo
// with no commits, git absent): the safe default that matches the
// pre-existing behaviour.
func CurrentBranch(repoDir string) string {
	// TODO: thread a context once callers (add-app, up) carry one; adding the
	// parameter here would cascade beyond the git-owning packages.
	out, err := execx.Git(context.Background(), repoDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "main"
	}
	branch := strings.TrimSpace(out)
	if branch == "" || branch == "HEAD" { // empty or detached HEAD
		return "main"
	}
	return branch
}
