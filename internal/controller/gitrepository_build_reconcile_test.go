package controller

import (
	"context"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	reconcileAppsNS       = "apps"
	reconcileControllerNS = "flywheel-system"
	reconcileSHA40        = "0123456789abcdef0123456789abcdef01234567"
	reconcileShortSHA     = "0123456"
)

// reconcileScheme registers the kinds a build Reconcile touches.
func reconcileScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		batchv1.AddToScheme, corev1.AddToScheme, sourcev1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// gitRepoWithArtifact builds a live per-app GitRepository whose artifact points
// at reconcileSHA40, timestamped so the Job name (ts + short SHA) is stable.
func gitRepoWithArtifact(name string, ts int64) *sourcev1.GitRepository {
	return &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: reconcileAppsNS},
		Spec:       sourcev1.GitRepositorySpec{URL: "http://git-server.flywheel-system.svc.cluster.local:8080/" + name + ".git"},
		Status: sourcev1.GitRepositoryStatus{
			Artifact: &meta.Artifact{
				Revision:       "main@sha1:" + reconcileSHA40,
				LastUpdateTime: metav1.NewTime(time.Unix(ts, 0)),
			},
		},
	}
}

func buildConfigCM(repo, buildsYAML string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: repo + buildConfigSuffix, Namespace: reconcileAppsNS},
		Data:       map[string]string{"builds.yaml": buildsYAML},
	}
}

func reconcileConfig() Config {
	return Config{
		Namespace:        reconcileControllerNS,
		BuilderNamespace: reconcileAppsNS,
		Registry:         "acme-local-registry",
		RegistryPort:     "50001",
		ClusterName:      "acme-local",
		ClientName:       "acme",
	}
}

func listBuildJobs(t *testing.T, c client.Client) []batchv1.Job {
	t.Helper()
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace(reconcileControllerNS)); err != nil {
		t.Fatal(err)
	}
	return jobs.Items
}

// TestReconcile_CreatesBuildJob is the core happy path: a live GitRepository
// plus its sibling `<app>-build-config` ConfigMap dispatches exactly one build
// Job, named deterministically from the app, artifact timestamp, and short SHA.
func TestReconcile_CreatesBuildJob(t *testing.T) {
	const ts = int64(1780399472)
	const repo = "sample-app"

	c := fake.NewClientBuilder().
		WithScheme(reconcileScheme(t)).
		WithObjects(
			gitRepoWithArtifact(repo, ts),
			buildConfigCM(repo, "builds:\n  - image: sample-app\n"),
		).
		Build()
	r := &GitRepositoryBuildReconciler{Client: c, Config: reconcileConfig()}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: reconcileAppsNS, Name: repo},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	jobs := listBuildJobs(t, c)
	if len(jobs) != 1 {
		t.Fatalf("expected exactly 1 build Job, got %d", len(jobs))
	}
	want := buildJobName(repo, "sample-app", ts, reconcileShortSHA)
	if jobs[0].Name != want {
		t.Errorf("build Job name = %q, want %q", jobs[0].Name, want)
	}
	if jobs[0].Namespace != reconcileControllerNS {
		t.Errorf("build Job namespace = %q, want %q", jobs[0].Namespace, reconcileControllerNS)
	}
}

// TestReconcile_SkipsExistingJob asserts idempotency: when the deterministic
// Job for a commit already exists, Reconcile leaves it untouched and creates no
// duplicate (the `job exists, skipping` continue).
func TestReconcile_SkipsExistingJob(t *testing.T) {
	const ts = int64(1780399472)
	const repo = "sample-app"

	jobName := buildJobName(repo, "sample-app", ts, reconcileShortSHA)
	existing := jobWithLabels(jobName, reconcileControllerNS, map[string]string{
		"app": "image-builder", "repo": repo,
	})
	// A sentinel proves the object is the pre-existing one, not a fresh create.
	existing.Annotations = map[string]string{"sentinel": "original"}

	c := fake.NewClientBuilder().
		WithScheme(reconcileScheme(t)).
		WithObjects(
			gitRepoWithArtifact(repo, ts),
			buildConfigCM(repo, "builds:\n  - image: sample-app\n"),
			existing,
		).
		Build()
	r := &GitRepositoryBuildReconciler{Client: c, Config: reconcileConfig()}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: reconcileAppsNS, Name: repo},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	jobs := listBuildJobs(t, c)
	if len(jobs) != 1 {
		t.Fatalf("expected the single existing Job to survive, got %d Jobs", len(jobs))
	}
	if jobs[0].Annotations["sentinel"] != "original" {
		t.Errorf("existing Job was replaced (sentinel lost): %+v", jobs[0].Annotations)
	}
}

// TestReconcile_InvalidSecretBlocksLaterBuilds documents and locks in the
// cross-build coupling at gitrepository_build_controller.go's build loop: a bad
// secret in build N fail-closes the whole Reconcile (returns an error to
// requeue), so builds *after* N get no Job this pass — even independent ones.
//
// DECISION (T23): preserve this coupling rather than isolate builds. The loop's
// contract is "recover once the Secret appears"; returning an error is what
// requeues, and per-build isolation would trade that simple recovery for
// partial-apply semantics better addressed alongside the applier's per-object
// aggregation work (T22). See the PR body.
func TestReconcile_InvalidSecretBlocksLaterBuilds(t *testing.T) {
	const ts = int64(1780399472)
	const repo = "monorepo"

	// build[0] "first" references a Secret that does not exist; build[1]
	// "second" is independent and would otherwise dispatch a Job.
	buildsYAML := "builds:\n" +
		"  - image: first\n" +
		"    secrets:\n" +
		"      - id: TOK\n" +
		"        src: missing-secret/token\n" +
		"  - image: second\n"

	c := fake.NewClientBuilder().
		WithScheme(reconcileScheme(t)).
		WithObjects(
			gitRepoWithArtifact(repo, ts),
			buildConfigCM(repo, buildsYAML),
		).
		Build()
	r := &GitRepositoryBuildReconciler{Client: c, Config: reconcileConfig()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: reconcileAppsNS, Name: repo},
	})
	if err == nil {
		t.Fatal("expected Reconcile to return an error (requeue) on the invalid secret")
	}

	// The coupling: neither the failing build nor the independent later build
	// produced a Job this pass.
	if jobs := listBuildJobs(t, c); len(jobs) != 0 {
		names := make([]string, 0, len(jobs))
		for _, j := range jobs {
			names = append(names, j.Name)
		}
		t.Errorf("expected no Jobs (bad secret aborts the loop before later builds), got %v", names)
	}
}
