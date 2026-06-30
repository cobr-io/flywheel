// Package allocator manages per-client port + registry-port assignments,
// persisted at ~/.config/flywheel/allocations.json. Pools per design
// § Port allocation:
//
//	registry_port: 50001–50099 (skip 5000 [AirPlay] and 50000 [k3d default])
//	http_port:     8080–8099
//	https_port:    8540–8559
//
// Allocation is `flywheel new` time. Removal is `flywheel destroy` (entry
// purged) or `flywheel allocator gc` (entries whose recorded repo path no
// longer exists). Pruning is explicit per design § Port allocation; the
// pool is large enough that leaks don't matter for years.
package allocator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/cobr-io/flywheel/internal/cli/netutil"
)

// Triple is a single client's allocated ports.
type Triple struct {
	RegistryPort int    `json:"registry_port"`
	HttpPort     int    `json:"http_port"`
	HttpsPort    int    `json:"https_port"`
	RepoPath     string `json:"repo_path"`
}

// File is the on-disk shape of ~/.config/flywheel/allocations.json.
type File struct {
	// Schema version of this file; bump only on breaking format changes.
	Schema string `json:"schema"`
	// Clients keyed by client name.
	Clients map[string]Triple `json:"clients"`
}

// SchemaLabel is the on-disk schema label for allocations.json.
const SchemaLabel = "v1"

// Pool documents one resource's allocation range.
type Pool struct {
	Min, Max int
	// Skip excludes specific values (e.g. 5000 = macOS AirPlay; 50000 =
	// k3d's own default registry port on some setups).
	Skip map[int]struct{}
}

// Pools are the canonical allocation ranges per design § Port allocation.
var Pools = struct {
	Registry, Http, Https Pool
}{
	Registry: Pool{
		Min: 50001, Max: 50099,
		Skip: map[int]struct{}{5000: {}, 50000: {}},
	},
	Http:  Pool{Min: 8080, Max: 8099},
	Https: Pool{Min: 8540, Max: 8559},
}

// DefaultPath is ~/.config/flywheel/allocations.json, expanded for the
// running user.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "flywheel", "allocations.json"), nil
}

// Load reads the allocations file at the given path. A missing file is
// treated as an empty allocator (so the first `flywheel new` on a fresh
// host doesn't error).
func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &File{Schema: SchemaLabel, Clients: map[string]Triple{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Clients == nil {
		f.Clients = map[string]Triple{}
	}
	if f.Schema == "" {
		f.Schema = SchemaLabel
	}
	return &f, nil
}

// Save atomically writes the allocations file. Creates the parent dir if
// missing.
func (f *File) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	return os.Rename(tmp, path)
}

// Allocate finds the first free triple from the pools and records it
// under clientName. The recorded repoPath is what `allocator gc` checks
// later to detect leaks from `rm -rf` without `destroy`.
// bindable reports whether a port is free for k3d to publish. Callers compose a
// docker-aware probe (dockerports.AvailabilityProbe); a nil probe falls back to
// the host-only netutil.PortIsBindable so other callers/tests stay simple.
func (f *File) Allocate(clientName, repoPath string, bindable func(int) bool) (Triple, error) {
	if bindable == nil {
		bindable = netutil.PortIsBindable
	}
	if _, exists := f.Clients[clientName]; exists {
		return Triple{}, fmt.Errorf("client %q already allocated; use destroy first", clientName)
	}
	registry, err := f.pickFree(Pools.Registry, func(t Triple) int { return t.RegistryPort }, bindable)
	if err != nil {
		return Triple{}, fmt.Errorf("registry_port: %w", err)
	}
	http, err := f.pickFree(Pools.Http, func(t Triple) int { return t.HttpPort }, bindable)
	if err != nil {
		return Triple{}, fmt.Errorf("http_port: %w", err)
	}
	https, err := f.pickFree(Pools.Https, func(t Triple) int { return t.HttpsPort }, bindable)
	if err != nil {
		return Triple{}, fmt.Errorf("https_port: %w", err)
	}
	t := Triple{RegistryPort: registry, HttpPort: http, HttpsPort: https, RepoPath: repoPath}
	f.Clients[clientName] = t
	return t, nil
}

// Release removes a client's allocation. Idempotent: removing a missing
// client is not an error (destroy may run twice).
func (f *File) Release(clientName string) {
	delete(f.Clients, clientName)
}

// GC removes entries whose recorded repo path no longer exists. Returns
// the list of pruned client names (sorted, for deterministic output).
func (f *File) GC() []string {
	var pruned []string
	for name, t := range f.Clients {
		if _, err := os.Stat(t.RepoPath); os.IsNotExist(err) {
			pruned = append(pruned, name)
			delete(f.Clients, name)
		}
	}
	sort.Strings(pruned)
	return pruned
}

// pickFree returns the lowest port in the pool that is BOTH un-ledgered
// (not recorded for any existing client) AND free per the injected `bindable`
// probe. The probe stops the allocator from handing out a port something
// outside its own bookkeeping already holds — another k3d cluster, a
// non-flywheel process, or a stale ledger after `rm -rf` without `destroy` —
// which would otherwise surface as a raw k3d FATA in `flywheel up`.
//
// The probe is best-effort and point-in-time (TOCTOU — a port can be taken
// between `init` and `up`). Production callers pass a docker-aware probe
// (dockerports.AvailabilityProbe) so the check is correct even when docker runs
// in a VM (colima/Docker Desktop/WSL2) and a host net.Listen would not see the
// docker-held port.
func (f *File) pickFree(p Pool, get func(Triple) int, bindable func(int) bool) (int, error) {
	used := map[int]struct{}{}
	for _, t := range f.Clients {
		used[get(t)] = struct{}{}
	}
	return PickFreePort(p, used, bindable)
}

// PickFreePort returns the lowest port in pool p that is neither in the
// `taken` set nor a Skip value nor currently held on the host per the
// `bindable` probe. It is the runtime counterpart to Allocate's per-pool
// pick (which is init-time): callers supply the taken-set and bind probe
// explicitly, so `flywheel up`'s host-port healing can reuse the canonical
// pool ranges while controlling both the ledger view and the bind
// semantics (e.g. a 0.0.0.0 probe that matches docker). Returns an error
// when the pool is exhausted.
func PickFreePort(p Pool, taken map[int]struct{}, bindable func(int) bool) (int, error) {
	for port := p.Min; port <= p.Max; port++ {
		if _, skip := p.Skip[port]; skip {
			continue
		}
		if _, t := taken[port]; t {
			continue
		}
		if !bindable(port) {
			continue
		}
		return port, nil
	}
	return 0, fmt.Errorf("pool %d–%d exhausted", p.Min, p.Max)
}
