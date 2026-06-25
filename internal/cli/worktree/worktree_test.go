package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo creates an empty git repo in a temp dir and returns its path.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init")
	return dir
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestGitRemoteURL_OriginPresent(t *testing.T) {
	dir := initRepo(t)
	run(t, dir, "remote", "add", "origin", "https://example.com/acme/web.git")

	url, ok := GitRemoteURL(dir)
	if !ok {
		t.Fatalf("expected ok=true for a worktree with an origin")
	}
	if want := "https://example.com/acme/web.git"; url != want {
		t.Fatalf("GitRemoteURL = %q, want %q", url, want)
	}
}

func TestGitRemoteURL_NoOrigin(t *testing.T) {
	dir := initRepo(t) // no remotes
	if url, ok := GitRemoteURL(dir); ok {
		t.Fatalf("expected ok=false for a worktree with no origin, got %q", url)
	}
}

func TestGitRemoteURL_NotAGitRepo(t *testing.T) {
	if url, ok := GitRemoteURL(t.TempDir()); ok {
		t.Fatalf("expected ok=false for a non-git dir, got %q", url)
	}
}

// A worktree with several remotes must resolve to origin specifically.
func TestGitRemoteURL_MultipleRemotesOriginWins(t *testing.T) {
	dir := initRepo(t)
	run(t, dir, "remote", "add", "upstream", "https://example.com/upstream/web.git")
	run(t, dir, "remote", "add", "origin", "https://example.com/fork/web.git")

	url, ok := GitRemoteURL(dir)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if want := "https://example.com/fork/web.git"; url != want {
		t.Fatalf("GitRemoteURL = %q, want origin's URL %q", url, want)
	}
}

func TestLooksLikeGitURL(t *testing.T) {
	cases := []struct {
		arg  string
		want bool
	}{
		{"https://github.com/acme/web.git", true},
		{"http://example.com/x.git", true},
		{"ssh://git@host/acme/web.git", true},
		{"git://host/acme/web.git", true},
		{"file:///tmp/acme/web.git", true},
		{"git@github.com:acme/web.git", true},
		// not URLs:
		{"web", false},
		{"my-app", false},
		{"./relative/path", false},
		{"/abs/path", false},
		{"../up", false},
	}
	for _, c := range cases {
		if got := LooksLikeGitURL(c.arg); got != c.want {
			t.Errorf("LooksLikeGitURL(%q) = %v, want %v", c.arg, got, c.want)
		}
	}
}

func TestRepoNameFromURL(t *testing.T) {
	cases := map[string]string{
		"https://example.com/acme/web.git": "web",
		"https://example.com/acme/web":     "web",
		"git@github.com:acme/web.git":      "web",
		"ssh://git@host/acme/web.git":      "web",
		"file:///tmp/acme/hello":           "hello",
	}
	for url, want := range cases {
		if got := RepoNameFromURL(url); got != want {
			t.Errorf("RepoNameFromURL(%q) = %q, want %q", url, got, want)
		}
	}
}

// writeApp writes builders/base/<name>/gitrepository.yaml under repoDir whose
// spec.url encodes worktreeName and whose source annotation is `source`
// (omitted when empty → legacy app).
func writeApp(t *testing.T, repoDir, name, worktreeName string) {
	t.Helper()
	dir := filepath.Join(repoDir, "builders", "base", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gr := "kind: GitRepository\nmetadata:\n  name: " + name + "\n" +
		"spec:\n  url: http://git-server/" + worktreeName + ".git\n"
	if err := os.WriteFile(filepath.Join(dir, "gitrepository.yaml"), []byte(gr), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseAppGitRepository(t *testing.T) {
	repo := t.TempDir()
	writeApp(t, repo, "web", "web-dir")
	app, err := ParseAppGitRepository(filepath.Join(repo, "builders", "base", "web", "gitrepository.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if app.Name != "web" || app.Worktree != "web-dir" {
		t.Fatalf("ParseAppGitRepository = %+v", app)
	}
}

func TestDeclaredApps(t *testing.T) {
	repo := t.TempDir()
	writeApp(t, repo, "web", "web-dir")
	writeApp(t, repo, "api", "api")
	apps, err := DeclaredApps(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 {
		t.Fatalf("DeclaredApps = %d apps, want 2", len(apps))
	}
	got := map[string]string{}
	for _, a := range apps {
		got[a.Name] = a.Worktree
	}
	if got["web"] != "web-dir" || got["api"] != "api" {
		t.Errorf("DeclaredApps worktrees = %v", got)
	}
}

func TestBranchPushed(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "bare.git")
	run(t, ".", "init", "-q", "--bare", bare)

	a := t.TempDir()
	run(t, a, "init", "-q")
	run(t, a, "config", "user.email", "t@example.com")
	run(t, a, "config", "user.name", "t")
	run(t, a, "checkout", "-q", "-b", "work")
	writeTestFile(t, a, "f", "1")
	run(t, a, "add", "-A")
	run(t, a, "commit", "-q", "-m", "c1")
	run(t, a, "remote", "add", "origin", bare)
	run(t, a, "push", "-q", "-u", "origin", "work")

	if ok, err := BranchPushed(a); err != nil || !ok {
		t.Fatalf("after push, BranchPushed = %v,%v; want true,nil", ok, err)
	}

	// Remote advances (a teammate pushes a commit) — local is now BEHIND but
	// still fully contained on origin, so it counts as pushed.
	b := t.TempDir()
	run(t, ".", "clone", "-q", bare, b)
	run(t, b, "config", "user.email", "t@example.com")
	run(t, b, "config", "user.name", "t")
	run(t, b, "checkout", "-q", "work")
	writeTestFile(t, b, "g", "2")
	run(t, b, "add", "-A")
	run(t, b, "commit", "-q", "-m", "c2")
	run(t, b, "push", "-q", "origin", "work")

	if ok, err := BranchPushed(a); err != nil || !ok {
		t.Fatalf("remote-ahead: BranchPushed = %v,%v; want true,nil (local fully contained)", ok, err)
	}

	// A local commit that isn't on origin → not pushed.
	writeTestFile(t, a, "h", "3")
	run(t, a, "add", "-A")
	run(t, a, "commit", "-q", "-m", "c3-local")
	if ok, _ := BranchPushed(a); ok {
		t.Fatalf("with an unpushed local commit, BranchPushed = true; want false")
	}
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestClone(t *testing.T) {
	// Build a hermetic source repo with one commit.
	src := initRepo(t)
	run(t, src, "config", "user.email", "t@example.com")
	run(t, src, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(src, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, src, "add", "Dockerfile")
	run(t, src, "commit", "-m", "init")

	dest := filepath.Join(t.TempDir(), "cloned")
	gotBranch, err := Clone(context.Background(), src, dest, "")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if !gotBranch {
		t.Errorf("Clone(branch=\"\") gotBranch = false, want true (nothing to check out)")
	}
	if _, err := os.Stat(filepath.Join(dest, "Dockerfile")); err != nil {
		t.Errorf("cloned dir missing Dockerfile: %v", err)
	}
	// Clone sets origin to the source, so the provenance probe finds it.
	if url, ok := GitRemoteURL(dest); !ok || url != src {
		t.Errorf("GitRemoteURL(clone) = %q,%v, want %q,true", url, ok, src)
	}
}

// currentBranch returns the checked-out branch of the repo at dir.
func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD in %s: %v", dir, err)
	}
	return strings.TrimSpace(string(out))
}

// srcWithBranch builds a hermetic source repo with one commit on its initial
// branch plus an extra branch `feature`. It returns the repo path and the name
// of the initial (default) branch, which varies with the host git config.
func srcWithBranch(t *testing.T) (dir, defaultBranch string) {
	t.Helper()
	src := initRepo(t)
	run(t, src, "config", "user.email", "t@example.com")
	run(t, src, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(src, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, src, "add", "Dockerfile")
	run(t, src, "commit", "-m", "init")
	def := currentBranch(t, src)
	run(t, src, "branch", "feature")
	return src, def
}

func TestClone_ChecksOutRequestedBranch(t *testing.T) {
	src, _ := srcWithBranch(t)

	dest := filepath.Join(t.TempDir(), "cloned")
	gotBranch, err := Clone(context.Background(), src, dest, "feature")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if !gotBranch {
		t.Fatalf("Clone(branch=feature) gotBranch = false, want true")
	}
	if b := currentBranch(t, dest); b != "feature" {
		t.Errorf("clone is on branch %q, want \"feature\"", b)
	}
}

func TestClone_MissingBranchFallsBackToDefault(t *testing.T) {
	src, def := srcWithBranch(t)

	dest := filepath.Join(t.TempDir(), "cloned")
	gotBranch, err := Clone(context.Background(), src, dest, "nonexistent")
	if err != nil {
		t.Fatalf("Clone: %v (a missing branch must not be a clone error)", err)
	}
	if gotBranch {
		t.Errorf("Clone(branch=nonexistent) gotBranch = true, want false")
	}
	if b := currentBranch(t, dest); b != def {
		t.Errorf("clone is on branch %q, want the remote default %q", b, def)
	}
}
