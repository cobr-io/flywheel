package appsync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/cobr-io/flywheel/internal/naming"
	"github.com/cobr-io/flywheel/internal/testgit"
)

// These tests exercise Reconciler against a sigs.k8s.io/controller-runtime
// fake client (the build-controller's *_test.go idiom) carrying real
// sourcev1.GitRepository / appsv1.Deployment objects, and — for the tests
// where a tick actually has to run — real on-disk git fixtures (the
// appsync_test.go style: bareRepo/writeFile/advanceBare, reused directly
// since this file lives in the same package).
//
// A tick's BareURL is gr.Spec.URL verbatim (production: the in-cluster
// git-server URL). So any test that needs Tick to really fetch/push can't use
// a fake unreachable URL merely shaped to pass the GitServerURLPrefix check —
// realAppFixture below builds the bare repo AT a path under a fresh
// "server root" directory, so that path is simultaneously a real,
// git-fetchable remote and a valid spec.url for a GitServerURLPrefix rooted
// at that same directory.

const (
	testBuilderNS    = "flywheel-system"
	testURLPrefix    = "http://git-server.flywheel-system.svc.cluster.local:8080"
	testPollInterval = 2 * time.Second
)

func newFakeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := sourcev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// testGR builds a minimal per-app GitRepository. branch == "" leaves
// spec.ref nil (an app whose GR hasn't declared a tracked branch yet).
func testGR(name, ns, url, branch string) *sourcev1.GitRepository {
	gr := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       sourcev1.GitRepositorySpec{URL: url},
	}
	if branch != "" {
		gr.Spec.Reference = &sourcev1.GitRepositoryRef{Branch: branch}
	}
	return gr
}

// bareRepoAt is bareRepo (appsync_test.go), but places the bare repository at
// exactly path instead of a fresh temp dir, so the path can double as a
// realistic spec.url.
func bareRepoAt(t *testing.T, path, content string) {
	t.Helper()
	seed := filepath.Join(t.TempDir(), "seed")
	testgit.Init(t, seed)
	writeFile(t, seed, "app.txt", content)
	testgit.Git(t, seed, "add", "-A")
	testgit.Git(t, seed, "commit", "-q", "-m", "init")
	testgit.Git(t, filepath.Dir(path), "clone", "-q", "--bare", seed, path)
}

// cloneWorktreeAt clones bare into mount/name — so the directory's basename
// matches what worktreeDir derives from bare's own basename — with a
// committer identity configured, the way the container mounts a developer's
// checkout. Returns the worktree path.
func cloneWorktreeAt(t *testing.T, bare, mount, name string) string {
	t.Helper()
	dir := filepath.Join(mount, name)
	testgit.Git(t, mount, "clone", "-q", bare, dir)
	testgit.Git(t, dir, "config", "user.email", "dev@dev")
	testgit.Git(t, dir, "config", "user.name", "dev")
	return dir
}

// realAppFixture builds one app's full on-disk shape: a bare repo at
// <serverRoot>/<name>.git (serverRoot is fresh per call) and a worktree clone
// at <mount>/<name>. bareURL is the bare repo's path — real enough for Tick
// to fetch/push against, and (since it lives under serverRoot) a valid
// spec.url for a Reconciler whose GitServerURLPrefix == serverRoot.
func realAppFixture(t *testing.T, name, content string) (bareURL, serverRoot, mount, wt string) {
	t.Helper()
	serverRoot = t.TempDir()
	bareURL = filepath.Join(serverRoot, name+".git")
	bareRepoAt(t, bareURL, content)
	mount = t.TempDir()
	wt = cloneWorktreeAt(t, bareURL, mount, name)
	return bareURL, serverRoot, mount, wt
}

// newTestReconciler builds a Reconciler wired against a fake client seeded
// with objs.
func newTestReconciler(t *testing.T, mount, urlPrefix string, objs ...client.Object) *Reconciler {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newFakeScheme(t)).WithObjects(objs...).Build()
	return &Reconciler{
		Client:             c,
		WorkspacesMount:    mount,
		GitServerURLPrefix: urlPrefix,
		BuilderNamespace:   testBuilderNS,
		PollInterval:       testPollInterval,
		ExecTimeout:        10 * time.Second,
		Logf:               t.Logf,
	}
}

func reqFor(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testBuilderNS, Name: name}}
}

// (a) A GitRepository whose spec.url does not carry GitServerURLPrefix is
// ignored outright: no requeue, no error, and — since ticking it would have
// required creating a Ticker — no cache entry. No real bare repo needed:
// filtering happens before anything would try to fetch it.
func TestReconcile_URLFilter_Ignored(t *testing.T) {
	gr := testGR("other-app", testBuilderNS, "http://not-our-git-server/other-app.git", "main")
	r := newTestReconciler(t, t.TempDir(), testURLPrefix, gr)

	res, err := r.Reconcile(context.Background(), reqFor("other-app"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("expected a bare no-op Result, got %+v", res)
	}
	if len(r.tickers) != 0 {
		t.Errorf("URL-filtered GitRepository must never get a cached Ticker, got %d", len(r.tickers))
	}
}

// (b) A normal (idle, in-sync) tick against a real worktree requeues at
// exactly PollInterval.
func TestReconcile_NormalTick_RequeuesAtPollInterval(t *testing.T) {
	bareURL, serverRoot, mount, _ := realAppFixture(t, "sample-app", "v0\n")

	gr := testGR("sample-app", testBuilderNS, bareURL, "main")
	r := newTestReconciler(t, mount, serverRoot, gr)

	res, err := r.Reconcile(context.Background(), reqFor("sample-app"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != testPollInterval {
		t.Errorf("RequeueAfter = %v, want PollInterval %v", res.RequeueAfter, testPollInterval)
	}
}

// (c) While the legacy git-auto-sync-<app> Deployment exists, Reconcile must
// not tick at all: spec.ref.branch and the reconcile-disabled annotation stay
// exactly as they were, even though the worktree is on a branch (feat/x) the
// GR isn't tracking (main) — which would otherwise trigger the branch-follow
// patch. It still requeues at PollInterval so the app is checked again once
// the Deployment is removed.
func TestReconcile_LegacyInterlock_SkipsTick(t *testing.T) {
	bareURL, serverRoot, mount, wt := realAppFixture(t, "sample-app", "v0\n")
	testgit.Git(t, wt, "switch", "-q", "-c", "feat/x")
	testgit.Git(t, wt, "push", "-q", bareURL, "feat/x:refs/heads/feat/x")

	gr := testGR("sample-app", testBuilderNS, bareURL, "main")
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: legacyDeploymentPrefix + "sample-app", Namespace: testBuilderNS,
	}}
	r := newTestReconciler(t, mount, serverRoot, gr, dep)

	res, err := r.Reconcile(context.Background(), reqFor("sample-app"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != testPollInterval {
		t.Errorf("RequeueAfter = %v, want PollInterval %v", res.RequeueAfter, testPollInterval)
	}

	var after sourcev1.GitRepository
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: testBuilderNS, Name: "sample-app"}, &after); err != nil {
		t.Fatalf("re-Get GitRepository: %v", err)
	}
	if after.Spec.Reference == nil || after.Spec.Reference.Branch != "main" {
		t.Errorf("interlock must not run the branch-follow patch; spec.ref.branch = %+v", after.Spec.Reference)
	}
	if v := after.Annotations[naming.KustomizeReconcileDisabledAnnotation]; v != "" {
		t.Errorf("interlock must not set the reconcile-disabled annotation, got %q", v)
	}
}

// markWarnedOnce is the mechanism gating the legacy-interlock's "warn exactly
// once" behavior; test it directly rather than scraping log output.
func TestMarkWarnedOnce(t *testing.T) {
	r := &Reconciler{}
	keyA := types.NamespacedName{Namespace: testBuilderNS, Name: "a"}
	keyB := types.NamespacedName{Namespace: testBuilderNS, Name: "b"}

	if !r.markWarnedOnce(keyA) {
		t.Error("first call for a new key must return true (should log)")
	}
	if r.markWarnedOnce(keyA) {
		t.Error("second call for the same key must return false (must not log again)")
	}
	if !r.markWarnedOnce(keyB) {
		t.Error("a different key must warn independently")
	}
}

// (d) A GitRepository deleted between enqueue and reconcile (or that never
// existed) is a bare no-op: no requeue, no error.
func TestReconcile_GRNotFound_NoRequeueNoError(t *testing.T) {
	r := newTestReconciler(t, t.TempDir(), testURLPrefix) // no objects seeded

	res, err := r.Reconcile(context.Background(), reqFor("ghost-app"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("expected a bare no-op Result, got %+v", res)
	}
}

// (e) A genuine divergence that conflicts (the same fixture shape as
// TestTick_ConflictStall) stalls the tick; Reconcile must requeue at the
// long stall interval (30s, design Q4), not PollInterval.
func TestReconcile_Stall_RequeuesAtStallInterval(t *testing.T) {
	bareURL, serverRoot, mount, wt := realAppFixture(t, "sample-app", "shared\n")
	writeFile(t, wt, "app.txt", "dev\n")
	testgit.Git(t, wt, "commit", "-q", "-am", "dev edits shared line")
	advanceBare(t, bareURL, "main", func(dir string) { writeFile(t, dir, "app.txt", "ci\n") })

	gr := testGR("sample-app", testBuilderNS, bareURL, "main")
	r := newTestReconciler(t, mount, serverRoot, gr)

	res, err := r.Reconcile(context.Background(), reqFor("sample-app"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != defaultStallInterval {
		t.Errorf("RequeueAfter = %v, want the stall interval %v", res.RequeueAfter, defaultStallInterval)
	}
}

// (f) A follow tick (tracked branch != checked-out branch) that also creates
// a brand-new bare branch exercises both FluxPatcher methods against the SAME
// fake-client object: EnsureBranch must set the reconcile-disabled annotation
// AND patch spec.ref.branch to the checked-out branch, and the push that
// follows must trigger PokeReconcile, setting the requestedAt annotation.
func TestReconcile_FollowTick_FluxPatcherShapes(t *testing.T) {
	bareURL, serverRoot, mount, wt := realAppFixture(t, "sample-app", "v0\n")
	testgit.Git(t, wt, "switch", "-q", "-c", "feat/new")
	writeFile(t, wt, "app.txt", "feat\n")
	testgit.Git(t, wt, "commit", "-q", "-am", "feat change")

	// GR still tracks main; the worktree is on feat/new, a branch not yet in
	// the bare repo — this fires branch-follow (Followed) AND a create-push
	// (Pushed), so Tick's poke rule fires PokeReconcile too.
	gr := testGR("sample-app", testBuilderNS, bareURL, "main")
	r := newTestReconciler(t, mount, serverRoot, gr)

	res, err := r.Reconcile(context.Background(), reqFor("sample-app"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != testPollInterval {
		t.Errorf("RequeueAfter = %v, want PollInterval %v", res.RequeueAfter, testPollInterval)
	}

	var after sourcev1.GitRepository
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: testBuilderNS, Name: "sample-app"}, &after); err != nil {
		t.Fatalf("re-Get GitRepository: %v", err)
	}
	if v := after.Annotations[naming.KustomizeReconcileDisabledAnnotation]; v != naming.KustomizeReconcileDisabledValue {
		t.Errorf("EnsureBranch must set the reconcile-disabled annotation, got %q", v)
	}
	if after.Spec.Reference == nil || after.Spec.Reference.Branch != "feat/new" {
		t.Errorf("EnsureBranch must patch spec.ref.branch to feat/new, got %+v", after.Spec.Reference)
	}
	if v := after.Annotations[naming.ReconcileRequestAnnotation]; v == "" {
		t.Error("PokeReconcile must set the reconcile-request annotation, got empty")
	}
}

// sanity check that a NotFound Get on the legacy Deployment (the overwhelming
// common case — no client has migrated away yet) is treated as "no
// interlock", not surfaced as an error.
func TestReconcile_NoLegacyDeployment_IsNotAnError(t *testing.T) {
	bareURL, serverRoot, mount, _ := realAppFixture(t, "sample-app", "v0\n")
	gr := testGR("sample-app", testBuilderNS, bareURL, "main")
	r := newTestReconciler(t, mount, serverRoot, gr)

	if _, err := r.Reconcile(context.Background(), reqFor("sample-app")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var dep appsv1.Deployment
	err := r.Get(context.Background(), types.NamespacedName{Namespace: testBuilderNS, Name: legacyDeploymentPrefix + "sample-app"}, &dep)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no legacy Deployment to exist, got err=%v", err)
	}
}
