// Package up implements `flywheel up` per design § flywheel up. The Run()
// function orchestrates; per-step logic lives in the helper packages
// (k3d, dockermirror, mirror, applier, flux, etc.).
//
// Destructive reconciliation of the git-managed layers is intentionally NOT
// flywheel's job: Flux owns those (every Flux Kustomization is prune:true).
// flywheel applies only recreatable machinery directly — and, as of issue #27,
// reaps its OWN superseded machinery (the prune-machinery step): resources
// labeled app.kubernetes.io/managed-by=flywheel that a version bump stops rendering.
// That prune is scoped by the label and a kind denylist so it can never delete
// an app/infra workload (those are unlabeled, Flux-owned) nor a Namespace or
// Flux Kustomization/GitRepository (whose deletion would cascade).
package up

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	flywheel "github.com/cobr-io/flywheel"
	"github.com/cobr-io/flywheel/internal/cli/age"
	"github.com/cobr-io/flywheel/internal/cli/applier"
	"github.com/cobr-io/flywheel/internal/cli/converge"
	"github.com/cobr-io/flywheel/internal/cli/doctor"
	"github.com/cobr-io/flywheel/internal/cli/embedcache"
	"github.com/cobr-io/flywheel/internal/cli/flux"
	"github.com/cobr-io/flywheel/internal/cli/gitcheckout"
	"github.com/cobr-io/flywheel/internal/cli/hostmount"
	"github.com/cobr-io/flywheel/internal/cli/imagepin"
	"github.com/cobr-io/flywheel/internal/cli/k3d"
	"github.com/cobr-io/flywheel/internal/cli/mirror"
	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/sourcemode"
	"github.com/cobr-io/flywheel/internal/cli/style"
	"github.com/cobr-io/flywheel/internal/cli/worktree"
	"github.com/cobr-io/flywheel/internal/naming"
)

// Options are the user-facing knobs for `up`.
type Options struct {
	RepoDir string // client repo dir; defaults to cwd
	Stdout  io.Writer
	Stdin   io.Reader // for the worktree-reconcile confirmation prompt

	// Wait gates waiting for Flux Kustomizations Ready before returning:
	// true = wait, false = skip, nil = default (wait), matching --wait's
	// true default. A plain bool can't distinguish "unset" from "explicit
	// false" (both are the zero value), so this mirrors Clone below.
	Wait *bool

	// Clone gates the worktree reconcile: true = clone missing worktrees,
	// false = skip, nil = ask on a TTY (skip otherwise).
	Clone *bool

	// Test seams.
	CacheRoot       string
	HomeOverride    string
	FlywheelSHA     string // tests inject a deterministic SHA; production uses embedcache.Populate
	SkipImageLoad   bool   // tests that pre-populate the registry
	SkipFluxInstall bool   // tests with Flux already present
}

// upState is the mutable state threaded through the up pipeline. Each step reads
// what earlier steps produced and writes what later steps consume; holding it in
// one struct is what lets the pipeline be a flat table (see Run) instead of a
// long function with data flow hidden between blocks.
type upState struct {
	ctx  context.Context
	opts Options
	out  io.Writer

	cfg    *schema.File
	layout gitcheckout.Layout

	cacheDir    string
	sha         string
	kubeContext string

	ageKeyContent string
	ageKeyPath    string

	workspacesRoot string
	resolvedImages map[string]string

	// buildKitClientRef is what the build Jobs' thin client container should
	// run: the in-cluster registry ref after mirror-buildkit-client succeeds,
	// or naming.BuildKitClientImage (Docker Hub) as the fallback. Flows to the
	// controller via the flywheel-config key images.buildkit_client.
	buildKitClientRef string

	a *applier.Applier

	repoBaseName string
	bootstrapDir string // rendered bootstrap tmpdir; Run removes it on exit

	keepDevLoop   []applier.ResourceRef
	keepBootstrap []applier.ResourceRef

	// bootstrapOK gates the prune step: if applying the flux-system tree
	// (the apply-flux-system step) fails, a resource that simply didn't get
	// applied must not be mistaken for a superseded orphan — so
	// prune-machinery is skipped. Starts true; only apply-flux-system clears it.
	bootstrapOK bool
}

// step is one entry in up's pipeline. run performs the work over the shared
// upState. A nil skip means "always run"; otherwise the step is skipped when
// skip reports true — this is how prune-machinery consults bootstrap state and
// how the --wait / test seams elide optional work. critical selects the failure
// policy: a critical step's error aborts the pipeline (wrapped with the step
// name); a non-critical step's error is logged as a warning and the pipeline
// continues. The name replaces the historical step number everywhere it used to
// appear — in these errors/warnings and in cross-package doc comments.
type step struct {
	name     string
	critical bool
	skip     func(*upState) bool
	run      func(*upState) error
}

// runSteps executes steps in declaration order, owning the warn-vs-abort policy
// so no individual step re-implements it. It is the single place the pipeline's
// control flow lives; the table in Run is pure data.
func runSteps(s *upState, steps []step) error {
	for _, st := range steps {
		if st.skip != nil && st.skip(s) {
			continue
		}
		if err := st.run(s); err != nil {
			if st.critical {
				return fmt.Errorf("%s: %w", st.name, err)
			}
			style.Warn(s.out, "%s: %v", st.name, err)
		}
	}
	return nil
}

// Run executes the up pipeline as a table of named steps (see the steps slice
// below), driven by runSteps. Steps replaced the old 1-15 numbering — which had
// gaps and was cited from other packages — with stable names; runSteps owns the
// abort-on-critical / warn-and-continue policy. Returns nil on success; a
// critical step's failure aborts and returns the first error.
func Run(ctx context.Context, opts Options) error {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.RepoDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts.RepoDir = wd
	}
	s := &upState{ctx: ctx, opts: opts, out: opts.Stdout, bootstrapOK: true}
	defer func() {
		if s.bootstrapDir != "" {
			_ = os.RemoveAll(s.bootstrapDir)
		}
	}()

	return runSteps(s, upSteps())
}

// upSteps is the up pipeline's step table, factored out of Run so tests can
// inspect it (e.g. locking a step's critical policy) without driving a full
// cluster run.
func upSteps() []step {
	return []step{
		{name: "load-config", critical: true, run: (*upState).loadConfig},
		{name: "version-check", critical: true, run: (*upState).versionCheck},
		{name: "check-host", critical: true, run: (*upState).checkHost},
		{name: "inspect-checkout", critical: true, run: (*upState).inspectCheckout},
		{name: "embed-cache", critical: true, run: (*upState).embedCache},
		{name: "age-key", critical: true, run: (*upState).ageKey},
		{name: "mkcert", critical: true, run: (*upState).mkcert},
		{name: "heal-host-ports", critical: true, run: (*upState).healPorts},
		{name: "create-registry", critical: true, run: (*upState).createRegistry},
		{name: "resolve-workspaces-root", critical: true, run: (*upState).resolveWorkspacesRoot},
		{name: "reconcile-worktrees", run: (*upState).reconcile},
		{name: "create-cluster", critical: true, run: (*upState).createCluster},
		{name: "verify-mounts", critical: true, run: (*upState).verifyMounts},
		{name: "mirror-images", critical: true, run: (*upState).mirrorImages},
		{name: "mirror-buildkit-client", run: (*upState).mirrorBuildKitClient},
		{name: "new-applier", critical: true, run: (*upState).newApplier},
		{name: "flux-install", critical: true, skip: skipFluxInstall, run: (*upState).fluxInstall},
		{name: "render-bootstrap", critical: true, run: (*upState).renderBootstrap},
		{name: "ensure-namespaces", critical: true, run: (*upState).ensureNamespaces},
		{name: "flywheel-config", critical: true, run: (*upState).flywheelConfig},
		{name: "dev-loop", critical: true, run: (*upState).devLoop},
		{name: "wait-git-server", critical: true, run: (*upState).waitGitServer},
		{name: "push-mirror", run: (*upState).pushMirror},
		{name: "apply-flux-system", critical: true, run: (*upState).applyFluxSystem},
		{name: "prune-machinery", skip: skipPrune, run: (*upState).pruneMachinery},
		{name: "create-secrets", critical: true, run: (*upState).createSecrets},
		{name: "wait-flux-kustomizations", skip: skipWait, run: (*upState).waitFluxKustomizations},
		{name: "success", critical: true, run: (*upState).printSuccess},
	}
}

func skipFluxInstall(s *upState) bool { return s.opts.SkipFluxInstall }
func skipWait(s *upState) bool        { return !waitEnabled(s.opts) }
func skipPrune(s *upState) bool       { return !s.bootstrapOK }

// loadConfig reads flywheel.yaml + .local, merges, and validates.
func (s *upState) loadConfig() error {
	cfg, err := converge.LoadConfig(s.opts.RepoDir)
	if err != nil {
		return err
	}
	s.cfg = cfg
	style.Step(s.out, "%s @ %s", cfg.Client.Name, cfg.Flywheel.Version)
	return nil
}

// versionCheck is the version-drift gate — run before any host/network work.
// `up` only proceeds when the installed binary and the pinned flywheel.version
// agree (or the user accepts a forward bump). A dev build skips inside
// checkVersionDrift.
func (s *upState) versionCheck() error {
	newVersion, err := checkVersionDrift(s.out, s.opts.Stdin, s.opts.RepoDir, s.cfg.Flywheel.Version)
	if err != nil {
		return err
	}
	s.cfg.Flywheel.Version = newVersion
	return nil
}

// checkHost runs the doctor host-prerequisite checks BEFORE any network work
// (closed material gap O7 / plan T1.3 reorder).
func (s *upState) checkHost() error {
	style.Step(s.out, "checking host prerequisites")
	checks := doctor.QuickChecks()
	if results := doctor.Run(checks); !allOK(results) {
		printDoctor(s.out, results)
		return fmt.Errorf("host prerequisites missing — fix the issues above and retry")
	}
	return nil
}

// inspectCheckout classifies the git checkout. A git linked worktree needs its
// shared git dir bind-mounted so the in-cluster git-deploy-controller can read
// it (issue #62); a *nested* worktree can't satisfy flywheel's sibling model, so
// refuse it here (before any network/cluster work) with remediation.
func (s *upState) inspectCheckout() error {
	layout, lerr := gitcheckout.Inspect(s.opts.RepoDir)
	if lerr != nil {
		style.Warn(s.out, "could not classify git checkout (%v); assuming a normal clone", lerr)
	}
	s.layout = layout
	if layout.IsWorktree && layout.Nested {
		if os.Getenv(gitcheckout.AllowNestedEnv) == "" {
			return fmt.Errorf("%s", gitcheckout.NestedRemediation(layout))
		}
		style.Warn(s.out, "%s is a nested git worktree; proceeding because %s is set (apps must live in-repo)",
			layout.Dir, gitcheckout.AllowNestedEnv)
	}
	if layout.IsWorktree && layout.CommonDir != "" {
		style.Detail(s.out, "git worktree: mounting shared git dir %s", layout.CommonDir)
	}
	return nil
}

// embedCache extracts the binary's embedded asset tree to
// ~/.cache/flywheel/<version>/ and stamps a deterministic commit. The resulting
// cacheDir is what the push-mirror step pushes into the in-cluster flywheel.git
// mirror; the SHA goes into flywheel-source's spec.ref.
func (s *upState) embedCache() error {
	cacheRoot := s.opts.CacheRoot
	if cacheRoot == "" {
		if s.opts.HomeOverride != "" {
			cacheRoot = filepath.Join(s.opts.HomeOverride, ".cache", "flywheel")
		} else {
			r, err := embedcache.DefaultRoot()
			if err != nil {
				return err
			}
			cacheRoot = r
		}
	}
	if s.opts.FlywheelSHA != "" {
		// Test path: caller pre-populated the cache and pinned the SHA.
		s.cacheDir = filepath.Join(cacheRoot, s.cfg.Flywheel.Version)
		s.sha = s.opts.FlywheelSHA
	} else {
		cacheDir, sha, err := embedcache.Populate(cacheRoot, s.cfg.Flywheel.Version, flywheel.Assets, ".")
		if err != nil {
			return err
		}
		s.cacheDir, s.sha = cacheDir, sha
	}
	style.Detail(s.out, "flywheel cache: %s @ %s", s.cacheDir, s.sha[:12])
	return nil
}

// ageKey loads the age private key to install as the in-cluster sops-age Secret.
func (s *upState) ageKey() error {
	content, path, err := loadAgeKey(s.opts.RepoDir, s.cfg.Client.Name, s.opts.HomeOverride)
	if err != nil {
		return err
	}
	s.ageKeyContent, s.ageKeyPath = content, path
	style.Detail(s.out, "age key: %s", path)
	return nil
}

// mkcert runs `mkcert -install` and generates cert/{cert,key}.pem for the domain.
func (s *upState) mkcert() error {
	return ensureMkcert(s.ctx, s.opts.RepoDir, s.cfg.Local.Domain, s.out)
}

// healPorts heals host-port collisions before k3d binds them. The ports in
// flywheel.yaml are allocated once at init time; by now one may be held by
// another process (issue #1). Reallocate any foreign-held port from its pool and
// persist it, so cluster creation doesn't crash with "address already in use". A
// port our own running cluster/registry holds is left as-is (re-running up stays
// idempotent).
func (s *upState) healPorts() error {
	return healHostPorts(s.ctx, s.opts, s.cfg, s.out)
}

// createRegistry creates the k3d registry. Wrapped so a port taken between the
// heal-host-ports probe and this bind (TOCTOU) self-heals + retries instead of
// crashing.
func (s *upState) createRegistry() error {
	return createRegistryHealOnce(s.ctx, s.opts, s.cfg, s.out)
}

// resolveWorkspacesRoot resolves the host dir the cluster bind-mounts as
// /workspaces (explicit paths.workspaces_root, else repoDir's parent).
func (s *upState) resolveWorkspacesRoot() error {
	wsRoot, err := workspacesRootFrom(s.cfg, s.opts.RepoDir)
	if err != nil {
		return err
	}
	s.workspacesRoot = wsRoot
	style.Detail(s.out, "workspaces=%s", wsRoot)
	return nil
}

// reconcile materialises app worktrees BEFORE the cluster mounts
// workspaces_root: clone any declared app whose source worktree is missing, so a
// fresh gitops-repo clone bootstraps in one command. Best-effort (never aborts).
func (s *upState) reconcile() error {
	reconcileWorktrees(s.ctx, s.opts, s.cfg, s.workspacesRoot, s.out)
	return nil
}

// createCluster creates the k3d cluster. Wrapped so an http/https port taken
// between the heal-host-ports probe and this bind self-heals + retries once
// instead of crashing k3d.
func (s *upState) createCluster() error {
	return createClusterHealOnce(s.ctx, s.opts, s.cfg, func() k3d.CreateClusterOpts {
		return k3d.CreateClusterOpts{
			Name:           s.cfg.Cluster.Name,
			K3sImage:       s.cfg.Cluster.K3sImage,
			Servers:        s.cfg.Cluster.Servers,
			Agents:         s.cfg.Cluster.Agents,
			RegistryName:   s.cfg.Cluster.Registry,
			HttpPort:       s.cfg.Cluster.HttpPort,
			HttpsPort:      s.cfg.Cluster.HttpsPort,
			WorkspacesRoot: s.workspacesRoot,
			GitCommonDir:   s.layout.CommonDir, // "" for a normal clone; a worktree's shared git dir otherwise
		}
	}, s.out)
}

// verifyMounts verifies the bind-mounts actually bridged into the cluster. The
// gitops repo must be visible at /workspaces/<repo>, or self-git-auto-sync can't
// push it and the client-* Kustomizations never find their source; on macOS,
// temp dirs (/tmp, /var/folders) don't bind-mount into k3d — fail fast with
// remediation instead of the cryptic downstream "Source artifact not found". A
// git worktree additionally needs its shared git dir mounted at its
// host-absolute path (issue #62), so verify that mount too.
func (s *upState) verifyMounts() error {
	repoBaseName := converge.ResolveRepoBaseName(s.opts.RepoDir)
	if visible, verr := k3d.WorkspaceVisible(s.ctx, s.cfg.Cluster.Name, repoBaseName); verr != nil {
		style.Warn(s.out, "could not verify the workspaces mount (%v); continuing", verr)
	} else if !visible {
		return fmt.Errorf("the gitops repo is not visible in the cluster at /workspaces/%s — workspaces_root %q did not bind-mount into k3d.\n%s",
			repoBaseName, s.workspacesRoot, hostmount.Remediation())
	}
	if s.layout.IsWorktree && s.layout.CommonDir != "" {
		if visible, verr := k3d.NodePathExists(s.ctx, s.cfg.Cluster.Name, s.layout.CommonDir); verr != nil {
			style.Warn(s.out, "could not verify the worktree git-dir mount (%v); continuing", verr)
		} else if !visible {
			return fmt.Errorf("%s", gitcheckout.UnreachableCommonDirRemediation(s.layout))
		}
	}
	return nil
}

// mirrorImages mirrors each Flywheel image into the cluster's LOCAL registry so
// every node pulls it on demand — immune to the per-node scheduling/GC gaps
// issue #14 fixed. Each image is resolved (cfg.Flywheel.Images override or
// default ghcr.io ref); a registry-hosted ref is copied registry→registry scoped
// to the host platform (containerd-store-safe, issue #50) under its :<version>
// tag, a local-only dogfood build is tag+pushed from the docker store under a
// content-addressed :dogfood-<sha> tag. EnsureInCluster returns the in-cluster
// pull ref, written back so render-bootstrap and dev-loop reference the registry
// path. Resolving the refs happens unconditionally (later steps need them);
// SkipImageLoad only elides the mirror itself (tests that pre-populate).
func (s *upState) mirrorImages() error {
	s.resolvedImages = imagepin.Resolve(s.cfg)
	if s.opts.SkipImageLoad {
		return nil
	}
	// Pre-flight: dogfood overrides that name no registry can only come from a
	// local `make images` build. Probe for all of them up front and stop with
	// build guidance, rather than attempting a doomed pull mid-mirror (a cryptic
	// Docker Hub "repository does not exist") or deferring to an in-cluster
	// ImagePullBackOff after the cluster is up.
	if missing := imagepin.CheckLocalOverrides(s.ctx, s.cfg); len(missing) > 0 {
		return fmt.Errorf("dogfood images: %w", imagepin.MissingDogfoodError(missing))
	}
	style.Step(s.out, "mirroring Flywheel images to the local registry")
	for _, name := range schema.ImageNames {
		ref := s.resolvedImages[name]
		loadName := name
		source := "dogfood build"
		if imagepin.IsDefault(name, s.cfg.Flywheel.Version, ref) {
			source = "released image, pulled from ghcr"
		}
		var served string
		if err := style.Spin(s.out,
			fmt.Sprintf("mirror %s → local registry (%s)", ref, source),
			func() error {
				var e error
				served, e = imagepin.EnsureInCluster(s.ctx, ref,
					s.cfg.Cluster.Registry, s.cfg.Cluster.RegistryPort, loadName, s.cfg.Flywheel.Version, s.out)
				return e
			},
		); err != nil {
			return fmt.Errorf("%s: %w", loadName, err)
		}
		s.resolvedImages[name] = served
	}
	return nil
}

// mirrorBuildKitClient pre-seeds the buildkit CLIENT image — the thin per-Job
// container; the build itself runs in the shared warm buildkitd daemon — into
// the cluster's local registry, so the first build Job scheduled on each node
// pulls it over the LAN (~2s) instead of from Docker Hub (~15-30s). That
// per-node cold pull is the cause of the bimodal early-bump dev-loop latency
// (issue #107; docs/dev/dev-loop-latency.md).
//
// Best-effort by design (non-critical step, never returns an error): on any
// failure — offline host, Hub outage — s.buildKitClientRef keeps the upstream
// Hub ref and build Jobs pull per node exactly as before this step existed.
// The host docker store caches the image across down/up cycles, so only the
// first-ever run pays the Hub download.
func (s *upState) mirrorBuildKitClient() error {
	s.buildKitClientRef = naming.BuildKitClientImage // fallback: per-node Hub pulls
	if s.opts.SkipImageLoad {
		return nil
	}
	// EnsureInCluster's local-mirror path tags+pushes from the host docker
	// store; make sure the image is there first (no-op when already cached).
	if err := exec.CommandContext(s.ctx, "docker", "image", "inspect", naming.BuildKitClientImage).Run(); err != nil {
		if out, perr := exec.CommandContext(s.ctx, "docker", "pull", naming.BuildKitClientImage).CombinedOutput(); perr != nil {
			style.Warn(s.out, "buildkit client pre-seed skipped (docker pull %s: %v); builds will pull it per node\n%s",
				naming.BuildKitClientImage, perr, strings.TrimSpace(string(out)))
			return nil
		}
	}
	if err := style.Spin(s.out,
		fmt.Sprintf("mirror %s → local registry (build-client pre-seed)", naming.BuildKitClientImage),
		func() error {
			ref, e := imagepin.EnsureInCluster(s.ctx, naming.BuildKitClientImage,
				s.cfg.Cluster.Registry, s.cfg.Cluster.RegistryPort, "moby/buildkit", s.cfg.Flywheel.Version, s.out)
			if e != nil {
				return e
			}
			s.buildKitClientRef = ref
			return nil
		},
	); err != nil {
		style.Warn(s.out, "buildkit client pre-seed failed (%v); builds will pull %s per node",
			err, naming.BuildKitClientImage)
	}
	return nil
}

// newApplier builds the SSA applier bound to the cluster's kube context.
func (s *upState) newApplier() error {
	s.kubeContext = k3d.KubeContext(s.cfg.Cluster.Name)
	a, err := applier.New("", s.kubeContext)
	if err != nil {
		return err
	}
	s.a = a
	return nil
}

// fluxInstall installs Flux (SSA via fieldManager=flux-controller) and waits for
// its controllers Ready. Skipped by SkipFluxInstall (tests with Flux present).
func (s *upState) fluxInstall() error {
	if err := style.Spin(s.out,
		fmt.Sprintf("installing Flux %s", flux.Version),
		func() error { return flux.Install(s.ctx, s.a, s.out) },
	); err != nil {
		return err
	}
	// 5-minute budget matches a typical `flux install --timeout 5m0s`. Cold
	// colima pulls the Flux controller images from ghcr.io on first run, which
	// can exceed 2 minutes.
	if err := converge.WaitForDeployments(s.ctx, s.a, naming.FluxNamespace, []string{
		"source-controller",
		"kustomize-controller",
	}, 5*time.Minute, s.out); err != nil {
		return fmt.Errorf("flux ready: %w", err)
	}
	// Flux just installed its CRDs (GitRepository, Kustomization,
	// ImageUpdateAutomation, ...). Invalidate the applier's discovery cache so
	// the dev-loop + flux-system manifests that reference those kinds map
	// correctly in the dev-loop / apply-flux-system steps.
	s.a.ResetMapper()
	return nil
}

// renderBootstrap renders the bootstrap flux-system manifest set from the
// binary's embedded templates into a tmpdir using runtime values (resolved image
// refs + cache SHA + repo basename). These resources are bootstrap-only —
// applied by later steps, then Flux reconciles their *sourceRef* targets (the
// Flywheel mirror + the client builders/apps/infra paths), never this directory.
// Keeping them out of the committed gitops repo eliminates the git-auto-sync ↔
// refresh-overlay race and makes .local edits flow through on the next `up`
// without any explicit refresh. Run removes bootstrapDir on exit.
func (s *upState) renderBootstrap() error {
	s.repoBaseName = converge.ResolveRepoBaseName(s.opts.RepoDir)
	dir, err := converge.RenderBootstrap(s.cfg, s.resolvedImages, s.sha, s.repoBaseName, s.buildKitClientRef)
	if err != nil {
		return err
	}
	s.bootstrapDir = dir
	return nil
}

// ensureNamespaces applies the rendered namespaces.yaml so the dev-loop pods
// have their namespaces before the dev-loop step lands them.
func (s *upState) ensureNamespaces() error {
	nsPath := filepath.Join(s.bootstrapDir, "namespaces.yaml")
	return style.Spin(s.out, "bootstrap: ensuring namespaces", func() error {
		raw, err := os.ReadFile(nsPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", nsPath, err)
		}
		return s.a.ApplyYAML(s.ctx, raw, s.out)
	})
}

// flywheelConfig regenerates the flywheel-config ConfigMap from the merged
// flywheel.yaml (closed material gap O3 / plan T1.13). Applied directly here so
// the dev-loop pods can read it immediately; Flux re-applies the committed copy
// in the apply-flux-system step (SSA no-op).
func (s *upState) flywheelConfig() error {
	return style.Spin(s.out, "bootstrap: applying flywheel-config ConfigMap", func() error {
		return converge.ApplyFlywheelConfig(s.ctx, s.a, s.cfg, s.repoBaseName, s.buildKitClientRef, s.out)
	})
}

// devLoop applies the dev-loop overlay (also where the inotify DaemonSet
// lands). Rewrites the overlay's image references for THIS
// client using the resolved (override-aware) refs from mirror-images, and
// patches git-server's memory limit (cfg.git_server.memory_limit) — the same
// limit rendered into the flywheel-dev-loop Flux Kustomization by
// apply-flux-system, so this direct apply and Flux's reconcile agree. keepDevLoop
// ∪ keepBootstrap form the keep set prune-machinery scans against — the resources
// THIS run applied.
func (s *upState) devLoop() error {
	devLoopDir := filepath.Join(s.cacheDir, "manifests", "dev-loop", "overlays", "local")
	return style.Spin(s.out, "bootstrap: dev-loop overlay", func() error {
		keep, e := converge.ApplyDevLoop(s.ctx, s.a, devLoopDir, s.resolvedImages, s.cfg.GitServerMemoryLimit(), s.out)
		s.keepDevLoop = keep
		return e
	})
}

// waitGitServer waits for the git-server Deployment Ready (the Waiter inside
// WaitForDeployments renders its own header).
func (s *upState) waitGitServer() error {
	return converge.WaitForDeployments(s.ctx, s.a, naming.FlywheelNamespace, []string{"git-server"}, 3*time.Minute, s.out)
}

// pushMirror pushes the cache into the in-cluster git-server as flywheel.git.
// Best-effort (non-critical): a Warn surfaces the error, but we continue — Flux's
// flywheel-source is then unreconciled, which is documented as a known gap.
func (s *upState) pushMirror() error {
	if err := style.Spin(s.out, "bootstrap: pushing cache into in-cluster mirror", func() error {
		return mirror.Push(s.ctx, "", s.kubeContext, naming.FlywheelNamespace, "git-server",
			"flywheel", s.cacheDir, s.sha, s.out)
	}); err != nil {
		return fmt.Errorf("%w (Flux flywheel-source won't reconcile until this works)", err)
	}
	return nil
}

// applyFluxSystem applies the bootstrap flux-system tree from the rendered
// tmpdir. Flux's Kustomization + GitRepository objects come into existence here
// with `spec.images` / `spec.ref.commit` already matching the resolved refs +
// cache SHA the rest of `up` is using — no follow-up refresh needed. Critical
// (issue #117): a failed apply here used to be swallowed as a WARN, and the
// Ready-wait derives its expected set from whatever Kustomizations the API
// server actually holds — so a dropped resource silently shrank the success
// criterion instead of failing the run, turning a deterministic apply
// rejection into a 20-minute wait-timeout mystery. down=destroy / up=always-
// recreate makes "re-run up" the remedy either way, so abort loudly here
// instead. bootstrapOK is still cleared on failure as belt-and-braces for
// prune-machinery (a resource that failed to apply must not be mistaken for
// an orphan), even though the abort means that step is normally never reached.
func (s *upState) applyFluxSystem() error {
	err := style.Spin(s.out,
		"bootstrap: applying flux-system (from in-memory bootstrap)",
		func() error {
			keep, e := s.a.ApplyKustomizeTracked(s.ctx, s.bootstrapDir, s.out)
			s.keepBootstrap = keep
			return e
		},
	)
	if err != nil {
		s.bootstrapOK = false
	}
	return err
}

// pruneMachinery prunes superseded flywheel machinery (issue #27). Only the
// resources THIS run re-applied (keepDevLoop ∪ keepBootstrap) are spared; any
// other managed-by=flywheel resource of the same kinds is an orphan from a prior
// version and gets removed (e.g. the old git-auto-sync-self Deployment the
// deploy-ref migration superseded). Skipped when bootstrapOK is false (dev-loop
// aborts the run outright, apply-flux-system clears the flag) so a resource that
// failed to apply isn't mistaken for an orphan; app/infra workloads (unlabeled,
// Flux-managed) and state / cascade kinds (Namespace, PVC, Secret, Flux
// Kustomization/GitRepository) are never touched. Best-effort: failures warn.
func (s *upState) pruneMachinery() error {
	keep := make([]applier.ResourceRef, 0, len(s.keepDevLoop)+len(s.keepBootstrap))
	keep = append(keep, s.keepDevLoop...)
	keep = append(keep, s.keepBootstrap...)
	pruned, err := converge.PruneOrphanedMachinery(s.ctx, s.a, keep, s.out)
	if err != nil {
		return err
	}
	if pruned > 0 {
		style.Detail(s.out, "pruned %d superseded resource(s)", pruned)
	}
	return nil
}

// createSecrets creates the age-key Secret + (mkcert) local-cert + mkcert-ca
// Secrets.
func (s *upState) createSecrets() error {
	if err := style.Spin(s.out, "creating SOPS age Secret", func() error {
		return createAgeSecret(s.ctx, s.a, s.ageKeyContent, s.out)
	}); err != nil {
		return fmt.Errorf("age secret: %w", err)
	}
	if err := style.Spin(s.out, "creating local-cert Secret", func() error {
		return createMkcertSecret(s.ctx, s.a, s.opts.RepoDir, s.out)
	}); err != nil {
		return fmt.Errorf("mkcert secret: %w", err)
	}
	if err := style.Spin(s.out, "creating mkcert-ca Secret", func() error {
		return createMkcertRootSecret(s.ctx, s.a, s.out)
	}); err != nil {
		return fmt.Errorf("mkcert root secret: %w", err)
	}
	return nil
}

// waitFluxKustomizations waits for Flux Kustomizations Ready (best-effort; the
// Waiter renders its own header). Skipped when --wait=false. Waits on the
// Kustomizations apply-flux-system actually applied (keepBootstrap), not
// whatever the API server happens to list — issue #117, Tier 2.
func (s *upState) waitFluxKustomizations() error {
	return waitForFluxKustomizations(s.ctx, s.a, kustomizationNames(s.keepBootstrap), 3*time.Minute, s.out)
}

// kustomizationNames extracts the Flux Kustomization names from a
// ResourceRef set, for waitForFluxKustomizations' expected set (issue #117,
// Tier 2): the bootstrap tree's keep set also includes the GitRepository and
// ConfigMap/Namespace kinds apply-flux-system applies alongside it, which
// aren't Kustomizations and have nothing to wait Ready on.
func kustomizationNames(refs []applier.ResourceRef) []string {
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.Kind == "Kustomization" {
			names = append(names, r.Name)
		}
	}
	return names
}

// printSuccess prints the closing summary. Don't fabricate an app URL here (no
// app exists yet, and the bare host would need the published HTTPS port); point
// at add-app, which prints the real URL for the name it scaffolds.
func (s *upState) printSuccess() error {
	domain := s.cfg.Local.Domain
	if domain == "" {
		domain = "localdev.me"
	}
	portSuffix := ""
	if p := s.cfg.Cluster.HttpsPort; p != 0 && p != 443 {
		portSuffix = fmt.Sprintf(":%d", p)
	}
	fmt.Fprintln(s.out)
	style.Summary(s.out, "Cluster up. Add an app:  flywheel add app <name>")
	style.Detail(s.out, "served at https://<name>.%s%s/", domain, portSuffix)
	return nil
}

// loadAgeKey returns the age private key to install as the in-cluster
// sops-age Secret. The committed repo key (clusters/local/age.key) is
// canonical and wins when present — this is what lets a fresh clone +
// `flywheel up` decrypt with no host key at all. It falls back to the host
// key (~/.config/flywheel/<client>/age.key) for repos created before the key
// was committed. The repo key is non-secret by design and a git checkout is
// 0644, so the 0600 mode-check applies only to the host key.
func loadAgeKey(repoDir, clientName, homeOverride string) (content, path string, err error) {
	repoKey := filepath.Join(repoDir, "clusters", "local", "age.key")
	if raw, rerr := os.ReadFile(repoKey); rerr == nil {
		return strings.TrimSpace(string(raw)) + "\n", repoKey, nil
	}
	if homeOverride != "" {
		path = filepath.Join(homeOverride, ".config", "flywheel", clientName, "age.key")
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", path, err
		}
		if err := age.CheckMode(path); err != nil {
			return "", path, err
		}
		return strings.TrimSpace(string(raw)) + "\n", path, nil
	}
	content, path, err = age.ReadPrivateKey(clientName)
	if err != nil {
		return "", path, err
	}
	if err := age.CheckMode(path); err != nil {
		return "", path, err
	}
	return strings.TrimSpace(content) + "\n", path, nil
}

// reconcileWorktrees materialises declared app worktrees that are missing under
// wsRoot, by cloning their workspace-block source (keyed by worktree, so a
// worktree shared by several builders is cloned at most once). Best-effort: it
// never aborts `up` — clone failures, missing local-only siblings, and apps
// referencing an undeclared worktree are warned and skipped. The trigger is
// explicit because it writes OUTSIDE the gitops repo: --clone clones without
// asking, --no-clone skips, and with neither it prompts on a TTY (and skips,
// with a hint, when there's no TTY).
func reconcileWorktrees(ctx context.Context, opts Options, cfg *schema.File, wsRoot string, out io.Writer) {
	var clonable []schema.WorkspaceRepo
	type presentRepo struct {
		name, path           string
		wantBranch, onBranch string // declared vs. actual; both "" when no branch is declared
	}
	var present []presentRepo
	for _, r := range cfg.Workspace.Repos {
		dir := filepath.Join(wsRoot, r.Name)
		if _, err := os.Stat(dir); err == nil {
			// A present worktree is left on whatever branch it's on —
			// switching a repo out from under the user would be dangerous.
			// We only read the actual branch to flag a mismatch against an
			// explicit declared branch (skip the git call when none is set).
			on := ""
			if r.Branch != "" {
				on = converge.CurrentBranch(dir)
			}
			present = append(present, presentRepo{name: r.Name, path: dir, wantBranch: r.Branch, onBranch: on})
			continue
		}
		if r.LocalOnly {
			style.Warn(out, "app worktree %q is declared local-only and missing — it won't build until its source is published ('flywheel publish-app')", r.Name)
			continue
		}
		clonable = append(clonable, r)
	}

	// Report worktrees we found in place, so a successful detection isn't
	// silent. Pad names so the paths line up into a scannable column.
	if len(present) > 0 {
		w := 0
		for _, p := range present {
			if len(p.name) > w {
				w = len(p.name)
			}
		}
		for _, p := range present {
			if p.wantBranch != "" && p.onBranch != p.wantBranch {
				style.Warn(out, "%s present but on branch %q, not the declared %q — leaving it as-is; run 'git -C %s checkout %s' if that's wrong",
					p.name, p.onBranch, p.wantBranch, p.path, p.wantBranch)
				continue
			}
			style.OK(out, "%-*s present  (%s)", w, p.name, p.path)
		}
	}

	// Apps whose worktree is neither present nor declared cannot be materialised.
	// The undeclared-app join lives in sourcemode; up adds the on-disk check
	// (only warn when the worktree is also missing).
	if apps, err := sourcemode.Undeclared(opts.RepoDir, cfg); err == nil {
		for _, a := range apps {
			if _, err := os.Stat(filepath.Join(wsRoot, a.Worktree)); err == nil {
				continue
			}
			style.Warn(out, "app %q references worktree %q, which is missing and not declared in workspace.repos — add it ('flywheel add app') or clone it manually", a.Name, a.Worktree)
		}
	}

	if len(clonable) == 0 {
		return
	}

	doClone := false
	switch {
	case opts.Clone != nil:
		doClone = *opts.Clone
	case isTTY(opts.Stdin):
		style.Detail(out, "%d app worktree(s) are declared but missing; they will be cloned into %s:", len(clonable), wsRoot)
		for _, r := range clonable {
			style.Detail(out, "  %s  ←  %s", r.Name, r.URL)
		}
		doClone = promptYesNo(opts.Stdin, out, "clone them now?", false)
	default:
		style.Warn(out, "%d app worktree(s) missing; re-run with --clone to materialise (or --no-clone to silence). Skipping.", len(clonable))
		return
	}
	if !doClone {
		style.Detail(out, "skipping worktree clone (%d missing)", len(clonable))
		return
	}

	var failed []string
	for _, r := range clonable {
		dest := filepath.Join(wsRoot, r.Name)
		var gotBranch bool
		if err := style.Spin(out, fmt.Sprintf("clone %s", r.Name), func() error {
			var e error
			gotBranch, e = worktree.Clone(ctx, r.URL, dest, r.Branch)
			return e
		}); err != nil {
			failed = append(failed, r.Name)
			style.Warn(out, "clone %s failed: %v", r.Name, err)
			continue
		}
		if r.Branch != "" && !gotBranch {
			style.Warn(out, "branch %q not found on %s; %s stays on the remote default branch", r.Branch, r.URL, r.Name)
		}
	}
	if len(failed) > 0 {
		style.Warn(out, "could not materialise: %s (clone manually, then re-run)", strings.Join(failed, ", "))
	}
}

// waitEnabled resolves Options.Wait's nil-means-default-true sentinel.
func waitEnabled(opts Options) bool {
	return opts.Wait == nil || *opts.Wait
}

func isTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func promptYesNo(stdin io.Reader, out io.Writer, question string, defaultYes bool) bool {
	hint := "[y/N]"
	if defaultYes {
		hint = "[Y/n]"
	}
	fmt.Fprintf(out, "%s %s: ", question, hint)
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		return false // EOF / read error (e.g. Ctrl-D) → decline, regardless of default
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default: // blank line or unrecognised input → take the default
		return defaultYes
	}
}

func workspacesRootFrom(cfg *schema.File, repoDir string) (string, error) {
	if cfg.Paths.WorkspacesRoot != "" {
		return cfg.Paths.WorkspacesRoot, nil
	}
	// Auto-detect: parent of repoDir, matching `flywheel new` step 8.
	return filepath.Dir(repoDir), nil
}

func allOK(results []doctor.Result) bool {
	for _, r := range results {
		if !r.OK() {
			return false
		}
	}
	return true
}

func printDoctor(out io.Writer, results []doctor.Result) {
	for _, r := range results {
		status := "OK"
		if !r.OK() {
			status = "FAIL"
		}
		fmt.Fprintf(out, "  [%s] %-8s — %s\n", status, r.Check.Name, r.Check.Description)
		if !r.OK() {
			fmt.Fprintf(out, "           %v\n", r.Err)
		}
	}
}
