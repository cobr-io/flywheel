// Package app implements `flywheel add app`: it scaffolds a per-app builder folder under
// `builders/base/<name>/` from the binary's embedded per-app-template.
// It also appends the new entry to `builders/base/kustomization.yaml`.
//
// Design: § flywheel add app <name> (v0.1: thin) — copy templates,
// substitute values, append to the parent kustomization, walk away.
// No 3-way merge, no app-workload scaffolding (that lives in
// apps/base/<name>/, hand-managed).
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	flywheel "github.com/cobr-io/flywheel"
	"github.com/cobr-io/flywheel/internal/cli/config"
	"github.com/cobr-io/flywheel/internal/cli/converge"
	"github.com/cobr-io/flywheel/internal/cli/imagepin"
	"github.com/cobr-io/flywheel/internal/cli/render"
	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/style"
	wt "github.com/cobr-io/flywheel/internal/cli/worktree"
	"github.com/cobr-io/flywheel/internal/naming"
)

// Options are the inputs to Run.
type Options struct {
	RepoDir    string // client repo root; defaults to cwd
	Worktree   string // host worktree dir (bare name / relative / absolute path); required
	Name       string // app name override; empty = derive from the worktree
	Image      string // build artefact image short name; defaults to the app name
	Context    string // docker build context (relative to the worktree); defaults to "."
	Dockerfile string // path within Context; defaults to "Dockerfile"
	Target     string // multi-stage build target stage; empty = the Dockerfile's last stage
	Namespace  string // target namespace for the workload; empty = cfg.Namespaces.Apps
	Branch     string // branch to check out on clone and record in the workspace entry; empty = remote default
	Stdout     io.Writer

	// TemplateFS overrides the embedded per-app-template tree (tests).
	// Defaults to flywheel.Assets sub-FS at manifests/per-app-template.
	TemplateFS fs.FS

	// AppsTemplateFS overrides the embedded apps-template tree (tests).
	// Defaults to flywheel.Assets sub-FS at manifests/apps-template.
	AppsTemplateFS fs.FS
}

// Result is the success outcome.
type Result struct {
	BuilderDir string // builders/base/<name>/ (absolute path)
	AppsDir    string // apps/base/<name>/ (absolute path)
	NextSteps  string
	URL        string // where the app is served once the pod is running
}

// appURL builds the browser URL for a scaffolded app from its Ingress host
// (<name>.<domain>) and the cluster's published HTTPS port. The port is
// included unless it's the implicit HTTPS default (443), so users don't have
// to guess it — hitting the bare host lands on whatever else owns :443.
func appURL(name, domain string, httpsPort int) string {
	host := name + "." + domain
	if httpsPort == 0 || httpsPort == 443 {
		return "https://" + host + "/"
	}
	return fmt.Sprintf("https://%s:%d/", host, httpsPort)
}

// dns1123Label matches a-z0-9 + dashes, length 1-63 (lower-case only).
// Kubernetes labels and object names live in this character set.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// validateDNSLabel returns an error if v is not a valid DNS-1123 label, naming
// the offending field (e.g. "--name", "--namespace"). This is the single home
// for the check the user-supplied name and namespace both need; the derived-name
// guard keeps its own message because its guidance ("pass --name") differs.
func validateDNSLabel(field, v string) error {
	if !dns1123Label.MatchString(v) {
		return fmt.Errorf("invalid %s %q: must be a DNS-1123 label (lowercase, dashes, 1-63 chars)", field, v)
	}
	return nil
}

// Run scaffolds an app end to end: it renders the per-app builder tree
// (builders/base/<name>/) and workload tree (apps/base/<name>/), registers both
// in their parent kustomizations, and records the app in flywheel.yaml's
// workspace block.
//
// The ordering is transactional, in three phases:
//
//   - derive-and-validate: every fallible check that only reads the repo —
//     config load, validation, worktree resolution (cloning a git URL into
//     workspaces_root when given one — a sibling, never the client repo), name
//     and image derivation, the Dockerfile pre-flight, the destination and
//     kustomization pre-checks, and the local-only guard.
//   - render-to-staging: render both template trees into a throwaway staging
//     directory. This is the step most likely to fail (a bad template).
//   - commit: a short tail whose steps are unlikely to fail — move the staged
//     trees into place, edit the kustomizations, and upsert the workspace entry
//     LAST (it is the registration).
//
// A failure before the commit tail therefore leaves flywheel.yaml and the
// builders/ apps/ trees byte-for-byte untouched. A failure inside the tail
// prints exactly what was written and how to undo it.
func Run(opts Options) (*Result, error) {
	// ----- defaults -----
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.RepoDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		opts.RepoDir = wd
	}
	if opts.Context == "" {
		opts.Context = "."
	}
	if opts.Dockerfile == "" {
		opts.Dockerfile = "Dockerfile"
	}
	if opts.TemplateFS == nil {
		sub, err := fs.Sub(flywheel.Assets, "manifests/per-app-template")
		if err != nil {
			return nil, fmt.Errorf("embedded per-app-template missing: %w", err)
		}
		opts.TemplateFS = sub
	}
	if opts.AppsTemplateFS == nil {
		sub, err := fs.Sub(flywheel.Assets, "manifests/apps-template")
		if err != nil {
			return nil, fmt.Errorf("embedded apps-template missing: %w", err)
		}
		opts.AppsTemplateFS = sub
	}

	// ===================== Phase: derive & validate =====================
	// Everything here only reads the repo (plus, for a git URL, clones into
	// workspaces_root — a sibling, never the client repo). No client-repo
	// mutation happens until the commit tail.

	// Config: read flywheel.yaml (+ .local) for workspaces_root, namespaces,
	// and cluster info.
	cfg, err := readConfig(opts.RepoDir)
	if err != nil {
		return nil, err
	}
	if cfg.Local.Domain == "" {
		return nil, errors.New("flywheel.yaml: local.domain is required (used as the Ingress host suffix for the scaffolded workload)")
	}

	// Namespace: explicit --namespace wins; otherwise the global apps namespace
	// (keeps existing behaviour byte-for-byte).
	if opts.Namespace == "" {
		opts.Namespace = cfg.Namespaces.Apps
	}
	if err := validateDNSLabel("--namespace", opts.Namespace); err != nil {
		return nil, err
	}

	// Worktree: <dir> may be a bare name (a child of workspaces_root), a relative
	// path (vs cwd), or an absolute path; it must resolve to an existing
	// directory that is a direct child of workspaces_root — the only layout the
	// cluster's single /workspaces bind-mount and git-server's basename scan
	// support.
	if opts.Worktree == "" {
		return nil, errors.New("worktree directory is required")
	}
	wsRoot := cfg.Paths.WorkspacesRoot
	if wsRoot == "" {
		wsRoot = filepath.Dir(opts.RepoDir)
	}

	// Clone mode: a git URL is cloned into workspaces_root as a sibling, then
	// registered exactly like an on-disk worktree. The clone sets the dest's
	// `origin` to the URL, so the source-provenance probe below records it
	// automatically (no special-casing).
	if wt.LooksLikeGitURL(opts.Worktree) {
		// Validate --name BEFORE cloning so an invalid name fails fast instead
		// of leaving a stray clone on disk (which a re-run would then refuse).
		if opts.Name != "" {
			if err := validateDNSLabel("--name", opts.Name); err != nil {
				return nil, err
			}
		}
		dirName := opts.Name
		if dirName == "" {
			dirName = sanitizeName(wt.RepoNameFromURL(opts.Worktree))
		}
		if dirName == "" {
			return nil, fmt.Errorf("could not derive a worktree directory name from %q; pass --name", opts.Worktree)
		}
		dest := filepath.Join(wsRoot, dirName)
		if _, statErr := os.Stat(dest); statErr == nil {
			return nil, fmt.Errorf("%s already exists; refuse to clone over it (remove it or pass --name)", dest)
		} else if !os.IsNotExist(statErr) {
			return nil, statErr
		}
		style.Detail(opts.Stdout, "cloning %s into %s", opts.Worktree, dest)
		gotBranch, err := wt.Clone(context.Background(), opts.Worktree, dest, opts.Branch)
		if err != nil {
			return nil, err
		}
		if opts.Branch != "" && !gotBranch {
			style.Warn(opts.Stdout, "branch %q not found on %s; staying on the remote default branch", opts.Branch, opts.Worktree)
		}
		opts.Worktree = dest // resolve the freshly-cloned dir below
	}

	worktree, worktreePath, err := resolveWorktree(opts.Worktree, wsRoot, opts.RepoDir)
	if err != nil {
		return nil, err
	}

	// Name: an explicit --name wins; otherwise derive it from a project manifest
	// in the worktree, falling back to the directory name.
	if opts.Name != "" {
		if err := validateDNSLabel("--name", opts.Name); err != nil {
			return nil, err
		}
	} else {
		derived, source, derr := deriveName(worktreePath)
		if derr != nil {
			return nil, derr
		}
		if derived != "" {
			opts.Name = derived
			style.Detail(opts.Stdout, "derived name '%s' from %s", derived, source)
		} else {
			opts.Name = sanitizeName(worktree)
			if opts.Name == "" {
				return nil, fmt.Errorf("could not derive a valid app name from directory %q; pass --name", worktree)
			}
			style.Detail(opts.Stdout, "no project manifest found; using directory name '%s'", opts.Name)
		}
	}
	if !dns1123Label.MatchString(opts.Name) {
		return nil, fmt.Errorf("derived app name %q is not a valid DNS-1123 label; pass --name", opts.Name)
	}

	// Image: default to the app name, then warn on names that overflow the build
	// Job's 63-char budget. The build Job is named
	// build-<gitrepo>[-<image>]-<ts>-<sha>, and the GitRepository is named after
	// the app, so the human part is the app name (and image, if it differs).
	// Over-budget names still build (the controller truncates + hashes) but the
	// build Pod name gets mangled.
	if opts.Image == "" {
		opts.Image = opts.Name
	}
	const buildJobHumanBudget = 38 // 63 - len("build-") - len("-<10-digit ts>-<7-char sha>")
	humanID := opts.Name
	if opts.Image != opts.Name {
		humanID += "-" + opts.Image
	}
	if len(humanID) > buildJobHumanBudget {
		style.Warn(opts.Stdout, "app name %q is long: build Pod names will be truncated to fit Kubernetes' 63-char Job-name limit (builds still work)", opts.Name)
	}

	// Dockerfile pre-flight: the build needs at least a Dockerfile. Without this
	// check add-app happily scaffolds an app that can never build — the failure
	// only surfaces much later in the buildkit build Job. Fail early instead.
	dockerfilePath := filepath.Join(worktreePath, opts.Context, opts.Dockerfile)
	if _, err := os.Stat(dockerfilePath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no Dockerfile at %s in %s; an app needs at least a Dockerfile to build. Pass --context/--dockerfile if it lives elsewhere",
				filepath.Join(opts.Context, opts.Dockerfile), worktreePath)
		}
		return nil, err
	}

	// Destinations: refuse if EITHER already exists, so we never leave the repo
	// half-scaffolded (builders rendered but apps render failed).
	builderDest := filepath.Join(opts.RepoDir, "builders", "base", opts.Name)
	appsDest := filepath.Join(opts.RepoDir, "apps", "base", opts.Name)
	for _, dest := range []string{builderDest, appsDest} {
		if _, err := os.Stat(dest); err == nil {
			return nil, fmt.Errorf("%s already exists; refuse to overwrite (delete it manually if you really want to start over)", dest)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}

	// Kustomizations: pre-check the two files the commit tail appends to, so a
	// malformed kustomization fails now — before any write — rather than after
	// the trees are already on disk.
	buildersKust := filepath.Join(opts.RepoDir, "builders", "base", "kustomization.yaml")
	appsKust := filepath.Join(opts.RepoDir, "apps", "base", "kustomization.yaml")
	for _, k := range []string{buildersKust, appsKust} {
		if err := preflightResources(k); err != nil {
			return nil, err
		}
	}

	// Source provenance: the GitRepository spec.url always points at the
	// in-cluster bare repo, so it can't tell a local-only app from a
	// remote-backed one. Record where the source actually came from — the
	// worktree's `origin` remote, or local-only when it has none — in the
	// workspace block, the single source of truth the guard, `up`-clone, and
	// `publish-app` all key on. Keyed by worktree: a second builder sharing a
	// worktree finds the entry already present (idempotent upsert).
	srcURL, hasRemote := wt.GitRemoteURL(worktreePath)
	localOnly := !hasRemote

	// Layer 1 of the local-only guard: a local-only app's source exists only
	// on this machine, so its manifests must not reach the integration branch
	// (other devs / the cluster could never reconcile it). Refuse on that
	// branch; on a feature branch proceed with a publish reminder.
	if localOnly {
		integ := cfg.IntegrationBranch()
		if converge.CurrentBranch(opts.RepoDir) == integ {
			return nil, fmt.Errorf("refuse to register a local-only app on %q: its source exists only on your machine and must not reach %s. Switch to a feature branch (git checkout -b <branch>) and re-run", integ, integ)
		}
		style.Warn(opts.Stdout, "'%s' has no external git remote — its source exists only on this machine. Before this branch merges to %s, push the worktree to a remote and run 'flywheel publish-app %s'", opts.Name, integ, opts.Name)
	}
	// Built here, written LAST (the registration).
	repo := schema.WorkspaceRepo{Name: worktree, URL: srcURL, LocalOnly: localOnly, Branch: opts.Branch}

	// ===================== Phase: render to staging =====================
	// The renders are the most likely thing to fail (a bad template). Doing them
	// into a throwaway dir under the repo (same filesystem, so the tail's moves
	// are atomic renames) means a failure leaves the client repo untouched.

	// The per-app git-auto-sync Deployment commits the *canonical* ghcr.io
	// ref — NOT a resolved/content-addressed registry ref. The client-builders
	// Flux Kustomization rewrites this name to whatever `up` mirrored into the
	// local registry (a dogfood `:dogfood-<sha>` build or the released
	// `:<version>` default), re-rendered fresh on every `up`. Committing the
	// resolved ref instead would pin a digest that goes stale the next time the
	// dogfood image is rebuilt — the ImagePullBackOff this avoids. This mirrors
	// how git-server / image-builder-controller are handled (stable ghcr name
	// in the manifest, rewritten by the Kustomization's spec.images).
	gitAutoSyncRef := imagepin.DefaultRef("git-auto-sync", cfg.Flywheel.Version)
	values := buildValues(opts, cfg, worktree, gitAutoSyncRef)

	staging, err := os.MkdirTemp(opts.RepoDir, ".flywheel-add-app-*")
	if err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(staging)

	stagedBuilder := filepath.Join(staging, "builder")
	if err := render.Tree(opts.TemplateFS, ".", stagedBuilder, values); err != nil {
		return nil, fmt.Errorf("render per-app-template: %w", err)
	}
	stagedApps := filepath.Join(staging, "apps")
	if err := render.Tree(opts.AppsTemplateFS, ".", stagedApps, values); err != nil {
		return nil, fmt.Errorf("render apps-template: %w", err)
	}

	// ===================== Phase: commit =====================
	// Short and unlikely to fail: move the staged trees into place, register them
	// in the kustomizations, then record the workspace entry LAST. Each completed
	// step is remembered so a later failure can report exactly what was written
	// (we cannot cleanly roll back the in-place kustomization edits).
	var written []string
	fail := func(err error) (*Result, error) {
		if len(written) > 0 {
			style.Warn(opts.Stdout, "add app failed after writing part of the repo; changes made so far:")
			for _, w := range written {
				style.Detail(opts.Stdout, "  %s", w)
			}
			style.Detail(opts.Stdout, "to undo: git -C %s checkout -- . && git -C %s clean -fd builders/base/%s apps/base/%s",
				opts.RepoDir, opts.RepoDir, opts.Name, opts.Name)
		}
		return nil, err
	}

	if err := os.Rename(stagedBuilder, builderDest); err != nil {
		return fail(fmt.Errorf("move rendered builder into place: %w", err))
	}
	written = append(written, "created "+builderDest)

	if err := appendResource(buildersKust, opts.Name); err != nil {
		return fail(fmt.Errorf("append to builders/base/kustomization.yaml: %w", err))
	}
	written = append(written, "registered ./"+opts.Name+" in builders/base/kustomization.yaml")

	if err := os.Rename(stagedApps, appsDest); err != nil {
		return fail(fmt.Errorf("move rendered workload into place: %w", err))
	}
	written = append(written, "created "+appsDest)

	if err := appendResource(appsKust, opts.Name); err != nil {
		return fail(fmt.Errorf("append to apps/base/kustomization.yaml: %w", err))
	}
	written = append(written, "registered ./"+opts.Name+" in apps/base/kustomization.yaml")

	// Ensure the target namespace exists. The default apps namespace is created
	// cluster-side (clusters/<env>/flux-system/namespaces.yaml), so only
	// ADDITIONAL namespaces need a managed object here. Idempotent: two apps
	// sharing one namespace yield exactly one Namespace doc.
	if opts.Namespace != cfg.Namespaces.Apps {
		nsManifest := filepath.Join(opts.RepoDir, "apps", "base", "namespaces.yaml")
		if err := ensureNamespace(nsManifest, opts.Namespace); err != nil {
			return fail(fmt.Errorf("ensure namespace %q: %w", opts.Namespace, err))
		}
		written = append(written, "declared namespace "+opts.Namespace+" in apps/base/namespaces.yaml")
		if err := appendResource(appsKust, "namespaces.yaml"); err != nil {
			return fail(fmt.Errorf("register namespaces.yaml in apps/base/kustomization.yaml: %w", err))
		}
		written = append(written, "registered ./namespaces.yaml in apps/base/kustomization.yaml")
	}

	// Registration LAST: the workspace block is the source of truth the guard,
	// `up`-clone, and `publish-app` key on — record the app only once everything
	// else is safely on disk.
	if err := config.UpsertWorkspaceRepo(filepath.Join(opts.RepoDir, naming.ConfigFile), repo); err != nil {
		return fail(fmt.Errorf("record %s in the workspace block: %w", worktree, err))
	}

	// ----- done -----
	style.Summary(opts.Stdout, "added builder:  %s", builderDest)
	style.Summary(opts.Stdout, "added workload: %s", appsDest)
	return &Result{
		BuilderDir: builderDest,
		AppsDir:    appsDest,
		NextSteps:  fmt.Sprintf("commit and push; Flux will pull and apply within %s", cfg.Flux.IntervalLocal),
		URL:        appURL(opts.Name, cfg.Local.Domain, cfg.Cluster.HttpsPort),
	}, nil
}

// buildValues constructs the value map for the per-app-template files.
// Field names mirror the template placeholders. AppName drives the logical
// identity (resource names, Ingress host, image); Worktree drives the physical
// bindings (the /workspaces mount, the bare-repo URL, the GitRepository URL).
func buildValues(opts Options, cfg *schema.File, worktree, gitAutoSyncRef string) map[string]any {
	return map[string]any{
		"AppName":           opts.Name,
		"Worktree":          worktree,
		"AppsNamespace":     opts.Namespace,
		"ClientName":        cfg.Client.Name,
		"FluxIntervalLocal": cfg.Flux.IntervalLocal,
		// flywheel's namespace is fixed (naming.FlywheelNamespace); the per-app
		// scaffolds (GitRepository, build-config, git-auto-sync) reference it as
		// a placeholder rather than a baked literal (task T14).
		"FlywheelNamespace": naming.FlywheelNamespace,
		"GitServerURL":      naming.GitServerURL(naming.FlywheelNamespace),
		"GitAutoSyncImage":  gitAutoSyncRef,
		"RegistryURL":       fmt.Sprintf("k3d-%s:5000", cfg.Cluster.Registry),
		"Image":             opts.Image,
		"Context":           opts.Context,
		"Dockerfile":        opts.Dockerfile,
		"Target":            opts.Target,
		"LocalDomain":       cfg.Local.Domain,
	}
}

// resolveWorktree turns the <dir> argument into (basename, absPath). The arg may
// be a bare name (a child of workspaces_root), a relative path (vs cwd), or an
// absolute path. It must name an existing directory that is a direct child of
// workspaces_root — the only layout the cluster's single /workspaces bind-mount
// and git-server's basename scan support.
func resolveWorktree(arg, workspacesRoot, cwd string) (name, abs string, err error) {
	root := filepath.Clean(workspacesRoot)
	switch {
	case filepath.IsAbs(arg):
		abs = filepath.Clean(arg)
	case strings.ContainsRune(arg, filepath.Separator):
		abs = filepath.Clean(filepath.Join(cwd, arg))
	default:
		abs = filepath.Join(root, arg)
	}
	info, statErr := os.Stat(abs)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return "", "", fmt.Errorf("nothing at %s; create the project first (a git worktree under %s with at least a Dockerfile) or pass a git URL to clone", abs, root)
		}
		return "", "", statErr
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("worktree %q is not a directory (%s)", arg, abs)
	}
	if filepath.Dir(abs) != root {
		return "", "", fmt.Errorf("worktree %s must be a direct child of workspaces_root (%s)", abs, root)
	}
	return filepath.Base(abs), abs, nil
}

// WorkspaceDirs lists the direct child directories of workspaces_root that are
// git worktrees (have a .git), excluding the gitops repo itself. It drives shell
// completion of the `add app` <dir> argument; callers treat an error as "no
// candidates" so completion degrades silently.
func WorkspaceDirs(repoDir string) ([]string, error) {
	cfg, err := readConfig(repoDir)
	if err != nil {
		return nil, err
	}
	root := cfg.Paths.WorkspacesRoot
	if root == "" {
		root = filepath.Dir(repoDir)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	cleanRepo := filepath.Clean(repoDir)
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if dir == cleanRepo {
			continue // skip the gitops repo itself
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// readConfig parses flywheel.yaml, merged with flywheel.yaml.local if
// present. The merge matters for namespace/interval defaults (filled by the
// shared loader when unset); image overrides no longer affect the committed
// git-auto-sync manifest (it pins the canonical ghcr.io name, rewritten at
// reconcile by the client-builders Kustomization), so a `.local` override
// reaches the per-app sidecar through `up`, not through this read.
func readConfig(repoDir string) (*schema.File, error) {
	return config.Load(repoDir, config.LoadOptions{})
}

// preflightResources verifies path is an appendable kustomization: it exists,
// is readable, and has a top-level `resources:` key. It mirrors appendResource's
// preconditions so the commit tail is unlikely to fail after the repo has been
// mutated. Kept as a separate read-only check (rather than folded into
// appendResource) so T24's yaml-editing consolidation can replace both without a
// merge conflict here.
func preflightResources(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		switch strings.TrimRight(line, " \t") {
		case "resources:", "resources: []":
			return nil
		}
	}
	return fmt.Errorf("%s missing a `resources:` key", path)
}

// appendResource inserts `  - ./<name>` under the `resources:` key of
// the given kustomization.yaml file. The file may ship with
// `resources: []` (rewritten to a block sequence on first add) or an
// existing block sequence (appended to). Idempotent: a re-add of the
// same name is a no-op (already in resources).
func appendResource(path, name string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	entry := "  - ./" + name
	if hasResourceLine(string(raw), entry) {
		return nil
	}

	lines := strings.Split(string(raw), "\n")
	idx := -1
	for i, line := range lines {
		// Look for the resources: key at column 0. (kustomize spec doesn't
		// allow it nested.)
		trim := strings.TrimRight(line, " \t")
		if trim == "resources:" || trim == "resources: []" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("%s missing a `resources:` key", path)
	}

	switch strings.TrimRight(lines[idx], " \t") {
	case "resources: []":
		// First app: rewrite the empty inline list as a block sequence.
		lines[idx] = "resources:"
		lines = insertAfter(lines, idx, entry)
	default:
		// Block sequence already; append after the last `  - ` line that
		// belongs to this key.
		end := lastResourceEntry(lines, idx)
		lines = insertAfter(lines, end, entry)
	}

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// ensureNamespace appends a Namespace document for `ns` to the managed
// apps/base/namespaces.yaml stream if absent, creating the file when missing.
// Idempotent (mirrors appendResource): re-adding the same namespace is a no-op,
// so apps sharing a namespace produce exactly one Namespace object.
func ensureNamespace(path, ns string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if hasNamespaceDoc(string(raw), ns) {
		return nil
	}
	doc := fmt.Sprintf("---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n  labels:\n    kubernetes.io/metadata.name: %s\n", ns, ns)
	content := string(raw)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return os.WriteFile(path, []byte(content+doc), 0o644)
}

// hasNamespaceDoc reports whether the stream already declares a Namespace named
// `ns` (the `  name: <ns>` line; the 4-space-indented metadata.name label won't
// match). Line scan mirrors hasResourceLine.
func hasNamespaceDoc(content, ns string) bool {
	want := "  name: " + ns
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimRight(line, " \t") == want {
			return true
		}
	}
	return false
}

// hasResourceLine returns true if `entry` (e.g. "  - ./foo") already
// appears verbatim in the kustomization file.
func hasResourceLine(content, entry string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimRight(line, " \t") == entry {
			return true
		}
	}
	return false
}

func insertAfter(lines []string, i int, entry string) []string {
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:i+1]...)
	out = append(out, entry)
	out = append(out, lines[i+1:]...)
	return out
}

// lastResourceEntry returns the index of the last `  - ...` line that
// belongs to the resources: key starting at `start`. If none yet, returns
// `start` so the caller inserts immediately after the key line.
func lastResourceEntry(lines []string, start int) int {
	last := start
	for i := start + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "  - ") {
			last = i
			continue
		}
		// Comment or blank within the block — keep scanning.
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		// Hit another top-level key (column 0 non-space, non-comment); stop.
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			break
		}
	}
	return last
}
