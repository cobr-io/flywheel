package deploybranch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A realistic manifest: the image setter line is buried deep, so an edit to
// `replicas` is in a different hunk (clean rebase) while a container rename is
// adjacent (conflict → reset-and-rebump). metadata.name differs from the
// container name so the rename edit is unambiguous.
const manifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: app
          image: app:0-placeholder # {"$imagepolicy": "flux-system:myapp"}
          ports:
            - containerPort: 8080
`

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// bareRepo builds a bare "remote" with one commit on main carrying manifest.
func bareRepo(t *testing.T) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, work, "init", "-q", "-b", "main")
	git(t, work, "config", "user.email", "t@t")
	git(t, work, "config", "user.name", "t")
	writeManifest(t, work, manifest)
	git(t, work, "add", "-A")
	git(t, work, "commit", "-q", "-m", "A: authored")

	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, work, "clone", "-q", "--bare", work, bare)
	return bare
}

func writeManifest(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "deployment.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cloneTemp(t *testing.T, bare string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	git(t, filepath.Dir(dir), "clone", "-q", bare, dir)
	git(t, dir, "config", "user.email", "t@t")
	git(t, dir, "config", "user.name", "t")
	return dir
}

func remoteHasBranch(t *testing.T, dir, branch string) bool {
	t.Helper()
	return gitOut(t, dir, "ls-remote", "--heads", "origin", branch) != ""
}

var imageRe = regexp.MustCompile(`app:\S+`)

func setImage(t *testing.T, dir, tag string) {
	t.Helper()
	p := filepath.Join(dir, "deployment.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	out := imageRe.ReplaceAllString(string(b), "app:"+tag)
	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

// simulateIUABump pushes a bump commit onto the deploy branch (creating it from
// main on the first bump), exactly like Flux's IUA committing a new image tag.
func simulateIUABump(t *testing.T, bare, deploy, tag string) {
	t.Helper()
	c := cloneTemp(t, bare)
	from := "origin/main"
	if remoteHasBranch(t, c, deploy) {
		from = "origin/" + deploy
	}
	git(t, c, "checkout", "-q", "-B", "b", from)
	setImage(t, c, tag)
	git(t, c, "commit", "-q", "-am", "chore: bump images "+tag)
	git(t, c, "push", "-q", "origin", "b:refs/heads/"+deploy)
}

// advanceAuthored applies editFn to the manifest and pushes a new main commit.
func advanceAuthored(t *testing.T, bare, msg string, editFn func(path string)) {
	t.Helper()
	c := cloneTemp(t, bare)
	git(t, c, "checkout", "-q", "-B", "m", "origin/main")
	editFn(filepath.Join(c, "deployment.yaml"))
	git(t, c, "commit", "-q", "-am", msg)
	git(t, c, "push", "-q", "origin", "m:refs/heads/main")
}

func replace(t *testing.T, path, old, new string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, old) {
		t.Fatalf("edit target %q not found in %s", old, path)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(s, old, new)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// showDeploy returns deployment.yaml as it stands on the deploy branch in bare.
func showDeploy(t *testing.T, bare, deploy string) string {
	t.Helper()
	return gitOut(t, ".", "--git-dir="+bare, "show", "refs/heads/"+deploy+":deployment.yaml")
}

func newMaintainer(t *testing.T, bare string) *Maintainer {
	return &Maintainer{
		WorkDir:   filepath.Join(t.TempDir(), "maint"),
		RemoteURL: bare,
		Authored:  "main",
		Deploy:    "flywheel/local-deploy",
	}
}

// pushBranch creates <branch> off origin/main in the bare repo with a distinct
// marker commit, so two such branches diverge in content.
func pushBranch(t *testing.T, bare, branch, marker string) {
	t.Helper()
	c := cloneTemp(t, bare)
	git(t, c, "checkout", "-q", "-B", branch, "origin/main")
	p := filepath.Join(c, "deployment.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(b, []byte("\n# "+marker+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, c, "commit", "-q", "-am", "on "+branch)
	git(t, c, "push", "-q", "origin", branch+":refs/heads/"+branch)
}

// A `flywheel use` branch switch must reset DEPLOY to the new branch, discarding
// the previous (divergent) branch's commits AND its bumps — a rebase-forward
// would wrongly replay the old branch's commits onto the new one.
func TestResetToAuthored_DiscardsOtherBranchCommitsAndBumps(t *testing.T) {
	bare := bareRepo(t)
	pushBranch(t, bare, "feat-x", "marker-x")
	pushBranch(t, bare, "feat-y", "marker-y")

	m := newMaintainer(t, bare)
	m.Authored = "feat-x"
	if _, err := m.Reconcile(context.Background()); err != nil { // seed DEPLOY = feat-x
		t.Fatal(err)
	}
	simulateIUABump(t, bare, "flywheel/local-deploy", "1-aaa") // DEPLOY = feat-x + bump

	// Switch to the divergent feat-y.
	m.Authored = "feat-y"
	res, err := m.ResetToAuthored(context.Background())
	if err != nil {
		t.Fatalf("ResetToAuthored: %v", err)
	}
	if !res.Reset || !res.Changed {
		t.Fatalf("expected Reset+Changed, got %+v", res)
	}
	got := showDeploy(t, bare, "flywheel/local-deploy")
	if !strings.Contains(got, "marker-y") {
		t.Errorf("DEPLOY should be feat-y, missing marker-y:\n%s", got)
	}
	if strings.Contains(got, "marker-x") {
		t.Errorf("DEPLOY leaked feat-x's commit (marker-x):\n%s", got)
	}
	if strings.Contains(got, "1-aaa") {
		t.Errorf("DEPLOY kept the old branch's bump (1-aaa):\n%s", got)
	}
	// DEPLOY tip == feat-y tip, with no bump layer.
	deployTip := gitOut(t, ".", "--git-dir="+bare, "rev-parse", "refs/heads/flywheel/local-deploy")
	featY := gitOut(t, ".", "--git-dir="+bare, "rev-parse", "refs/heads/feat-y")
	if deployTip != featY {
		t.Errorf("DEPLOY tip %s != feat-y tip %s", deployTip, featY)
	}
}

func TestReconcile_SeedsDeployFromAuthored(t *testing.T) {
	bare := bareRepo(t)
	m := newMaintainer(t, bare)

	res, err := m.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.Seeded || !res.Changed {
		t.Errorf("expected Seeded+Changed, got %+v", res)
	}
	if !remoteHasBranch(t, cloneTemp(t, bare), "flywheel/local-deploy") {
		t.Fatal("deploy branch was not created")
	}
	if got := showDeploy(t, bare, "flywheel/local-deploy"); !strings.Contains(got, "0-placeholder") {
		t.Errorf("seeded deploy should equal authored (placeholder), got:\n%s", got)
	}
}

func TestReconcile_NoopWhenAuthoredUnchanged(t *testing.T) {
	bare := bareRepo(t)
	m := newMaintainer(t, bare)
	if _, err := m.Reconcile(context.Background()); err != nil { // seed
		t.Fatal(err)
	}
	simulateIUABump(t, bare, "flywheel/local-deploy", "1-aaa")
	before := gitOut(t, ".", "--git-dir="+bare, "rev-parse", "refs/heads/flywheel/local-deploy")

	res, err := m.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Changed {
		t.Errorf("expected no change (deploy already sits on authored), got %+v", res)
	}
	after := gitOut(t, ".", "--git-dir="+bare, "rev-parse", "refs/heads/flywheel/local-deploy")
	if before != after {
		t.Errorf("deploy tip moved on a no-op: %s -> %s", before, after)
	}
}

func TestReconcile_RebaseForwardWhenAuthoredAdvancesAwayFromImage(t *testing.T) {
	bare := bareRepo(t)
	m := newMaintainer(t, bare)
	if _, err := m.Reconcile(context.Background()); err != nil { // seed
		t.Fatal(err)
	}
	simulateIUABump(t, bare, "flywheel/local-deploy", "1-aaa")
	advanceAuthored(t, bare, "scale replicas", func(p string) { replace(t, p, "replicas: 1", "replicas: 2") })

	res, err := m.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.RebasedForward || !res.Changed {
		t.Fatalf("expected RebasedForward+Changed, got %+v", res)
	}
	got := showDeploy(t, bare, "flywheel/local-deploy")
	if !strings.Contains(got, "replicas: 2") {
		t.Errorf("deploy missing the authored change (replicas: 2):\n%s", got)
	}
	if !strings.Contains(got, "app:1-aaa") {
		t.Errorf("deploy missing the carried bump (app:1-aaa):\n%s", got)
	}
}

func TestReconcile_ResetFallbackWhenAuthoredTouchesImageHunk(t *testing.T) {
	bare := bareRepo(t)
	m := newMaintainer(t, bare)
	if _, err := m.Reconcile(context.Background()); err != nil { // seed
		t.Fatal(err)
	}
	simulateIUABump(t, bare, "flywheel/local-deploy", "1-aaa")
	// Rename the container — the line directly above the image setter line, so the
	// bump's diff and the authored edit land in the same hunk → rebase conflict.
	advanceAuthored(t, bare, "rename container", func(p string) { replace(t, p, "- name: app", "- name: web") })

	res, err := m.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.ResetFallback || !res.Changed {
		t.Fatalf("expected ResetFallback+Changed, got %+v", res)
	}
	got := showDeploy(t, bare, "flywheel/local-deploy")
	if !strings.Contains(got, "- name: web") {
		t.Errorf("deploy missing the authored change (rename):\n%s", got)
	}
	if !strings.Contains(got, "0-placeholder") {
		t.Errorf("reset-and-rebump should leave the placeholder for the IUA to re-bump:\n%s", got)
	}
}

func TestReconcile_CarriesMultipleBumpsForward(t *testing.T) {
	bare := bareRepo(t)
	m := newMaintainer(t, bare)
	if _, err := m.Reconcile(context.Background()); err != nil { // seed
		t.Fatal(err)
	}
	simulateIUABump(t, bare, "flywheel/local-deploy", "1-aaa")
	simulateIUABump(t, bare, "flywheel/local-deploy", "2-bbb") // a second bump on top
	advanceAuthored(t, bare, "scale replicas", func(p string) { replace(t, p, "replicas: 1", "replicas: 2") })

	res, err := m.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.RebasedForward {
		t.Fatalf("expected RebasedForward, got %+v", res)
	}
	got := showDeploy(t, bare, "flywheel/local-deploy")
	if !strings.Contains(got, "replicas: 2") || !strings.Contains(got, "app:2-bbb") {
		t.Errorf("deploy should be authored + latest bump (replicas:2, app:2-bbb):\n%s", got)
	}
	// The bump layer on top of authored is exactly the two replayed bumps.
	layer := gitOut(t, ".", "--git-dir="+bare, "rev-list", "--count", "refs/heads/main..refs/heads/flywheel/local-deploy")
	if layer != "2" {
		t.Errorf("expected 2 bump commits on top of authored, got %s", layer)
	}
}
