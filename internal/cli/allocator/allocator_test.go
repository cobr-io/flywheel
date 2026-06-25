package allocator

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// stubBindable overrides the host-bindability probe for the duration of a
// test and restores it on cleanup. A nil predicate means "all ports
// bindable", which isolates ledger-selection tests from whatever ports
// happen to be in use on the host running the suite.
func stubBindable(t *testing.T, pred func(int) bool) {
	t.Helper()
	if pred == nil {
		pred = func(int) bool { return true }
	}
	prev := portIsBindable
	portIsBindable = pred
	t.Cleanup(func() { portIsBindable = prev })
}

// T0.3 — allocates a fresh triple, refuses a duplicate, prunes a
// missing-repo entry.

func TestAllocate_FreshTripleFromLowestFreePort(t *testing.T) {
	stubBindable(t, nil)
	f := &File{Schema: SchemaLabel, Clients: map[string]Triple{}}
	tr, err := f.Allocate("acme", "/Users/dev/acme-gitops")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if tr.RegistryPort != Pools.Registry.Min {
		t.Errorf("registry_port = %d, want %d (lowest free)", tr.RegistryPort, Pools.Registry.Min)
	}
	if tr.HttpPort != Pools.Http.Min {
		t.Errorf("http_port = %d, want %d", tr.HttpPort, Pools.Http.Min)
	}
	if tr.HttpsPort != Pools.Https.Min {
		t.Errorf("https_port = %d, want %d", tr.HttpsPort, Pools.Https.Min)
	}
}

func TestAllocate_RefusesDuplicateClient(t *testing.T) {
	stubBindable(t, nil)
	f := &File{Schema: SchemaLabel, Clients: map[string]Triple{}}
	if _, err := f.Allocate("acme", "/path/one"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Allocate("acme", "/path/two"); err == nil {
		t.Fatal("second Allocate for same client should fail")
	}
}

func TestAllocate_SecondClientGetsNextFreePorts(t *testing.T) {
	stubBindable(t, nil)
	f := &File{Schema: SchemaLabel, Clients: map[string]Triple{}}
	_, _ = f.Allocate("first", "/p/1")
	tr, err := f.Allocate("second", "/p/2")
	if err != nil {
		t.Fatal(err)
	}
	if tr.RegistryPort != Pools.Registry.Min+1 {
		t.Errorf("second registry = %d, want %d", tr.RegistryPort, Pools.Registry.Min+1)
	}
	if tr.HttpPort != Pools.Http.Min+1 {
		t.Errorf("second http = %d, want %d", tr.HttpPort, Pools.Http.Min+1)
	}
}

func TestAllocate_SkipsForbiddenPorts(t *testing.T) {
	// Registry pool skips 5000 and 50000 per design. Min is 50001 so 50000
	// is below-range anyway; check 5000 is also skipped (if it were in
	// range). Construct a synthetic pool that includes 5000 to validate
	// the skip mechanism.
	stubBindable(t, nil)
	f := &File{Schema: SchemaLabel, Clients: map[string]Triple{}}
	tr, _ := f.Allocate("a", "/p")
	for _, skip := range []int{5000, 50000} {
		if tr.RegistryPort == skip {
			t.Errorf("allocated forbidden port %d", skip)
		}
	}
}

func TestRelease_IdempotentOnMissingClient(t *testing.T) {
	f := &File{Schema: SchemaLabel, Clients: map[string]Triple{}}
	f.Release("never-allocated") // should not panic or error
}

func TestRelease_RemovesEntry(t *testing.T) {
	stubBindable(t, nil)
	f := &File{Schema: SchemaLabel, Clients: map[string]Triple{}}
	_, _ = f.Allocate("acme", "/p")
	f.Release("acme")
	if _, ok := f.Clients["acme"]; ok {
		t.Error("Release did not remove client")
	}
}

func TestGC_PrunesMissingRepoEntries(t *testing.T) {
	tmp := t.TempDir()
	alive := filepath.Join(tmp, "alive-repo")
	if err := os.MkdirAll(alive, 0o755); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(tmp, "this-was-rm-rfd")

	f := &File{Schema: SchemaLabel, Clients: map[string]Triple{
		"alive": {RepoPath: alive},
		"dead":  {RepoPath: dead},
	}}
	pruned := f.GC()
	if len(pruned) != 1 || pruned[0] != "dead" {
		t.Errorf("GC pruned = %v, want [dead]", pruned)
	}
	if _, ok := f.Clients["dead"]; ok {
		t.Error("dead client still present after GC")
	}
	if _, ok := f.Clients["alive"]; !ok {
		t.Error("alive client should survive GC")
	}
}

func TestGC_SortedDeterministicOutput(t *testing.T) {
	f := &File{Schema: SchemaLabel, Clients: map[string]Triple{
		"zeta":  {RepoPath: "/nope/zeta"},
		"alpha": {RepoPath: "/nope/alpha"},
		"mid":   {RepoPath: "/nope/mid"},
	}}
	pruned := f.GC()
	if !sort.StringsAreSorted(pruned) {
		t.Errorf("GC output not sorted: %v", pruned)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "sub", "allocations.json") // tests MkdirAll
	stubBindable(t, nil)
	f := &File{Schema: SchemaLabel, Clients: map[string]Triple{}}
	_, _ = f.Allocate("acme", "/p/acme")
	if err := f.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Clients["acme"].RegistryPort != Pools.Registry.Min {
		t.Errorf("round-trip lost data; got %+v", got.Clients["acme"])
	}
}

func TestLoad_MissingFileIsEmptyAllocator(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("Load on missing file should return empty, not error: %v", err)
	}
	if len(f.Clients) != 0 {
		t.Errorf("empty allocator should have 0 clients, got %d", len(f.Clients))
	}
}

// Issue #22 — pickFree must skip a port that's un-ledgered but already
// held on the host, not just ports in its own ledger. This is the
// deterministic, stub-driven version: the probe reports the pool minimum
// as unbindable and pickFree must hand out the next one.
func TestPickFree_SkipsHostBoundPort(t *testing.T) {
	bound := Pools.Registry.Min
	stubBindable(t, func(port int) bool { return port != bound })

	f := &File{Schema: SchemaLabel, Clients: map[string]Triple{}}
	got, err := f.pickFree(Pools.Registry, func(tr Triple) int { return tr.RegistryPort })
	if err != nil {
		t.Fatalf("pickFree: %v", err)
	}
	if got == bound {
		t.Fatalf("pickFree returned host-bound port %d; want it skipped", bound)
	}
	if got != bound+1 {
		t.Errorf("pickFree = %d, want %d (lowest un-ledgered AND bindable)", got, bound+1)
	}
}

// Issue #1 — PickFreePort is the runtime (up-time) counterpart used by
// host-port healing. It takes an explicit taken-set and bind probe.

func TestPickFreePort_LowestFreeBindable(t *testing.T) {
	got, err := PickFreePort(Pools.Http, map[int]struct{}{}, func(int) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if got != Pools.Http.Min {
		t.Errorf("PickFreePort = %d, want %d", got, Pools.Http.Min)
	}
}

func TestPickFreePort_SkipsTakenAndUnbindable(t *testing.T) {
	taken := map[int]struct{}{Pools.Http.Min: {}} // reserved by another client
	unbindable := Pools.Http.Min + 1              // held on the host
	bindable := func(p int) bool { return p != unbindable }
	got, err := PickFreePort(Pools.Http, taken, bindable)
	if err != nil {
		t.Fatal(err)
	}
	if got != Pools.Http.Min+2 {
		t.Errorf("PickFreePort = %d, want %d (skipped taken %d and unbindable %d)",
			got, Pools.Http.Min+2, Pools.Http.Min, unbindable)
	}
}

func TestPickFreePort_HonoursSkipSet(t *testing.T) {
	// The registry pool skips 5000 and 50000; neither is in range (min 50001),
	// so build a synthetic pool whose first value is in its Skip set.
	p := Pool{Min: 7000, Max: 7002, Skip: map[int]struct{}{7000: {}}}
	got, err := PickFreePort(p, map[int]struct{}{}, func(int) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if got != 7001 {
		t.Errorf("PickFreePort = %d, want 7001 (7000 is skipped)", got)
	}
}

func TestPickFreePort_ExhaustedPool(t *testing.T) {
	p := Pool{Min: 9000, Max: 9001}
	if _, err := PickFreePort(p, map[int]struct{}{}, func(int) bool { return false }); err == nil {
		t.Fatal("expected an exhausted-pool error when nothing is bindable")
	}
}

// Issue #22 — end-to-end via Allocate using a REAL net.Listen on the
// pool minimum (the probe path exercised in production). Allocate must
// not hand out the port we're holding.
func TestAllocate_SkipsRealHostBoundRegistryPort(t *testing.T) {
	min := Pools.Registry.Min
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", min))
	if err != nil {
		t.Skipf("port %d already in use on host; cannot run real-listener test: %v", min, err)
	}
	defer func() { _ = ln.Close() }()

	f := &File{Schema: SchemaLabel, Clients: map[string]Triple{}}
	tr, err := f.Allocate("acme", "/p/acme")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if tr.RegistryPort == min {
		t.Errorf("registry_port = %d, but that port is bound on the host; allocator should have skipped it", min)
	}
}
