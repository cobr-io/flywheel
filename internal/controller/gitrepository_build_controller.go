package controller

import (
	"context"
	_ "embed"
	"fmt"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"
)

//go:embed templates/build-job.yaml
var buildJobTemplate string

// buildConfigSuffix is appended to a GitRepository's name to form the name
// of its sibling build-config ConfigMap (e.g. `sample-app` →
// `sample-app-build-config`).
const buildConfigSuffix = "-build-config"

// BuildEntry mirrors one entry under `builds:` in the build-config ConfigMap.
// Field names match the legacy `[[builds]]` TOML format so monorepos producing
// multiple images Just Work. JSON tags are used because sigs.k8s.io/yaml
// converts YAML → JSON → Go struct.
type BuildEntry struct {
	Image      string `json:"image"`
	Context    string `json:"context,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
	// Target selects a multi-stage build stage. Empty => buildkit builds the
	// Dockerfile's last stage (its long-standing default).
	Target  string        `json:"target,omitempty"`
	Secrets []BuildSecret `json:"secrets,omitempty"`
}

// BuildSecret references a Kubernetes Secret key to expose to the build as a
// BuildKit `--secret`. ID is the BuildKit secret id the Dockerfile reads via
// `RUN --mount=type=secret,id=<ID>`; Src is `<secretName>/<key>` naming the
// Secret (in the builder/Job namespace) and the data key to project.
type BuildSecret struct {
	ID  string `json:"id"`
	Src string `json:"src"`
}

// secretMountPath is where projected build-secret files land inside the thin
// buildctl client Pod. buildctl reads each file client-side and streams it to
// the warm daemon over the build session, so the file never touches the
// daemon's filesystem.
const secretMountPath = "/run/build-secrets"

var (
	// buildSecretIDRe constrains a BuildKit secret id. The id doubles as a
	// file name under secretMountPath, so it must be a single safe path
	// component — see validateBuildSecret for the explicit dot/traversal guard.
	buildSecretIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	// secretNameRe is a permissive RFC1123 DNS-subdomain check for the
	// referenced Secret name; the apiserver is the final authority.
	secretNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	// secretKeyRe matches the characters Kubernetes allows in a Secret data key.
	secretKeyRe = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
)

// validateBuildSecret checks one BuildSecret is well-formed and returns the
// parsed Secret name and key. It is the pure, cluster-free half of validation:
// the id must be a single safe path component (no '.', '..', leading dot, or
// '/' that could escape secretMountPath), and Src must be exactly
// `<secretName>/<key>` with both halves valid.
func validateBuildSecret(s BuildSecret) (name, key string, err error) {
	id := s.ID
	if id == "" {
		return "", "", fmt.Errorf("build secret: id is empty")
	}
	// The id becomes a file name under secretMountPath. Reject path traversal
	// and dotfiles explicitly: the apiserver's projected-volume path check
	// would also reject these, but with a cryptic Job-create failure instead
	// of this clear message.
	if id == "." || id == ".." || strings.HasPrefix(id, ".") || strings.Contains(id, "/") {
		return "", "", fmt.Errorf("build secret id %q must not be '.', '..', start with a dot, or contain '/'", id)
	}
	if !buildSecretIDRe.MatchString(id) {
		return "", "", fmt.Errorf("build secret id %q must match %s", id, buildSecretIDRe)
	}
	// Exactly one '/': Cut splits on the first, so a second slash lands in key.
	name, key, found := strings.Cut(s.Src, "/")
	if !found || strings.Contains(key, "/") {
		return "", "", fmt.Errorf("build secret %q: src %q must be exactly <secretName>/<key> (one '/')", id, s.Src)
	}
	if name == "" || key == "" {
		return "", "", fmt.Errorf("build secret %q: src %q has an empty secret name or key", id, s.Src)
	}
	if !secretNameRe.MatchString(name) {
		return "", "", fmt.Errorf("build secret %q: invalid secret name %q in src", id, name)
	}
	if !secretKeyRe.MatchString(key) {
		return "", "", fmt.Errorf("build secret %q: invalid key %q in src", id, key)
	}
	return name, key, nil
}

type buildConfig struct {
	Builds  []BuildEntry  `json:"builds"`
	Secrets []BuildSecret `json:"secrets,omitempty"`
}

type GitRepositoryBuildReconciler struct {
	client.Client
	Config Config
}

func (r *GitRepositoryBuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sourcev1.GitRepository{}).
		// Also reconcile when a `<app>-build-config` ConfigMap changes.
		// The orphan-GR branch in Reconcile fires on `ConfigMap NotFound +
		// GR live`, but deleting the ConfigMap does not touch the
		// GitRepository object, so the `For` watch alone never re-triggers
		// a reconcile when Flux prunes the ConfigMap (the GR carries
		// `kustomize.toolkit.fluxcd.io/reconcile: disabled`, so Flux can't
		// prune the GR itself either). Watching the ConfigMap and mapping
		// its delete event back to the owning GR is what lets the reaper
		// run promptly instead of waiting for an incidental GR event.
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.mapBuildConfigToGitRepository)).
		Complete(r)
}

// mapBuildConfigToGitRepository turns an event on a `<app>-build-config`
// ConfigMap in the builder namespace into a reconcile request for the
// GitRepository it belongs to. Events on any other ConfigMap (wrong
// namespace, or a name without the build-config suffix) map to nothing.
func (r *GitRepositoryBuildReconciler) mapBuildConfigToGitRepository(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.Config.BuilderNamespace {
		return nil
	}
	name := obj.GetName()
	if !strings.HasSuffix(name, buildConfigSuffix) {
		return nil
	}
	repo := strings.TrimSuffix(name, buildConfigSuffix)
	if repo == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: repo},
	}}
}

func (r *GitRepositoryBuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("gitrepository", req.NamespacedName)

	// GitRepositories outside the builder namespace are not our concern.
	if req.Namespace != r.Config.BuilderNamespace {
		return ctrl.Result{}, nil
	}

	var gr sourcev1.GitRepository
	if err := r.Get(ctx, req.NamespacedName, &gr); err != nil {
		if apierrors.IsNotFound(err) {
			// GitRepository was deleted (e.g. `builders/base/<name>/`
			// removed from the gitops repo). Cross-namespace
			// OwnerReferences are not honoured by Kubernetes garbage
			// collection — the Jobs live in flywheel-system, the
			// GitRepository lives in `apps` — so we sweep here instead.
			if err := r.reapJobsForRepo(ctx, req.Name); err != nil {
				log.Error(err, "reaping orphaned build jobs failed")
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if gr.Status.Artifact == nil {
		return ctrl.Result{}, nil
	}

	// Skip the self-builder: flywheel and flux-system aren't apps to be
	// rebuilt on commits.
	if gr.Name == "flux-system" || gr.Name == "flywheel" {
		return ctrl.Result{}, nil
	}

	cfg, err := r.loadBuildConfig(ctx, gr.Namespace, gr.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Build-config ConfigMap is gone but the GitRepository
			// remains. This typically means the user removed
			// `builders/base/<name>/` and Flux pruned the ConfigMap, but
			// could not prune the GitRepository because git-auto-sync
			// stamped `kustomize.toolkit.fluxcd.io/reconcile: disabled`
			// on it (per-app Open Issue #11 fix). Treat the GR as
			// orphaned: reap its Jobs and delete the GR directly (annot
			// only blocks Flux's reconciles, not generic API deletes).
			log.Info("build-config ConfigMap missing; reaping orphan GitRepository + Jobs")
			if err := r.reapJobsForRepo(ctx, gr.Name); err != nil {
				return ctrl.Result{}, err
			}
			if err := r.Delete(ctx, &gr); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete orphan gitrepository %s/%s: %w",
					gr.Namespace, gr.Name, err)
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if len(cfg.Builds) == 0 {
		log.Info("build-config has no builds; nothing to do")
		return ctrl.Result{}, nil
	}

	revision := gr.Status.Artifact.Revision
	fullSHA := parseSHA(revision)
	if fullSHA == "" {
		log.Info("revision missing sha component", "revision", revision)
		return ctrl.Result{}, nil
	}
	shortSHA := fullSHA[:7]
	ts := gr.Status.Artifact.LastUpdateTime.Unix()

	for _, b := range cfg.Builds {
		if b.Context == "" {
			b.Context = "."
		}
		if b.Dockerfile == "" {
			b.Dockerfile = "Dockerfile"
		}

		jobName := buildJobName(gr.Name, b.Image, ts, shortSHA)

		var existing batchv1.Job
		if err := r.Get(ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: jobName}, &existing); err == nil {
			log.V(1).Info("job exists, skipping", "job", jobName)
			continue
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}

		// Fail closed before creating the Job: a malformed or missing secret
		// would otherwise surface as an opaque mid-build failure. Returning the
		// error requeues, so the build recovers once the Secret appears.
		if err := r.validateBuildSecrets(ctx, b.Secrets); err != nil {
			log.Error(err, "invalid build secrets; will retry", "image", b.Image)
			return ctrl.Result{}, err
		}

		job, err := r.renderJob(jobName, gr, b, fullSHA, shortSHA, ts)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		log.Info("created build job", "job", jobName, "image", b.Image, "commit", shortSHA)
	}

	return ctrl.Result{}, nil
}

func (r *GitRepositoryBuildReconciler) loadBuildConfig(ctx context.Context, ns, repoName string) (*buildConfig, error) {
	var cm corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: repoName + buildConfigSuffix}, &cm); err != nil {
		return nil, err
	}
	raw, ok := cm.Data["builds.yaml"]
	if !ok {
		return nil, fmt.Errorf("build-config ConfigMap %s missing 'builds.yaml' key", cm.Name)
	}
	var cfg buildConfig
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("parse builds.yaml: %w", err)
	}
	for i := range cfg.Builds {
		if cfg.Builds[i].Secrets == nil {
			cfg.Builds[i].Secrets = cfg.Secrets
		}
	}
	return &cfg, nil
}

// validateBuildSecrets fail-closes a build whose secrets are malformed or whose
// referenced Secret/key is missing or empty in the builder namespace. Returning
// an error requeues the GitRepository, so a build that races ahead of its Secret
// recovers automatically once the Secret lands. The Secret *value* is read here
// only to assert it is non-empty (an empty token otherwise fails opaquely
// mid-build); it is never logged.
func (r *GitRepositoryBuildReconciler) validateBuildSecrets(ctx context.Context, secrets []BuildSecret) error {
	for _, s := range secrets {
		name, key, err := validateBuildSecret(s)
		if err != nil {
			return err
		}
		var sec corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: name}, &sec); err != nil {
			return fmt.Errorf("build secret %q: get Secret %s/%s: %w", s.ID, r.Config.Namespace, name, err)
		}
		val, ok := sec.Data[key]
		if !ok {
			return fmt.Errorf("build secret %q: Secret %s/%s has no key %q", s.ID, r.Config.Namespace, name, key)
		}
		if len(val) == 0 {
			return fmt.Errorf("build secret %q: Secret %s/%s key %q is empty", s.ID, r.Config.Namespace, name, key)
		}
	}
	return nil
}

// renderSecret is one BuildKit `--secret id=<ID>,src=<Path>` flag plus the file
// path the projected volume places the material at.
type renderSecret struct {
	ID   string
	Path string
}

// secretSource groups one Secret's projected items under a single
// projected-volume source. BuildSecrets that reference the same Secret name
// collapse into one source with multiple items.
type secretSource struct {
	SecretName string
	Items      []secretItem
}

type secretItem struct {
	Key  string // data key within the Secret
	Path string // file name under secretMountPath (the BuildKit id)
}

// buildSecretRender turns a build's secrets into the two shapes the template
// needs: a flat list for the buildctl `--secret` args, and per-Secret sources
// for the projected volume (grouped by Secret name, first-seen order). It
// re-runs validateBuildSecret so the function is correct in isolation; callers
// that already validated simply never hit the error path.
func buildSecretRender(secrets []BuildSecret) ([]renderSecret, []secretSource, error) {
	var flat []renderSecret
	var order []string
	byName := map[string]*secretSource{}
	for _, s := range secrets {
		name, key, err := validateBuildSecret(s)
		if err != nil {
			return nil, nil, err
		}
		flat = append(flat, renderSecret{ID: s.ID, Path: secretMountPath + "/" + s.ID})
		src, ok := byName[name]
		if !ok {
			src = &secretSource{SecretName: name}
			byName[name] = src
			order = append(order, name)
		}
		src.Items = append(src.Items, secretItem{Key: key, Path: s.ID})
	}
	sources := make([]secretSource, 0, len(order))
	for _, n := range order {
		sources = append(sources, *byName[n])
	}
	return flat, sources, nil
}

type renderCtx struct {
	JobName       string
	Repo          string
	Image         string
	Context       string
	Dockerfile    string
	Target        string
	GitContextURL string
	Destination   string
	FullSHA       string
	ShortSHA      string
	Timestamp     int64
	Namespace     string
	ClientName    string
	ClusterName   string
	BuildKitAddr  string
	// BuildKitClientImage is the thin client container image the Job runs —
	// Config.BuildKitClientImageOrDefault(): the mirrored in-cluster ref, or
	// the upstream Hub ref as fallback (issue #107).
	BuildKitClientImage string
	Secrets             []renderSecret
	SecretVolumes       []secretSource
}

// gitContextURL builds the remote git context buildkitd fetches directly,
// in BuildKit's `<repo>.git#<ref>[:<subdir>]` form. Feeding the daemon the
// git ref instead of a pre-fetched source tarball lets it clone server-side
// (warm, cached) and removes the fetch-source initContainer from the build
// Pod entirely. The ref is the full commit SHA, so what's built is exactly
// the revision Flux observed — same provenance as the artifact path.
func gitContextURL(repoURL, fullSHA, context string) string {
	u := repoURL + "#" + fullSHA
	if context != "" && context != "." {
		u += ":" + context
	}
	return u
}

func (r *GitRepositoryBuildReconciler) renderJob(jobName string, gr sourcev1.GitRepository, b BuildEntry, fullSHA, shortSHA string, ts int64) (*batchv1.Job, error) {
	tmpl, err := template.New("job").Parse(buildJobTemplate)
	if err != nil {
		return nil, err
	}
	secs, secVols, err := buildSecretRender(b.Secrets)
	if err != nil {
		return nil, err
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, renderCtx{
		JobName:             jobName,
		Repo:                gr.Name,
		Image:               b.Image,
		Context:             b.Context,
		Dockerfile:          b.Dockerfile,
		Target:              b.Target,
		GitContextURL:       gitContextURL(gr.Spec.URL, fullSHA, b.Context),
		Destination:         fmt.Sprintf("%s/%s/%s:%d-%s", r.Config.RegistryURL(), r.Config.ClientName, b.Image, ts, shortSHA),
		FullSHA:             fullSHA,
		ShortSHA:            shortSHA,
		Timestamp:           ts,
		Namespace:           r.Config.Namespace,
		ClientName:          r.Config.ClientName,
		ClusterName:         r.Config.ClusterName,
		BuildKitAddr:        r.Config.BuildKitAddrOrDefault(),
		BuildKitClientImage: r.Config.BuildKitClientImageOrDefault(),
		Secrets:             secs,
		SecretVolumes:       secVols,
	}); err != nil {
		return nil, err
	}

	var job batchv1.Job
	if err := yaml.Unmarshal([]byte(buf.String()), &job); err != nil {
		return nil, fmt.Errorf("unmarshal rendered Job: %w (rendered: %s)", err, buf.String())
	}
	return &job, nil
}

// reapJobsForRepo deletes every build Job (and cascading
// PropagationPolicy=Background — its Pods) labelled with
// `app=image-builder,repo=<repoName>` in the controller's namespace.
// Called when the source GitRepository has been deleted, since
// Kubernetes garbage collection cannot follow cross-namespace owner
// references and the Jobs would otherwise outlive the resource that
// triggered them.
func (r *GitRepositoryBuildReconciler) reapJobsForRepo(ctx context.Context, repoName string) error {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs,
		client.InNamespace(r.Config.Namespace),
		client.MatchingLabels{"app": "image-builder", "repo": repoName},
	); err != nil {
		return fmt.Errorf("list build jobs for repo %q: %w", repoName, err)
	}
	propagation := metav1.DeletePropagationBackground
	for i := range jobs.Items {
		if err := r.Delete(ctx, &jobs.Items[i], &client.DeleteOptions{
			PropagationPolicy: &propagation,
		}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete job %s/%s: %w",
				jobs.Items[i].Namespace, jobs.Items[i].Name, err)
		}
	}
	return nil
}

// parseSHA extracts the 40-char SHA from Flux's revision format `<branch>@sha1:<full-sha>`.
func parseSHA(revision string) string {
	idx := strings.Index(revision, "sha1:")
	if idx < 0 {
		return ""
	}
	sha := revision[idx+len("sha1:"):]
	if len(sha) != 40 {
		return ""
	}
	return sha
}

func sanitize(s string) string {
	return strings.NewReplacer("/", "-", "_", "-", ".", "-").Replace(strings.ToLower(s))
}

// maxJobNameLen is the Kubernetes ceiling on a Job's name: the Job controller
// stamps the name verbatim into the `job-name` Pod label, and label values are
// capped at 63 characters. A longer name makes Pod (hence build) creation fail.
const maxJobNameLen = 63

// buildJobName composes the build Job (and Pod) name as
// `build-<human>-<ts>-<shortSHA>`. The `<ts>-<shortSHA>` suffix already makes
// the name unique per commit, so `<human>` (the repo/image identifier) is
// purely for readability and is safe to shorten:
//
//   - repo and image are deduped when equal (the common single-image case,
//     where `add-app` defaults the image name to the app name — otherwise the
//     app name appears twice).
//   - if the result would exceed Kubernetes' 63-char Job-name limit, the human
//     part is truncated and a short content hash appended, so arbitrarily long
//     (but still valid ≤63-char DNS-1123) app names build instead of silently
//     failing at Job creation.
func buildJobName(repo, image string, ts int64, shortSHA string) string {
	human := sanitize(repo)
	if img := sanitize(image); img != human {
		human += "-" + img
	}

	// Budget for the human part = 63 minus the fixed, non-human characters:
	// "build-" + "-" + <ts decimal> + "-" + <shortSHA>.
	fixed := len("build-") + 1 + len(strconv.FormatInt(ts, 10)) + 1 + len(shortSHA)
	if budget := maxJobNameLen - fixed; len(human) > budget {
		h := fnv.New32a()
		_, _ = h.Write([]byte(human))
		hash := fmt.Sprintf("%08x", h.Sum32())
		keep := max(budget-1-len(hash), 0) // room for "-" + hash
		human = strings.TrimRight(human[:keep], "-") + "-" + hash
	}

	return fmt.Sprintf("build-%s-%d-%s", human, ts, shortSHA)
}
