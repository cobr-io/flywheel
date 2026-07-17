// Command git-auto-sync is the per-app worktree<->bare-repo sync controller.
// It replaces the per-app git-auto-sync sidecar (scripts/git-auto-sync/sync.sh)
// with a controller-runtime Reconciler over per-app Flux GitRepositories,
// closing the TOCTOU race described in
// docs/designs/2026-07-17-per-app-sync-controller-design.md: sync.sh samples
// the checked-out branch once per iteration, so a `git checkout` landing
// mid-iteration can apply a stale branch's decisions to a worktree that has
// already moved on, poisoning the bare repo.
//
// See docs/plans/2026-07-17-per-app-sync-controller-plan.md: Phase 1 built
// this scaffolding, Phase 2 implemented internal/appsync.Ticker, and Phase 3
// (this file's manager wiring) drives it via internal/appsync.Reconciler,
// one Ticker per per-app GitRepository discovered in BuilderNamespace.
//
// Configuration is entirely via environment (set by the Deployment, mirroring
// git-deploy-controller's pattern):
//
//	WORKSPACES_MOUNT   hostPath worktrees mount             (default "/workspaces")
//	GIT_SERVER_URL     in-cluster git-server base URL       (default the svc DNS)
//	BUILDER_NAMESPACE  namespace of per-app GitRepositories  (default "flywheel-system")
//	POLL_INTERVAL      tick cadence                         (default "2s")
//	MAX_CONCURRENT     reconcile parallelism                (default "4")
//	HEALTH_PROBE_ADDR  healthz/readyz bind                  (default ":8081")
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/cobr-io/flywheel/internal/appsync"
	"github.com/cobr-io/flywheel/internal/naming"
)

// newScheme builds the client scheme. The Reconciler (Phase 3) accesses
// GitRepository as a typed sourcev1 object, so sourcev1 MUST be registered
// here — the default client scheme only carries core kinds, and a missing
// registration fails every Get/Patch at runtime ("no kind is registered").
// apps/v1 (Deployment — the legacy-interlock check, Phase 3) rides along for
// free: it's part of clientgoscheme's default registration.
func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := sourcev1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

func main() {
	// Every worktree under WORKSPACES_MOUNT is a host bind-mount written by
	// BOTH this container (as root) and the host developer (non-root). A
	// default umask would make root-created files/dirs (e.g. new
	// .git/objects/<xx> fan-out dirs) mode 0644/0755 — unwritable by the host
	// user afterward, wedging their next commit (the sync.sh EACCES class this
	// controller exists to fix). Clearing the umask keeps everything this
	// process creates group/other-writable; must run before any file I/O.
	syscall.Umask(0)

	log.SetFlags(log.LstdFlags | log.LUTC)
	ctrl.SetLogger(zap.New())

	scheme, err := newScheme()
	if err != nil {
		log.Fatalf("build scheme: %v", err)
	}

	workspacesMount := envOr("WORKSPACES_MOUNT", "/workspaces")
	gitServerURL := strings.TrimSuffix(envOr("GIT_SERVER_URL", naming.GitServerURL(naming.FlywheelNamespace)), "/")
	builderNamespace := envOr("BUILDER_NAMESPACE", naming.FlywheelNamespace)
	poll := envDuration("POLL_INTERVAL", 2*time.Second)
	maxConcurrent := envInt("MAX_CONCURRENT", 4)
	healthAddr := envOr("HEALTH_PROBE_ADDR", ":8081")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: healthAddr,
		// Per-app GitRepositories only ever live in BUILDER_NAMESPACE; scoping
		// the cache avoids watching (and paying informer memory for) every
		// other namespace on the cluster.
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				builderNamespace: {},
			},
		},
		// Single replica, no HA story — matches every other flywheel
		// controller (image-builder-controller, git-deploy-controller).
		LeaderElection: false,
	})
	if err != nil {
		log.Fatalf("unable to start manager: %v", err)
	}

	rec := &appsync.Reconciler{
		Client:                  mgr.GetClient(),
		WorkspacesMount:         workspacesMount,
		GitServerURLPrefix:      gitServerURL,
		BuilderNamespace:        builderNamespace,
		PollInterval:            poll,
		MaxConcurrentReconciles: maxConcurrent,
		Logf:                    log.Printf,
	}
	if err := rec.SetupWithManager(mgr); err != nil {
		log.Fatalf("unable to create git-auto-sync reconciler: %v", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Fatalf("unable to set up health check: %v", err)
	}
	// readyz tracks informer-cache sync, not any one app's tick outcome
	// (design "Error handling / observability"): a single wedged app must
	// not flip readiness for the whole process, since a pod restart would
	// just land every app back in the same state. WaitForCacheSync returns
	// immediately (true) once the initial sync has completed, so this is
	// cheap on every probe after startup.
	if err := mgr.AddReadyzCheck("readyz", func(req *http.Request) error {
		if !mgr.GetCache().WaitForCacheSync(req.Context()) {
			return fmt.Errorf("informer cache not yet synced")
		}
		return nil
	}); err != nil {
		log.Fatalf("unable to set up readiness check: %v", err)
	}

	log.Printf("git-auto-sync: workspaces=%s git-server=%s builder-namespace=%s poll=%s max-concurrent=%d health=%s",
		workspacesMount, gitServerURL, builderNamespace, poll, maxConcurrent, healthAddr)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Fatalf("problem running manager: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("warning: %s=%q is not a valid duration; using %s", key, os.Getenv(key), def)
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("warning: %s=%q is not a valid integer; using %d", key, os.Getenv(key), def)
	}
	return def
}
