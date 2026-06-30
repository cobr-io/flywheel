package doctor

import (
	"reflect"
	"testing"

	"github.com/cobr-io/flywheel/internal/cli/allocator"
)

func TestPortCollisions(t *testing.T) {
	clients := map[string]allocator.Triple{
		"acme": {RegistryPort: 50001, HttpPort: 8080, HttpsPort: 8540},
	}
	no := func(string) bool { return false }
	yes := func(string) bool { return true }

	t.Run("foreign-held port is flagged", func(t *testing.T) {
		// http_port held by something else; nothing owned by this client.
		held := func(p int) bool { return p == 8080 }
		got := portCollisions(clients, held, no, no)
		want := []string{"8080 (client acme)"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("portCollisions = %v, want %v", got, want)
		}
	})

	t.Run("ports held by this client's own k3d objects are NOT flagged", func(t *testing.T) {
		// Everything is held, but the client owns its registry + cluster.
		held := func(int) bool { return true }
		got := portCollisions(clients, held, yes, yes)
		if len(got) != 0 {
			t.Errorf("portCollisions = %v, want none (all owned)", got)
		}
	})

	t.Run("free ports produce no collisions", func(t *testing.T) {
		held := func(int) bool { return false }
		got := portCollisions(clients, held, no, no)
		if len(got) != 0 {
			t.Errorf("portCollisions = %v, want none", got)
		}
	})

	t.Run("registry foreign-held but http/https owned by running cluster", func(t *testing.T) {
		held := func(int) bool { return true }            // all held
		regOwned := func(string) bool { return false }    // registry NOT ours
		clusterOwned := func(string) bool { return true } // cluster IS ours
		got := portCollisions(clients, held, regOwned, clusterOwned)
		want := []string{"50001 (client acme)"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("portCollisions = %v, want %v", got, want)
		}
	})
}
