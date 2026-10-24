package checks

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
)

// maxHTTPBodyBytes bounds how much of a response body the HTTP check reads,
// so a hostile scored service cannot exhaust the engine's memory. Expected
// content appearing after this many bytes is not seen.
const maxHTTPBodyBytes = 64 * 1024

// maxHTTPHeaderBytes bounds the response header size for the same reason.
const maxHTTPHeaderBytes = 64 * 1024

// HTTP performs an HTTP or HTTPS GET against host and checks the response.
//
// Params:
//   - port (int, required): TCP port, 1-65535.
//   - path (string, optional, default "/"): request path, which may include a
//     query string; must start with "/".
//   - expect_status (int, optional, default 200): required status code,
//     100-599.
//   - expect_contains (string, optional): if non-empty, the first
//     maxHTTPBodyBytes of the body must contain it.
//   - https (bool, optional, default false): use HTTPS instead of plain HTTP.
//   - server_name (string, optional): if non-empty, the request is addressed
//     to this name (Host header, and TLS SNI and certificate verification)
//     while the connection still goes to host:port, like curl's --resolve.
//   - insecure_skip_verify (bool, optional, default false): accept any TLS
//     certificate, e.g. a self-signed one.
//
// Any other param is rejected.
//
// Redirects are not followed: the scored service is judged on its own
// response, so a redirect only passes when expect_status names that code.
// Each call uses a fresh, proxy-free transport with keep-alives disabled, so
// no connection or other state carries over between checks.
func HTTP(ctx context.Context, host string, params map[string]any) Result {
	if err := checkHost(host); err != nil {
		return fail("%v", err)
	}
	p, err := parseHTTPParams(params)
	if err != nil {
		return fail("%v", err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(p.port))
	scheme := "http"
	if p.https {
		scheme = "https"
	}
	urlHost := addr
	if p.serverName != "" {
		// Bare, with no port, so the Host header is just the name a
		// virtual host matches on; the dial goes to addr regardless.
		urlHost = p.serverName
	}
	base := scheme + "://" + urlHost
	url := base + p.path
	shown := base + redactedPath(p.path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fail("building request for %s: %v", shown, withoutURL(err))
	}

	dial := (&net.Dialer{}).DialContext
	if p.serverName != "" {
		// The URL names serverName, but the connection always goes to the
		// scored host: serverName is never looked up.
		dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
	}
	transport := &http.Transport{
		Proxy:                  nil, // never route checks through an environment proxy
		DialContext:            dial,
		TLSClientConfig:        &tls.Config{InsecureSkipVerify: p.insecureSkipVerify},
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: maxHTTPHeaderBytes,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return fail("requesting %s: %v", shown, withoutURL(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != p.expectStatus {
		return fail("status %d, want %d", resp.StatusCode, p.expectStatus)
	}
	if p.expectContains != "" {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBodyBytes))
		if err != nil {
			return fail("reading response body: %v", err)
		}
		if !bytes.Contains(body, []byte(p.expectContains)) {
			// Deliberately doesn't echo expectContains: Detail may be shown
			// publicly, and that string is exactly what red team would want.
			return fail("response did not contain expected content")
		}
	}
	return Result{Success: true, Detail: "status " + strconv.Itoa(resp.StatusCode)}
}

// redactedPath strips path's query string and fragment, for building a
// Detail message: Detail may be shown publicly, and a query string is where
// an admin is most likely to have put a token or key. The request itself
// still uses the full path.
func redactedPath(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		return path[:i]
	}
	return path
}

// withoutURL strips the URL that net/http and net/url wrap their errors with
// (a *url.Error prints its full URL, query string included), leaving just the
// underlying cause.
func withoutURL(err error) error {
	var urlErr *neturl.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// httpParams is HTTP's parsed params, with defaults applied.
type httpParams struct {
	port           int
	path           string
	expectStatus   int
	expectContains string
	https          bool
	serverName     string
	// insecureSkipVerify disables TLS certificate verification.
	insecureSkipVerify bool
}

// parseHTTPParams parses and validates HTTP's params. It is the only place
// they are checked, for ValidateParams and the check alike.
func parseHTTPParams(params map[string]any) (httpParams, error) {
	if err := rejectUnknownParams(params, "port", "path", "expect_status", "expect_contains",
		"https", "server_name", "insecure_skip_verify"); err != nil {
		return httpParams{}, err
	}
	port, err := paramPort(params)
	if err != nil {
		return httpParams{}, err
	}
	path, ok, err := stringParam(params, "path")
	if err != nil {
		return httpParams{}, err
	}
	if !ok {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return httpParams{}, fmt.Errorf("param %q must start with \"/\", got %q", "path", path)
	}
	expectStatus, ok, err := intParam(params, "expect_status")
	if err != nil {
		return httpParams{}, err
	}
	if !ok {
		expectStatus = http.StatusOK
	}
	if expectStatus < 100 || expectStatus > 599 {
		return httpParams{}, fmt.Errorf("param %q must be between 100 and 599, got %d", "expect_status", expectStatus)
	}
	expectContains, _, err := stringParam(params, "expect_contains")
	if err != nil {
		return httpParams{}, err
	}
	https, _, err := boolParam(params, "https")
	if err != nil {
		return httpParams{}, err
	}
	serverName, _, err := stringParam(params, "server_name")
	if err != nil {
		return httpParams{}, err
	}
	insecureSkipVerify, _, err := boolParam(params, "insecure_skip_verify")
	if err != nil {
		return httpParams{}, err
	}
	return httpParams{
		port:               port,
		path:               path,
		expectStatus:       int(expectStatus),
		expectContains:     expectContains,
		https:              https,
		serverName:         serverName,
		insecureSkipVerify: insecureSkipVerify,
	}, nil
}
