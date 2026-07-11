package initcmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	flywheel "github.com/cobr-io/flywheel"
	"github.com/cobr-io/flywheel/internal/cli/age"
	"github.com/cobr-io/flywheel/internal/cli/allocator"
	"github.com/cobr-io/flywheel/internal/cli/converge"
	"github.com/cobr-io/flywheel/internal/cli/dockerports"
	"github.com/cobr-io/flywheel/internal/cli/embedcache"
	"github.com/cobr-io/flywheel/internal/cli/render"
)

// embeddedSkeleton returns the embedded client-skeleton sub-FS — the
// production default for SkeletonFS.
func embeddedSkeleton() fs.FS {
	sub, err := fs.Sub(flywheel.Assets, "templates/client-skeleton")
	if err != nil {
		// This is a programming error (embedded path missing), not a
		// runtime condition — panic so it's caught immediately at any
		// `go test` run.
		panic(fmt.Sprintf("embedded skeleton missing: %v", err))
	}
	return sub
}

// Options is the full input surface to Run. The CLI front-door
// (cmd/flywheel/main.go) constructs production deps; tests inject
// FixedResolver / FixedKeypair / etc.
type Options struct {
	// User-facing inputs (CLI flags / positional).
	Name    string // client name; defaults to basename(TargetDir) if unset
	Org     string // optional GitHub org hint
	Version string // optional --version=vX.Y.Z; empty = flywheel.BuildVersion (the running binary's label)

	// Target dir for the client repo. Two modes:
	//   - TargetDir set: initialise that directory (mkdir if missing).
	//     Refuses any non-empty target (only .git/ allowed).
	//   - TargetDir empty + ParentDir+Name set: legacy sibling mode,
	//     creates <ParentDir>/<Name>-gitops. Retained for tests that
	//     predate the init rename; the CLI front-door always sets
	//     TargetDir.
	TargetDir string
	ParentDir string // legacy: where `<name>-gitops/` is created (back-compat for older tests)

	// Filesystem locations (overridable for tests).
	AllocationsPath string // ~/.config/flywheel/allocations.json; default
	CacheRoot       string // ~/.cache/flywheel; default
	AgeKeyDir       string // ~/.config/flywheel/<client>/; default (writes age.key inside)
	HomeOverride    string // tests inject; if set, allocations + age default to <home>/...

	// Source assets.
	// SkeletonFS is the client-skeleton tree to render. Defaults to the
	// binary's embedded copy (flywheel.Assets sub-FS at
	// templates/client-skeleton). Tests pass an os.DirFS at a fixture dir.
	SkeletonFS        fs.FS
	FlywheelRepoURL   string // default https://github.com/cobr-io/flywheel
	FluxIntervalLocal string // baked into rendered flywheel.yaml; default "10s"
	Domain            string // baked into flywheel.yaml; default localdev.me

	// Injectable deps for determinism.
	Age   age.Generator    // default RealGenerator{}
	Clock func() time.Time // default time.Now
	// BindableProbe decides whether a port is free during allocation. Default
	// (nil) builds the docker-aware probe (dockerports.AvailabilityProbe); tests
	// inject a deterministic stub so allocation doesn't depend on the host's
	// live docker/port state.
	BindableProbe func(int) bool

	// FlywheelSHA is the commit SHA stamped into rendered output (state
	// file, in-cluster GitRepository spec). Empty = derive via
	// embedcache.Populate. Tests inject a fixed value for stable goldens.
	FlywheelSHA string

	// Behaviour switches.
	SkipGitCommit    bool // tests skip the `git add/commit` step
	SkipEmbedExtract bool // tests skip embedcache.Populate; requires FlywheelSHA to be set
}

// Result is the success outcome of Run. Returned for tests + the CLI
// wrapper to print user guidance.
type Result struct {
	RepoDir     string
	FlywheelTag string
	FlywheelSHA string
	Triple      allocator.Triple
	AgeKeyPath  string
	NextSteps   string
	// HooksNote is a one-line advisory the CLI surfaces when the commit
	// hooks could not be auto-activated (e.g. `pre-commit` not on PATH).
	// Empty when hooks were installed cleanly or install was skipped.
	HooksNote string
}

// Run executes the 10-step pipeline from design § flywheel init.
// Name is allowed to be empty here — resolveTargetDir derives it from
// the basename of TargetDir (init mode) or cwd if neither is set.
func Run(opts Options) (*Result, error) {
	if opts.FlywheelRepoURL == "" {
		opts.FlywheelRepoURL = "https://github.com/cobr-io/flywheel"
	}
	if opts.FluxIntervalLocal == "" {
		opts.FluxIntervalLocal = "10s"
	}
	if opts.Domain == "" {
		opts.Domain = "localdev.me"
	}
	if opts.Age == nil {
		opts.Age = age.RealGenerator{}
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.SkeletonFS == nil {
		opts.SkeletonFS = embeddedSkeleton()
	}

	// Step 1 — resolve target dir + ensure it's safe to initialise into.
	repoDir, err := resolveTargetDir(&opts)
	if err != nil {
		return nil, err
	}
	if err := refuseUnsafeTarget(repoDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", repoDir, err)
	}

	// Step 2 — git init (host git is fine; the design allows it).
	if !opts.SkipGitCommit {
		if err := gitCmd(repoDir, "init", "--quiet", "--initial-branch=main"); err != nil {
			return nil, fmt.Errorf("git init: %w", err)
		}
	}

	// Step 3 — allocate ports + record repo path.
	allocPath, err := resolveAllocationsPath(opts)
	if err != nil {
		return nil, err
	}
	alloc, err := allocator.Load(allocPath)
	if err != nil {
		return nil, err
	}
	// Prefer a docker-aware probe so the initial allocation avoids ports docker
	// already publishes (e.g. another k3d cluster). Best-effort: if docker isn't
	// reachable yet, the probe falls back to a host-only check, and `up`'s
	// portheal re-checks against docker before binding anyway. Tests inject a
	// deterministic probe via opts.BindableProbe.
	probe := opts.BindableProbe
	if probe == nil {
		probe, _ = dockerports.AvailabilityProbe(context.Background())
	}
	triple, err := alloc.Allocate(opts.Name, repoDir, probe)
	if err != nil {
		return nil, fmt.Errorf("allocate ports: %w", err)
	}
	if err := alloc.Save(allocPath); err != nil {
		return nil, err
	}

	// Step 4 — pin flywheel.version + derive a deterministic SHA from
	// the binary's embedded asset tree (used in the rendered in-cluster
	// GitRepository spec.ref.commit + .flywheel-state.yaml). No network.
	tag := opts.Version
	if tag == "" {
		tag = flywheel.BuildVersion
	}
	sha := opts.FlywheelSHA
	if sha == "" {
		if opts.SkipEmbedExtract {
			return nil, fmt.Errorf("SkipEmbedExtract requires FlywheelSHA to be set")
		}
		cacheRoot := opts.CacheRoot
		if cacheRoot == "" {
			if opts.HomeOverride != "" {
				cacheRoot = filepath.Join(opts.HomeOverride, ".cache", "flywheel")
			} else {
				cacheRoot, err = embedcache.DefaultRoot()
				if err != nil {
					return nil, err
				}
			}
		}
		// prefix="." extracts both `templates/` and `manifests/` so the
		// cache layout matches `up`'s expectations (filepath.Join(cacheDir,
		// "manifests", "dev-loop", ...)) and `up` can hit its cache-marker
		// fast path without re-extracting.
		_, sha, err = embedcache.Populate(cacheRoot, tag, flywheel.Assets, ".")
		if err != nil {
			return nil, fmt.Errorf("populate embed cache: %w", err)
		}
	}

	// Step 5/6 — load existing age keypair if one already exists for this
	// client (preserves the per-developer identity across destroy/init
	// cycles so any committed *.sops.yaml content stays decryptable);
	// otherwise generate a fresh keypair and persist it to
	// ~/.config/flywheel/<client>/age.key (0600).
	kp, keyPath, err := loadOrGenerateAge(opts)
	if err != nil {
		return nil, fmt.Errorf("age keypair: %w", err)
	}
	values := buildValues(opts, triple, tag, sha, kp.PublicKey)
	if err := render.Tree(opts.SkeletonFS, ".", repoDir, values); err != nil {
		return nil, fmt.Errorf("render skeleton: %w", err)
	}

	// Step 6b — write the committed local age private key. Unlike every other
	// environment, the local cluster's key ships IN the repo: it only ever
	// decrypts clusters/local/* dev secrets on a localhost k3d cluster, so
	// committing it removes the onboarding key-transport problem (a teammate
	// clones and `flywheel up` can decrypt with no key handoff). The host copy
	// written by loadOrGenerateAge stays the identity anchor across
	// destroy/init; this is the canonical copy `up` reads. Mode 0644: git only
	// preserves the +x bit and the key is non-secret by design. .gitignore
	// commits this exact path while ignoring clusters/*/age.key for every other
	// env.
	localKeyDir := filepath.Join(repoDir, "clusters", "local")
	if err := os.MkdirAll(localKeyDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", localKeyDir, err)
	}
	if err := os.WriteFile(filepath.Join(localKeyDir, "age.key"), []byte(kp.PrivateKey+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("write committed local age key: %w", err)
	}

	// Step 7 — write .flywheel-state.yaml (committed). It records only the
	// cluster baseline: init converges at `sha`, so converged_sha starts there.
	state := &converge.State{Cluster: converge.ClusterState{ConvergedSHA: sha}}
	raw, err := state.Marshal()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".flywheel-state.yaml"), raw, 0o644); err != nil {
		return nil, err
	}

	// Step 8 — write flywheel.yaml.local (gitignored) with auto-detected
	// workspaces_root. Default is the *parent of repoDir* (so
	// /Users/dev/src/github.com/cobr-io/ as workspaces with this client
	// at /workspaces/<name>-gitops in-cluster).
	workspacesRoot := filepath.Dir(repoDir)
	localYaml := fmt.Sprintf("paths:\n  workspaces_root: %s\n", workspacesRoot)
	if err := os.WriteFile(filepath.Join(repoDir, "flywheel.yaml.local"), []byte(localYaml), 0o644); err != nil {
		return nil, err
	}

	// Step 9 — git add + commit.
	var hooksNote string
	if !opts.SkipGitCommit {
		if err := gitCmd(repoDir, "add", "-A"); err != nil {
			return nil, fmt.Errorf("git add: %w", err)
		}
		msg := fmt.Sprintf("chore: bootstrap from flywheel %s", tag)
		if err := gitCmd(repoDir, "-c", "user.email=flywheel@dev.local",
			"-c", "user.name=flywheel",
			"commit", "--quiet", "-m", msg); err != nil {
			return nil, fmt.Errorf("git commit: %w", err)
		}

		// Step 9b — activate the scaffolded commit hooks. Best-effort:
		// `pre-commit install` wires .git/hooks/ when the tool is present;
		// if it's missing we never fail init — just hand back a tip the CLI
		// surfaces so the developer can enable them later.
		hooksNote = installHooks(repoDir)
	}

	// Step 10 — return next steps.
	return &Result{
		RepoDir:     repoDir,
		FlywheelTag: tag,
		FlywheelSHA: sha,
		Triple:      triple,
		AgeKeyPath:  keyPath,
		NextSteps:   nextStepsTip(repoDir),
		HooksNote:   hooksNote,
	}, nil
}

// installHooks activates the scaffolded pre-commit hooks in repoDir.
// Returns "" when hooks were wired (or there's nothing to do), or a
// one-line tip when the `pre-commit` tool is unavailable so the caller
// can guide the developer. Never returns an error: missing dev tooling
// must not fail scaffolding.
func installHooks(repoDir string) string {
	if _, err := exec.LookPath("pre-commit"); err != nil {
		return "commit hooks not activated: `pre-commit` not on PATH. " +
			"Install it (https://pre-commit.com), then run `pre-commit install` in the repo."
	}
	if err := gitCmd(repoDir, "rev-parse", "--git-dir"); err != nil {
		return "" // no git repo to install into (shouldn't happen post-commit)
	}
	cmd := exec.Command("pre-commit", "install", "--install-hooks")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Sprintf("commit hooks not activated: `pre-commit install` failed (%v). "+
			"Re-run it in the repo. Output: %s", err, strings.TrimSpace(string(out)))
	}
	return ""
}

// resolveTargetDir picks the directory `init` will work in, and derives
// the client Name from its basename if unset. Three modes:
//   - TargetDir set: use it (mkdir below if missing).
//   - TargetDir empty + ParentDir+Name set: legacy sibling mode for
//     pre-rename tests — repoDir = <ParentDir>/<Name>-gitops.
//   - TargetDir empty + ParentDir empty: in-place at cwd.
//
// nextStepsTip suggests the next command for the user. If they ran
// `flywheel init` in place (cwd == repoDir), they don't need to cd —
// suggest just `flywheel up`.
func nextStepsTip(repoDir string) string {
	if wd, err := os.Getwd(); err == nil {
		if abs, err := filepath.Abs(wd); err == nil && abs == repoDir {
			return "flywheel up"
		}
	}
	return fmt.Sprintf("cd %s && flywheel up", filepath.Base(repoDir))
}

func resolveTargetDir(opts *Options) (string, error) {
	switch {
	case opts.TargetDir != "":
		abs, err := filepath.Abs(opts.TargetDir)
		if err != nil {
			return "", err
		}
		if opts.Name == "" {
			opts.Name = filepath.Base(abs)
		}
		return abs, nil
	case opts.ParentDir != "" && opts.Name != "":
		// Legacy sibling mode (back-compat for the original `flywheel new` tests).
		return filepath.Join(opts.ParentDir, opts.Name+"-gitops"), nil
	default:
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		if opts.Name == "" {
			opts.Name = filepath.Base(wd)
		}
		return wd, nil
	}
}

// refuseUnsafeTarget rejects targets that contain anything other than
// `.git/` — `init` is opt-in scaffolding, not a merge.
func refuseUnsafeTarget(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil // mkdir below
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		return fmt.Errorf("%s is not empty (contains %s) — refuse to init over existing content", dir, e.Name())
	}
	return nil
}

func resolveAllocationsPath(opts Options) (string, error) {
	if opts.AllocationsPath != "" {
		return opts.AllocationsPath, nil
	}
	if opts.HomeOverride != "" {
		return filepath.Join(opts.HomeOverride, ".config", "flywheel", "allocations.json"), nil
	}
	return allocator.DefaultPath()
}

// hostAgeKeyPath returns the on-disk path for this client's age key,
// honouring HomeOverride (tests). Production uses age.HostKeyPath
// which respects $HOME.
func hostAgeKeyPath(opts Options) (string, error) {
	if opts.HomeOverride != "" {
		return filepath.Join(opts.HomeOverride, ".config", "flywheel", opts.Name, "age.key"), nil
	}
	return age.HostKeyPath(opts.Name)
}

// loadOrGenerateAge returns the keypair to scaffold this client with:
// an existing on-disk key if present (preserving per-developer
// identity across destroy/init), otherwise a freshly generated one
// (also written to disk). Returns the path either way so the CLI can
// print it for the operator.
func loadOrGenerateAge(opts Options) (age.Keypair, string, error) {
	keyPath, err := hostAgeKeyPath(opts)
	if err != nil {
		return age.Keypair{}, "", err
	}
	kp, err := age.LoadKeypairFromPath(keyPath)
	if err == nil {
		return kp, keyPath, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return age.Keypair{}, keyPath, fmt.Errorf("load existing age key %s: %w", keyPath, err)
	}
	kp, err = opts.Age.Generate()
	if err != nil {
		return age.Keypair{}, keyPath, fmt.Errorf("generate age keypair: %w", err)
	}
	if _, err := age.WritePrivateKeyAt(keyPath, kp.PrivateKey); err != nil {
		return age.Keypair{}, keyPath, fmt.Errorf("write age key: %w", err)
	}
	return kp, keyPath, nil
}

// buildValues constructs the map passed to the Go-template renderer.
// Field names mirror the template placeholders in
// templates/client-skeleton/*.tmpl and manifests/per-app-template/*.tmpl.
func buildValues(opts Options, triple allocator.Triple, tag, sha, agePub string) map[string]any {
	// RepoBaseName is the actual filesystem basename of the client repo
	// — used wherever a path like /workspaces/<basename> or the bare repo
	// URL <basename>.git needs to match what git-auto-sync-self mounts.
	// Distinct from ClientName: legacy `flywheel new acme` produced repo dir
	// `acme-gitops` but ClientName=acme. `init` lets these differ
	// freely, so the templates pull from RepoBaseName here.
	repoBaseName := filepath.Base(triple.RepoPath)
	return map[string]any{
		"ClientName":        opts.Name,
		"RepoBaseName":      repoBaseName,
		"Org":               opts.Org,
		"Domain":            opts.Domain,
		"ClusterName":       opts.Name + "-local",
		"Registry":          opts.Name + "-local-registry",
		"RegistryPort":      triple.RegistryPort,
		"HttpPort":          triple.HttpPort,
		"HttpsPort":         triple.HttpsPort,
		"FlywheelVersion":   tag,
		"FlywheelSHA":       sha,
		"FlywheelRepoURL":   opts.FlywheelRepoURL,
		"FluxIntervalLocal": opts.FluxIntervalLocal,
		"AgePublicKey":      agePub,
	}
}

func gitCmd(cwd string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %v\n%s", args, err, out)
	}
	return nil
}
