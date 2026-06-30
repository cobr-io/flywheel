// Package dockerports answers "which host ports does docker already publish?"
// by asking the docker daemon, which is the authority for whether a new
// container (e.g. a k3d registry or cluster loadbalancer) can publish a host
// port.
//
// This exists because a host-side net.Listen probe is NOT that authority on
// every backend. Under colima (and Docker Desktop) docker runs in a VM and
// forwards published ports, so a host net.Listen can succeed even when docker
// already owns the port — flywheel's port allocator/heal then hands out a
// colliding port and `up` crashes with docker's "port is already allocated".
// `docker ps` reports the daemon's published-port ledger identically across
// Linux, colima, Docker Desktop, and WSL2, so it is the cross-platform signal.
package dockerports

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/cobr-io/flywheel/internal/cli/netutil"
)

// queryTimeout bounds the `docker ps` call so a wedged daemon degrades to the
// host-only probe instead of hanging init/up.
const queryTimeout = 10 * time.Second

// PublishedPorts returns the set of host TCP ports currently published by any
// running docker container, parsed from `docker ps --format '{{.Ports}}'`.
// Best-effort: on a docker error it returns a nil set and the error so callers
// can warn and fall back to a host-only probe.
func PublishedPorts(ctx context.Context) (map[int]struct{}, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Ports}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w\n%s", err, out)
	}
	return parsePublishedPorts(splitNonEmptyLines(string(out))), nil
}

// AvailabilityProbe returns a probe reporting whether a host port is free for a
// new docker publish: NOT already published by docker AND bindable on the host
// (the latter still catches non-docker host squatters). On a docker query error
// it returns a host-only probe plus the error, so callers warn and proceed —
// never worse than the pre-existing host-only behavior.
func AvailabilityProbe(ctx context.Context) (func(int) bool, error) {
	published, err := PublishedPorts(ctx)
	return composeProbe(published, netutil.PortIsBindableWildcard), err
}

// composeProbe builds the availability probe: a port is free only if it is NOT
// in the docker-published set AND the host bind probe says it's free. The
// docker term is what makes this correct when a host bind would wrongly succeed
// for a docker-held port (docker-in-VM backends). Split out for unit testing.
func composeProbe(published map[int]struct{}, hostBindable func(int) bool) func(int) bool {
	return func(port int) bool {
		if _, taken := published[port]; taken {
			return false
		}
		return hostBindable(port)
	}
}

// parsePublishedPorts extracts the set of published host TCP ports from the
// per-container `{{.Ports}}` fields. Each field is a comma-separated list of
// mappings; we collect the host port of every `<addr>:<port>-><cport>/tcp`
// mapping (across 0.0.0.0 / [::] / 127.0.0.1 / host-IP forms, expanding ranges).
// Container-only exposes (`5000/tcp`, no `->`) and udp are ignored.
func parsePublishedPorts(portsFields []string) map[int]struct{} {
	set := map[int]struct{}{}
	for _, field := range portsFields {
		for _, seg := range strings.Split(field, ",") {
			for _, p := range hostPortsOf(strings.TrimSpace(seg)) {
				set[p] = struct{}{}
			}
		}
	}
	return set
}

// hostPortsOf returns the published host TCP port(s) of a single mapping
// segment, or nil if it publishes no host tcp port.
func hostPortsOf(seg string) []int {
	if seg == "" {
		return nil
	}
	// Proto is the suffix after the last '/'. Only tcp publishes conflict with
	// our tcp publishes.
	slash := strings.LastIndex(seg, "/")
	if slash < 0 || seg[slash+1:] != "tcp" {
		return nil
	}
	mapping := seg[:slash] // e.g. "0.0.0.0:50001->5000" or "5000"
	arrow := strings.Index(mapping, "->")
	if arrow < 0 {
		return nil // exposed but not published to a host port
	}
	host := mapping[:arrow] // "0.0.0.0:50001" | "[::]:50001" | "0.0.0.0:8000-8002"
	colon := strings.LastIndex(host, ":")
	if colon < 0 {
		return nil
	}
	return expandPortSpec(host[colon+1:]) // "50001" or "8000-8002"
}

// expandPortSpec parses "N" or a "LO-HI" range into the list of ports.
func expandPortSpec(spec string) []int {
	if dash := strings.Index(spec, "-"); dash >= 0 {
		lo, err1 := strconv.Atoi(spec[:dash])
		hi, err2 := strconv.Atoi(spec[dash+1:])
		if err1 != nil || err2 != nil || lo > hi {
			return nil
		}
		ports := make([]int, 0, hi-lo+1)
		for p := lo; p <= hi; p++ {
			ports = append(ports, p)
		}
		return ports
	}
	p, err := strconv.Atoi(spec)
	if err != nil {
		return nil
	}
	return []int{p}
}

func splitNonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return out
}
