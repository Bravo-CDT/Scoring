package checks

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPExclusive_Success(t *testing.T) {
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/blog", http.StatusFound)
			return
		}
		w.Write([]byte("Powered by WordPress"))
	})
	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, closedPort := splitHostPortInt(t, closedLn.Addr().String())
	closedLn.Close() // nothing listens on this port now

	res := HTTPExclusive(context.Background(), host, map[string]any{
		"port": port, "expect_contains_ci": "wordpress", "closed_port": closedPort,
	})
	if !res.Success {
		t.Fatalf("expected success, got Detail=%q", res.Detail)
	}
}

func TestHTTPExclusive_FollowsRedirects(t *testing.T) {
	// Unlike the plain http check, http_exclusive is meant to judge the page
	// a browser would actually see, redirects included.
	followed := false
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			w.Write([]byte("WordPress site"))
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})

	res := HTTPExclusive(context.Background(), host, map[string]any{
		"port": port, "expect_contains_ci": "wordpress", "closed_port": mustClosedPort(t),
	})
	if !res.Success {
		t.Errorf("expected success, got Detail=%q", res.Detail)
	}
	if !followed {
		t.Error("expected the redirect to be followed")
	}
}

func TestHTTPExclusive_ContentMatchIsCaseInsensitive(t *testing.T) {
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("This site runs WORDPRESS"))
	})

	res := HTTPExclusive(context.Background(), host, map[string]any{
		"port": port, "expect_contains_ci": "wordpress", "closed_port": mustClosedPort(t),
	})
	if !res.Success {
		t.Errorf("expected case-insensitive match to succeed, got Detail=%q", res.Detail)
	}
}

func TestHTTPExclusive_ContentMismatch(t *testing.T) {
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("defaced"))
	})

	res := HTTPExclusive(context.Background(), host, map[string]any{
		"port": port, "expect_contains_ci": "SecretMarker", "closed_port": mustClosedPort(t),
	})
	if res.Success {
		t.Fatal("expected failure on content mismatch")
	}
	if strings.Contains(res.Detail, "SecretMarker") {
		t.Errorf("Detail = %q leaks the expected content string", res.Detail)
	}
}

func TestHTTPExclusive_StatusMismatch(t *testing.T) {
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	res := HTTPExclusive(context.Background(), host, map[string]any{
		"port": port, "expect_contains_ci": "wordpress", "closed_port": mustClosedPort(t),
	})
	if res.Success {
		t.Fatal("expected failure on status mismatch")
	}
	if !strings.Contains(res.Detail, "500") {
		t.Errorf("Detail = %q, expected it to report the actual status", res.Detail)
	}
}

func TestHTTPExclusive_ClosedPortUnexpectedlyOpen(t *testing.T) {
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("wordpress"))
	})
	leakHost, leakPort := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if leakHost != host {
		t.Fatalf("test setup: expected both servers on the same host, got %q and %q", host, leakHost)
	}

	res := HTTPExclusive(context.Background(), host, map[string]any{
		"port": port, "expect_contains_ci": "wordpress", "closed_port": leakPort,
	})
	if res.Success {
		t.Fatal("expected failure when the closed_port is actually reachable")
	}
	if !strings.Contains(res.Detail, "closed_port") {
		t.Errorf("Detail = %q, expected it to mention closed_port", res.Detail)
	}
}

func TestHTTPExclusive_FailureDetailOmitsQueryString(t *testing.T) {
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("wordpress"))
	})

	res := HTTPExclusive(context.Background(), host, map[string]any{
		"port": port, "path": "/blog?token=hunter2", "expect_status": 404,
		"expect_contains_ci": "wordpress", "closed_port": mustClosedPort(t),
	})
	if res.Success {
		t.Fatal("expected failure: server returns 200, not the expected 404")
	}
	if strings.Contains(res.Detail, "hunter2") || strings.Contains(res.Detail, "token") {
		t.Errorf("Detail = %q leaks the path's query string", res.Detail)
	}
}

func TestHTTPExclusive_InvalidParams(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]any
		mention string
	}{
		{"missing port", map[string]any{"expect_contains_ci": "x", "closed_port": 8080}, "port"},
		{"missing expect_contains_ci", map[string]any{"port": 80, "closed_port": 8080}, "expect_contains_ci"},
		{"empty expect_contains_ci", map[string]any{"port": 80, "expect_contains_ci": "", "closed_port": 8080}, "expect_contains_ci"},
		{"missing closed_port", map[string]any{"port": 80, "expect_contains_ci": "x"}, "closed_port"},
		{"closed_port out of range", map[string]any{"port": 80, "expect_contains_ci": "x", "closed_port": 70000}, "closed_port"},
		{"path not absolute", map[string]any{"port": 80, "expect_contains_ci": "x", "closed_port": 8080, "path": "blog"}, "path"},
		{"unknown param", map[string]any{"port": 80, "expect_contains_ci": "x", "closed_port": 8080, "expect_contain_ci": "x"}, "expect_contain_ci"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := HTTPExclusive(context.Background(), "127.0.0.1", tt.params)
			if res.Success {
				t.Fatal("expected failure for invalid params")
			}
			if !strings.Contains(res.Detail, tt.mention) {
				t.Errorf("Detail = %q, expected it to mention %q", res.Detail, tt.mention)
			}
		})
	}
}

func TestHTTPExclusive_RejectsEmptyHost(t *testing.T) {
	res := HTTPExclusive(context.Background(), "", map[string]any{
		"port": 80, "expect_contains_ci": "x", "closed_port": 8080,
	})
	if res.Success {
		t.Fatal("expected failure for an empty host")
	}
	if !strings.Contains(res.Detail, "host") {
		t.Errorf("Detail = %q, expected it to mention the host", res.Detail)
	}
}

func TestHTTPExclusive_RespectsContextDeadline(t *testing.T) {
	release := make(chan struct{})
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := HTTPExclusive(ctx, host, map[string]any{
		"port": port, "expect_contains_ci": "wordpress", "closed_port": mustClosedPort(t),
	})
	elapsed := time.Since(start)

	if res.Success {
		t.Error("expected failure when the server never responds")
	}
	if elapsed > 2*time.Second {
		t.Errorf("HTTPExclusive took %v, expected it to respect the ~50ms context deadline", elapsed)
	}
}

// mustClosedPort returns a port number nothing is listening on, for tests
// that don't care about the closed_port assertion itself.
func mustClosedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port := splitHostPortInt(t, ln.Addr().String())
	ln.Close()
	return port
}
