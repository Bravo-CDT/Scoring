package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Bravo-CDT/Scoring/internal/auth"
	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/engine"
	"github.com/Bravo-CDT/Scoring/internal/scheduler"
	"github.com/Bravo-CDT/Scoring/internal/scorer"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

const (
	testUsername = "admin"
	testPassword = "secret"
	waitTimeout  = 5 * time.Second
)

var testHash = func() string {
	h, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		panic("generating test bcrypt hash: " + err.Error())
	}
	return string(h)
}()

// configYAML renders a valid config.yaml with the named tcp_connect services.
func configYAML(names ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `
session_name: "comp-1"
starting_score: 1000
round_interval_seconds: 1
admin:
  username: %s
  password_hash: %q
services:
`, testUsername, testHash)
	for _, n := range names {
		fmt.Fprintf(&b, `  - name: %s
    host: 127.0.0.1
    check_type: tcp_connect
    points_per_failure: 10
    params: {port: 9}
`, n)
	}
	return b.String()
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	t          *testing.T
	url        string
	st         *store.Store
	sess       store.Session
	e          *engine.Engine
	configPath string
	logs       *syncBuffer
}

func okCheck(context.Context, string, map[string]any) checks.Result {
	return checks.Result{Success: true, Detail: "ok"}
}

func failCheck(context.Context, string, map[string]any) checks.Result {
	return checks.Result{Success: false, Detail: "connection refused"}
}

// testRound is the harness scheduler's round interval: short, so a started
// service is checked within milliseconds.
const testRound = 20 * time.Millisecond

// newHarness builds the full stack (store, scorer, scheduler, engine, auth)
// the way the binary does, with service rows created as at startup, and
// serves the API over a real HTTP server.
func newHarness(t *testing.T, check checks.CheckFunc, services ...string) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, configPath: filepath.Join(dir, "config.yaml"), logs: &syncBuffer{}}
	h.writeConfig(configYAML(services...))

	st, err := store.Open(filepath.Join(dir, "scoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	h.st = st

	if h.sess, err = st.GetOrCreateSession("comp-1", 1000); err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	for _, name := range services {
		if _, err := st.GetOrCreateService(h.sess.ID, name); err != nil {
			t.Fatalf("GetOrCreateService(%q): %v", name, err)
		}
	}

	registry := checks.NewRegistry()
	registry.Register("tcp_connect", check)
	sched := scheduler.New(registry, scorer.New(st, h.sess.ID, nil), h.configPath, testRound)
	h.e = engine.New(st, h.sess, sched, h.configPath)
	// Registered after the store's Close, so it runs first.
	t.Cleanup(func() { h.e.Stop() })
	// Registered after that, so it runs before both: as in app.Close, the
	// round loop is gone before anything it records into is torn down.
	ctx, cancel := context.WithCancel(context.Background())
	done := sched.Run(ctx, 1)
	t.Cleanup(func() { cancel(); <-done })

	a := auth.New(testUsername, testHash)
	srv := httptest.NewServer(newServer(h.e, st, h.sess.ID, a, h.configPath, log.New(h.logs, "", 0)))
	t.Cleanup(srv.Close)
	h.url = srv.URL
	return h
}

func (h *harness) writeConfig(contents string) {
	h.t.Helper()
	if err := os.WriteFile(h.configPath, []byte(contents), 0o600); err != nil {
		h.t.Fatalf("writing config: %v", err)
	}
}

func (h *harness) service(name string) store.Service {
	h.t.Helper()
	svc, found, err := h.st.GetServiceByName(h.sess.ID, name)
	if err != nil || !found {
		h.t.Fatalf("GetServiceByName(%q): found=%v err=%v", name, found, err)
	}
	return svc
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// object decodes the body as a JSON object.
func (r response) object(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("decoding %q as an object: %v", r.body, err)
	}
	return out
}

// array decodes the body as a JSON array of objects.
func (r response) array(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("decoding %q as an array: %v", r.body, err)
	}
	return out
}

// do sends a request with an optional raw body and bearer token. Every
// response, whatever its status, must be JSON.
func (h *harness) do(method, path, token, body string) response {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.url+path, rd)
	if err != nil {
		h.t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("reading body: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		h.t.Errorf("%s %s: Content-Type = %q, want application/json", method, path, ct)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: raw}
}

func (h *harness) mustStatus(r response, want int, what string) {
	h.t.Helper()
	if r.status != want {
		h.t.Fatalf("%s: status = %d, want %d (body %s)", what, r.status, want, r.body)
	}
}

func (h *harness) login() string {
	h.t.Helper()
	r := h.do(http.MethodPost, "/login", "",
		fmt.Sprintf(`{"username":%q,"password":%q}`, testUsername, testPassword))
	h.mustStatus(r, http.StatusOK, "login")
	token, _ := r.object(h.t)["token"].(string)
	if token == "" {
		h.t.Fatal("login returned an empty token")
	}
	return token
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// adminRoutes is every route behind RequireAdmin.
var adminRoutes = []string{
	"/engine/start",
	"/engine/stop",
	"/scoring/pause",
	"/scoring/resume",
	"/services/svc1/start",
	"/services/svc1/stop",
	"/services/svc1/restart",
	"/services/svc1/scoring/pause",
	"/services/svc1/scoring/resume",
	"/reload",
}

func TestStatus_DocumentedFields(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	r := h.do(http.MethodGet, "/status", "", "")
	h.mustStatus(r, http.StatusOK, "GET /status")

	out := r.object(t)
	// countdown_target is omitted until an admin sets one.
	wantTop := []string{"blue_score", "engine_state", "next_round_at", "red_score", "round",
		"scoring_paused", "server_time", "services", "session_name", "starting_score"}
	if got := keys(out); !slices.Equal(got, wantTop) {
		t.Errorf("status keys = %v, want %v", got, wantTop)
	}
	if out["session_name"] != "comp-1" || out["starting_score"] != 1000.0 ||
		out["blue_score"] != 1000.0 || out["red_score"] != 0.0 {
		t.Errorf("unexpected session/score fields: %v", out)
	}
	if out["engine_state"] != "stopped" {
		t.Errorf("engine_state = %v, want stopped on a fresh process", out["engine_state"])
	}
	if out["scoring_paused"] != true {
		t.Errorf("scoring_paused = %v, want true on a fresh process", out["scoring_paused"])
	}
	if ts, _ := out["server_time"].(string); !strings.HasSuffix(ts, "Z") {
		t.Errorf("server_time = %q, want an RFC 3339 UTC time", ts)
	} else if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("server_time %q: %v", ts, err)
	}

	services, _ := out["services"].([]any)
	if len(services) != 1 {
		t.Fatalf("services = %v, want one entry", out["services"])
	}
	svc, _ := services[0].(map[string]any)
	// last_check_time is omitted before any check has run.
	wantSvc := []string{"consecutive_failures", "latency_ms", "name", "routine_enabled", "scoring_paused", "up"}
	if got := keys(svc); !slices.Equal(got, wantSvc) {
		t.Errorf("service keys = %v, want %v", got, wantSvc)
	}
	if svc["name"] != "svc1" {
		t.Errorf("service name = %v, want svc1", svc["name"])
	}
}

func TestLogin(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	tests := []struct {
		name string
		body string
		want int
	}{
		{"valid credentials", `{"username":"admin","password":"secret"}`, http.StatusOK},
		{"wrong password", `{"username":"admin","password":"wrong"}`, http.StatusUnauthorized},
		{"wrong username", `{"username":"root","password":"secret"}`, http.StatusUnauthorized},
		{"missing fields", `{}`, http.StatusUnauthorized},
		{"empty body", ``, http.StatusBadRequest},
		{"malformed JSON", `{"username":`, http.StatusBadRequest},
		{"wrong type", `{"username":1,"password":"secret"}`, http.StatusBadRequest},
		{"unknown field", `{"username":"admin","password":"secret","admin":true}`, http.StatusBadRequest},
		{"trailing data", `{"username":"admin","password":"secret"} {}`, http.StatusBadRequest},
		{"oversized body", `{"username":"admin","password":"` + strings.Repeat("x", 8192) + `"}`,
			http.StatusRequestEntityTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := h.do(http.MethodPost, "/login", "", tc.body)
			h.mustStatus(r, tc.want, "POST /login")
			out := r.object(t)
			if tc.want == http.StatusOK {
				if tok, _ := out["token"].(string); tok == "" {
					t.Errorf("token missing from %s", r.body)
				}
			} else if _, ok := out["error"]; !ok {
				t.Errorf("error response %s has no error field", r.body)
			}
		})
	}
}

func TestAdminRoutes_RequireValidToken(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	for _, route := range adminRoutes {
		for _, token := range []string{"", "not-a-real-token"} {
			t.Run(fmt.Sprintf("%s token=%q", route, token), func(t *testing.T) {
				r := h.do(http.MethodPost, route, token, "")
				h.mustStatus(r, http.StatusUnauthorized, "POST "+route)
			})
		}
	}

	// Nothing those requests asked for happened.
	st := h.do(http.MethodGet, "/status", "", "").object(t)
	if st["engine_state"] != "stopped" {
		t.Errorf("engine_state = %v after unauthenticated start, want stopped", st["engine_state"])
	}
	if events := h.do(http.MethodGet, "/events", "", "").array(t); len(events) != 0 {
		t.Errorf("unauthenticated requests produced events: %v", events)
	}
}

func TestPerServiceRoutes_UnknownService404(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	token := h.login()
	routes := []struct{ method, path string }{
		{http.MethodGet, "/services/nope/results"},
		{http.MethodPost, "/services/nope/start"},
		{http.MethodPost, "/services/nope/stop"},
		{http.MethodPost, "/services/nope/restart"},
		{http.MethodPost, "/services/nope/scoring/pause"},
		{http.MethodPost, "/services/nope/scoring/resume"},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			r := h.do(rt.method, rt.path, token, "")
			h.mustStatus(r, http.StatusNotFound, rt.method+" "+rt.path)
			if _, ok := r.object(t)["error"]; !ok {
				t.Errorf("404 body %s has no error field", r.body)
			}
		})
	}
}

func TestMux_MethodNotAllowedAndNotFound(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	tests := []struct {
		method, path string
		want         int
		allow        string
	}{
		{http.MethodGet, "/engine/start", http.StatusMethodNotAllowed, "POST"},
		{http.MethodGet, "/services/svc1/restart", http.StatusMethodNotAllowed, "POST"},
		{http.MethodPost, "/status", http.StatusMethodNotAllowed, "GET"},
		{http.MethodDelete, "/events", http.StatusMethodNotAllowed, "GET"},
		{http.MethodGet, "/no/such/route", http.StatusNotFound, ""},
		{http.MethodPost, "/services/svc1/explode", http.StatusNotFound, ""},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			r := h.do(tc.method, tc.path, "", "")
			h.mustStatus(r, tc.want, tc.method+" "+tc.path)
			if tc.allow != "" && !strings.Contains(r.header.Get("Allow"), tc.allow) {
				t.Errorf("Allow = %q, want it to include %s", r.header.Get("Allow"), tc.allow)
			}
			if _, ok := r.object(t)["error"]; !ok {
				t.Errorf("body %s has no error field", r.body)
			}
		})
	}
}

func TestAdminActions_AreIdempotentAndReturnCurrentState(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	token := h.login()

	tests := []struct {
		path  string
		check func(t *testing.T, st map[string]any)
	}{
		{"/engine/start", func(t *testing.T, st map[string]any) {
			if st["engine_state"] != "running" {
				t.Errorf("engine_state = %v, want running", st["engine_state"])
			}
			// Starting the engine never lifts the scoring pause.
			if st["scoring_paused"] != true {
				t.Errorf("scoring_paused = %v, want true: start must not auto-resume", st["scoring_paused"])
			}
		}},
		{"/scoring/resume", func(t *testing.T, st map[string]any) {
			if st["scoring_paused"] != false {
				t.Errorf("scoring_paused = %v, want false", st["scoring_paused"])
			}
		}},
		{"/scoring/pause", func(t *testing.T, st map[string]any) {
			if st["scoring_paused"] != true {
				t.Errorf("scoring_paused = %v, want true", st["scoring_paused"])
			}
		}},
		{"/services/svc1/stop", func(t *testing.T, st map[string]any) {
			if svc := serviceIn(t, st, "svc1"); svc["routine_enabled"] != false {
				t.Errorf("routine_enabled = %v, want false", svc["routine_enabled"])
			}
		}},
		{"/services/svc1/start", func(t *testing.T, st map[string]any) {
			if svc := serviceIn(t, st, "svc1"); svc["routine_enabled"] != true {
				t.Errorf("routine_enabled = %v, want true", svc["routine_enabled"])
			}
		}},
		{"/services/svc1/scoring/pause", func(t *testing.T, st map[string]any) {
			if svc := serviceIn(t, st, "svc1"); svc["scoring_paused"] != true {
				t.Errorf("service scoring_paused = %v, want true", svc["scoring_paused"])
			}
		}},
		{"/services/svc1/scoring/resume", func(t *testing.T, st map[string]any) {
			if svc := serviceIn(t, st, "svc1"); svc["scoring_paused"] != false {
				t.Errorf("service scoring_paused = %v, want false", svc["scoring_paused"])
			}
		}},
		{"/engine/stop", func(t *testing.T, st map[string]any) {
			if st["engine_state"] != "stopped" {
				t.Errorf("engine_state = %v, want stopped", st["engine_state"])
			}
		}},
	}
	for _, tc := range tests {
		for i := 1; i <= 2; i++ {
			r := h.do(http.MethodPost, tc.path, token, "")
			h.mustStatus(r, http.StatusOK, fmt.Sprintf("POST %s (call %d)", tc.path, i))
			t.Run(fmt.Sprintf("%s call %d", tc.path, i), func(t *testing.T) { tc.check(t, r.object(t)) })
		}
	}
}

func serviceIn(t *testing.T, status map[string]any, name string) map[string]any {
	t.Helper()
	services, _ := status["services"].([]any)
	for _, s := range services {
		if m, _ := s.(map[string]any); m["name"] == name {
			return m
		}
	}
	t.Fatalf("service %q not in status %v", name, status)
	return nil
}

func TestRestart(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	token := h.login()
	h.mustStatus(h.do(http.MethodPost, "/engine/start", token, ""), http.StatusOK, "engine start")

	t.Run("no body", func(t *testing.T) {
		r := h.do(http.MethodPost, "/services/svc1/restart", token, "")
		h.mustStatus(r, http.StatusOK, "restart")
		if svc := serviceIn(t, r.object(t), "svc1"); svc["scoring_paused"] != false {
			t.Errorf("scoring_paused = %v, want false without pause_scoring", svc["scoring_paused"])
		}
	})
	t.Run("pause_scoring false", func(t *testing.T) {
		r := h.do(http.MethodPost, "/services/svc1/restart", token, `{"pause_scoring":false}`)
		h.mustStatus(r, http.StatusOK, "restart")
	})
	for _, body := range []string{`{"pause_scoring":`, `{"pause_scoring":"yes"}`, `{"pauseScoring":true}`, `[]`} {
		t.Run("bad body "+body, func(t *testing.T) {
			r := h.do(http.MethodPost, "/services/svc1/restart", token, body)
			h.mustStatus(r, http.StatusBadRequest, "restart with "+body)
		})
	}
	t.Run("oversized body", func(t *testing.T) {
		body := `{"pause_scoring":true` + strings.Repeat(" ", 8192) + `}`
		h.mustStatus(h.do(http.MethodPost, "/services/svc1/restart", token, body),
			http.StatusRequestEntityTooLarge, "restart with oversized body")
		if h.service("svc1").ScoringPaused {
			t.Error("a rejected request paused the service")
		}
	})
	t.Run("pause_scoring true", func(t *testing.T) {
		r := h.do(http.MethodPost, "/services/svc1/restart", token, `{"pause_scoring":true}`)
		h.mustStatus(r, http.StatusOK, "restart")
		svc := serviceIn(t, r.object(t), "svc1")
		if svc["scoring_paused"] != true || svc["routine_enabled"] != true {
			t.Errorf("service = %v, want scoring paused with routine running", svc)
		}
		if !h.service("svc1").ScoringPaused {
			t.Error("pause not persisted")
		}
	})
}

func TestResults_DetailOnlyForAdmin(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	svc := h.service("svc1")
	ts := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	if _, err := h.st.InsertCheckResult(store.CheckResult{
		SessionID: h.sess.ID, ServiceID: svc.ID, Timestamp: ts,
		Success: false, LatencyMS: 5, Detail: "super-secret-credential-leak",
	}); err != nil {
		t.Fatalf("InsertCheckResult: %v", err)
	}
	token := h.login()

	for _, tc := range []struct {
		name, token string
		wantDetail  bool
	}{
		{"public", "", false},
		{"invalid token", "bogus", false},
		{"admin", token, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := h.do(http.MethodGet, "/services/svc1/results", tc.token, "")
			h.mustStatus(r, http.StatusOK, "GET results")
			if leaked := bytes.Contains(r.body, []byte("super-secret-credential-leak")); leaked != tc.wantDetail {
				t.Errorf("detail in body = %v, want %v: %s", leaked, tc.wantDetail, r.body)
			}
			rows := r.array(t)
			if len(rows) != 1 {
				t.Fatalf("rows = %v, want 1", rows)
			}
			want := []string{"id", "latency_ms", "success", "timestamp"}
			if tc.wantDetail {
				want = []string{"detail", "id", "latency_ms", "success", "timestamp"}
			}
			if got := keys(rows[0]); !slices.Equal(got, want) {
				t.Errorf("keys = %v, want %v", got, want)
			}
			if rows[0]["timestamp"] != "2026-10-15T12:00:00Z" || rows[0]["success"] != false ||
				rows[0]["latency_ms"] != 5.0 {
				t.Errorf("row = %v", rows[0])
			}
		})
	}
}

func TestResults_EmptyAndRetired(t *testing.T) {
	h := newHarness(t, okCheck, "svc1", "svc2")
	r := h.do(http.MethodGet, "/services/svc1/results", "", "")
	h.mustStatus(r, http.StatusOK, "GET results")
	if string(bytes.TrimSpace(r.body)) != "[]" {
		t.Errorf("empty results body = %s, want []", r.body)
	}

	// A service retired by a reload keeps its history queryable.
	if _, err := h.st.InsertCheckResult(store.CheckResult{
		SessionID: h.sess.ID, ServiceID: h.service("svc2").ID, Success: true,
	}); err != nil {
		t.Fatalf("InsertCheckResult: %v", err)
	}
	h.writeConfig(configYAML("svc1"))
	h.mustStatus(h.do(http.MethodPost, "/reload", h.login(), ""), http.StatusOK, "reload")
	r = h.do(http.MethodGet, "/services/svc2/results", "", "")
	h.mustStatus(r, http.StatusOK, "GET retired results")
	if rows := r.array(t); len(rows) != 1 {
		t.Errorf("retired service rows = %v, want 1", rows)
	}
}

func TestResults_Pagination(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	svc := h.service("svc1")
	base := time.Now().UTC().Add(-time.Hour)
	var ids []int64
	for i := range 60 {
		id, err := h.st.InsertCheckResult(store.CheckResult{
			SessionID: h.sess.ID, ServiceID: svc.ID, Timestamp: base.Add(time.Duration(i) * time.Second), Success: true,
		})
		if err != nil {
			t.Fatalf("InsertCheckResult: %v", err)
		}
		ids = append(ids, id)
	}

	idsOf := func(r response) []int64 {
		var out []int64
		for _, row := range r.array(t) {
			out = append(out, int64(row["id"].(float64)))
		}
		return out
	}
	if got := idsOf(h.do(http.MethodGet, "/services/svc1/results", "", "")); len(got) != 50 || got[0] != ids[59] {
		t.Errorf("default page: %d rows starting %v, want 50 starting %d", len(got), got[:1], ids[59])
	}
	if got := idsOf(h.do(http.MethodGet, "/services/svc1/results?limit=nope", "", "")); len(got) != 50 {
		t.Errorf("invalid limit: %d rows, want default 50", len(got))
	}
	got := idsOf(h.do(http.MethodGet, "/services/svc1/results?limit=2", "", ""))
	if !slices.Equal(got, []int64{ids[59], ids[58]}) {
		t.Errorf("limit=2: %v, want [%d %d]", got, ids[59], ids[58])
	}
	got = idsOf(h.do(http.MethodGet, fmt.Sprintf("/services/svc1/results?limit=2&before=%d", ids[58]), "", ""))
	if !slices.Equal(got, []int64{ids[57], ids[56]}) {
		t.Errorf("second page: %v, want [%d %d]", got, ids[57], ids[56])
	}
}

func TestEvents_ShapeAndDetail(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	svc := h.service("svc1")
	ts := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, ev := range []store.Event{
		{SessionID: h.sess.ID, Level: "info", Actor: "admin", Type: "engine_started",
			Message: "engine started", Timestamp: ts},
		{SessionID: h.sess.ID, ServiceID: &svc.ID, Level: "error", Actor: "system",
			Type: "routine_start_failed", Message: "svc1 routine failed to start",
			Detail: "open /etc/secret/config.yaml: permission denied", Timestamp: ts.Add(time.Second)},
	} {
		if err := h.st.InsertEvent(ev); err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
	}
	token := h.login()

	for _, tc := range []struct {
		name, token string
		wantDetail  bool
	}{
		{"public", "", false},
		{"admin", token, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := h.do(http.MethodGet, "/events", tc.token, "")
			h.mustStatus(r, http.StatusOK, "GET /events")
			if leaked := bytes.Contains(r.body, []byte("/etc/secret")); leaked != tc.wantDetail {
				t.Errorf("detail in body = %v, want %v: %s", leaked, tc.wantDetail, r.body)
			}
			events := r.array(t)
			if len(events) != 2 {
				t.Fatalf("events = %v, want 2", events)
			}
			svcEvent, engineEvent := events[0], events[1] // newest first

			wantSvc := []string{"actor", "id", "level", "message", "service_id", "timestamp", "type"}
			if tc.wantDetail {
				wantSvc = []string{"actor", "detail", "id", "level", "message", "service_id", "timestamp", "type"}
			}
			if got := keys(svcEvent); !slices.Equal(got, wantSvc) {
				t.Errorf("service event keys = %v, want %v", got, wantSvc)
			}
			if svcEvent["service_id"] != float64(svc.ID) || svcEvent["level"] != "error" ||
				svcEvent["actor"] != "system" || svcEvent["type"] != "routine_start_failed" ||
				svcEvent["timestamp"] != "2026-10-15T12:00:01Z" {
				t.Errorf("service event = %v", svcEvent)
			}
			// No service id and empty detail: both omitted for everyone.
			wantEngine := []string{"actor", "id", "level", "message", "timestamp", "type"}
			if got := keys(engineEvent); !slices.Equal(got, wantEngine) {
				t.Errorf("engine event keys = %v, want %v", got, wantEngine)
			}
		})
	}
}

func TestEvents_Pagination(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	base := time.Now().UTC().Add(-time.Hour)
	for i := range 5 {
		if err := h.st.InsertEvent(store.Event{
			SessionID: h.sess.ID, Level: "info", Actor: "system", Type: "test",
			Message: fmt.Sprintf("event %d", i), Timestamp: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
	}
	messages := func(r response) []string {
		out := []string{}
		for _, ev := range r.array(t) {
			out = append(out, ev["message"].(string))
		}
		return out
	}

	page1 := h.do(http.MethodGet, "/events?limit=2", "", "")
	if got := messages(page1); !slices.Equal(got, []string{"event 4", "event 3"}) {
		t.Errorf("page 1 = %v", got)
	}
	last := int64(page1.array(t)[1]["id"].(float64))
	page2 := h.do(http.MethodGet, fmt.Sprintf("/events?limit=2&before=%d", last), "", "")
	if got := messages(page2); !slices.Equal(got, []string{"event 2", "event 1"}) {
		t.Errorf("page 2 = %v", got)
	}

	for _, q := range []string{"limit=0", "limit=-3", "limit=abc", "before=-1", "before=xyz", "limit=100000"} {
		if got := messages(h.do(http.MethodGet, "/events?"+q, "", "")); len(got) != 5 {
			t.Errorf("?%s: %d events, want all 5", q, len(got))
		}
	}

	all := h.do(http.MethodGet, "/events", "", "").array(t)
	oldest := int64(all[len(all)-1]["id"].(float64))
	r := h.do(http.MethodGet, fmt.Sprintf("/events?before=%d", oldest), "", "")
	if string(bytes.TrimSpace(r.body)) != "[]" {
		t.Errorf("past-the-end page = %s, want []", r.body)
	}
}

func TestParseLimitAndBefore(t *testing.T) {
	limits := map[string]int{"": 50, "1": 1, "50": 50, "500": 500, "501": 500, "999999999999": 500,
		"0": 50, "-1": 50, "abc": 50, "2.5": 50}
	for in, want := range limits {
		r := httptest.NewRequest(http.MethodGet, "/events?limit="+in, nil)
		if got := parseLimit(r); got != want {
			t.Errorf("parseLimit(%q) = %d, want %d", in, got, want)
		}
	}
	befores := map[string]int64{"": 0, "0": 0, "17": 17, "-5": 0, "abc": 0, "9223372036854775807": 9223372036854775807}
	for in, want := range befores {
		r := httptest.NewRequest(http.MethodGet, "/events?before="+in, nil)
		if got := parseBefore(r); got != want {
			t.Errorf("parseBefore(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestReload(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	token := h.login()

	r := h.do(http.MethodPost, "/reload", token, "")
	h.mustStatus(r, http.StatusOK, "no-op reload")
	if got := string(bytes.TrimSpace(r.body)); got != `{"added":[],"removed":[],"failed":[]}` {
		t.Errorf("no-op reload body = %s, want empty lists", got)
	}

	h.writeConfig(configYAML("svc2"))
	r = h.do(http.MethodPost, "/reload", token, "")
	h.mustStatus(r, http.StatusOK, "reload")
	out := r.object(t)
	if fmt.Sprint(out["added"]) != "[svc2]" || fmt.Sprint(out["removed"]) != "[svc1]" {
		t.Errorf("reload = %v, want added [svc2] removed [svc1]", out)
	}

	h.writeConfig("not: [valid")
	r = h.do(http.MethodPost, "/reload", token, "")
	h.mustStatus(r, http.StatusInternalServerError, "invalid reload")
	if msg, _ := r.object(t)["error"].(string); msg == "" {
		t.Errorf("invalid reload body %s has no error", r.body)
	}
}

// failedList returns the response's "failed" list as service -> error,
// failing the test if the field is missing or not a list.
func failedList(t *testing.T, body map[string]any) map[string]string {
	t.Helper()
	list, ok := body["failed"].([]any)
	if !ok {
		t.Fatalf(`response %v has no "failed" list`, body)
	}
	out := make(map[string]string, len(list))
	for _, item := range list {
		f, _ := item.(map[string]any)
		svc, _ := f["service"].(string)
		msg, _ := f["error"].(string)
		out[svc] = msg
	}
	return out
}

// A config edit that breaks one service must show up in the response of
// every route that starts routines, not only in the event log, and must not
// disturb the routines already running.
func TestStartFailures_AreReportedInResponses(t *testing.T) {
	h := newHarness(t, okCheck, "svc1", "svc2")
	token := h.login()

	r := h.do(http.MethodPost, "/engine/start", token, "")
	h.mustStatus(r, http.StatusOK, "engine start")
	if out := r.object(t); len(failedList(t, out)) != 0 || out["engine_state"] != "running" {
		t.Fatalf("engine start = %v, want running with an empty failed list", out)
	}
	for _, path := range []string{"/services/svc1/start", "/services/svc1/restart"} {
		r := h.do(http.MethodPost, path, token, "")
		h.mustStatus(r, http.StatusOK, path)
		if f := failedList(t, r.object(t)); len(f) != 0 {
			t.Errorf("%s failed = %v, want empty", path, f)
		}
	}

	// svc2's params gain a typo: the file no longer validates, so no routine
	// can (re)start, while running ones carry on.
	h.writeConfig(configYAML("svc1") + `  - name: svc2
    host: 127.0.0.1
    check_type: tcp_connect
    points_per_failure: 10
    params: {prot: 9}
`)
	svc1Running := func() bool {
		status := h.do(http.MethodGet, "/status", "", "").object(t)
		return serviceIn(t, status, "svc1")["routine_enabled"] == true
	}
	wantFailure := func(r response, status int, what string) {
		t.Helper()
		h.mustStatus(r, status, what)
		out := r.object(t)
		if msg := failedList(t, out)["svc2"]; !strings.Contains(msg, "prot") {
			t.Errorf("%s: failed = %v, want svc2 with the config error naming prot", what, out["failed"])
		}
		if !svc1Running() {
			t.Errorf("%s: svc1 stopped running", what)
		}
	}

	t.Run("restart", func(t *testing.T) {
		r := h.do(http.MethodPost, "/services/svc2/restart", token, `{"pause_scoring":true}`)
		wantFailure(r, http.StatusInternalServerError, "restart")
		if msg, _ := r.object(t)["error"].(string); !strings.Contains(msg, "svc2") {
			t.Errorf("restart error = %q, want it to name svc2", msg)
		}
		if !h.service("svc2").ScoringPaused {
			t.Error("restart failed to start the routine but should still have applied the pause")
		}
	})
	t.Run("engine start", func(t *testing.T) {
		// svc2 is enabled but stopped after the failed restart.
		r := h.do(http.MethodPost, "/engine/start", token, "")
		wantFailure(r, http.StatusOK, "engine start")
		if out := r.object(t); out["engine_state"] != "running" {
			t.Errorf("engine start = %v, want the status alongside the failures", out)
		}
	})
	t.Run("service start", func(t *testing.T) {
		wantFailure(h.do(http.MethodPost, "/services/svc2/start", token, ""), http.StatusInternalServerError, "service start")
	})
	t.Run("reload", func(t *testing.T) {
		// The broken file itself fails the reload before any change.
		r := h.do(http.MethodPost, "/reload", token, "")
		h.mustStatus(r, http.StatusInternalServerError, "reload of an invalid file")
		if out := r.object(t); len(failedList(t, out)) != 0 || out["error"] == nil {
			t.Errorf("invalid reload = %v, want an error and an empty failed list", out)
		}
		// A valid file whose new service cannot start (no http check is
		// registered here) is applied, with the failure reported.
		h.writeConfig(configYAML("svc1", "svc2") + `  - name: svc3
    host: 127.0.0.1
    check_type: http
    points_per_failure: 10
    params: {port: 80}
`)
		r = h.do(http.MethodPost, "/reload", token, "")
		h.mustStatus(r, http.StatusOK, "reload adding svc3")
		out := r.object(t)
		if fmt.Sprint(out["added"]) != "[svc3]" || !strings.Contains(failedList(t, out)["svc3"], "http") {
			t.Errorf("reload = %v, want svc3 added and reported as failed to start", out)
		}
		if !svc1Running() {
			t.Error("svc1 stopped running")
		}
	})
}

func TestReload_EmptyConfigPathUsesEngineDefault(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	a := auth.New(testUsername, testHash)
	srv := httptest.NewServer(NewServer(h.e, h.st, h.sess.ID, a, ""))
	t.Cleanup(srv.Close)
	h.url = srv.URL
	token := h.login()

	h.writeConfig(configYAML("svc1", "svc2"))
	r := h.do(http.MethodPost, "/reload", token, "")
	h.mustStatus(r, http.StatusOK, "reload")
	if fmt.Sprint(r.object(t)["added"]) != "[svc2]" {
		t.Errorf("reload = %s, want svc2 added", r.body)
	}
}

func TestScoring_StaysPausedUntilAdminResumes(t *testing.T) {
	h := newHarness(t, failCheck, "svc1")
	token := h.login()
	h.mustStatus(h.do(http.MethodPost, "/engine/start", token, ""), http.StatusOK, "engine start")

	blue := func() float64 { return h.do(http.MethodGet, "/status", "", "").object(t)["blue_score"].(float64) }
	waitFor(t, "a failing check to be recorded", func() bool {
		return len(h.do(http.MethodGet, "/services/svc1/results", "", "").array(t)) > 0
	})
	if b := blue(); b != 1000 {
		t.Fatalf("blue_score = %v before resume, want 1000 (scoring starts paused)", b)
	}

	h.mustStatus(h.do(http.MethodPost, "/scoring/resume", token, ""), http.StatusOK, "resume")
	waitFor(t, "a failure to cost blue points", func() bool { return blue() < 1000 })
}

func TestInternalErrors_PublicGeneric_AdminVerbose(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	token := h.login()
	h.st.Close() // every query now fails

	for _, path := range []string{"/status", "/events", "/services/svc1/results"} {
		t.Run(path, func(t *testing.T) {
			r := h.do(http.MethodGet, path, "", "")
			h.mustStatus(r, http.StatusInternalServerError, "GET "+path)
			if got := r.object(t)["error"]; got != "internal server error" {
				t.Errorf("public 500 error = %q, want the generic message", got)
			}
		})
	}
	if logs := h.logs.String(); !strings.Contains(logs, "sql") {
		t.Errorf("real errors not logged; log = %q", logs)
	}

	r := h.do(http.MethodPost, "/engine/start", token, "")
	h.mustStatus(r, http.StatusInternalServerError, "POST /engine/start")
	if msg, _ := r.object(t)["error"].(string); !strings.Contains(msg, "engine") {
		t.Errorf("admin 500 error = %q, want the underlying cause", msg)
	}
}
