package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestBuildJobName covers the build Job naming: repo/image dedupe, the
// distinct-image (monorepo) case, sanitisation, and truncation-with-hash so
// long app names stay within Kubernetes' 63-char Job-name limit instead of
// silently failing at Job creation.
func TestBuildJobName(t *testing.T) {
	const ts = int64(1780399472) // 10 digits
	const sha = "2dd4803"        // always 7

	t.Run("dedupes repo==image (single-image default)", func(t *testing.T) {
		got := buildJobName("hello-py", "hello-py", ts, sha)
		if want := "build-hello-py-1780399472-2dd4803"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("keeps both for distinct image (monorepo)", func(t *testing.T) {
		got := buildJobName("myrepo", "api", ts, sha)
		if want := "build-myrepo-api-1780399472-2dd4803"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("sanitises invalid characters", func(t *testing.T) {
		got := buildJobName("My_App.v2", "My_App.v2", ts, sha)
		if want := "build-my-app-v2-1780399472-2dd4803"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("truncates long names within 63 chars and stays unique", func(t *testing.T) {
		// 50-char app name — a valid DNS-1123 label that would otherwise
		// produce a 126-char Job name and fail at creation.
		long := strings.Repeat("a", 50)
		got := buildJobName(long, long, ts, sha)
		if len(got) > maxJobNameLen {
			t.Fatalf("job name %q is %d chars, exceeds %d", got, len(got), maxJobNameLen)
		}
		if !strings.HasPrefix(got, "build-") || !strings.HasSuffix(got, "-"+sha) {
			t.Errorf("malformed truncated name: %q", got)
		}
		// A different long name with the same prefix must not collide.
		other := strings.Repeat("a", 49) + "b"
		if got == buildJobName(other, other, ts, sha) {
			t.Errorf("distinct long names collided: %q", got)
		}
	})
}

// TestReapJobsForRepo asserts that when a GitRepository is deleted,
// the controller's clean-up sweeps every Job in the controller's
// namespace that was labelled with the dead repo's name — and leaves
// Jobs belonging to other repos alone.
func TestReapJobsForRepo(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	const ns = "flywheel-system"
	doomed := jobWithLabels("build-jig-jig-1-abc", ns, map[string]string{
		"app":  "image-builder",
		"repo": "jig",
	})
	keeper := jobWithLabels("build-tmpapp-tmpapp-1-def", ns, map[string]string{
		"app":  "image-builder",
		"repo": "tmpapp",
	})
	unrelated := jobWithLabels("some-other-job", ns, map[string]string{
		"app": "something-else",
	})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(doomed, keeper, unrelated).
		Build()

	r := &GitRepositoryBuildReconciler{
		Client: c,
		Scheme: scheme,
		Config: Config{Namespace: ns},
	}

	if err := r.reapJobsForRepo(context.Background(), "jig"); err != nil {
		t.Fatalf("reapJobsForRepo: %v", err)
	}

	var remaining batchv1.JobList
	if err := c.List(context.Background(), &remaining); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, j := range remaining.Items {
		names[j.Name] = true
	}
	if names["build-jig-jig-1-abc"] {
		t.Errorf("doomed Job %q should have been deleted", "build-jig-jig-1-abc")
	}
	if !names["build-tmpapp-tmpapp-1-def"] {
		t.Errorf("Job for another live repo was wrongly deleted")
	}
	if !names["some-other-job"] {
		t.Errorf("unrelated Job (different app label) was wrongly deleted")
	}
}

// TestReapJobsForRepo_NoMatchesIsNotAnError asserts the sweep is a
// no-op when nothing matches the labels (e.g. a GitRepository was
// deleted that never had any builds run).
func TestReapJobsForRepo_NoMatchesIsNotAnError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &GitRepositoryBuildReconciler{
		Client: c,
		Scheme: scheme,
		Config: Config{Namespace: "flywheel-system"},
	}
	if err := r.reapJobsForRepo(context.Background(), "nonexistent"); err != nil {
		t.Errorf("expected no error sweeping with zero matches, got %v", err)
	}
}

// TestReconcile_OrphanGRCleanup asserts the controller deletes an
// orphaned GitRepository and reaps its build Jobs when the
// build-config ConfigMap has been pruned. The Open Issue #11 fix
// stamps `kustomize.toolkit.fluxcd.io/reconcile: disabled` on every
// per-app GitRepository, which blocks Flux's own prune — so the
// controller has to clean it up directly.
func TestReconcile_OrphanGRCleanup(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := sourcev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	const (
		appsNS       = "apps"
		controllerNS = "flywheel-system"
		repoName     = "jig"
	)

	// Live GR (Flux-applied, has the disabled annotation) but the
	// ConfigMap was pruned by Flux when the user removed builders/base/jig.
	gr := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      repoName,
			Namespace: appsNS,
			Annotations: map[string]string{
				"kustomize.toolkit.fluxcd.io/reconcile": "disabled",
			},
		},
		Status: sourcev1.GitRepositoryStatus{
			Artifact: &meta.Artifact{
				Revision: "main@sha1:0123456789abcdef0123456789abcdef01234567",
			},
		},
	}
	job := jobWithLabels("build-jig-jig-1-abc", controllerNS, map[string]string{
		"app":  "image-builder",
		"repo": repoName,
	})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gr, job).
		Build()

	r := &GitRepositoryBuildReconciler{
		Client: c,
		Scheme: scheme,
		Config: Config{Namespace: controllerNS, BuilderNamespace: appsNS},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: appsNS, Name: repoName},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// GR should be gone.
	var stillThere sourcev1.GitRepository
	err = c.Get(context.Background(), client.ObjectKey{Namespace: appsNS, Name: repoName}, &stillThere)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected orphan GitRepository deleted, got err=%v", err)
	}

	// Job should be gone.
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("expected orphan Job reaped, %d remain", len(jobs.Items))
	}
}

// TestMapBuildConfigToGitRepository asserts the ConfigMap→GitRepository
// watch mapping: a `<app>-build-config` ConfigMap in the apps namespace
// enqueues a reconcile for the owning GR, and anything else maps to
// nothing. This is what lets the orphan-GR reaper fire when Flux prunes
// the ConfigMap (deleting the ConfigMap does not touch the GR, so the
// primary `For(&GitRepository{})` watch would never re-trigger).
func TestMapBuildConfigToGitRepository(t *testing.T) {
	const appsNS = "apps"
	r := &GitRepositoryBuildReconciler{Config: Config{BuilderNamespace: appsNS}}

	cm := func(name, ns string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	}

	tests := []struct {
		name     string
		obj      client.Object
		wantName string // "" means expect no requests
	}{
		{"build-config in apps ns maps to its GR", cm("sample-app-build-config", appsNS), "sample-app"},
		{"wrong namespace maps to nothing", cm("sample-app-build-config", "flywheel-system"), ""},
		{"non build-config name maps to nothing", cm("kube-root-ca.crt", appsNS), ""},
		{"bare suffix maps to nothing", cm("-build-config", appsNS), ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reqs := r.mapBuildConfigToGitRepository(context.Background(), tc.obj)
			if tc.wantName == "" {
				if len(reqs) != 0 {
					t.Fatalf("expected no requests, got %v", reqs)
				}
				return
			}
			if len(reqs) != 1 {
				t.Fatalf("expected 1 request, got %d: %v", len(reqs), reqs)
			}
			if reqs[0].Namespace != appsNS || reqs[0].Name != tc.wantName {
				t.Errorf("got %s/%s, want %s/%s", reqs[0].Namespace, reqs[0].Name, appsNS, tc.wantName)
			}
		})
	}
}

// TestValidateBuildSecret covers the pure (cluster-free) validation of a
// BuildSecret's id and src: the id must be a single safe path component (the
// '.'/'..'/leading-dot/'/'/ guards), and src must be exactly <secretName>/<key>
// with both halves valid (the zero-slash and ≥2-slash guards).
func TestValidateBuildSecret(t *testing.T) {
	tests := []struct {
		name     string
		secret   BuildSecret
		wantName string
		wantKey  string
		wantErr  bool
	}{
		{"valid", BuildSecret{ID: "GITHUB_TOKEN", Src: "ci-creds/token"}, "ci-creds", "token", false},
		{"valid dotted key", BuildSecret{ID: "npmrc", Src: "ci-creds/.npmrc"}, "ci-creds", ".npmrc", false},
		{"empty id", BuildSecret{ID: "", Src: "ci-creds/token"}, "", "", true},
		{"id is dot", BuildSecret{ID: ".", Src: "ci-creds/token"}, "", "", true},
		{"id is dotdot", BuildSecret{ID: "..", Src: "ci-creds/token"}, "", "", true},
		{"id leading dot", BuildSecret{ID: ".hidden", Src: "ci-creds/token"}, "", "", true},
		{"id with slash", BuildSecret{ID: "a/b", Src: "ci-creds/token"}, "", "", true},
		{"src zero slashes", BuildSecret{ID: "TOK", Src: "ci-creds"}, "", "", true},
		{"src two slashes", BuildSecret{ID: "TOK", Src: "ci-creds/sub/token"}, "", "", true},
		{"src empty name", BuildSecret{ID: "TOK", Src: "/token"}, "", "", true},
		{"src empty key", BuildSecret{ID: "TOK", Src: "ci-creds/"}, "", "", true},
		{"src invalid name", BuildSecret{ID: "TOK", Src: "Ci_Creds/token"}, "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name, key, err := validateBuildSecret(tc.secret)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %+v, got name=%q key=%q", tc.secret, name, key)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if name != tc.wantName || key != tc.wantKey {
				t.Errorf("got name=%q key=%q, want name=%q key=%q", name, key, tc.wantName, tc.wantKey)
			}
		})
	}
}

// TestBuildSecretRender asserts the two render shapes: a flat --secret list
// (id + projected path) and per-Secret projected sources grouped by Secret
// name in first-seen order, so multiple keys from one Secret collapse into a
// single projected-volume source.
func TestBuildSecretRender(t *testing.T) {
	secrets := []BuildSecret{
		{ID: "GITHUB_TOKEN", Src: "ci-creds/token"},
		{ID: "NPM_TOKEN", Src: "ci-creds/npm"}, // same Secret, second key
		{ID: "PIP_CONF", Src: "pip-creds/conf"},
	}
	flat, sources, err := buildSecretRender(secrets)
	if err != nil {
		t.Fatalf("buildSecretRender: %v", err)
	}
	if len(flat) != 3 {
		t.Fatalf("want 3 flat secrets, got %d", len(flat))
	}
	if flat[0].ID != "GITHUB_TOKEN" || flat[0].Path != "/run/build-secrets/GITHUB_TOKEN" {
		t.Errorf("flat[0] = %+v", flat[0])
	}
	// ci-creds (2 items) then pip-creds (1 item), order preserved.
	if len(sources) != 2 {
		t.Fatalf("want 2 grouped sources, got %d: %+v", len(sources), sources)
	}
	if sources[0].SecretName != "ci-creds" || len(sources[0].Items) != 2 {
		t.Errorf("source[0] should be ci-creds with 2 items, got %+v", sources[0])
	}
	if sources[0].Items[0].Key != "token" || sources[0].Items[0].Path != "GITHUB_TOKEN" {
		t.Errorf("source[0].Items[0] = %+v", sources[0].Items[0])
	}
	if sources[1].SecretName != "pip-creds" || len(sources[1].Items) != 1 {
		t.Errorf("source[1] should be pip-creds with 1 item, got %+v", sources[1])
	}
}

// TestRenderJob_WithSecrets asserts the secret wiring renders end-to-end: the
// buildctl `--secret` flag, the read-only mount, and the projected volume with
// the deliberate 0444 defaultMode and items grouped under their Secret.
func TestRenderJob_WithSecrets(t *testing.T) {
	r := &GitRepositoryBuildReconciler{
		Config: Config{Namespace: "flywheel-system", ClientName: "acme", ClusterName: "acme-local", Registry: "acme-local-registry"},
	}
	gr := sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "sample-app", Namespace: "apps"},
		Spec:       sourcev1.GitRepositorySpec{URL: "http://git/sample-app.git"},
		Status:     sourcev1.GitRepositoryStatus{Artifact: &meta.Artifact{URL: "http://src/artifact.tar.gz"}},
	}
	b := BuildEntry{
		Image: "sample-app", Context: ".", Dockerfile: "Dockerfile",
		Secrets: []BuildSecret{
			{ID: "GITHUB_TOKEN", Src: "ci-creds/token"},
			{ID: "NPM_TOKEN", Src: "ci-creds/npm"},
		},
	}
	job, err := r.renderJob("build-sample-app-1-abc1234", gr, b, strings.Repeat("a", 40), "abc1234", 1)
	if err != nil {
		t.Fatalf("renderJob: %v", err)
	}
	spec := job.Spec.Template.Spec
	args := strings.Join(spec.Containers[0].Args, " ")
	if !strings.Contains(args, "id=GITHUB_TOKEN,src=/run/build-secrets/GITHUB_TOKEN") {
		t.Errorf("missing --secret flag for GITHUB_TOKEN; args: %s", args)
	}
	if !strings.Contains(args, "id=NPM_TOKEN,src=/run/build-secrets/NPM_TOKEN") {
		t.Errorf("missing --secret flag for NPM_TOKEN; args: %s", args)
	}

	mounts := spec.Containers[0].VolumeMounts
	if len(mounts) != 1 || mounts[0].MountPath != "/run/build-secrets" || !mounts[0].ReadOnly {
		t.Fatalf("want one read-only mount at /run/build-secrets, got %+v", mounts)
	}

	if len(spec.Volumes) != 1 || spec.Volumes[0].Projected == nil {
		t.Fatalf("want one projected volume, got %+v", spec.Volumes)
	}
	proj := spec.Volumes[0].Projected
	if proj.DefaultMode == nil || *proj.DefaultMode != 0o444 {
		t.Errorf("want projected defaultMode 0444 (octal); got %v", proj.DefaultMode)
	}
	// Both keys collapse into a single source for the shared Secret "ci-creds".
	if len(proj.Sources) != 1 || proj.Sources[0].Secret == nil {
		t.Fatalf("want one projected secret source, got %+v", proj.Sources)
	}
	src := proj.Sources[0].Secret
	if src.Name != "ci-creds" || len(src.Items) != 2 {
		t.Errorf("want ci-creds with 2 items, got name=%q items=%+v", src.Name, src.Items)
	}
}

// TestValidateBuildSecrets_Cluster covers the fail-closed cluster check:
// missing Secret, missing key, and empty value all error; a present non-empty
// value passes. The check reads from the controller's (builder) namespace.
func TestValidateBuildSecrets_Cluster(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	const ns = "flywheel-system"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "ci-creds", Namespace: ns},
			Data:       map[string][]byte{"token": []byte("s3cr3t"), "empty": {}},
		},
	).Build()
	r := &GitRepositoryBuildReconciler{Client: c, Scheme: scheme, Config: Config{Namespace: ns}}

	cases := []struct {
		name    string
		secrets []BuildSecret
		wantErr bool
	}{
		{"present non-empty", []BuildSecret{{ID: "T", Src: "ci-creds/token"}}, false},
		{"missing secret", []BuildSecret{{ID: "T", Src: "nope/token"}}, true},
		{"missing key", []BuildSecret{{ID: "T", Src: "ci-creds/absent"}}, true},
		{"empty value", []BuildSecret{{ID: "T", Src: "ci-creds/empty"}}, true},
		{"malformed src", []BuildSecret{{ID: "T", Src: "ci-creds"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := r.validateBuildSecrets(context.Background(), tc.secrets)
			if tc.wantErr != (err != nil) {
				t.Errorf("validateBuildSecrets(%+v) err=%v, wantErr=%v", tc.secrets, err, tc.wantErr)
			}
		})
	}
}

func jobWithLabels(name, ns string, labels map[string]string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
	}
}

// TestRenderJob_BuildKitWiring guards the (now load-bearing) build-job
// template: the build container must stay named "build" (the image-scan
// controller pokes off that container's exit), drive buildctl against the
// configured daemon, push to the per-build destination with insecure=true,
// and map context/dockerfile correctly.
func TestRenderJob_BuildKitWiring(t *testing.T) {
	r := &GitRepositoryBuildReconciler{
		Config: Config{
			Namespace:   "flywheel-system",
			ClientName:  "acme",
			ClusterName: "acme-local",
			Registry:    "acme-local-registry",
			// BuildKitAddr left empty → exercises the default.
		},
	}
	gr := sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "sample-app", Namespace: "apps"},
		Spec: sourcev1.GitRepositorySpec{
			URL: "http://git-server.flywheel-system.svc.cluster.local:8080/sample-app.git",
		},
		Status: sourcev1.GitRepositoryStatus{
			Artifact: &meta.Artifact{URL: "http://src/artifact.tar.gz"},
		},
	}
	b := BuildEntry{Image: "sample-app", Context: "sub", Dockerfile: "Dockerfile"}

	job, err := r.renderJob("build-sample-app-1-abc1234", gr, b, "abc1234abc1234abc1234abc1234abc1234abcd", "abc1234", 1)
	if err != nil {
		t.Fatalf("renderJob: %v", err)
	}

	spec := job.Spec.Template.Spec
	cs := spec.Containers
	if len(cs) != 1 || cs[0].Name != "build" {
		t.Fatalf("build container must be the single container named 'build' (scan-poke depends on it); got %+v", cs)
	}
	args := append(append([]string{}, cs[0].Command...), cs[0].Args...)
	joined := strings.Join(args, " ")

	wantSubstrings := []string{
		"buildctl",
		"--addr tcp://buildkitd.flywheel-system:1234",
		// Remote git context: <repo>.git#<fullSHA>:<subdir>, cloned by the
		// daemon. The ref is the full SHA so provenance == what Flux observed.
		"context=http://git-server.flywheel-system.svc.cluster.local:8080/sample-app.git#abc1234abc1234abc1234abc1234abc1234abcd:sub",
		"filename=Dockerfile",
		// Short commit SHA injected as a build-arg so Dockerfiles can stamp it.
		"build-arg:COMMIT=abc1234",
		"k3d-acme-local-registry:5000/acme/sample-app:1-abc1234",
		"registry.insecure=true",
		"push=true",
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(joined, w) {
			t.Errorf("rendered build args missing %q\ngot: %s", w, joined)
		}
	}
	// Must NOT still be invoking kaniko.
	if strings.Contains(joined, "executor") || strings.Contains(joined, "--destination") {
		t.Errorf("rendered args still reference kaniko: %s", joined)
	}
	// The fetch-source apparatus must be gone: no initContainer, no local
	// workspace volume/mount, no `--local`. buildkitd fetches the context.
	if len(spec.InitContainers) != 0 {
		t.Errorf("expected no initContainers (git context is daemon-side); got %+v", spec.InitContainers)
	}
	if len(spec.Volumes) != 0 || len(cs[0].VolumeMounts) != 0 {
		t.Errorf("expected no workspace volume/mounts; volumes=%+v mounts=%+v", spec.Volumes, cs[0].VolumeMounts)
	}
	if strings.Contains(joined, "--local") || strings.Contains(joined, "/workspace") {
		t.Errorf("rendered args still reference a local context: %s", joined)
	}
}
