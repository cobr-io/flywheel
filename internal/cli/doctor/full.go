package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/cobr-io/flywheel/internal/cli/allocator"
	"github.com/cobr-io/flywheel/internal/cli/dockerports"
	"github.com/cobr-io/flywheel/internal/cli/gitcheckout"
	"github.com/cobr-io/flywheel/internal/cli/hostmount"
	"github.com/cobr-io/flywheel/internal/cli/k3d"
	"github.com/cobr-io/flywheel/internal/cli/netutil"
)

// FullChecks runs every QuickCheck plus port-live-collision detection
// against allocator entries (per design § Port allocation: "flywheel
// doctor validates no live collision").
//
// `homeOverride` lets tests inject a custom HOME (allocations.json
// lookup honours it).
func FullChecks(homeOverride string) []Check {
	checks := QuickChecks()
	checks = append(checks, commitHookChecks()...)
	if runtime.GOOS == "linux" {
		// Linux/WSL only: macOS mkcert wires browser trust via the system
		// keychain, so certutil is irrelevant there.
		checks = append(checks, nssCertutilCheck())
	}
	checks = append(checks, allocatorPortCollisionCheck(homeOverride))
	if cwd, err := os.Getwd(); err == nil {
		checks = append(checks, workspaceMountCheck(cwd))
		checks = append(checks, worktreeCheck(cwd))
		checks = append(checks, workspaceCheck(cwd))
	}
	return checks
}

// worktreeCheck reports whether the git checkout layout is one `flywheel up`
// supports. A normal clone or a supported sibling git worktree passes; a nested
// worktree fails (flywheel's sibling model can't be satisfied), and a sibling
// worktree whose shared git dir sits on an unshareable macOS temp path fails
// (the mount wouldn't bridge into k3d). Read-only; a no-op outside a repo.
func worktreeCheck(repoDir string) Check {
	return Check{
		Name:        "worktree",
		Description: "git checkout is a clone or a supported sibling worktree",
		Run: func(ctx context.Context) error {
			layout, err := gitcheckout.Inspect(repoDir)
			if err != nil || !layout.IsWorktree {
				return nil // unclassifiable or a normal clone — nothing to report
			}
			if layout.Nested {
				return fmt.Errorf("%s", gitcheckout.NestedRemediation(layout))
			}
			if runtime.GOOS == "darwin" {
				if matched, bad := hostmount.UnshareableTempDir(layout.CommonDir); bad {
					return fmt.Errorf("the worktree's shared git dir %s is under %s, which Docker Desktop "+
						"won't bind-mount into k3d; run flywheel from a full clone instead", layout.CommonDir, matched)
				}
			}
			return nil
		},
	}
}

// workspaceMountCheck (macOS) warns when the gitops repo sits on a host path
// Docker Desktop won't bind-mount into k3d (temp dirs like /tmp, /var/folders) —
// the cluster wouldn't see the worktree, so `up` can't reconcile the client
// content. Read-only; no-op off darwin or on a shareable path.
func workspaceMountCheck(repoDir string) Check {
	return Check{
		Name:        "workspace-mount",
		Description: "gitops repo lives on a path Docker Desktop can bind-mount into k3d",
		Run: func(ctx context.Context) error {
			if runtime.GOOS != "darwin" {
				return nil
			}
			if matched, bad := hostmount.UnshareableTempDir(repoDir); bad {
				return fmt.Errorf("%s is under %s, which Docker Desktop won't bind-mount into k3d; "+
					"clone your gitops repo under your home directory instead", repoDir, matched)
			}
			return nil
		},
	}
}

// nssCertutilCheck (Linux/WSL only) verifies `certutil` is on PATH.
// mkcert needs it to install its root CA into the NSS trust store that
// Firefox and Chrome/Chromium read; without it `mkcert -install` covers
// only the system store and browsers still reject `*.<domain>` certs. A
// dev convenience surfaced in full `flywheel doctor` — like pre-commit/yq
// it never gates `up` (SeverityWarn via Warnf). Skipped entirely when
// mkcert is absent (the mkcert quick-check already reports that). Note
// for WSL users: the browser usually runs on Windows with its own trust
// store, so mkcert's CA must also be imported there — see README §
// Windows (WSL).
func nssCertutilCheck() Check {
	return Check{
		Name:        "certutil",
		Description: "libnss3-tools — lets mkcert wire Firefox/Chrome browser trust",
		Run: func(ctx context.Context) error {
			if _, err := exec.LookPath("mkcert"); err != nil {
				return nil
			}
			if _, err := exec.LookPath("certutil"); err != nil {
				return Warnf("certutil not on PATH: install libnss3-tools " +
					"(Debian/Ubuntu: `sudo apt install libnss3-tools`; Fedora: " +
					"`sudo dnf install nss-tools`) so `mkcert -install` can add its " +
					"CA to Firefox/Chrome trust")
			}
			return nil
		},
	}
}

// commitHookChecks probe the tools the scaffolded commit hooks need:
// `pre-commit` (the framework that wires .git/hooks) and mikefarah `yq`
// (the SOPS-shape guard's only dependency). Both are dev conveniences,
// not runtime prerequisites — they appear in full `flywheel doctor` so a
// developer can see why hooks aren't firing, but never gate `up`
// (SeverityWarn).
func commitHookChecks() []Check {
	return []Check{
		advisoryBinaryCheck("pre-commit", "activates this repo's commit hooks (SOPS-shape, local-only guard)"),
		yqCheck(),
	}
}

// yqCheck verifies mikefarah's yq is on PATH. The kislyuk/python yq is a
// different tool with an incompatible expression syntax, so we assert the
// binary identifies as mikefarah's. A dev convenience (SeverityWarn) —
// see commitHookChecks.
func yqCheck() Check {
	return Check{
		Name:        "yq",
		Description: "mikefarah yq — drives the SOPS-shape commit hook",
		Run: func(ctx context.Context) error {
			if _, err := exec.LookPath("yq"); err != nil {
				return Warnf("yq not on PATH: %v", err)
			}
			out, err := exec.CommandContext(ctx, "yq", "--version").CombinedOutput()
			if err != nil {
				return Warnf("yq --version failed: %v (%s)", err, string(out))
			}
			if !strings.Contains(strings.ToLower(string(out)), "mikefarah") {
				return Warnf("found a non-mikefarah yq (%s); the SOPS-shape hook needs mikefarah/yq",
					strings.TrimSpace(string(out)))
			}
			return nil
		},
	}
}

// allocatorPortCollisionCheck reports a collision when an allocated port is held
// by something OTHER than the owning client's own k3d objects. "Held" is decided
// against docker's published-port ledger first (the authority — a host
// net.Listen is blind to docker-held ports when docker runs in a VM, e.g.
// colima/Docker Desktop/WSL2), then a host bind probe (to also catch non-docker
// squatters). A port held by the client's own running registry/cluster is
// expected and skipped (owned-guard via RegistryExists/ClusterRunning).
func allocatorPortCollisionCheck(homeOverride string) Check {
	return Check{
		Name:        "ports",
		Description: "no foreign process holds an allocated port",
		Run: func(ctx context.Context) error {
			path := allocationsPath(homeOverride)
			alloc, err := allocator.Load(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			// Best-effort docker ledger; on error fall back to the host probe only.
			published, _ := dockerports.PublishedPorts(ctx)
			held := func(port int) bool {
				if _, ok := published[port]; ok {
					return true
				}
				return !netutil.PortIsBindableWildcard(port)
			}
			// By convention init names the cluster <client>-local and the
			// registry <client>-local-registry; ports those own are expected.
			regOwned := func(client string) bool {
				ok, _ := k3d.RegistryExists(ctx, client+"-local-registry")
				return ok
			}
			clusterOwned := func(client string) bool {
				ok, _ := k3d.ClusterRunning(ctx, client+"-local")
				return ok
			}
			collisions := portCollisions(alloc.Clients, held, regOwned, clusterOwned)
			if len(collisions) > 0 {
				return fmt.Errorf("ports already in use by another process/cluster: %v — free them or set a different port in flywheel.yaml",
					collisions)
			}
			return nil
		},
	}
}

// portCollisions returns "<port> (client <name>)" for every allocated port that
// `held` reports taken AND that is not owned by the client's own k3d objects
// (regOwned for registry_port, clusterOwned for http/https). Pure decision core
// — no docker/k3d shell-outs — so it's unit-tested; the real probes are injected
// by allocatorPortCollisionCheck. Output is sorted for deterministic messages.
func portCollisions(clients map[string]allocator.Triple, held func(int) bool, regOwned, clusterOwned func(client string) bool) []string {
	var collisions []string
	for client, t := range clients {
		slots := []struct {
			port  int
			owned bool
		}{
			{t.RegistryPort, regOwned(client)},
			{t.HttpPort, clusterOwned(client)},
			{t.HttpsPort, clusterOwned(client)},
		}
		for _, s := range slots {
			if !s.owned && held(s.port) {
				collisions = append(collisions, fmt.Sprintf("%d (client %s)", s.port, client))
			}
		}
	}
	sort.Strings(collisions)
	return collisions
}

func allocationsPath(homeOverride string) string {
	if homeOverride != "" {
		return filepath.Join(homeOverride, ".config", "flywheel", "allocations.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "flywheel", "allocations.json")
}
