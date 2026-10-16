package checks

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
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
//   - expect_status (int, optional, default 200): required status code.
//   - expect_contains (string, optional): if non-empty, the first
//     maxHTTPBodyBytes of the body must contain it.
//
// Redirects are not followed: the scored service is judged on its own
// response, so a redirect only passes when expect_status names that code.
// Each call uses a fresh, proxy-free transport with keep-alives disabled, so
// no connection or other state carries over between checks.
func HTTP(ctx context.Context, host string, params map[string]any) Result {
	if err := checkHost(host); err != nil {
		return fail("%v", err)
	}
	port, err := paramPort(params)
	if err != nil {
		return fail("%v", err)
	}
	path, ok, err := stringParam(params, "path")
	if err != nil {
		return fail("%v", err)
	}
	if !ok {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return fail("param %q must start with \"/\", got %q", "path", path)
	}
	expectStatus, ok, err := intParam(params, "expect_status")
	if err != nil {
		return fail("%v", err)
	}
	if !ok {
		expectStatus = http.StatusOK
	}
	expectContains, _, err := stringParam(params, "expect_contains")
	if err != nil {
		return fail("%v", err)
	}

	url := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fail("building request for %s: %v", url, err)
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
		return fail("requesting %s: %v", url, err)
	}
	defer resp.Body.Close()

	if int64(resp.StatusCode) != expectStatus {
		return fail("status %d, want %d", resp.StatusCode, expectStatus)
	}
	if expectContains != "" {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBodyBytes))
		if err != nil {
			return fail("reading response body: %v", err)
		}
		if !bytes.Contains(body, []byte(expectContains)) {
			// Deliberately doesn't echo expectContains: Detail may be shown
			// publicly, and that string is exactly what red team would want.
			return fail("response did not contain expected content")
		}
	}
	return Result{Success: true, Detail: "status " + strconv.Itoa(resp.StatusCode)}
}
