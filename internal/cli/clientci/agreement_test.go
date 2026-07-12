package clientci

// This file proves that check-local-only.sh (the frozen bash guard shipped into
// client repos) and internal/cli/sourcemode (the Go owner of the same join +
// integration-branch rule) reach the SAME verdict on a shared fixture corpus.
// The script can't be updated once shipped, so this is where drift between the
// two is caught — in flywheel's own CI, not on a client's machine.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cobr-io/flywheel/internal/cli/config"
	"github.com/cobr-io/flywheel/internal/cli/sourcemode"
)

// goBlocks computes the Go side's verdict for the repo at dir on the effective
// target branch (baseRef when set, else the fixture branch), mirroring the
// script's target resolution.
func goBlocks(t *testing.T, dir, branch, baseRef string) bool {
	t.Helper()
	cfg, err := config.Load(dir, config.LoadOptions{})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	localOnly, err := sourcemode.LocalOnlyApps(dir, cfg)
	if err != nil {
		t.Fatalf("LocalOnlyApps: %v", err)
	}
	target := baseRef
	if target == "" {
		target = branch
	}
	return sourcemode.Guard(localOnly, target, cfg.IntegrationBranch()) == sourcemode.Block
}

// TestGoBashAgree runs check-local-only.sh and sourcemode.Guard over the same
// fixtures and asserts they block/allow identically. The last case documents a
// shared blind spot (see below).
func TestGoBashAgree(t *testing.T) {
	requireTools(t)

	cases := []struct {
		name              string
		branch            string
		integrationBranch string
		app               string
		source            string
		baseRef           string
	}{
		{"local-only on integration branch", "main", "", "web", "local-only", ""},
		{"local-only on feature branch", "feature-x", "", "web", "local-only", ""},
		{"local-only via base-ref", "feature-x", "", "web", "local-only", "main"},
		{"remote-backed on integration branch", "main", "", "web", "https://example.com/acme/web.git", ""},
		{"legacy/undeclared app on integration branch", "main", "", "web", "", ""},
		{"configured integration branch blocks", "develop", "develop", "web", "local-only", ""},
		{"configured integration branch, off it", "main", "develop", "web", "local-only", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := fakeRepo(t, c.branch, c.integrationBranch, c.app, c.source)
			bashBlocked := run(t, dir, c.baseRef) != 0
			goBlocked := goBlocks(t, dir, c.branch, c.baseRef)
			if bashBlocked != goBlocked {
				t.Errorf("disagreement: bash blocked=%v, go blocked=%v", bashBlocked, goBlocked)
			}
		})
	}
}

// TestGoBashAgree_HandRenamedManifestBlindSpot documents a KNOWN, SHARED blind
// spot. Both guards derive an app's worktree solely from its GitRepository
// spec.url basename, then look that worktree up in the workspace block. If a
// developer hand-edits spec.url (or renames the worktree) so its basename no
// longer matches the local_only workspace entry, the app's true local-only
// source becomes invisible to BOTH guards — they agree, and they agree on
// *allowing* it. Neither can see through a hand-renamed manifest; the agreement
// test locks in that they fail identically rather than one silently diverging.
func TestGoBashAgree_HandRenamedManifestBlindSpot(t *testing.T) {
	requireTools(t)
	dir := renamedRepo(t)

	// The workspace declares worktree "web" local_only, but the manifest points
	// at "web-renamed" — so both guards see no local-only app and allow the
	// commit on the integration branch.
	if run(t, dir, "") != 0 {
		t.Fatal("bash: expected the hand-renamed manifest to slip past the guard (exit 0)")
	}
	if goBlocks(t, dir, "main", "") {
		t.Fatal("go: expected the hand-renamed manifest to slip past the guard (Allow)")
	}
}

// renamedRepo builds a client repo on `main` whose workspace block flags
// worktree "web" local_only, but whose app manifest's spec.url basename is
// "web-renamed" (a worktree with no workspace entry).
func renamedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	fy := "schema: v1alpha1\nworkspace:\n  repos:\n    - name: web\n      local_only: true\n"
	if err := os.WriteFile(filepath.Join(dir, "flywheel.yaml"), []byte(fy), 0o644); err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Join(dir, "builders", "base", "web")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gr := "apiVersion: source.toolkit.fluxcd.io/v1\nkind: GitRepository\n" +
		"metadata:\n  name: web\n  namespace: flywheel-system\n" +
		"spec:\n  url: http://git-server/web-renamed.git\n"
	if err := os.WriteFile(filepath.Join(appDir, "gitrepository.yaml"), []byte(gr), 0o644); err != nil {
		t.Fatal(err)
	}

	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "t@example.com")
	git(t, dir, "config", "user.name", "t")
	git(t, dir, "checkout", "-q", "-b", "main")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "init")
	return dir
}
