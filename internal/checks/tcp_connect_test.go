package checks

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTCPConnect_Success(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	host, port := splitHostPortInt(t, ln.Addr().String())

	res := TCPConnect(context.Background(), host, map[string]any{"port": port})
	if !res.Success {
		t.Errorf("expected success, got Detail=%q", res.Detail)
	}
}

func TestTCPConnect_ClosedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, port := splitHostPortInt(t, ln.Addr().String())
	ln.Close() // now nothing is listening on this port

	res := TCPConnect(context.Background(), host, map[string]any{"port": port})
	if res.Success {
		t.Error("expected failure connecting to a closed port")
	}
}

func TestTCPConnect_IPv6(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer ln.Close()
	host, port := splitHostPortInt(t, ln.Addr().String())

	res := TCPConnect(context.Background(), host, map[string]any{"port": port})
	if !res.Success {
		t.Errorf("expected success against IPv6 loopback, got Detail=%q", res.Detail)
	}
}

func TestTCPConnect_InvalidPort(t *testing.T) {
	for name, params := range map[string]map[string]any{
		"missing":      {},
		"nil params":   nil,
		"wrong type":   {"port": "22"},
		"out of range": {"port": 70000},
	} {
		t.Run(name, func(t *testing.T) {
			res := TCPConnect(context.Background(), "127.0.0.1", params)
			if res.Success {
				t.Error("expected failure for invalid port param")
			}
			if !strings.Contains(res.Detail, "port") {
				t.Errorf("Detail = %q, expected it to mention the port param", res.Detail)
			}
		})
	}
}

func TestTCPConnect_RespectsContextDeadline(t *testing.T) {
	// 192.0.2.0/24 is reserved for documentation (RFC 5737) and never
	// routed; the context deadline — not the network — is what ends this
	// call, so the test is fast and has no real external dependency.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := TCPConnect(ctx, "192.0.2.1", map[string]any{"port": 81})
	elapsed := time.Since(start)

	if res.Success {
		t.Error("expected failure against an unroutable address")
	}
	if elapsed > 2*time.Second {
		t.Errorf("TCPConnect took %v, expected it to respect the ~50ms context deadline", elapsed)
	}
}

func TestTCPConnect_CanceledContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	host, port := splitHostPortInt(t, ln.Addr().String())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := TCPConnect(ctx, host, map[string]any{"port": port})
	if res.Success {
		t.Error("expected failure with an already-canceled context, even against a live listener")
	}
}
