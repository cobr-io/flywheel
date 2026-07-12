package initcmd

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	flywheel "github.com/cobr-io/flywheel"
	"github.com/cobr-io/flywheel/internal/cli/age"
	"github.com/cobr-io/flywheel/internal/cli/converge"
)

// TestGitCmd_ErrorUnwrapsToExitError proves gitCmd's error preserves the
// errors.Is/As chain through init's `git <step>: %w` wrappers. The old
// `%v: %v` wrap flattened the underlying *exec.ExitError to a string and would
// fail this test; execx wraps with %w so it survives.
func TestGitCmd_ErrorUnwrapsToExitError(t *testing.T) {
	// A non-repo dir makes `git rev-parse --git-dir` exit non-zero.
	err := gitCmd(t.TempDir(), "rev-parse", "--git-dir")
	if err == nil {
		t.Fatal("expected git to fail in a non-repo directory")
	}
	// Wrap it exactly as init.Run does on its git steps.
	wrapped := fmt.Errorf("git init: %w", err)
	var ee *exec.ExitError
	if !errors.As(wrapped, &ee) {
		t.Fatalf("errors.As could not reach *exec.ExitError through %q", wrapped)
	}
	if !errors.Is(wrapped, err) {
		t.Fatalf("errors.Is(wrapped, err) = false; the %%w chain is broken")
	}
}

// T1.1 — `flywheel init` golden-file test: generated tree
// matches checked-in golden for a representative (name, org) pair,
// exercising the --version=vX.Y.Z flag-pinned path.
//
// Run `go test ./internal/cli/initcmd/... -update` to (re)bless goldens
// after changing templates.

var updateGoldens = flag.Bool("update", false, "write goldens instead of comparing")

const (
	fixedSHA     = "0123456789abcdef0123456789abcdef01234567"
	fixedAgePub  = "age1qx5vhc3z8q-FIXED-FOR-TESTS"
	fixedAgePriv = "AGE-SECRET-KEY-FIXED-FOR-TESTS"
)

func skeletonDir(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	root := wd
	for {
		if _, err := os.Stat(filepath.Join(root, "templates", "client-skeleton")); err == nil {
			return filepath.Join(root, "templates", "client-skeleton")
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("could not locate templates/client-skeleton")
		}
		root = parent
	}
}

func runInitForGolden(t *testing.T, name, version string) string {
	t.Helper()
	parent := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	opts := Options{
		Name:             name,
		Org:              "cobr-io",
		Version:          version,
		ParentDir:        parent,
		HomeOverride:     home,
		SkeletonFS:       os.DirFS(skeletonDir(t)),
		FlywheelRepoURL:  "https://example.invalid/flywheel",
		Age:              age.FixedKeypair(age.Keypair{PublicKey: fixedAgePub, PrivateKey: fixedAgePriv}),
		FlywheelSHA:      fixedSHA,
		SkipEmbedExtract: true,
		SkipGitCommit:    true,                           // goldens don't include .git/
		BindableProbe:    func(int) bool { return true }, // deterministic: ignore host docker/port state
	}
	if _, err := Run(opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return filepath.Join(parent, name+"-gitops")
}

func TestInit_Golden(t *testing.T) {
	// Exercises the --version=X.Y.Z flag-pinned path (skips the LatestTag
	// resolver) and the full scaffold tree.
	out := runInitForGolden(t, "acme", "v0.1.0")
	compareWithGolden(t, out, "default")
}

func TestInit_RefusesNonEmptyTarget(t *testing.T) {
	// init semantics: an empty target dir (or one with only .git/) is
	// fine; any other content makes Run refuse.
	parent := t.TempDir()
	target := filepath.Join(parent, "acme-gitops")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// Drop a stray file so the target is non-empty.
	if err := os.WriteFile(filepath.Join(target, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Name:             "acme",
		ParentDir:        parent,
		HomeOverride:     t.TempDir(),
		SkeletonFS:       os.DirFS(skeletonDir(t)),
		Age:              age.FixedKeypair(age.Keypair{PublicKey: fixedAgePub, PrivateKey: fixedAgePriv}),
		FlywheelSHA:      fixedSHA,
		SkipEmbedExtract: true,
		SkipGitCommit:    true,
		BindableProbe:    func(int) bool { return true }, // deterministic: ignore host docker/port state
	}
	if _, err := Run(opts); err == nil {
		t.Fatal("Run should refuse to init into a non-empty dir")
	}
}

func TestInit_InitInPlace_TargetDirEmpty(t *testing.T) {
	// init semantics: TargetDir empty target is fine, Name derived from
	// basename if unset.
	parent := t.TempDir()
	target := filepath.Join(parent, "myproject-gitops")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		// No Name set → derived from TargetDir basename.
		TargetDir:        target,
		HomeOverride:     t.TempDir(),
		SkeletonFS:       os.DirFS(skeletonDir(t)),
		Age:              age.FixedKeypair(age.Keypair{PublicKey: fixedAgePub, PrivateKey: fixedAgePriv}),
		FlywheelSHA:      fixedSHA,
		SkipEmbedExtract: true,
		SkipGitCommit:    true,
		BindableProbe:    func(int) bool { return true }, // deterministic: ignore host docker/port state
	}
	res, err := Run(opts)
	if err != nil {
		t.Fatalf("init in-place into empty dir: %v", err)
	}
	if res.RepoDir != target {
		t.Errorf("RepoDir=%q want %q", res.RepoDir, target)
	}
	// Name was derived from basename.
	if _, err := os.Stat(filepath.Join(target, "flywheel.yaml")); err != nil {
		t.Errorf("flywheel.yaml not rendered: %v", err)
	}
}

func TestInit_ReadmeTitle_NoDoubledGitopsSuffix(t *testing.T) {
	// Regression for #38: when the repo dir / client name already ends in
	// `-gitops`, the README must render the name verbatim — the title and
	// the quick-start `cd` path are the actual repo basename, never
	// `<name>-gitops` (which would double the suffix to `acme-gitops-gitops`).
	parent := t.TempDir()
	target := filepath.Join(parent, "acme-gitops")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		// No Name set → derived from TargetDir basename = "acme-gitops".
		TargetDir:        target,
		HomeOverride:     t.TempDir(),
		SkeletonFS:       os.DirFS(skeletonDir(t)),
		Age:              age.FixedKeypair(age.Keypair{PublicKey: fixedAgePub, PrivateKey: fixedAgePriv}),
		FlywheelSHA:      fixedSHA,
		SkipEmbedExtract: true,
		SkipGitCommit:    true,
		BindableProbe:    func(int) bool { return true }, // deterministic: ignore host docker/port state
	}
	if _, err := Run(opts); err != nil {
		t.Fatalf("init in-place into acme-gitops: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(target, "README.md"))
	if err != nil {
		t.Fatalf("read rendered README: %v", err)
	}
	readme := string(b)
	if strings.Contains(readme, "acme-gitops-gitops") {
		t.Errorf("README doubled the -gitops suffix:\n%s", readme)
	}
	if !strings.Contains(readme, "# acme-gitops\n") {
		t.Errorf("README title is not `# acme-gitops`:\n%s", readme)
	}
	if !strings.Contains(readme, "cd acme-gitops\n") {
		t.Errorf("README quick-start `cd` path is not `cd acme-gitops`:\n%s", readme)
	}
}

func TestInit_InitInPlace_OnlyGitOK(t *testing.T) {
	// .git/-only targets are OK (matches `git init` then `flywheel init`).
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		TargetDir:        target,
		HomeOverride:     t.TempDir(),
		SkeletonFS:       os.DirFS(skeletonDir(t)),
		Age:              age.FixedKeypair(age.Keypair{PublicKey: fixedAgePub, PrivateKey: fixedAgePriv}),
		FlywheelSHA:      fixedSHA,
		SkipEmbedExtract: true,
		SkipGitCommit:    true,
		BindableProbe:    func(int) bool { return true }, // deterministic: ignore host docker/port state
	}
	if _, err := Run(opts); err != nil {
		t.Fatalf("init with only .git/ present should succeed: %v", err)
	}
}

func TestInit_DefaultsToBuildVersion(t *testing.T) {
	// No --version provided ⇒ tag defaults to flywheel.BuildVersion (the
	// binary's embedded label). No network resolution.
	parent := t.TempDir()
	home := t.TempDir()
	opts := Options{
		Name:             "acme",
		ParentDir:        parent,
		HomeOverride:     home,
		SkeletonFS:       os.DirFS(skeletonDir(t)),
		Age:              age.FixedKeypair(age.Keypair{PublicKey: fixedAgePub, PrivateKey: fixedAgePriv}),
		FlywheelSHA:      fixedSHA,
		SkipEmbedExtract: true,
		SkipGitCommit:    true,
		BindableProbe:    func(int) bool { return true }, // deterministic: ignore host docker/port state
	}
	res, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.FlywheelTag != "v0.0.0-dev" {
		t.Errorf("FlywheelTag = %q, want v0.0.0-dev (BuildVersion default)", res.FlywheelTag)
	}
	if res.FlywheelSHA != fixedSHA {
		t.Errorf("FlywheelSHA = %q, want %q", res.FlywheelSHA, fixedSHA)
	}
}

func TestInit_DefaultsToReleaseTagFromBuildVersion(t *testing.T) {
	// Regression for #39: a release-built binary stamps a v-prefixed release
	// tag into flywheel.BuildVersion (via the {{.Tag}} ldflags, PR #30). When
	// no --version is given, `init` must pin that release tag — NOT a bare git
	// build id — both in the Result and in the rendered flywheel.yaml. This
	// guards the "default: latest release tag" promise so it's enforced by CI
	// rather than a manual post-tag check.
	//
	// Mutates the package-global flywheel.BuildVersion, so this test must NOT
	// run in parallel and must restore the original value on exit.
	const releaseTag = "v0.1.0"
	orig := flywheel.BuildVersion
	flywheel.BuildVersion = releaseTag
	defer func() { flywheel.BuildVersion = orig }()

	parent := t.TempDir()
	home := t.TempDir()
	opts := Options{
		Name:             "acme",
		ParentDir:        parent,
		HomeOverride:     home,
		SkeletonFS:       os.DirFS(skeletonDir(t)),
		Age:              age.FixedKeypair(age.Keypair{PublicKey: fixedAgePub, PrivateKey: fixedAgePriv}),
		FlywheelSHA:      fixedSHA,
		SkipEmbedExtract: true,
		SkipGitCommit:    true,
	}
	res, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.FlywheelTag != releaseTag {
		t.Errorf("FlywheelTag = %q, want %q (release tag from BuildVersion)", res.FlywheelTag, releaseTag)
	}

	// The pin must also reach the rendered flywheel.yaml the client commits.
	b, err := os.ReadFile(filepath.Join(res.RepoDir, "flywheel.yaml"))
	if err != nil {
		t.Fatalf("read rendered flywheel.yaml: %v", err)
	}
	if !strings.Contains(string(b), "version: "+releaseTag) {
		t.Errorf("flywheel.yaml does not pin %q:\n%s", "version: "+releaseTag, b)
	}
}

func TestInit_ReusesExistingAgeKey(t *testing.T) {
	// Regression: after `flywheel destroy` (which leaves the age key on
	// disk), re-running `flywheel init` must reuse the existing key
	// rather than refusing — otherwise any committed *.sops.yaml in the
	// repo becomes undecryptable after each cluster recreation.
	parent := t.TempDir()
	home := t.TempDir()
	// Pre-seed an existing key for "acme" at the expected path.
	keyDir := filepath.Join(home, ".config", "flywheel", "acme")
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	existing, err := age.RealGenerator{}.Generate()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(keyDir, "age.key")
	if err := os.WriteFile(keyPath, []byte(existing.PrivateKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Use a Generator that returns a DIFFERENT key — if init incorrectly
	// generates a fresh keypair instead of reading the existing file,
	// the rendered manifests will pick up THIS key and the assertion
	// below will fail.
	bogus := age.FixedKeypair(age.Keypair{
		PublicKey:  "age1-BOGUS-WOULD-INDICATE-INIT-IGNORED-EXISTING-KEY",
		PrivateKey: "AGE-SECRET-KEY-BOGUS",
	})

	res, err := Run(Options{
		Name:             "acme",
		ParentDir:        parent,
		HomeOverride:     home,
		SkeletonFS:       os.DirFS(skeletonDir(t)),
		Age:              bogus,
		FlywheelSHA:      fixedSHA,
		SkipEmbedExtract: true,
		SkipGitCommit:    true,
		BindableProbe:    func(int) bool { return true }, // deterministic: ignore host docker/port state
	})
	if err != nil {
		t.Fatalf("Run with pre-existing key: %v", err)
	}

	// Rendered .sops.yaml should carry the EXISTING public key, not the
	// bogus one.
	sopsRaw, err := os.ReadFile(filepath.Join(res.RepoDir, ".sops.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sopsRaw), existing.PublicKey) {
		t.Errorf(".sops.yaml didn't pick up existing public key %q:\n%s",
			existing.PublicKey, sopsRaw)
	}
	if strings.Contains(string(sopsRaw), "BOGUS") {
		t.Errorf(".sops.yaml used Generator's bogus key (init regenerated instead of loading):\n%s", sopsRaw)
	}

	// The on-disk key file should be byte-identical to what we seeded.
	got, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != existing.PrivateKey+"\n" {
		t.Errorf("on-disk key file was modified by init")
	}
}

func TestInit_AgeKeyStableAcrossTwoInits(t *testing.T) {
	// Simulates the lived flow: `flywheel init` → `flywheel destroy` →
	// `flywheel init` again. The rendered public key must be identical
	// across the two inits — otherwise destroy + re-init silently
	// orphans any committed *.sops.yaml in the gitops repo.
	//
	// Uses age.RealGenerator (not a fixed keypair) so that a regression
	// where init incorrectly regenerates would produce a different key
	// the second time, and the assertion below would catch it. Destroy
	// is simulated by handing the second Run a fresh allocations.json
	// + a fresh ParentDir (matches what `flywheel destroy --yes` clears
	// + the operator scaffolding into a fresh dir); HomeOverride stays
	// the same so the on-disk age.key persists across the two calls.
	home := t.TempDir()

	base := Options{
		Name:             "acme",
		HomeOverride:     home,
		SkeletonFS:       os.DirFS(skeletonDir(t)),
		Age:              age.RealGenerator{},
		FlywheelSHA:      fixedSHA,
		SkipEmbedExtract: true,
		SkipGitCommit:    true,
		BindableProbe:    func(int) bool { return true }, // deterministic: ignore host docker/port state
	}

	first := base
	first.ParentDir = t.TempDir()
	first.AllocationsPath = filepath.Join(t.TempDir(), "alloc1.json")
	res1, err := Run(first)
	if err != nil {
		t.Fatalf("first init: %v", err)
	}
	pub1 := readAgePublicKey(t, res1.RepoDir)

	second := base
	second.ParentDir = t.TempDir()
	second.AllocationsPath = filepath.Join(t.TempDir(), "alloc2.json")
	res2, err := Run(second)
	if err != nil {
		t.Fatalf("second init (after simulated destroy): %v", err)
	}
	pub2 := readAgePublicKey(t, res2.RepoDir)

	if pub1 != pub2 {
		t.Errorf("age public key changed across two inits:\n  first:  %s\n  second: %s",
			pub1, pub2)
	}
}

// readAgePublicKey extracts the rendered age recipient from .sops.yaml (the
// value init wrote from the values map used to render the SOPS creation rules).
func readAgePublicKey(t *testing.T, repoDir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoDir, ".sops.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line := strings.TrimSpace(line); strings.HasPrefix(line, "age:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "age:"))
		}
	}
	t.Fatalf(".sops.yaml has no age recipient")
	return ""
}

func TestInit_AgeKeyWrittenWithMode0600(t *testing.T) {
	parent := t.TempDir()
	home := t.TempDir()
	opts := Options{
		Name:             "acme",
		ParentDir:        parent,
		HomeOverride:     home,
		SkeletonFS:       os.DirFS(skeletonDir(t)),
		Age:              age.FixedKeypair(age.Keypair{PublicKey: fixedAgePub, PrivateKey: fixedAgePriv}),
		FlywheelSHA:      fixedSHA,
		SkipEmbedExtract: true,
		SkipGitCommit:    true,
		BindableProbe:    func(int) bool { return true }, // deterministic: ignore host docker/port state
	}
	res, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(res.AgeKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("age key mode = %o, want 0600", info.Mode().Perm())
	}
	if !strings.HasPrefix(res.AgeKeyPath, home) {
		t.Errorf("age key path %q should be under HomeOverride %q", res.AgeKeyPath, home)
	}
}

func TestInit_PortsRecorded(t *testing.T) {
	parent := t.TempDir()
	home := t.TempDir()
	opts := Options{
		Name:             "acme",
		ParentDir:        parent,
		HomeOverride:     home,
		SkeletonFS:       os.DirFS(skeletonDir(t)),
		Age:              age.FixedKeypair(age.Keypair{PublicKey: fixedAgePub, PrivateKey: fixedAgePriv}),
		FlywheelSHA:      fixedSHA,
		SkipEmbedExtract: true,
		SkipGitCommit:    true,
		BindableProbe:    func(int) bool { return true }, // deterministic: ignore host docker/port state
	}
	res, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Triple.RegistryPort != 50001 {
		t.Errorf("registry_port = %d, want 50001", res.Triple.RegistryPort)
	}
	if res.Triple.RepoPath != filepath.Join(parent, "acme-gitops") {
		t.Errorf("repo_path = %q, want %q", res.Triple.RepoPath, filepath.Join(parent, "acme-gitops"))
	}
}

func TestInit_StateFileRecordsClusterBaseline(t *testing.T) {
	parent := t.TempDir()
	home := t.TempDir()
	opts := Options{
		Name:             "acme",
		Org:              "cobr-io",
		Version:          "v0.1.0",
		ParentDir:        parent,
		HomeOverride:     home,
		SkeletonFS:       os.DirFS(skeletonDir(t)),
		Age:              age.FixedKeypair(age.Keypair{PublicKey: fixedAgePub, PrivateKey: fixedAgePriv}),
		FlywheelSHA:      fixedSHA,
		SkipEmbedExtract: true,
		SkipGitCommit:    true,
		BindableProbe:    func(int) bool { return true }, // deterministic: ignore host docker/port state
	}
	res, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(res.RepoDir, ".flywheel-state.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	state, err := converge.ParseState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if state.Cluster.ConvergedSHA != fixedSHA {
		t.Errorf("state.cluster.converged_sha = %q, want %q", state.Cluster.ConvergedSHA, fixedSHA)
	}
}

// compareWithGolden walks the rendered tree and compares each file to
// the equivalent file under testdata/golden/<flavour>/. The .git/
// directory is excluded (SkipGitCommit=true means it doesn't exist).
//
// With -update, writes the rendered files into testdata/golden/<flavour>/
// instead of comparing.
func compareWithGolden(t *testing.T, renderedRoot, flavour string) {
	t.Helper()
	goldenRoot := filepath.Join("testdata", "golden", flavour)

	if *updateGoldens {
		_ = os.RemoveAll(goldenRoot)
		if err := os.MkdirAll(goldenRoot, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Walk rendered.
	var renderedFiles []string
	_ = filepath.WalkDir(renderedRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(renderedRoot, p)
		if rel == "flywheel.yaml.local" {
			return nil // host-specific; absolute path differs each run
		}
		renderedFiles = append(renderedFiles, rel)
		return nil
	})

	if *updateGoldens {
		for _, rel := range renderedFiles {
			raw, err := os.ReadFile(filepath.Join(renderedRoot, rel))
			if err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(goldenRoot, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// Also commit the .flywheel-state.yaml since the test relies on
		// stable absolute paths for repoPath in the state. Strip the
		// non-deterministic absolute parent dir.
		stripStatePathsForGolden(t, filepath.Join(goldenRoot, ".flywheel-state.yaml"), renderedRoot)
		t.Logf("wrote %d golden files under %s", len(renderedFiles), goldenRoot)
		return
	}

	// Compare every rendered file to the golden.
	for _, rel := range renderedFiles {
		got, err := os.ReadFile(filepath.Join(renderedRoot, rel))
		if err != nil {
			t.Errorf("%s: read rendered: %v", rel, err)
			continue
		}
		want, err := os.ReadFile(filepath.Join(goldenRoot, rel))
		if err != nil {
			t.Errorf("%s: golden missing or unreadable: %v (re-run with -update?)", rel, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s: rendered != golden\n--- got ---\n%s\n--- want ---\n%s", rel, got, want)
		}
	}

	// Detect *removed* files (golden has them; rendered doesn't).
	_ = filepath.WalkDir(goldenRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(goldenRoot, p)
		if _, err := os.Stat(filepath.Join(renderedRoot, rel)); os.IsNotExist(err) {
			t.Errorf("%s: present in golden but not rendered", rel)
		}
		return nil
	})
}

// stripStatePathsForGolden is a no-op: .flywheel-state.yaml today holds
// only `cluster.converged_sha`, which is already deterministic (tests
// inject a fixed SHA), so there's no host-specific path left to scrub.
// Kept as a named call site so a future field that reintroduces
// non-determinism (e.g. an absolute path) has an obvious place to add
// real scrubbing.
func stripStatePathsForGolden(_ *testing.T, _, _ string) {}

// Sanity: ensures we're testing against an in-repo template directory,
// not somewhere weird (helps when running from `go test ./...` vs. the
// directory directly).
func init() {
	_ = exec.Command
}
