package selfsync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cobr-io/flywheel/internal/deploybranch"
)

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
`

// --- fake Flux ---------------------------------------------------------------

type fakeFlux struct {
	configured string
	suspends   []bool
	pokeGR     int
	pokeKust   int
	waits      []string
}

func (f *fakeFlux) ConfiguredAuthored(context.Context) (string, error) { return f.configured, nil }
func (f *fakeFlux) SuspendIUA(_ context.Context, s bool) error {
	f.suspends = append(f.suspends, s)
	return nil
}
func (f *fakeFlux) PokeGitRepository(context.Context) error { f.pokeGR++; return nil }
func (f *fakeFlux) WaitArtifact(_ context.Context, sha string) error {
	f.waits = append(f.waits, sha)
	return nil
}
func (f *fakeFlux) PokeKustomization(context.Context) error { f.pokeKust++; return nil }

// --- git helpers -------------------------------------------------------------

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

func bareRepo(t *testing.T) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "seed")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, work, "init", "-q", "-b", "main")
	git(t, work, "config", "user.email", "t@t")
	git(t, work, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(work, "deployment.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "-A")
	git(t, work, "commit", "-q", "-m", "A: authored")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, work, "clone", "-q", "--bare", work, bare)
	return bare
}

func cloneWorktree(t *testing.T, bare string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "worktree")
	git(t, filepath.Dir(wt), "clone", "-q", bare, wt)
	git(t, wt, "config", "user.email", "dev@dev")
	git(t, wt, "config", "user.name", "dev")
	return wt
}

// commitWorktree edits deployment.yaml in the worktree and commits on its
// current branch (the developer authoring); it does NOT push — the loop does.
func commitWorktree(t *testing.T, wt, old, new, msg string) {
	t.Helper()
	p := filepath.Join(wt, "deployment.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, old) {
		t.Fatalf("edit target %q not found", old)
	}
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(s, old, new)), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "commit", "-q", "-am", msg)
}

// simulateIUABump pushes a bump commit onto the deploy branch in the bare repo.
func simulateIUABump(t *testing.T, bare, deploy, tag string) {
	t.Helper()
	c := filepath.Join(t.TempDir(), "iua")
	git(t, filepath.Dir(c), "clone", "-q", bare, c)
	git(t, c, "config", "user.email", "t@t")
	git(t, c, "config", "user.name", "t")
	from := "origin/main"
	if gitOut(t, c, "ls-remote", "--heads", "origin", deploy) != "" {
		from = "origin/" + deploy
	}
	git(t, c, "checkout", "-q", "-B", "b", from)
	p := filepath.Join(c, "deployment.yaml")
	b, _ := os.ReadFile(p)
	out := strings.ReplaceAll(string(b), "app:0-placeholder", "app:"+tag)
	out = strings.ReplaceAll(out, "app:1-aaa", "app:"+tag) // tolerate a prior bump
	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, c, "commit", "-q", "-am", "chore: bump images "+tag)
	git(t, c, "push", "-q", "origin", "b:refs/heads/"+deploy)
}

func showDeploy(t *testing.T, bare, deploy string) string {
	t.Helper()
	return gitOut(t, ".", "--git-dir="+bare, "show", "refs/heads/"+deploy+":deployment.yaml")
}

const deployBranch = "flywheel/local-deploy"

func newLoop(t *testing.T, bare, wt string, flux Flux) *Loop {
	return &Loop{
		Worktree:      &Worktree{Dir: wt, BareURL: bare},
		Deploy:        &deploybranch.Maintainer{WorkDir: filepath.Join(t.TempDir(), "maint"), RemoteURL: bare, Deploy: deployBranch},
		Flux:          flux,
		DefaultBranch: "main",
		PollInterval:  time.Second,
	}
}

// --- tests -------------------------------------------------------------------

// First tick seeds DEPLOY = AUTHORED even though the worktree didn't advance,
// suspends/resumes the IUA around the rebuild, and pokes Flux.
func TestTick_SeedsAndPokes(t *testing.T) {
	bare := bareRepo(t)
	flux := &fakeFlux{}
	l := newLoop(t, bare, cloneWorktree(t, bare), flux)

	res, err := l.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.Rebuilt || !res.Deploy.Seeded {
		t.Fatalf("expected seed rebuild, got %+v", res)
	}
	// With no configured branch, AUTHORED falls back to DefaultBranch ("main").
	if res.Authored != "main" {
		t.Errorf("AUTHORED fallback = %q, want the default %q", res.Authored, "main")
	}
	if gitOut(t, ".", "--git-dir="+bare, "rev-parse", "--verify", "--quiet", "refs/heads/"+deployBranch) == "" {
		t.Fatal("deploy branch not created")
	}
	if len(flux.suspends) != 2 || flux.suspends[0] != true || flux.suspends[1] != false {
		t.Errorf("expected suspend [true,false], got %v", flux.suspends)
	}
	if flux.pokeGR != 1 || flux.pokeKust != 1 {
		t.Errorf("expected one poke each (GR=%d, Kust=%d)", flux.pokeGR, flux.pokeKust)
	}
}

// An idle tick (worktree unchanged) does no rebuild and no Kubernetes work.
func TestTick_IdleDoesNothing(t *testing.T) {
	bare := bareRepo(t)
	flux := &fakeFlux{}
	l := newLoop(t, bare, cloneWorktree(t, bare), flux)
	if _, err := l.Tick(context.Background()); err != nil { // seed
		t.Fatal(err)
	}
	suspendsAfterSeed := len(flux.suspends)

	res, err := l.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Rebuilt || res.Pushed {
		t.Errorf("idle tick should do nothing, got %+v", res)
	}
	if len(flux.suspends) != suspendsAfterSeed {
		t.Errorf("idle tick touched the IUA: %v", flux.suspends)
	}
	if flux.pokeGR != 1 || flux.pokeKust != 1 {
		t.Errorf("idle tick poked Flux (GR=%d, Kust=%d)", flux.pokeGR, flux.pokeKust)
	}
}

// When the worktree's AUTHORED advances, the loop pushes it and rebuilds DEPLOY.
func TestTick_AuthoredAdvancePushesAndRebuilds(t *testing.T) {
	bare := bareRepo(t)
	wt := cloneWorktree(t, bare)
	flux := &fakeFlux{}
	l := newLoop(t, bare, wt, flux)
	if _, err := l.Tick(context.Background()); err != nil { // seed
		t.Fatal(err)
	}
	commitWorktree(t, wt, "replicas: 1", "replicas: 2", "scale")

	res, err := l.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.Pushed || !res.Rebuilt {
		t.Fatalf("expected push+rebuild, got %+v", res)
	}
	if got := showDeploy(t, bare, deployBranch); !strings.Contains(got, "replicas: 2") {
		t.Errorf("deploy missing the authored change:\n%s", got)
	}
	if flux.pokeGR != 2 || flux.pokeKust != 2 {
		t.Errorf("expected a second poke (GR=%d, Kust=%d)", flux.pokeGR, flux.pokeKust)
	}
}

// An IUA bump on DEPLOY survives an idle tick and is carried forward onto the
// next AUTHORED advance — authored history never sees it.
func TestTick_CarriesIUABumpForward(t *testing.T) {
	bare := bareRepo(t)
	wt := cloneWorktree(t, bare)
	flux := &fakeFlux{}
	l := newLoop(t, bare, wt, flux)
	if _, err := l.Tick(context.Background()); err != nil { // seed → deploy = A
		t.Fatal(err)
	}
	simulateIUABump(t, bare, deployBranch, "1-aaa") // deploy = A + bump

	// Idle tick: bump stays, no rebuild.
	if res, err := l.Tick(context.Background()); err != nil || res.Rebuilt {
		t.Fatalf("idle tick should not rebuild: res=%+v err=%v", res, err)
	}
	if got := showDeploy(t, bare, deployBranch); !strings.Contains(got, "app:1-aaa") {
		t.Fatalf("bump lost on idle tick:\n%s", got)
	}

	// AUTHORED advances → rebuild carries the bump forward.
	commitWorktree(t, wt, "replicas: 1", "replicas: 2", "scale")
	res, err := l.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.Rebuilt {
		t.Fatalf("expected rebuild on advance, got %+v", res)
	}
	got := showDeploy(t, bare, deployBranch)
	if !strings.Contains(got, "replicas: 2") || !strings.Contains(got, "app:1-aaa") {
		t.Errorf("deploy should be authored + carried bump (replicas:2, app:1-aaa):\n%s", got)
	}
	// The worktree's authored branch never received the bump.
	if wtManifest := gitOut(t, ".", "--git-dir="+filepath.Join(wt, ".git"), "show", "refs/heads/main:deployment.yaml"); strings.Contains(wtManifest, "app:1-aaa") {
		t.Errorf("authored branch was polluted with a bump:\n%s", wtManifest)
	}
}

// `flywheel use feat-y` re-points DEPLOY even when feat-y is already in sync with
// the bare repo (no push this tick) — the regression the review caught: the
// rebuild must trigger on a configured-branch *switch*, not only on a push. The
// switch resets DEPLOY to the new branch, discarding the old branch's bump.
func TestTick_BranchSwitchResetsDeploy(t *testing.T) {
	bare := bareRepo(t)
	wt := cloneWorktree(t, bare)
	flux := &fakeFlux{}
	l := newLoop(t, bare, wt, flux)
	if _, err := l.Tick(context.Background()); err != nil { // seed DEPLOY = main
		t.Fatal(err)
	}
	simulateIUABump(t, bare, deployBranch, "1-aaa") // DEPLOY = main + bump

	// Developer creates feat-y and pushes it, so it is ALREADY in sync with bare.
	git(t, wt, "switch", "-q", "-c", "feat-y")
	commitWorktree(t, wt, "replicas: 1", "replicas: 9", "feat-y change")
	git(t, wt, "push", "-q", bare, "feat-y:feat-y")

	// flywheel use feat-y.
	flux.configured = "feat-y"
	pokesBefore := flux.pokeGR

	res, err := l.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Pushed {
		t.Errorf("feat-y already synced; expected no push, got Pushed=true")
	}
	if !res.Rebuilt || !res.Deploy.Reset {
		t.Fatalf("expected a reset rebuild on the switch, got %+v", res)
	}
	got := showDeploy(t, bare, deployBranch)
	if !strings.Contains(got, "replicas: 9") {
		t.Errorf("DEPLOY should be feat-y now:\n%s", got)
	}
	if strings.Contains(got, "app:1-aaa") {
		t.Errorf("DEPLOY kept the old branch's bump after the switch:\n%s", got)
	}
	if flux.pokeGR <= pokesBefore {
		t.Errorf("expected a Flux poke after the switch (GR pokes %d → %d)", pokesBefore, flux.pokeGR)
	}
}
