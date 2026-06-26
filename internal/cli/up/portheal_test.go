package up

import (
	"testing"

	"github.com/cobr-io/flywheel/internal/cli/allocator"
	"github.com/cobr-io/flywheel/internal/cli/schema"
)

func emptyLedger() *allocator.File {
	return &allocator.File{Schema: allocator.SchemaLabel, Clients: map[string]allocator.Triple{}}
}

func httpSlot(current int, owned bool) portSlot {
	return portSlot{
		key: "http_port", pool: allocator.Pools.Http, current: current, owned: owned,
		get: func(t allocator.Triple) int { return t.HttpPort },
		set: func(f *schema.File, p int) { f.Cluster.HttpPort = p },
	}
}

func httpsSlot(current int, owned bool) portSlot {
	return portSlot{
		key: "https_port", pool: allocator.Pools.Https, current: current, owned: owned,
		get: func(t allocator.Triple) int { return t.HttpsPort },
		set: func(f *schema.File, p int) { f.Cluster.HttpsPort = p },
	}
}

func registrySlot(current int, owned bool) portSlot {
	return portSlot{
		key: "registry_port", pool: allocator.Pools.Registry, current: current, owned: owned,
		get: func(t allocator.Triple) int { return t.RegistryPort },
		set: func(f *schema.File, p int) { f.Cluster.RegistryPort = p },
	}
}

// allBindableExcept returns a probe where every port is bindable except the
// listed ones (simulating ports a foreign process currently holds).
func allBindableExcept(held ...int) func(int) bool {
	set := map[int]struct{}{}
	for _, p := range held {
		set[p] = struct{}{}
	}
	return func(p int) bool { _, ok := set[p]; return !ok }
}

func TestPlanPortHeal_NoCollisionDoesNothing(t *testing.T) {
	got, err := planPortHeal([]portSlot{httpSlot(8080, false)}, emptyLedger(), "acme", allBindableExcept())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want no reallocation when the port is free, got %+v", got)
	}
}

func TestPlanPortHeal_ReallocatesForeignHeldPort(t *testing.T) {
	got, err := planPortHeal([]portSlot{httpSlot(8080, false)}, emptyLedger(), "acme", allBindableExcept(8080))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].was != 8080 || got[0].port != 8081 {
		t.Fatalf("want http 8080 → 8081, got %+v", got)
	}
}

func TestPlanPortHeal_LeavesPortOwnedByOwnCluster(t *testing.T) {
	// owned=true models our own running cluster legitimately holding the port;
	// even though it's "in use", healing must not reshuffle it (idempotency).
	got, err := planPortHeal([]portSlot{httpSlot(8080, true)}, emptyLedger(), "acme", allBindableExcept(8080))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("an owned (self-held) port must not heal, got %+v", got)
	}
}

func TestPlanPortHeal_AvoidsPortReservedByAnotherClient(t *testing.T) {
	ledger := &allocator.File{Schema: allocator.SchemaLabel, Clients: map[string]allocator.Triple{
		"other": {HttpPort: 8081},
	}}
	got, err := planPortHeal([]portSlot{httpSlot(8080, false)}, ledger, "acme", allBindableExcept(8080))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].port != 8082 {
		t.Fatalf("want 8082 (8080 held, 8081 reserved by other client), got %+v", got)
	}
}

func TestPlanPortHeal_DoesNotAvoidOwnLedgerEntry(t *testing.T) {
	// The client's own stale ledger entry must not block reuse of the next port.
	ledger := &allocator.File{Schema: allocator.SchemaLabel, Clients: map[string]allocator.Triple{
		"acme": {HttpPort: 8080},
	}}
	got, err := planPortHeal([]portSlot{httpSlot(8080, false)}, ledger, "acme", allBindableExcept(8080))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].port != 8081 {
		t.Fatalf("want 8081 (own entry not avoided), got %+v", got)
	}
}

func TestPlanPortHeal_HealsEachPoolIndependently(t *testing.T) {
	// registry is free; http + https are both foreign-held → only the latter two heal.
	slots := []portSlot{registrySlot(50001, false), httpSlot(8080, false), httpsSlot(8540, false)}
	got, err := planPortHeal(slots, emptyLedger(), "acme", allBindableExcept(8080, 8540))
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]int{}
	for _, a := range got {
		byKey[a.slot.key] = a.port
	}
	if _, healed := byKey["registry_port"]; healed {
		t.Errorf("registry_port was free; should not have healed: %+v", got)
	}
	if byKey["http_port"] != 8081 || byKey["https_port"] != 8541 {
		t.Errorf("want http 8081 + https 8541, got %+v", byKey)
	}
}

func TestPlanPortHeal_PoolExhaustedErrors(t *testing.T) {
	s := portSlot{
		key: "http_port", pool: allocator.Pool{Min: 8080, Max: 8081}, current: 8080,
		get: func(t allocator.Triple) int { return t.HttpPort },
	}
	if _, err := planPortHeal([]portSlot{s}, emptyLedger(), "acme", func(int) bool { return false }); err == nil {
		t.Fatal("want an error when the pool cannot satisfy a reallocation")
	}
}
