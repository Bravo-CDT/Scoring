package checks

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// hostPort returns the host and integer port a test server listens on.
func hostPort(t *testing.T, ts *httptest.Server) (string, int) {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	return splitHostPortInt(t, u.Host)
}

// newServer starts a test server and registers its shutdown with t.Cleanup.
func newServer(t *testing.T, h http.HandlerFunc) (string, int) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return hostPort(t, ts)
}

func TestHTTP_Success(t *testing.T) {
	var gotPath string
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Welcome to the site"))
	})

	res := HTTP(context.Background(), host, map[string]any{
		"port": port, "path": "/index.html?x=1", "expect_status": 200, "expect_contains": "Welcome",
	})
	if !res.Success {
		t.Errorf("expected success, got Detail=%q", res.Detail)
	}
	if gotPath != "/index.html?x=1" {
		t.Errorf("server saw request URI %q, want %q", gotPath, "/index.html?x=1")
	}
}

func TestHTTP_Defaults(t *testing.T) {
	// With only a port, the check GETs "/" and expects 200.
	var gotPath string
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	})

	res := HTTP(context.Background(), host, map[string]any{"port": port})
	if !res.Success {
		t.Errorf("expected success with default path/status, got Detail=%q", res.Detail)
	}
	if gotPath != "/" {
		t.Errorf("default path = %q, want %q", gotPath, "/")
	}
}

func TestHTTP_ExpectStatusInt64(t *testing.T) {
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	res := HTTP(context.Background(), host, map[string]any{"port": int64(port), "expect_status": int64(204)})
	if !res.Success {
		t.Errorf("expected success, got Detail=%q", res.Detail)
	}
}

func TestHTTP_StatusMismatch(t *testing.T) {
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	res := HTTP(context.Background(), host, map[string]any{
		"port": port, "path": "/", "expect_status": 200,
	})
	if res.Success {
		t.Error("expected failure on status mismatch")
	}
	if !strings.Contains(res.Detail, "500") {
		t.Errorf("Detail = %q, expected it to report the actual status", res.Detail)
	}
}

func TestHTTP_ContentMismatch(t *testing.T) {
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("defaced"))
	})

	res := HTTP(context.Background(), host, map[string]any{
		"port": port, "path": "/", "expect_status": 200, "expect_contains": "SecretMarker",
	})
	if res.Success {
		t.Error("expected failure on content mismatch")
	}
	if strings.Contains(res.Detail, "SecretMarker") {
		t.Errorf("Detail = %q leaks the expected content string", res.Detail)
	}
}

func TestHTTP_FailureDetailOmitsQueryString(t *testing.T) {
	// Point the check at a port nothing listens on, so it fails in the
	// request itself, the error path that echoes the URL.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, port := splitHostPortInt(t, ln.Addr().String())
	ln.Close() // now nothing is listening on this port

	res := HTTP(context.Background(), host, map[string]any{
		"port": port, "path": "/health?api_key=hunter2",
	})
	if res.Success {
		t.Fatal("expected failure against a closed port")
	}
	if strings.Contains(res.Detail, "hunter2") || strings.Contains(res.Detail, "api_key") || strings.Contains(res.Detail, "?") {
		t.Errorf("Detail = %q leaks the path's query string", res.Detail)
	}
	if !strings.Contains(res.Detail, "/health") {
		t.Errorf("Detail = %q, expected it to still name the path being checked", res.Detail)
	}
}

func TestHTTP_DoesNotFollowRedirects(t *testing.T) {
	// A scored service is judged on its own response; a redirect elsewhere
	// (possibly to a red-team-controlled host) must not be followed.
	followed := false
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			w.Write([]byte("Welcome"))
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})

	res := HTTP(context.Background(), host, map[string]any{"port": port, "expect_contains": "Welcome"})
	if res.Success {
		t.Error("expected failure: a 302 does not satisfy the default expect_status 200")
	}
	if followed {
		t.Error("check followed the redirect")
	}

	res = HTTP(context.Background(), host, map[string]any{"port": port, "expect_status": 302})
	if !res.Success {
		t.Errorf("expected success when the redirect status itself is expected, got Detail=%q", res.Detail)
	}
}

func TestHTTP_BodyReadIsBounded(t *testing.T) {
	// The marker sits past the read limit, so the check must not see it.
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("a", maxHTTPBodyBytes)))
		w.Write([]byte("Welcome"))
	})

	res := HTTP(context.Background(), host, map[string]any{"port": port, "expect_contains": "Welcome"})
	if res.Success {
		t.Error("expected failure: expected content beyond the body read limit must not be found")
	}
}

func TestHTTP_InvalidParams(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]any
		mention string
	}{
		{"missing port", map[string]any{}, "port"},
		{"nil params", nil, "port"},
		{"port out of range", map[string]any{"port": 0}, "port"},
		{"path wrong type", map[string]any{"port": 80, "path": 5}, "path"},
		{"path not absolute", map[string]any{"port": 80, "path": "index.html"}, "path"},
		{"expect_status wrong type", map[string]any{"port": 80, "expect_status": "200"}, "expect_status"},
		{"expect_contains wrong type", map[string]any{"port": 80, "expect_contains": 7}, "expect_contains"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := HTTP(context.Background(), "127.0.0.1", tt.params)
			if res.Success {
				t.Fatal("expected failure for invalid params")
			}
			if !strings.Contains(res.Detail, tt.mention) {
				t.Errorf("Detail = %q, expected it to mention %q", res.Detail, tt.mention)
			}
		})
	}
}

func TestHTTP_RespectsContextDeadline_HungHeaders(t *testing.T) {
	release := make(chan struct{})
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	// Registered after newServer, so it runs first and unblocks the
	// handler before ts.Close waits on it.
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := HTTP(ctx, host, map[string]any{"port": port})
	elapsed := time.Since(start)

	if res.Success {
		t.Error("expected failure when the server never responds")
	}
	if elapsed > 2*time.Second {
		t.Errorf("HTTP took %v, expected it to respect the ~50ms context deadline", elapsed)
	}
}

func TestHTTP_RespectsContextDeadline_HungBody(t *testing.T) {
	// Headers arrive promptly but the body never finishes: the bounded body
	// read must also be cut off by the context.
	release := make(chan struct{})
	host, port := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := HTTP(ctx, host, map[string]any{"port": port, "expect_contains": "Welcome"})
	elapsed := time.Since(start)

	if res.Success {
		t.Error("expected failure when the body never completes")
	}
	if elapsed > 2*time.Second {
		t.Errorf("HTTP took %v, expected it to respect the ~50ms context deadline", elapsed)
	}
}

// newTLSServer starts a self-signed HTTPS test server and registers its
// shutdown with t.Cleanup.
func newTLSServer(t *testing.T, h http.HandlerFunc) (string, int) {
	t.Helper()
	ts := httptest.NewTLSServer(h)
	t.Cleanup(ts.Close)
	return hostPort(t, ts)
}

func TestHTTP_HTTPSWithServerName(t *testing.T) {
	var gotHost string
	host, port := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
	})

	res := HTTP(context.Background(), host, map[string]any{
		"port":                 port,
		"https":                true,
		"server_name":          "orderwindow.krusty.krab",
		"insecure_skip_verify": true,
	})
	if !res.Success {
		t.Fatalf("expected success, got %q", res.Detail)
	}
	if gotHost != "orderwindow.krusty.krab" {
		t.Errorf("Host = %q, want %q", gotHost, "orderwindow.krusty.krab")
	}
}

func TestHTTP_HTTPSVerifiesCertByDefault(t *testing.T) {
	host, port := newTLSServer(t, func(http.ResponseWriter, *http.Request) {})

	res := HTTP(context.Background(), host, map[string]any{
		"port":        port,
		"https":       true,
		"server_name": "orderwindow.krusty.krab",
	})
	if res.Success {
		t.Fatal("expected failure against a self-signed certificate")
	}
	if !strings.Contains(res.Detail, "certificate") {
		t.Errorf("Detail = %q, expected it to mention the certificate", res.Detail)
	}
}

func TestHTTP_HTTPSParamTypes(t *testing.T) {
	for _, params := range []map[string]any{
		{"port": 443, "https": "yes"},
		{"port": 443, "server_name": 5},
	} {
		if err := ValidateParams("http", params); err == nil {
			t.Errorf("ValidateParams(%v) = nil, want an error", params)
		}
	}
}
