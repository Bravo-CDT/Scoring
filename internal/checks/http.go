package checks

import (
	"bytes"
	"context"
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

// HTTP performs a plain-HTTP GET against host and checks the response.
//
// Params:
//   - port (int, required): TCP port, 1-65535.
//   - path (string, optional, default "/"): request path, which may include a
//     query string; must start with "/".
//   - expect_status (int, optional, default 200): required status code,
//     100-599.
//   - expect_contains (string, optional): if non-empty, the first
//     maxHTTPBodyBytes of the body must contain it.
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

	base := "http://" + net.JoinHostPort(host, strconv.Itoa(p.port))
	url := base + p.path
	// Deliberately doesn't echo the path's query string (or fragment) in
	// Detail: Detail may be shown publicly, and a query string is where an
	// admin is most likely to have put a token or key. The request itself
	// still uses the full url.
	shown := url
	if i := strings.IndexAny(p.path, "?#"); i >= 0 {
		shown = base + p.path[:i]
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fail("building request for %s: %v", shown, withoutURL(err))
	}

	transport := &http.Transport{
		Proxy:                  nil, // never route checks through an environment proxy
		DialContext:            (&net.Dialer{}).DialContext,
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
}

// parseHTTPParams parses and validates HTTP's params. It is the only place
// they are checked, for ValidateParams and the check alike.
func parseHTTPParams(params map[string]any) (httpParams, error) {
	if err := rejectUnknownParams(params, "port", "path", "expect_status", "expect_contains"); err != nil {
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
	return httpParams{
		port:           port,
		path:           path,
		expectStatus:   int(expectStatus),
		expectContains: expectContains,
	}, nil
}
