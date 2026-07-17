// Reconciler (this file) resolves the design's discovery record — one
// GitRepository per per-app worktree — to a Ticker and drives it on every
// reconcile. It lives in this package rather than internal/controller (home
// of the image-builder-controller's reconcilers) because the design's own
// API/data model section lists it as part of internal/appsync: it needs
// nothing internal/controller.Config carries (registry/buildkit/cluster
// identity), and keeping Ticker, FluxPatcher and Reconciler in one package
// avoids a cross-package dependency for what is otherwise a single small
// type. See docs/designs/2026-07-17-per-app-sync-controller-design.md
// "Approach" and docs/plans/2026-07-17-per-app-sync-controller-plan.md
// "Phase 3".
package appsync

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
)

// defaultStallInterval is the requeue after a TickResult.Stalled tick (a
// rebase conflict) — parity with sync.sh's 30s sleep on the same condition
// (design step 7 / plan Q4).
const defaultStallInterval = 30 * time.Second

// legacyDeploymentPrefix names the per-app git-auto-sync sidecar Deployment
// this controller replaces: `git-auto-sync-<gr.Name>` in BuilderNamespace
// (manifests/per-app-template/git-auto-sync.yaml.tmpl — deleted for new apps
// in Phase 4, but still present in any client repo that hasn't migrated yet).
// Note this is keyed by the GitRepository's metadata.name (the app name), NOT
// the worktree basename worktreeDir derives from spec.url — the legacy
// template names the Deployment after AppName, same as the GR itself.
const legacyDeploymentPrefix = "git-auto-sync-"

// Reconciler drives one Ticker per per-app GitRepository in BuilderNamespace
// (design "Reconciler on per-app GitRepositories"). The controller-runtime
// workqueue guarantees one GitRepository is never reconciled concurrently
// with itself, so a given app's Ticker is only ever driven by one goroutine
// at a time; MaxConcurrentReconciles bounds how many DIFFERENT apps tick in
// parallel.
type Reconciler struct {
	client.Client

	// WorkspacesMount is the hostPath every app worktree is mounted under.
	// Dir = filepath.Join(WorkspacesMount, basename(spec.url) minus ".git").
	WorkspacesMount string
	// GitServerURLPrefix filters discovery to per-app GitRepositories: only
	// spec.url values with this prefix are ours (design Open Question 1) —
	// anything else is left alone (no requeue, no error): a future
	// non-app GitRepository in the same namespace must not be tugged at.
	GitServerURLPrefix string
	// BuilderNamespace is where per-app GitRepositories AND the legacy
	// git-auto-sync-<app> sidecar Deployments (the interlock check) live.
	BuilderNamespace string
	// PollInterval is the requeue after any tick that did not stall.
	PollInterval time.Duration
	// StallInterval is the requeue after TickResult.Stalled; zero uses
	// defaultStallInterval.
	StallInterval time.Duration
	// MaxConcurrentReconciles bounds cross-app reconcile parallelism (each
	// app is already serialized against itself by the workqueue).
	MaxConcurrentReconciles int
	// ExecTimeout bounds every git exec a cached Ticker runs; zero uses
	// Ticker's own default.
	ExecTimeout time.Duration
	// Logf receives every Ticker's log lines, app-name-prefixed. Optional.
	Logf func(string, ...any)

	mu      sync.Mutex
	tickers map[types.NamespacedName]*Ticker
	warned  map[types.NamespacedName]bool // legacy-interlock warn-once, forever
}

// SetupWithManager registers the Reconciler to watch GitRepository objects,
// bounding cross-app parallelism to MaxConcurrentReconciles.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sourcev1.GitRepository{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r)
}

// Reconcile performs one sync tick for the GitRepository named by req, or
// determines the GitRepository is not ours / no longer exists and does
// nothing. See TickResult's field docs for what a tick can do; this method's
// job is mapping that result onto a requeue interval.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("gitrepository", req.NamespacedName)

	var gr sourcev1.GitRepository
	if err := r.Get(ctx, req.NamespacedName, &gr); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted between enqueue and reconcile (or never existed) —
			// nothing to tick. A stale cache entry under this key, if any, is
			// harmless: it is only ever reused if a GitRepository of the same
			// name/namespace reappears, which is the correct behavior anyway.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !strings.HasPrefix(gr.Spec.URL, r.GitServerURLPrefix) {
		return ctrl.Result{}, nil
	}

	// Legacy interlock (design "Legacy interlock"): a Deployment from the old
	// bash sidecar still exists for this app. Two writers on one worktree is
	// the exact hazard this controller exists to remove, so skip entirely
	// until the client repo's migration deletes builders/base/<app>/
	// git-auto-sync.yaml (Flux prunes the Deployment). Warn once so the
	// operator can see WHY an app isn't being driven by the new controller,
	// without spamming a line every poll interval forever.
	depName := legacyDeploymentPrefix + gr.Name
	var dep appsv1.Deployment
	switch err := r.Get(ctx, types.NamespacedName{Namespace: r.BuilderNamespace, Name: depName}, &dep); {
	case err == nil:
		if r.markWarnedOnce(req.NamespacedName) {
			log.Info("legacy git-auto-sync sidecar Deployment still present; skipping until it is removed from the gitops repo",
				"deployment", depName)
		}
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}

	// A GitRepository that matched the URL prefix but whose path has no usable
	// basename (e.g. spec.url == the prefix with nothing after it) can't be
	// mapped to any worktree directory — a permanent misconfiguration, not a
	// transient condition the poll interval would ever resolve. Return the
	// error so controller-runtime backs off this one app with increasing
	// delay instead of hammering it every PollInterval forever.
	dir, err := worktreeDir(r.WorkspacesMount, gr.Spec.URL)
	if err != nil {
		log.Error(err, "cannot derive a worktree directory from spec.url")
		return ctrl.Result{}, err
	}
	t := r.tickerFor(req.NamespacedName, dir, gr.Spec.URL)

	// A worktree directory that doesn't exist yet (the clone step hasn't run)
	// or exists but isn't a git repo is NOT a Tick error: Tick's first step
	// (`git symbolic-ref --short HEAD`) fails exactly the same way a detached
	// HEAD does — err != nil at the very first check — and Tick treats that
	// as "nothing to sync yet", returning (TickResult{}, nil). So there is no
	// special case to write here: the branch below already maps that skip
	// onto a normal PollInterval requeue, which self-heals as soon as the
	// worktree appears, with no crash loop and no error in the interim.
	res, err := t.Tick(ctx, trackedBranch(&gr))
	if err != nil {
		log.Error(err, "sync tick failed; backing off this app only", "dir", dir)
		return ctrl.Result{}, err
	}

	if res.Stalled {
		return ctrl.Result{RequeueAfter: r.stallInterval()}, nil
	}
	return ctrl.Result{RequeueAfter: r.PollInterval}, nil
}

// tickerFor returns the cached Ticker for key, creating it on first use.
// ExecTimeout, Logf and the FluxPatcher are wired once (they never change for
// a given app); Dir/BareURL are refreshed every call, which is cheap and
// stays correct even in the unusual case that spec.url is edited in place.
func (r *Reconciler) tickerFor(key types.NamespacedName, dir, bareURL string) *Ticker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tickers == nil {
		r.tickers = make(map[types.NamespacedName]*Ticker)
	}
	t, ok := r.tickers[key]
	if !ok {
		t = &Ticker{
			Flux:        &gitRepoFlux{Client: r.Client, Name: key.Name, Namespace: key.Namespace},
			ExecTimeout: r.ExecTimeout,
			Logf:        r.appLogf(key.Name),
		}
		r.tickers[key] = t
	}
	t.Dir = dir
	t.BareURL = bareURL
	return t
}

// appLogf prefixes every log line from name's Ticker with its app name, so
// the shared process's `kubectl logs` output attributes each line to the
// right app (design "Log parity with sync.sh ... app-prefixed").
func (r *Reconciler) appLogf(name string) func(string, ...any) {
	if r.Logf == nil {
		return nil
	}
	return func(format string, args ...any) {
		r.Logf(name+": "+format, args...)
	}
}

// markWarnedOnce records that the legacy-interlock warning has fired for key
// and reports whether this call is the first (i.e. whether the caller should
// actually log). Forever-once: once warned, an app never warns again for the
// life of this process, even if the Deployment flaps in and out of existence.
func (r *Reconciler) markWarnedOnce(key types.NamespacedName) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.warned == nil {
		r.warned = make(map[types.NamespacedName]bool)
	}
	if r.warned[key] {
		return false
	}
	r.warned[key] = true
	return true
}

func (r *Reconciler) stallInterval() time.Duration {
	if r.StallInterval > 0 {
		return r.StallInterval
	}
	return defaultStallInterval
}

// trackedBranch reads the GR's live spec.ref.branch, or "" when unset — an
// absent Reference or empty Branch both mean the GR hasn't declared a branch
// yet, which Tick's own branch-follow step treats identically (it patches
// spec.ref.branch to whatever the worktree is on).
func trackedBranch(gr *sourcev1.GitRepository) string {
	if gr.Spec.Reference == nil {
		return ""
	}
	return gr.Spec.Reference.Branch
}

// worktreeDir derives an app's worktree directory from its GitRepository's
// spec.url: the URL path's basename (the bare repo's directory name on the
// git-server, e.g. ".../sample-app.git"), minus the ".git" suffix, joined
// under mount. This is deliberately the URL's basename, NOT the
// GitRepository's own metadata.name — the two may differ (the GR is named
// after the app; its source is the worktree's bare repo, keyed by directory
// basename — see manifests/per-app-template/gitrepository.yaml.tmpl). Errors
// only on a degenerate basename ("", ".", "/") — a URL with nothing after the
// GitServerURLPrefix — since joining that with mount would point the Ticker
// at the mount root itself rather than any one app's worktree.
func worktreeDir(mount, rawURL string) (string, error) {
	base := strings.TrimSuffix(path.Base(rawURL), ".git")
	if base == "" || base == "." || base == "/" {
		return "", fmt.Errorf("spec.url %q has no usable path basename", rawURL)
	}
	return filepath.Join(mount, base), nil
}
