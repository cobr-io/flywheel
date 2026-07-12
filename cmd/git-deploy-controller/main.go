// Command git-deploy-controller is the dev-loop's self/gitops sync controller.
// It replaces the git-auto-sync-self sidecar (sync.sh) for the gitops repo:
// each tick it pushes the host worktree's AUTHORED branch to the in-cluster bare
// repo and maintains a DEPLOY branch (= AUTHORED + the IUA's image bumps) that
// Flux deploys, keeping ephemeral image bumps off the developer's branch.
//
// Configuration is entirely via environment (set by the Deployment, mostly from
// the flywheel-config ConfigMap):
//
//	REPO_BASE_NAME           gitops repo basename, e.g. "acme-gitops" (required)
//	WORKSPACES_MOUNT         hostPath worktrees mount (default "/workspaces")
//	GIT_SERVER_URL           in-cluster git-server base URL (default the svc DNS)
//	WORKTREE                 explicit override; else <WORKSPACES_MOUNT>/<REPO_BASE_NAME>
//	BARE_REPO_URL            explicit override; else <GIT_SERVER_URL>/<REPO_BASE_NAME>.git
//	DEFAULT_BRANCH           AUTHORED fallback branch (default "main")
//	DEPLOY_BRANCH            the deploy branch (default "flywheel/local-deploy")
//	DEPLOY_WORKDIR           maintainer working clone dir (default "/tmp/deploy-clone")
//	POLL_INTERVAL            tick cadence, e.g. "2s" (default 2s)
//	GITREPOSITORY_NAME/_NAMESPACE       self GitRepository (default flux-system/flux-system)
//	IUA_NAME/_NAMESPACE                 ImageUpdateAutomation (default flywheel-self/flux-system)
//	KUSTOMIZATION_NAME/_NAMESPACE       apps Kustomization (default client-apps/flux-system)
//	HEALTH_PROBE_ADDR        healthz/readyz bind address (default ":8081")
//
// Health probes (see health.go): /healthz reports 200 once the process is up
// (liveness — nothing here warrants a restart decision besides "is the
// process running"). /readyz reports 200 once the loop has completed at least
// one Tick attempt and hasn't gone quiet for readinessWindow polls; a hung
// git/Flux call flips it to 503 without killing the process, which would just
// restart into the same stuck state.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cobr-io/flywheel/internal/deploybranch"
	"github.com/cobr-io/flywheel/internal/naming"
	"github.com/cobr-io/flywheel/internal/selfsync"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cl, err := client.New(ctrl.GetConfigOrDie(), client.Options{})
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}

	// The dev-loop/base manifest is static (no per-client templating), so the
	// per-repo paths are derived from REPO_BASE_NAME (sourced from flywheel-config).
	repoBase := mustEnv("REPO_BASE_NAME")
	gitServerURL := strings.TrimSuffix(envOr("GIT_SERVER_URL", naming.GitServerURL(naming.FlywheelNamespace)), "/")
	worktree := envOr("WORKTREE", filepath.Join(envOr("WORKSPACES_MOUNT", "/workspaces"), repoBase))
	bareURL := envOr("BARE_REPO_URL", gitServerURL+"/"+repoBase+".git")
	defaultBranch := envOr("DEFAULT_BRANCH", "main")
	deployBranch := envOr("DEPLOY_BRANCH", naming.DeployBranch)
	workDir := envOr("DEPLOY_WORKDIR", "/tmp/deploy-clone")
	poll := envDuration("POLL_INTERVAL", 2*time.Second)

	loop := &selfsync.Loop{
		Worktree: &selfsync.Worktree{Dir: worktree, BareURL: bareURL},
		Deploy: &deploybranch.Maintainer{
			WorkDir:   workDir,
			RemoteURL: bareURL,
			Deploy:    deployBranch,
		},
		Flux: &selfsync.K8sFlux{
			Client:                 cl,
			GitRepoName:            envOr("GITREPOSITORY_NAME", "flux-system"),
			GitRepoNamespace:       envOr("GITREPOSITORY_NAMESPACE", naming.FluxNamespace),
			IUAName:                envOr("IUA_NAME", "flywheel-self"),
			IUANamespace:           envOr("IUA_NAMESPACE", naming.FluxNamespace),
			KustomizationName:      envOr("KUSTOMIZATION_NAME", "client-apps"),
			KustomizationNamespace: envOr("KUSTOMIZATION_NAMESPACE", naming.FluxNamespace),
		},
		DefaultBranch: defaultBranch,
		PollInterval:  poll,
		Logf:          log.Printf,
	}

	log.Printf("git-deploy-controller: worktree=%s bare=%s default=%s deploy=%s poll=%s",
		worktree, bareURL, defaultBranch, deployBranch, poll)

	healthAddr := envOr("HEALTH_PROBE_ADDR", ":8081")
	healthSrv := &http.Server{Addr: healthAddr, Handler: newHealthMux(loop, poll)}
	go func() {
		if err := healthSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("health probe server: %v", err)
		}
	}()

	if err := loop.Run(ctrl.SetupSignalHandler()); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("loop: %v", err)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %s is not set", key)
	}
	return v
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
		log.Printf("warning: %s=%q is not a valid duration; using %s", key, v, def)
	}
	return def
}
