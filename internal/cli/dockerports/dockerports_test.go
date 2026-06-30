package dockerports

import (
	"sort"
	"testing"
)

func keys(m map[int]struct{}) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// TestComposeProbe_SkipsDockerHeldEvenWhenHostBindable is the colima case: a
// port docker already publishes must be reported unavailable even though the
// host bind probe (net.Listen) would succeed for it in a docker-in-VM backend.
func TestComposeProbe_SkipsDockerHeldEvenWhenHostBindable(t *testing.T) {
	published := map[int]struct{}{50002: {}}
	hostAlwaysBindable := func(int) bool { return true } // colima: host bind never conflicts

	probe := composeProbe(published, hostAlwaysBindable)

	if probe(50002) {
		t.Error("probe(50002) = true; a docker-published port must be reported unavailable even when host-bindable")
	}
	if !probe(50003) {
		t.Error("probe(50003) = false; a free port must be reported available")
	}
}

// TestComposeProbe_RespectsHostProbeForNonDockerSquatter: a non-docker host
// process (not in the published set) is still caught by the host term.
func TestComposeProbe_RespectsHostProbeForNonDockerSquatter(t *testing.T) {
	probe := composeProbe(nil, func(port int) bool { return port != 8081 })
	if probe(8081) {
		t.Error("probe(8081) = true; a host-held (non-docker) port must be reported unavailable")
	}
	if !probe(8082) {
		t.Error("probe(8082) = false; a free port must be reported available")
	}
}

func TestParsePublishedPorts(t *testing.T) {
	tests := []struct {
		name   string
		fields []string
		want   []int
	}{
		{
			name:   "k3d registry: 0.0.0.0 + [::] dual-stack, same port",
			fields: []string{"0.0.0.0:50001->5000/tcp, [::]:50001->5000/tcp"},
			want:   []int{50001},
		},
		{
			name: "k3d serverlb: multiple host ports on one container",
			fields: []string{
				"0.0.0.0:8080->80/tcp, [::]:8080->80/tcp, 0.0.0.0:8540->443/tcp, [::]:8540->443/tcp, 0.0.0.0:61178->6443/tcp",
			},
			want: []int{8080, 8540, 61178},
		},
		{
			name:   "loopback-bound publish",
			fields: []string{"127.0.0.1:5000->5000/tcp"},
			want:   []int{5000},
		},
		{
			name:   "host-IP-specific publish",
			fields: []string{"192.168.1.10:9000->9000/tcp"},
			want:   []int{9000},
		},
		{
			name:   "port range expands",
			fields: []string{"0.0.0.0:8000-8002->8000-8002/tcp"},
			want:   []int{8000, 8001, 8002},
		},
		{
			name:   "udp is ignored",
			fields: []string{"0.0.0.0:53->53/udp"},
			want:   nil,
		},
		{
			name:   "container-only expose (no host port) is ignored",
			fields: []string{"5000/tcp"},
			want:   nil,
		},
		{
			name:   "mixed tcp publish + udp + bare expose on one container",
			fields: []string{"0.0.0.0:8443->443/tcp, 0.0.0.0:53->53/udp, 9000/tcp"},
			want:   []int{8443},
		},
		{
			name:   "empty field (no published ports)",
			fields: []string{""},
			want:   nil,
		},
		{
			name:   "multiple containers, dedup across them",
			fields: []string{"0.0.0.0:50001->5000/tcp", "0.0.0.0:50001->5000/tcp", "[::]:8540->443/tcp"},
			want:   []int{8540, 50001},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := keys(parsePublishedPorts(tc.fields))
			if len(got) != len(tc.want) {
				t.Fatalf("ports = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ports = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
