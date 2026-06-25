// Package up implements `flywheel up` per design § flywheel up (15-step
// pipeline). The Run() function orchestrates; per-step logic lives in
// the helper packages (k3d, dockermirror, mirror, applier, flux, etc.).
//
// v0.1.0 caveats — these step-level simplifications are documented as
// "deviations" in the Phase 1 gate evidence and tracked for v0.1.X:
//
//   - Step 4 (reconcile diff): always treated as additive in v0.1.0;
//     no destructive-set detection, no `--yes-additive` gating beyond
//     a verbose log. T1.4/T1.14/T1.15 land in v0.1.1.
//   - Step 12 (orphan deletes): stub. Same v0.1.1 follow-up.
package up

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
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
	"github.com/cobr-io/flywheel/internal/cli/hostmount"
	"github.com/cobr-io/flywheel/internal/cli/imagepin"
	"github.com/cobr-io/flywheel/internal/cli/k3d"
	"github.com/cobr-io/flywheel/internal/cli/mirror"
	"github.com/cobr-io/flywheel/internal/cli/reconcile"
	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/style"
	"github.com/cobr-io/flywheel/internal/cli/worktree"
)

// Options are the user-facing knobs for `up`.
type Options struct {
	RepoDir string // client repo dir; defaults to cwd
	Yes     bool   // approve destructive ops
	YesAdd  bool   // approve +additive only
	Wait    bool   // wait for Flux Kustomizations Ready before returning
	Stdout  io.Writer
	Stdin   io.Reader // for the worktree-reconcile confirmation prompt

	// Clone gates the worktree reconcile: true = clone missing worktrees,
	// false = skip, nil = ask on a TTY (skip otherwise).
	Clone *bool

	// Test seams.
	FlywheelRepoURL string // informational only; embedded assets are the source of truth
	CacheRoot       string
	HomeOverride    string
	FlywheelSHA     string // tests inject a deterministic SHA; production uses embedcache.Populate
	SkipImageLoad   bool   // tests that pre-populate the registry
	SkipFluxInstall bool   // tests with Flux already present
}

// Run is the 15-step pipeline. Returns nil on success; partial failures
// abort early and return the first error.
func Run(ctx context.Context, opts Options) error {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Wait == false {
		// Default Wait=true unless explicitly disabled (zero value means
		// "user didn't set"). Go's zero-value bool collapses true/false
		// → pass a sentinel pointer in a real CLI; the simple v0.1.0
		// dispatcher always opts in.
		opts.Wait = true
	}
	if opts.RepoDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts.RepoDir = wd
	}
	out := opts.Stdout

	// Step 1 — read flywheel.yaml + .local, merge, validate.
	cfg, err := converge.LoadConfig(opts.RepoDir)
	if err != nil {
		return fmt.Errorf("step 1 (read config): %w", err)
	}
	style.Step(out, "%s @ %s", cfg.Client.Name, cfg.Flywheel.Version)

	// Step 2 — doctor BEFORE network (closed material gap O7 / plan
	// T1.3 reorder).
	style.Step(out, "checking host prerequisites")
	checks := doctor.QuickChecks()
	if results := doctor.Run(checks); !allOK(results) {
		printDoctor(out, results)
		return fmt.Errorf("step 2: host prerequisites missing — fix the issues above and retry")
	}

	// Step 3 — extract the binary's embedded asset tree to
	// ~/.cache/flywheel/<version>/ and stamp a deterministic commit. The
	// resulting cacheDir is what step 11c pushes into the in-cluster
	// flywheel.git mirror; the SHA goes into flywheel-source's spec.ref.
	cacheRoot := opts.CacheRoot
	if cacheRoot == "" {
		if opts.HomeOverride != "" {
			cacheRoot = filepath.Join(opts.HomeOverride, ".cache", "flywheel")
		} else {
			cacheRoot, err = embedcache.DefaultRoot()
			if err != nil {
				return err
			}
		}
	}
	var cacheDir, sha string
	if opts.FlywheelSHA != "" {
		// Test path: caller pre-populated the cache and pinned the SHA.
		cacheDir = filepath.Join(cacheRoot, cfg.Flywheel.Version)
		sha = opts.FlywheelSHA
	} else {
		cacheDir, sha, err = embedcache.Populate(cacheRoot, cfg.Flywheel.Version, flywheel.Assets, ".")
		if err != nil {
			return fmt.Errorf("step 3 (embed cache): %w", err)
		}
	}
	style.Detail(out, "flywheel cache: %s @ %s", cacheDir, sha[:12])

	// Step 4 — reconcile diff. v0.1.0 has no destructive-set detection:
	// every `up` is treated as additive. (The prior profile-switch
	// detection was removed along with the multi-profile machinery —
	// there is now a single local TLS setup, so nothing to switch away
	// from.) Destructive-set detection + gating lands in v0.1.1 (plan
	// T1.4/T1.14/T1.15); the empty plan keeps step 12 a no-op until then.
	kubeContext := k3d.KubeContext(cfg.Cluster.Name)
	plan := &reconcile.Plan{}
	if len(plan.Changes) > 0 {
		style.Step(out, "reconcile diff:")
		fmt.Fprint(out, plan.Render())
		approval := reconcile.Approval{Yes: opts.Yes, YesAdditive: opts.YesAdd}
		if plan.NeedsConfirmation(approval) {
			return fmt.Errorf("step 4: destructive changes require --yes (see plan above); "+
				"%d resource(s) would be deleted. CRDs/PVCs are never auto-deleted regardless",
				len(plan.DeletableDestructive()))
		}
	} else {
		style.Step(out, "reconcile diff: no destructive changes")
	}

	// Step 5 — load age key + mkcert generate.
	ageKeyContent, ageKeyPath, err := loadAgeKey(opts.RepoDir, cfg.Client.Name, opts.HomeOverride)
	if err != nil {
		return fmt.Errorf("step 5 (age key): %w", err)
	}
	style.Detail(out, "age key: %s", ageKeyPath)
	if err := ensureMkcert(ctx, opts.RepoDir, cfg.Local.Domain, out); err != nil {
		return fmt.Errorf("step 5 (mkcert): %w", err)
	}

	// Step 5b — heal host-port collisions before k3d binds them. The ports in
	// flywheel.yaml are allocated once at init time; by now one may be held by
	// another process (issue #1). Reallocate any foreign-held port from its
	// pool and persist it, so cluster creation doesn't crash with "address
	// already in use". A port our own running cluster/registry holds is left
	// as-is (re-running up stays idempotent).
	if err := healHostPorts(ctx, opts, cfg, out); err != nil {
		return fmt.Errorf("step 5b (host ports): %w", err)
	}

	// Step 6 — k3d registry create.
	if err := style.Spin(out,
		fmt.Sprintf("k3d registry %s:%d", cfg.Cluster.Registry, cfg.Cluster.RegistryPort),
		func() error { return k3d.CreateRegistry(ctx, cfg.Cluster.Registry, cfg.Cluster.RegistryPort) },
	); err != nil {
		return fmt.Errorf("step 6: %w", err)
	}

	// Step 7 — k3d cluster create.
	workspacesRoot, err := workspacesRootFrom(cfg, opts.RepoDir)
	if err != nil {
		return fmt.Errorf("step 7 (workspaces_root): %w", err)
	}
	style.Detail(out, "workspaces=%s", workspacesRoot)

	// Step 6b — reconcile app worktrees BEFORE the cluster mounts
	// workspaces_root: clone any declared app whose source worktree is
	// missing, so a fresh gitops-repo clone bootstraps in one command.
	reconcileWorktrees(ctx, opts, cfg, workspacesRoot, out)

	if err := style.Spin(out,
		fmt.Sprintf("k3d cluster %s", cfg.Cluster.Name),
		func() error {
			return k3d.CreateCluster(ctx, k3d.CreateClusterOpts{
				Name:           cfg.Cluster.Name,
				K3sImage:       cfg.Cluster.K3sImage,
				Servers:        cfg.Cluster.Servers,
				Agents:         cfg.Cluster.Agents,
				RegistryName:   cfg.Cluster.Registry,
				HttpPort:       cfg.Cluster.HttpPort,
				HttpsPort:      cfg.Cluster.HttpsPort,
				WorkspacesRoot: workspacesRoot,
			})
		},
	); err != nil {
		return fmt.Errorf("step 7: %w", err)
	}

	// Step 7b — verify the bind-mount actually bridged. The gitops repo must be
	// visible in-cluster at /workspaces/<repo>, or self-git-auto-sync can't push
	// it and the client-* Kustomizations never find their source. On macOS, temp
	// dirs (/tmp, /var/folders) don't bind-mount into k3d — fail fast with
	// remediation instead of the cryptic downstream "Source artifact not found".
	if visible, verr := k3d.WorkspaceVisible(ctx, cfg.Cluster.Name, converge.ResolveRepoBaseName(opts.RepoDir)); verr != nil {
		style.Warn(out, "could not verify the workspaces mount (%v); continuing", verr)
	} else if !visible {
		return fmt.Errorf("the gitops repo is not visible in the cluster at /workspaces/%s — workspaces_root %q did not bind-mount into k3d.\n%s",
			converge.ResolveRepoBaseName(opts.RepoDir), workspacesRoot, hostmount.Remediation())
	}

	// Step 8 — inotify handled by privileged DaemonSet in step 11a.

	// Step 9 — mirror each Flywheel image into the cluster's LOCAL registry so
	// every node pulls it on demand — immune to the per-node scheduling/GC gaps
	// issue #14 fixed. Each image is resolved (cfg.Flywheel.Images
	// override or default ghcr.io ref); a released ghcr ref is pulled to the
	// host then pushed under its :<version> tag, a dogfood override under a
	// content-addressed :dogfood-<sha> tag. EnsureInCluster returns the
	// in-cluster pull ref, written back so renderBootstrap and applyDevLoop
	// reference the registry path.
	resolvedImages := imagepin.Resolve(cfg)
	if !opts.SkipImageLoad {
		style.Step(out, "mirroring Flywheel images to the local registry")
		for _, name := range schema.ImageNames {
			ref := resolvedImages[name]
			loadName := name
			source := "dogfood build"
			if imagepin.IsDefault(name, cfg.Flywheel.Version, ref) {
				source = "released image, pulled from ghcr"
			}
			var served string
			if err := style.Spin(out,
				fmt.Sprintf("mirror %s → local registry (%s)", ref, source),
				func() error {
					var e error
					served, e = imagepin.EnsureInCluster(ctx, ref,
						cfg.Cluster.Registry, cfg.Cluster.RegistryPort, loadName, cfg.Flywheel.Version, out)
					return e
				},
			); err != nil {
				return fmt.Errorf("step 9 (%s): %w", loadName, err)
			}
			resolvedImages[name] = served
		}
	}

	a, err := applier.New("", kubeContext)
	if err != nil {
		return fmt.Errorf("applier: %w", err)
	}

	// Step 10 — Flux install (SSA via fieldManager=flux-controller).
	if !opts.SkipFluxInstall {
		if err := style.Spin(out,
			fmt.Sprintf("installing Flux %s", flux.Version),
			func() error { return flux.Install(ctx, a, out) },
		); err != nil {
			return fmt.Errorf("step 10: %w", err)
		}
		// 5-minute budget matches a typical `flux install --timeout 5m0s`.
		// Cold colima pulls the Flux controller images from ghcr.io on
		// first run, which can exceed 2 minutes.
		if err := converge.WaitForDeployments(ctx, a, "flux-system", []string{
			"source-controller",
			"kustomize-controller",
		}, 5*time.Minute, out); err != nil {
			return fmt.Errorf("step 10 (Flux ready): %w", err)
		}
		// Flux just installed its CRDs (GitRepository, Kustomization,
		// ImageUpdateAutomation, ...). Invalidate the applier's discovery
		// cache so the dev-loop + flux-system manifests that reference
		// those kinds map correctly in 11a/11d.
		a.ResetMapper()
	}

	// Step 11 prelude — render the bootstrap flux-system manifest set
	// from the binary's embedded templates into a tmpdir using runtime
	// values (resolved image refs + cache SHA + repo basename). These
	// resources are bootstrap-only — applied here, then Flux reconciles
	// their *sourceRef* targets (the Flywheel mirror + the client
	// builders/apps/infra paths), never this directory. Keeping them
	// out of the committed gitops repo eliminates the git-auto-sync ↔
	// refresh-overlay race and makes .local edits flow through on the
	// next `up` without any explicit refresh.
	repoBaseName := converge.ResolveRepoBaseName(opts.RepoDir)
	branch := converge.CurrentBranch(opts.RepoDir)
	bootstrapDir, err := converge.RenderBootstrap(cfg, resolvedImages, sha, repoBaseName, branch)
	if err != nil {
		return fmt.Errorf("step 11 (render bootstrap): %w", err)
	}
	defer os.RemoveAll(bootstrapDir)

	nsPath := filepath.Join(bootstrapDir, "namespaces.yaml")
	if err := style.Spin(out, "bootstrap: ensuring namespaces", func() error {
		raw, err := os.ReadFile(nsPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", nsPath, err)
		}
		return a.ApplyYAML(ctx, raw, out)
	}); err != nil {
		return fmt.Errorf("ensure namespaces: %w", err)
	}

	// Step 11 prelude — regenerate flywheel-config ConfigMap from the
	// merged flywheel.yaml (closed material gap O3 / plan T1.13). Applied
	// directly here so the dev-loop pods in 11a can read it immediately;
	// Flux re-applies the committed copy in 11d (SSA no-op).
	if err := style.Spin(out, "bootstrap: applying flywheel-config ConfigMap", func() error {
		return converge.ApplyFlywheelConfig(ctx, a, cfg, out)
	}); err != nil {
		return fmt.Errorf("flywheel-config: %w", err)
	}

	// Step 11a — apply dev-loop overlay.
	devLoopDir := filepath.Join(cacheDir, "manifests", "dev-loop", "overlays", "local")
	// Rewrite the overlay's image references for THIS client using the
	// resolved (override-aware) refs from step 9.
	if err := style.Spin(out, "bootstrap 11a: dev-loop overlay", func() error {
		return converge.ApplyDevLoop(ctx, a, devLoopDir, resolvedImages, out)
	}); err != nil {
		return fmt.Errorf("step 11a: %w", err)
	}

	// Step 11b — wait for git-server Ready.
	// Step 11b: covered by the Waiter inside waitForDeployments — no
	// step header here, since the Waiter prints its own.
	if err := converge.WaitForDeployments(ctx, a, "flywheel-system", []string{"git-server"}, 3*time.Minute, out); err != nil {
		return fmt.Errorf("step 11b: %w", err)
	}

	// Step 11c — push cache into in-cluster git-server as flywheel.git.
	// Step 11c — best-effort push (a Warn surfaces the error, but we
	// continue: Flux's flywheel-source will be unreconciled, which is
	// documented as a known gap).
	if err := style.Spin(out, "bootstrap 11c: pushing cache into in-cluster mirror", func() error {
		return mirror.Push(ctx, "", kubeContext, "flywheel-system", "git-server",
			"flywheel", cacheDir, sha, out)
	}); err != nil {
		style.Warn(out, "step 11c: %v (Flux flywheel-source won't reconcile until this works)", err)
	}

	// Step 11d — apply the bootstrap flux-system tree from the tmpdir
	// rendered above. Flux's Kustomization + GitRepository objects come
	// into existence here with `spec.images` / `spec.ref.commit` already
	// matching the resolved refs + cache SHA the rest of `up` is using
	// — no follow-up refresh needed.
	if err := style.Spin(out,
		"bootstrap 11d: applying flux-system (from in-memory bootstrap)",
		func() error { return a.ApplyKustomize(ctx, bootstrapDir, out) },
	); err != nil {
		style.Warn(out, "step 11d: %v", err)
	}

	// Step 12 — orphan deletes: apply the approved destructive set from
	// step 4 (CRDs/PVCs are tiered out and only reported).
	if len(plan.Changes) > 0 {
		style.Step(out, "applying approved deletions")
		if err := applyOrphanDeletes(ctx, a, plan, out); err != nil {
			return fmt.Errorf("step 12: %w", err)
		}
	}

	// Step 13 — create age-key Secret + (mkcert) local-cert + mkcert-ca Secrets.
	if err := style.Spin(out, "creating SOPS age Secret", func() error {
		return createAgeSecret(ctx, a, ageKeyContent, out)
	}); err != nil {
		return fmt.Errorf("step 13 (age secret): %w", err)
	}
	if err := style.Spin(out, "creating local-cert Secret", func() error {
		return createMkcertSecret(ctx, a, opts.RepoDir, out)
	}); err != nil {
		return fmt.Errorf("step 13 (mkcert secret): %w", err)
	}
	if err := style.Spin(out, "creating mkcert-ca Secret", func() error {
		return createMkcertRootSecret(ctx, a, out)
	}); err != nil {
		return fmt.Errorf("step 13 (mkcert root secret): %w", err)
	}

	// Step 14 — wait for Flux Kustomizations Ready (best-effort in v0.1.0).
	// The Waiter inside waitForFluxKustomizations renders its own header.
	if opts.Wait {
		if err := waitForFluxKustomizations(ctx, a, 3*time.Minute, out); err != nil {
			style.Warn(out, "step 14: %v", err)
		}
	}

	// Step 15 — print success. Don't fabricate an app URL here (no app
	// exists yet, and the bare host would need the published HTTPS port);
	// point at add-app, which prints the real URL for the name it scaffolds.
	domain := cfg.Local.Domain
	if domain == "" {
		domain = "localdev.me"
	}
	portSuffix := ""
	if p := cfg.Cluster.HttpsPort; p != 0 && p != 443 {
		portSuffix = fmt.Sprintf(":%d", p)
	}
	fmt.Fprintln(out)
	style.Summary(out, "Cluster up. Add an app:  flywheel add app <name>")
	style.Detail(out, "served at https://<name>.%s%s/", domain, portSuffix)
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
	declared := make(map[string]bool, len(cfg.Workspace.Repos))
	var clonable []schema.WorkspaceRepo
	type presentRepo struct{ name, path string }
	var present []presentRepo
	for _, r := range cfg.Workspace.Repos {
		declared[r.Name] = true
		dir := filepath.Join(wsRoot, r.Name)
		if _, err := os.Stat(dir); err == nil {
			present = append(present, presentRepo{r.Name, dir}) // leave it on its current branch
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
			style.OK(out, "%-*s present  (%s)", w, p.name, p.path)
		}
	}

	// Apps whose worktree is neither present nor declared cannot be materialised.
	if apps, err := worktree.DeclaredApps(opts.RepoDir); err == nil {
		for _, a := range apps {
			if a.Worktree == "" || declared[a.Worktree] {
				continue
			}
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
		doClone = promptYesNo(opts.Stdin, out, "clone them now?")
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

func isTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func promptYesNo(stdin io.Reader, out io.Writer, question string) bool {
	fmt.Fprintf(out, "%s [y/N]: ", question)
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
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
