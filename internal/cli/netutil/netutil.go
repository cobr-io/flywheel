// Package netutil holds small networking helpers shared across the CLI
// (e.g. probing whether a TCP port can currently be bound on the host).
package netutil

import (
	"fmt"
	"net"
	"time"
)

// PortIsBindable reports whether a TCP listener can be opened on the
// given port right now, by actually attempting net.Listen on the
// loopback address.
//
// This is a best-effort, point-in-time probe with two important caveats:
//
//   - TOCTOU: the result is only valid at the instant of the probe. A
//     port reported bindable here can be taken by another process before
//     the caller (e.g. `flywheel up` → k3d) actually binds it. Treat the
//     result as a strong hint, not a guarantee.
//   - Loopback vs 0.0.0.0: we probe 127.0.0.1, whereas k3d binds 0.0.0.0.
//     A port bound only on a non-loopback interface (e.g. some specific
//     LAN IP) can pass this probe yet still fail k3d's wildcard bind.
//     The common case — a registry/cluster already holding 0.0.0.0:<port>
//     — is still caught, because 0.0.0.0 subsumes 127.0.0.1.
func PortIsBindable(port int) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = l.Close()
	// Small grace period so the port leaves TIME_WAIT before a real
	// binder (k3d) tries to claim it moments later.
	time.Sleep(50 * time.Millisecond)
	return true
}
