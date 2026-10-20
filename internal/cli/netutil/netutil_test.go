package netutil

import (
	"net"
	"testing"
)

// TestPortIsBindableWildcard_LoopbackListenerIsTaken locks issue #106: a
// listener on 127.0.0.1:<port> must make the port unavailable. On macOS a
// 0.0.0.0 bind coexists with it, so a bind-only probe reported it free.
func TestPortIsBindableWildcard_LoopbackListenerIsTaken(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	if PortIsBindableWildcard(port) {
		t.Fatalf("port %d has a loopback listener but PortIsBindableWildcard reported it free", port)
	}
}

// TestPortIsBindableWildcard_FreePort: a port nothing holds is available, so
// the probe can't pass the test above by always answering "taken". A just-
// freed ephemeral port is occasionally (~1%) reused by other host traffic
// before the probe runs, so any of a few fresh ports reported free suffices.
func TestPortIsBindableWildcard_FreePort(t *testing.T) {
	var tried []int
	for range 5 {
		l, err := net.Listen("tcp", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()

		if PortIsBindableWildcard(port) {
			return
		}
		tried = append(tried, port)
	}
	t.Fatalf("freed ports %v all reported taken by PortIsBindableWildcard", tried)
}
