package up

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/cobr-io/flywheel/internal/cli/allocator"
	"github.com/cobr-io/flywheel/internal/cli/config"
	"github.com/cobr-io/flywheel/internal/cli/dockerports"
	"github.com/cobr-io/flywheel/internal/cli/k3d"
	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/style"
	"github.com/cobr-io/flywheel/internal/naming"
)

// portSlot is one host port subject to up-time collision healing.
type portSlot struct {
	key     string                     // flywheel.yaml key under cluster: (e.g. "http_port")
	pool    allocator.Pool             // canonical range to reallocate within
	current int                        // the configured value
	owned   bool                       // a live k3d object already holds it → never heal
	get     func(allocator.Triple) int // reads this port out of a ledger Triple
	set     func(*schema.File, int)    // writes a new value into the in-memory cfg
}

// portAssignment records one reallocation decision.
type portAssignment struct {
	slot portSlot
	was  int
	port int
}

// healHostPorts re-probes the configured host ports BEFORE the registry
// (create-registry) and cluster (create-cluster) are created, and reallocates any that a
// foreign process now holds — so a static port that drifted out from under
// the allocation self-heals instead of crashing k3d with "address already in
// use" (issue #1). Each reallocated port is written back to flywheel.yaml and
// the allocator ledger so it stays stable on the next up.
//
// A port legitimately held by THIS setup's own k3d objects (a running
// cluster's loadbalancer, an existing registry) is left untouched, keeping
// re-runs idempotent: re-binding is only a "collision" when the object that
// would bind it does not already exist.
func healHostPorts(ctx context.Context, opts Options, cfg *schema.File, out io.Writer) error {
	registryHeld, err := k3d.RegistryExists(ctx, cfg.Cluster.Registry)
	if err != nil {
		return fmt.Errorf("probe registry: %w", err)
	}
	clusterUp, err := k3d.ClusterRunning(ctx, cfg.Cluster.Name)
	if err != nil {
		return fmt.Errorf("probe cluster: %w", err)
	}

	slots := []portSlot{
		{
			key: "registry_port", pool: allocator.Pools.Registry,
			current: cfg.Cluster.RegistryPort, owned: registryHeld,
			get: func(t allocator.Triple) int { return t.RegistryPort },
			set: func(f *schema.File, p int) { f.Cluster.RegistryPort = p },
		},
		{
			key: "http_port", pool: allocator.Pools.Http,
			current: cfg.Cluster.HttpPort, owned: clusterUp,
			get: func(t allocator.Triple) int { return t.HttpPort },
			set: func(f *schema.File, p int) { f.Cluster.HttpPort = p },
		},
		{
			key: "https_port", pool: allocator.Pools.Https,
			current: cfg.Cluster.HttpsPort, owned: clusterUp,
			get: func(t allocator.Triple) int { return t.HttpsPort },
			set: func(f *schema.File, p int) { f.Cluster.HttpsPort = p },
		},
	}

	ledger, ledgerPath := loadLedger(opts.HomeOverride)
	// Probe against docker's published-port ledger (not just a host net.Listen),
	// so a port another k3d cluster/container already publishes is caught even
	// when docker runs in a VM (colima/Docker Desktop/WSL2) where a host bind
	// would wrongly succeed. Falls back to host-only if docker is unreachable.
	probe, derr := dockerports.AvailabilityProbe(ctx)
	if derr != nil {
		style.Warn(out, "could not read docker published ports (%v); using host-only port probe", derr)
	}
	assignments, err := planPortHeal(slots, ledger, cfg.Client.Name, probe)
	if err != nil {
		return err
	}
	if len(assignments) == 0 {
		return nil
	}

	style.Step(out, "healing host-port collisions")
	yamlPath := filepath.Join(opts.RepoDir, naming.ConfigFile)
	for _, a := range assignments {
		if err := config.SetClusterPort(yamlPath, a.slot.key, a.port); err != nil {
			return fmt.Errorf("persist %s=%d: %w", a.slot.key, a.port, err)
		}
		a.slot.set(cfg, a.port)
		style.Detail(out, "%s: %d in use → reallocated to %d (persisted to flywheel.yaml)", a.slot.key, a.was, a.port)
	}

	// Keep the allocator ledger consistent so a future `flywheel new` / `up`
	// for another client doesn't hand out a port we just claimed. Best-effort:
	// the flywheel.yaml write above is the source of truth, so a ledger write
	// failure must not fail up.
	updateLedger(ledger, cfg, opts.RepoDir)
	if err := ledger.Save(ledgerPath); err != nil {
		style.Warn(out, "could not update the allocator ledger (%v); continuing", err)
	}
	return nil
}

// planPortHeal is the pure decision core of healHostPorts: for each slot not
// already owned by a live k3d object, if its current port is not bindable it
// reallocates the lowest free port from the slot's pool — excluding ports the
// host holds, ports already reserved by OTHER clients in the ledger, and ports
// chosen for sibling slots in this same pass. Returns the reallocations (empty
// when nothing collides) or an error if a pool is exhausted.
func planPortHeal(slots []portSlot, ledger *allocator.File, self string, bindable func(int) bool) ([]portAssignment, error) {
	reservedByOthers := func(get func(allocator.Triple) int) map[int]struct{} {
		taken := map[int]struct{}{}
		for name, t := range ledger.Clients {
			if name == self {
				continue
			}
			taken[get(t)] = struct{}{}
		}
		return taken
	}

	var out []portAssignment
	for _, s := range slots {
		if s.owned || bindable(s.current) {
			continue
		}
		taken := reservedByOthers(s.get)
		for _, a := range out { // don't reuse a port a sibling slot just claimed
			taken[a.port] = struct{}{}
		}
		newPort, err := allocator.PickFreePort(s.pool, taken, bindable)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.key, err)
		}
		out = append(out, portAssignment{slot: s, was: s.current, port: newPort})
	}
	return out, nil
}

// loadLedger reads the allocator ledger, falling back to an empty one on any
// read/parse error so a corrupt or missing ledger never blocks up.
func loadLedger(homeOverride string) (*allocator.File, string) {
	path := allocationsPath(homeOverride)
	f, err := allocator.Load(path)
	if err != nil {
		f = &allocator.File{Schema: allocator.SchemaLabel, Clients: map[string]allocator.Triple{}}
	}
	return f, path
}

// updateLedger upserts this client's healed triple into the ledger. A new
// entry records the abs repo path so `allocator gc` can prune it later.
func updateLedger(ledger *allocator.File, cfg *schema.File, repoDir string) {
	if ledger.Clients == nil {
		ledger.Clients = map[string]allocator.Triple{}
	}
	t := ledger.Clients[cfg.Client.Name]
	t.RegistryPort = cfg.Cluster.RegistryPort
	t.HttpPort = cfg.Cluster.HttpPort
	t.HttpsPort = cfg.Cluster.HttpsPort
	if t.RepoPath == "" {
		if abs, err := filepath.Abs(repoDir); err == nil {
			t.RepoPath = abs
		} else {
			t.RepoPath = repoDir
		}
	}
	ledger.Clients[cfg.Client.Name] = t
}

// allocationsPath resolves ~/.config/flywheel/allocations.json, honouring a
// test home override. It delegates the precedence rule to allocator.ResolvePath
// (shared with `down`), adding up's own "never block on a missing home" fallback
// to a relative path.
func allocationsPath(homeOverride string) string {
	p, err := allocator.ResolvePath("", homeOverride)
	if err != nil {
		return filepath.Join(".config", "flywheel", "allocations.json")
	}
	return p
}
