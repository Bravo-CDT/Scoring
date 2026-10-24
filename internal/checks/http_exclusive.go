package checks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// HTTPExclusive checks a service that must look right on its public port and
// must not also be reachable on a second, supposedly unexposed port (the
// nginx-in-front-of-docker shape: port 80 should serve the real site, the
// container's own port must not be reachable directly). Both checks must
// pass.
//
// Unlike HTTP, this check follows redirects (up to the standard library's
// default limit) and matches expect_contains_ci case-insensitively: it is
// judging what a browser would actually see, not a scored service's exact
// own response.
//
// Params:
//   - port (int, required): the public port, 1-65535.
//   - path (string, optional, default "/"): request path, starting with "/".
//   - expect_status (int, optional, default 200): required status code after
//     following redirects, 100-599.
//   - expect_contains_ci (string, required): case-insensitive substring the
//     first maxHTTPBodyBytes of the body must contain.
//   - closed_port (int, required): a port, 1-65535, that must NOT accept a
//     TCP connection.
func HTTPExclusive(ctx context.Context, host string, params map[string]any) Result {
	if err := checkHost(host); err != nil {
		return fail("%v", err)
	}
	p, err := parseHTTPExclusiveParams(params)
	if err != nil {
		return fail("%v", err)
	}

	if res, ok := checkPublicContent(ctx, host, p); !ok {
		return res
	}
	if res, ok := checkPortIsClosed(ctx, host, p.closedPort); !ok {
		return res
	}
	return Result{Success: true, Detail: fmt.Sprintf("status %d; closed_port %d unreachable", p.expectStatus, p.closedPort)}
}

// checkPublicContent performs the public-facing GET, following redirects.
// ok is false when the sub-check failed, with res already a failed Result.
func checkPublicContent(ctx context.Context, host string, p httpExclusiveParams) (res Result, ok bool) {
	base := "http://" + net.JoinHostPort(host, strconv.Itoa(p.port))
	url := base + p.path
	shown := base + redactedPath(p.path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fail("building request for %s: %v", shown, withoutURL(err)), false
	}

	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{}).DialContext,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: maxHTTPHeaderBytes,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	resp, err := client.Do(req)
	if err != nil {
		return fail("requesting %s: %v", shown, withoutURL(err)), false
	}
	defer resp.Body.Close()

	if resp.StatusCode != p.expectStatus {
		return fail("status %d, want %d", resp.StatusCode, p.expectStatus), false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBodyBytes))
	if err != nil {
		return fail("reading response body: %v", err), false
	}
	if !bytes.Contains(bytes.ToLower(body), bytes.ToLower([]byte(p.expectContainsCI))) {
		// Deliberately doesn't echo expectContainsCI: Detail may be shown
		// publicly, and that string is exactly what red team would want.
		return fail("response did not contain expected content"), false
	}
	return Result{}, true
}

// checkPortIsClosed succeeds (ok=true) when host:port refuses or times out a
// TCP connection -- the pass condition for a port that must not be exposed.
// A connection that succeeds is the failure case, so it's closed immediately
// with no data exchanged.
func checkPortIsClosed(ctx context.Context, host string, port int) (res Result, ok bool) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return Result{}, true
	}
	conn.Close()
	return fail("closed_port %s unexpectedly accepted a connection", addr), false
}

// httpExclusiveParams is HTTPExclusive's parsed params, with defaults
// applied.
type httpExclusiveParams struct {
	port             int
	path             string
	expectStatus     int
	expectContainsCI string
	closedPort       int
}

// parseHTTPExclusiveParams parses and validates HTTPExclusive's params. It is
// the only place they are checked, for ValidateParams and the check alike.
func parseHTTPExclusiveParams(params map[string]any) (httpExclusiveParams, error) {
	if err := rejectUnknownParams(params, "port", "path", "expect_status", "expect_contains_ci", "closed_port"); err != nil {
		return httpExclusiveParams{}, err
	}
	port, err := paramPort(params)
	if err != nil {
		return httpExclusiveParams{}, err
	}
	path, ok, err := stringParam(params, "path")
	if err != nil {
		return httpExclusiveParams{}, err
	}
	if !ok {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return httpExclusiveParams{}, fmt.Errorf("param %q must start with \"/\", got %q", "path", path)
	}
	expectStatus, ok, err := intParam(params, "expect_status")
	if err != nil {
		return httpExclusiveParams{}, err
	}
	if !ok {
		expectStatus = http.StatusOK
	}
	if expectStatus < 100 || expectStatus > 599 {
		return httpExclusiveParams{}, fmt.Errorf("param %q must be between 100 and 599, got %d", "expect_status", expectStatus)
	}
	expectContainsCI, ok, err := stringParam(params, "expect_contains_ci")
	if err != nil {
		return httpExclusiveParams{}, err
	}
	if !ok || expectContainsCI == "" {
		return httpExclusiveParams{}, errMissingRequired("expect_contains_ci")
	}
	closedPortRaw, ok, err := intParam(params, "closed_port")
	if err != nil {
		return httpExclusiveParams{}, err
	}
	if !ok {
		return httpExclusiveParams{}, errMissingRequired("closed_port")
	}
	if closedPortRaw < 1 || closedPortRaw > 65535 {
		return httpExclusiveParams{}, fmt.Errorf("param %q must be between 1 and 65535, got %d", "closed_port", closedPortRaw)
	}

	return httpExclusiveParams{
		port:             port,
		path:             path,
		expectStatus:     int(expectStatus),
		expectContainsCI: expectContainsCI,
		closedPort:       int(closedPortRaw),
	}, nil
}
