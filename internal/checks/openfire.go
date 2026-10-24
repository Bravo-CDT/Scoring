package checks

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	neturl "net/url"
	"strconv"
	"strings"
)

// Openfire logs in to an Openfire admin console over plain HTTP on
// params["port"]. It mirrors the curl pair
//
//	curl -c jar http://host:port/login.jsp
//	curl -b jar -d username=user -d password=password -d csrf=<jar's csrf> \
//	     -d login=true -d url=/index.jsp http://host:port/login.jsp
//
// and succeeds if the POST answers 302 with a Location that does not point
// back at login.jsp (Openfire's own redirect on a failed login).
//
// Params:
//   - port (int, required): TCP port, 1-65535 (9090 for Openfire).
//   - user (string, required): the admin console username.
//   - password (string, required): its password.
//
// Any other param is rejected.
//
// As in HTTP, redirects are not followed, and each call uses a fresh,
// proxy-free transport with keep-alives disabled and its own cookie jar.
func Openfire(ctx context.Context, host string, params map[string]any) Result {
	if err := checkHost(host); err != nil {
		return fail("%v", err)
	}
	p, err := parseOpenfireParams(params)
	if err != nil {
		return fail("%v", err)
	}

	loginURL := "http://" + net.JoinHostPort(host, strconv.Itoa(p.port)) + "/login.jsp"
	u, err := neturl.Parse(loginURL)
	if err != nil {
		return fail("building URL %s: %v", loginURL, withoutURL(err))
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return fail("creating cookie jar: %v", err)
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
		Jar:       jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// GET the login page for its csrf cookie, which the jar keeps.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loginURL, nil)
	if err != nil {
		return fail("building request for %s: %v", loginURL, withoutURL(err))
	}
	resp, err := client.Do(req)
	if err != nil {
		return fail("requesting %s: %v", loginURL, withoutURL(err))
	}
	drainAndClose(resp)
	var csrf string
	for _, c := range jar.Cookies(u) {
		if c.Name == "csrf" {
			csrf = c.Value
		}
	}
	if csrf == "" {
		return fail("GET %s: status %d, no csrf cookie", loginURL, resp.StatusCode)
	}

	// POST the login form; the jar sends the csrf cookie back.
	form := neturl.Values{
		"username": {p.user},
		"password": {p.password},
		"csrf":     {csrf},
		"login":    {"true"},
		"url":      {"/index.jsp"},
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fail("building login request for %s: %v", loginURL, withoutURL(err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = client.Do(req)
	if err != nil {
		return fail("logging in at %s: %v", loginURL, withoutURL(err))
	}
	drainAndClose(resp)

	if resp.StatusCode != http.StatusFound {
		return fail("login as %q: status %d, want 302", p.user, resp.StatusCode)
	}
	if strings.Contains(resp.Header.Get("Location"), "login.jsp") {
		return fail("login as %q: redirected back to login.jsp", p.user)
	}
	return Result{Success: true, Detail: "logged in as " + strconv.Quote(p.user)}
}

// drainAndClose reads and discards at most maxHTTPBodyBytes of resp's body, then
// closes it. Openfire's pages are not inspected, only status and headers.
func drainAndClose(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxHTTPBodyBytes))
	resp.Body.Close()
}

// openfireParams is Openfire's parsed params.
type openfireParams struct {
	port     int
	user     string
	password string
}

// parseOpenfireParams parses and validates Openfire's params. It is the only
// place they are checked, for ValidateParams and the check alike.
func parseOpenfireParams(params map[string]any) (openfireParams, error) {
	if err := rejectUnknownParams(params, "port", "user", "password"); err != nil {
		return openfireParams{}, err
	}
	port, err := paramPort(params)
	if err != nil {
		return openfireParams{}, err
	}
	p := openfireParams{port: port}
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"user", &p.user},
		{"password", &p.password},
	} {
		s, ok, err := stringParam(params, f.key)
		if err != nil {
			return openfireParams{}, err
		}
		if !ok || s == "" {
			return openfireParams{}, errMissingRequired(f.key)
		}
		*f.dst = s
	}
	return p, nil
}
