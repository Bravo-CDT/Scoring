// Package e2e drives the fully wired scoring engine over HTTP, in-process.
//
// Every stack is built by app.New, the same function cmd/scoringengine calls,
// so these tests exercise the real wiring rather than a copy of it. The only
// substitution is the one app.Options exists for: a fake check registry
// (always-failing tcp_connect), so the tests are deterministic and touch no
// real network unless they mean to.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Bravo-CDT/Scoring/internal/app"
	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

const (
	adminUser     = "admin"
	adminPassword = "e2e-secret"
	hashEnvVar    = "E2E_ADMIN_PASSWORD_HASH"

	startingScore    = 1000
	pointsPerFailure = 10

	// failDetail is what the fake check reports when a service is down.
	failDetail = "simulated outage: connection refused"

	// waitTimeout bounds every poll. Rounds run every second, so anything
	// that should happen does so within a couple of seconds.
	waitTimeout = 10 * time.Second
	// quietPeriod is how long a negative assertion ("nothing changed") waits:
	// comfortably more than two rounds.
	quietPeriod = 2500 * time.Millisecond
)

// writeConfig writes a config with 1-second rounds and one tcp_connect
// service per name. The admin hash comes from the environment, as in
// production.
func writeConfig(t *testing.T, path string, services ...string) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "session_name: e2e-comp\nstarting_score: %d\nround_interval_seconds: 1\n", startingScore)
	fmt.Fprintf(&b, "admin:\n  username: %s\n  password_hash: ${%s}\nservices:\n", adminUser, hashEnvVar)
	for _, name := range services {
		fmt.Fprintf(&b, "  - name: %s\n    host: 127.0.0.1\n    check_type: tcp_connect\n"+
			"    points_per_failure: %d\n    params: {port: 9}\n", name, pointsPerFailure)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
}

// failingRegistry replaces tcp_connect with a check that always fails, so
// score movement is deterministic and no real network is touched.
func failingRegistry() *checks.Registry {
	r := checks.NewRegistry()
	r.Register("tcp_connect", func(context.Context, string, map[string]any) checks.Result {
		return checks.Result{Success: false, Detail: failDetail}
	})
	return r
}

// stack is one simulated process: an app.App served over a test listener.
type stack struct {
	t      *testing.T
	app    *app.App
	server *httptest.Server
	closed bool
}

func newStack(t *testing.T, dbPath, configPath string) *stack {
	t.Helper()
	return newStackWithRegistry(t, dbPath, configPath, failingRegistry())
}

func newStackWithRegistry(t *testing.T, dbPath, configPath string, registry *checks.Registry) *stack {
	t.Helper()
	a, err := app.New(app.Options{
		ConfigPath: configPath,
		DBPath:     dbPath,
		Registry:   registry,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	s := &stack{t: t, app: a, server: httptest.NewServer(a.Handler)}
	t.Cleanup(s.close)
	return s
}

// close shuts the stack down the way main does: stop serving, then stop the
// round loop and the engine and close the store.
func (s *stack) close() {
	if s.closed {
		return
	}
	s.closed = true
	s.server.Close()
	if err := s.app.Close(); err != nil {
		s.t.Errorf("app.Close: %v", err)
	}
}

// do sends a request and decodes the JSON response into out (if non-nil),
// returning the status code.
func (s *stack) do(method, path, token string, body, out any) int {
	s.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			s.t.Fatalf("encoding body for %s %s: %v", method, path, err)
		}
	}
	req, err := http.NewRequest(method, s.server.URL+path, &buf)
	if err != nil {
		s.t.Fatalf("building %s %s: %v", method, path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.server.Client().Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			s.t.Fatalf("decoding %s %s (status %d): %v", method, path, resp.StatusCode, err)
		}
	}
	return resp.StatusCode
}

// admin POSTs an admin action and fails the test unless it succeeds.
func (s *stack) admin(token, path string) {
	s.t.Helper()
	var out map[string]any
	if code := s.do(http.MethodPost, path, token, nil, &out); code != http.StatusOK {
		s.t.Fatalf("POST %s: status %d, body %v", path, code, out)
	}
}

func (s *stack) login() string {
	s.t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	creds := map[string]string{"username": adminUser, "password": adminPassword}
	if code := s.do(http.MethodPost, "/login", "", creds, &out); code != http.StatusOK || out.Token == "" {
		s.t.Fatalf("login: status %d, token %q", code, out.Token)
	}
	return out.Token
}

type serviceStatus struct {
	Name           string `json:"name"`
	RoutineEnabled bool   `json:"routine_enabled"`
	ScoringPaused  bool   `json:"scoring_paused"`
}

type status struct {
	BlueScore     int             `json:"blue_score"`
	RedScore      int             `json:"red_score"`
	EngineState   string          `json:"engine_state"`
	ScoringPaused bool            `json:"scoring_paused"`
	Services      []serviceStatus `json:"services"`
}

func (st status) service(t *testing.T, name string) serviceStatus {
	t.Helper()
	for _, svc := range st.Services {
		if svc.Name == name {
			return svc
		}
	}
	t.Fatalf("service %q not in status %+v", name, st.Services)
	return serviceStatus{}
}

func (s *stack) status() status {
	s.t.Helper()
	var st status
	if code := s.do(http.MethodGet, "/status", "", nil, &st); code != http.StatusOK {
		s.t.Fatalf("GET /status: status %d", code)
	}
	return st
}

type result struct {
	ID      int64   `json:"id"`
	Success bool    `json:"success"`
	Detail  *string `json:"detail"`
}

// results returns a service's check history, newest first, as seen with the
// given token ("" for the public view).
func (s *stack) results(name, token string) []result {
	s.t.Helper()
	var out []result
	path := "/services/" + name + "/results?limit=500"
	if code := s.do(http.MethodGet, path, token, nil, &out); code != http.StatusOK {
		s.t.Fatalf("GET %s: status %d", path, code)
	}
	return out
}

// latestResultID is the ID of a service's newest check result, 0 if none.
func (s *stack) latestResultID(name string) int64 {
	s.t.Helper()
	if rs := s.results(name, ""); len(rs) > 0 {
		return rs[0].ID
	}
	return 0
}

// awaitNewResults waits until name has recorded n results after the current
// newest one, proving its routine is still checking.
func (s *stack) awaitNewResults(name string, n int) {
	s.t.Helper()
	after := s.latestResultID(name)
	waitFor(s.t, fmt.Sprintf("%d new %s results after #%d", n, name, after), func() bool {
		count := 0
		for _, r := range s.results(name, "") {
			if r.ID > after {
				count++
			}
		}
		return count >= n
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", waitTimeout, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func setAdminHash(t *testing.T) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	t.Setenv(hashEnvVar, string(hash))
}

// TestEndToEnd_LifecycleAndRestart walks one competition through every
// control a scoring admin uses, then restarts the process against the same
// database and config and checks exactly what does and does not survive.
func TestEndToEnd_LifecycleAndRestart(t *testing.T) {
	setAdminHash(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "scoring.db")
	configPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, configPath, "svc1", "svc2")

	s1 := newStack(t, dbPath, configPath)

	// A fresh process comes up safe: engine stopped, scoring paused, every
	// configured service synced from the config, nothing checked yet.
	st := s1.status()
	if st.EngineState != "stopped" || !st.ScoringPaused {
		t.Fatalf("fresh process: engine_state=%q scoring_paused=%v, want stopped and paused", st.EngineState, st.ScoringPaused)
	}
	if st.BlueScore != startingScore || len(st.Services) != 2 {
		t.Fatalf("fresh process: blue_score=%d services=%+v, want %d and svc1+svc2", st.BlueScore, st.Services, startingScore)
	}

	// Admin endpoints need a token.
	if code := s1.do(http.MethodPost, "/engine/start", "", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("POST /engine/start without token: status %d, want 401", code)
	}
	token := s1.login()

	t.Log("start: checks run, but scoring is still globally paused, so the score holds")
	s1.admin(token, "/engine/start")
	s1.awaitNewResults("svc1", 2)
	if got := s1.status().BlueScore; got != startingScore {
		t.Fatalf("blue_score = %d while globally paused since start, want %d", got, startingScore)
	}

	t.Log("resume: failing checks now deduct points")
	s1.admin(token, "/scoring/resume")
	waitFor(t, "a deduction after resuming scoring", func() bool {
		st := s1.status()
		return st.BlueScore < startingScore && st.RedScore > 0
	})

	t.Log("global pause: checks continue, score freezes")
	s1.admin(token, "/scoring/pause")
	s1.awaitNewResults("svc1", 1) // let any result scored before the pause land
	frozen := s1.status().BlueScore
	s1.awaitNewResults("svc1", 2)
	s1.awaitNewResults("svc2", 2)
	if got := s1.status().BlueScore; got != frozen {
		t.Fatalf("blue_score moved under global pause: %d -> %d", frozen, got)
	}
	s1.admin(token, "/scoring/resume")

	t.Log("per-service pause: svc1 keeps checking but stops scoring (svc2's routine stopped, so svc1 is the only source)")
	s1.admin(token, "/services/svc2/stop")
	s1.admin(token, "/services/svc1/scoring/pause")
	s1.awaitNewResults("svc1", 1)
	frozen = s1.status().BlueScore
	s1.awaitNewResults("svc1", 2)
	st = s1.status()
	if st.BlueScore != frozen {
		t.Fatalf("blue_score moved while svc1 was paused and svc2 stopped: %d -> %d", frozen, st.BlueScore)
	}
	if st.ScoringPaused || !st.service(t, "svc1").ScoringPaused || st.service(t, "svc2").RoutineEnabled {
		t.Fatalf("unexpected state after per-service pause: %+v", st)
	}

	t.Log("/reload adds svc3, which starts immediately and (unpaused) scores")
	writeConfig(t, configPath, "svc1", "svc2", "svc3")
	var reload struct {
		Added   []string `json:"added"`
		Removed []string `json:"removed"`
	}
	if code := s1.do(http.MethodPost, "/reload", token, nil, &reload); code != http.StatusOK {
		t.Fatalf("POST /reload: status %d", code)
	}
	if len(reload.Added) != 1 || reload.Added[0] != "svc3" || len(reload.Removed) != 0 {
		t.Fatalf("reload = %+v, want added [svc3], removed []", reload)
	}
	if !s1.status().service(t, "svc3").RoutineEnabled {
		t.Fatal("svc3 routine not running after reload on a running engine")
	}
	beforeSvc3 := s1.status().BlueScore
	waitFor(t, "svc3 failures to deduct points", func() bool { return s1.status().BlueScore < beforeSvc3 })

	t.Log("detail text: check results show it to everyone, events only to the admin")
	publicView := s1.results("svc1", "")
	if len(publicView) == 0 || publicView[0].Detail == nil || *publicView[0].Detail != failDetail {
		t.Fatalf("public view of svc1 results lacks the check detail: %+v", publicView)
	}
	var events []map[string]any
	s1.do(http.MethodGet, "/events?limit=500", "", nil, &events)
	for _, ev := range events {
		if _, ok := ev["detail"]; ok {
			t.Fatalf("public event exposes detail: %v", ev)
		}
	}

	t.Log("engine stop: no new check results")
	s1.admin(token, "/engine/stop")
	historyBefore := s1.results("svc1", token)
	svc3Before := s1.latestResultID("svc3")
	time.Sleep(quietPeriod)
	if got := s1.latestResultID("svc1"); got != historyBefore[0].ID {
		t.Fatalf("svc1 recorded result #%d after engine stop (last was #%d)", got, historyBefore[0].ID)
	}
	if got := s1.latestResultID("svc3"); got != svc3Before {
		t.Fatalf("svc3 recorded result #%d after engine stop (last was #%d)", got, svc3Before)
	}
	st = s1.status()
	for _, svc := range st.Services {
		if svc.RoutineEnabled {
			t.Fatalf("%s routine still running after engine stop", svc.Name)
		}
	}
	scoreBefore := st.BlueScore

	// Before restarting, drop svc3 from the config: startup must retire it.
	writeConfig(t, configPath, "svc1", "svc2")
	s1.close()

	t.Log("restart: a genuinely fresh stack on the same database and config")
	s2 := newStack(t, dbPath, configPath)
	st = s2.status()
	if st.EngineState != "stopped" || !st.ScoringPaused {
		t.Fatalf("after restart: engine_state=%q scoring_paused=%v, want stopped and paused", st.EngineState, st.ScoringPaused)
	}
	if st.BlueScore != scoreBefore {
		t.Fatalf("after restart: blue_score=%d, want %d (score must not reset)", st.BlueScore, scoreBefore)
	}
	if len(st.Services) != 2 {
		t.Fatalf("after restart: services=%+v, want svc1 and svc2 (svc3 retired at startup)", st.Services)
	}
	if !st.service(t, "svc1").ScoringPaused {
		t.Fatal("after restart: svc1's per-service scoring pause was lost")
	}
	if st.service(t, "svc2").ScoringPaused {
		t.Fatal("after restart: svc2 scoring_paused, want false")
	}

	historyAfter := s2.results("svc1", "")
	if len(historyAfter) != len(historyBefore) || historyAfter[0].ID != historyBefore[0].ID {
		t.Fatalf("after restart: svc1 history has %d results (newest #%d), want %d (newest #%d)",
			len(historyAfter), historyAfter[0].ID, len(historyBefore), historyBefore[0].ID)
	}
	if rs := s2.results("svc3", ""); len(rs) == 0 {
		t.Fatal("after restart: retired svc3's history is no longer queryable")
	}
	events = nil
	s2.do(http.MethodGet, "/events?limit=500", "", nil, &events)
	if !hasEvent(events, "engine_started") || !hasEvent(events, "service_removed") {
		t.Fatalf("after restart: event history lacks the earlier engine_started and the startup service_removed: %v", events)
	}
	// Everything the second boot did on its own is the system's doing, not an
	// admin's: events are newest first, so the boot's own events lead the list
	// up to and including its process_started.
	sawProcessStarted := false
	for _, ev := range events {
		if ev["actor"] != "system" {
			t.Errorf("second boot's startup event %v: actor %v, want system", ev["type"], ev["actor"])
		}
		if ev["type"] == "process_started" {
			sawProcessStarted = true
			break
		}
	}
	if !sawProcessStarted {
		t.Fatalf("after restart: no process_started event for the second boot: %v", events)
	}
	for _, typ := range []string{"service_removed", "config_reloaded"} {
		if ev := firstEvent(events, typ); ev == nil || ev["actor"] != "system" {
			t.Errorf("after restart: newest %s = %v, want one by system", typ, ev)
		}
	}

	if code := s2.do(http.MethodPost, "/engine/start", token, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("old token on the new process: status %d, want 401", code)
	}
	token2 := s2.login()

	t.Log("start again: svc2's routine stop did not survive; scoring stays paused until resumed")
	s2.admin(token2, "/engine/start")
	if !s2.status().service(t, "svc2").RoutineEnabled {
		t.Fatal("after restart: svc2's routine did not start (routine stop must reset on restart)")
	}
	s2.awaitNewResults("svc2", 2)
	if got := s2.status().BlueScore; got != scoreBefore {
		t.Fatalf("after restart: blue_score moved while globally paused: %d -> %d", scoreBefore, got)
	}

	s2.admin(token2, "/scoring/resume")
	waitFor(t, "svc2 to deduct points after resume", func() bool { return s2.status().BlueScore < scoreBefore })
}

func hasEvent(events []map[string]any, eventType string) bool {
	return firstEvent(events, eventType) != nil
}

// firstEvent returns the first event of eventType in events (the newest, as
// /events lists newest first), or nil.
func firstEvent(events []map[string]any, eventType string) map[string]any {
	for _, ev := range events {
		if ev["type"] == eventType {
			return ev
		}
	}
	return nil
}

func countEvents(events []map[string]any, eventType string) int {
	n := 0
	for _, ev := range events {
		if ev["type"] == eventType {
			n++
		}
	}
	return n
}

// TestEndToEnd_MassFailureIgnoresStoppedRoutines checks that app wires the
// scheduler's running state into the scorer: a routine an admin stopped while
// it was failing must drop out of the mass-failure share, rather than keep
// counting its stale failure for the rest of the competition.
func TestEndToEnd_MassFailureIgnoresStoppedRoutines(t *testing.T) {
	setAdminHash(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	// tcp_connect fails on port 9 and passes on any other, so a service's
	// outcome is set by its config entry.
	r := checks.NewRegistry()
	r.Register("tcp_connect", func(_ context.Context, _ string, params map[string]any) checks.Result {
		if params["port"] == 9 {
			return checks.Result{Success: false, Detail: failDetail}
		}
		return checks.Result{Success: true, Detail: "ok"}
	})
	writePorts := func(ports map[string]int) {
		t.Helper()
		var b strings.Builder
		fmt.Fprintf(&b, "session_name: e2e-comp\nstarting_score: %d\nround_interval_seconds: 1\nmass_failure_fraction: 0.5\n", startingScore)
		fmt.Fprintf(&b, "admin:\n  username: %s\n  password_hash: ${%s}\nservices:\n", adminUser, hashEnvVar)
		for _, name := range []string{"down", "up1", "up2", "up3"} {
			fmt.Fprintf(&b, "  - name: %s\n    host: 127.0.0.1\n    check_type: tcp_connect\n"+
				"    points_per_failure: %d\n    params: {port: %d}\n", name, pointsPerFailure, ports[name])
		}
		if err := os.WriteFile(configPath, []byte(b.String()), 0o600); err != nil {
			t.Fatalf("writing config: %v", err)
		}
	}
	writePorts(map[string]int{"down": 9, "up1": 10, "up2": 10, "up3": 10})

	s := newStackWithRegistry(t, filepath.Join(dir, "scoring.db"), configPath, r)
	token := s.login()
	s.admin(token, "/engine/start")
	s.admin(token, "/scoring/resume")
	for _, name := range []string{"down", "up1", "up2", "up3"} {
		s.awaitNewResults(name, 1) // 1 of 4 failing arms the safety net
	}
	if s.status().ScoringPaused {
		t.Fatal("autopaused at 1 of 4 failing with threshold 0.5")
	}

	// Stop the failing routine, then make up1 fail. Counting only running
	// routines that is 1 of 3; counting the stopped one's stale failure too
	// it would be 2 of 4, which meets the threshold and would autopause.
	s.admin(token, "/services/down/stop")
	writePorts(map[string]int{"down": 9, "up1": 9, "up2": 10, "up3": 10})
	s.admin(token, "/services/up1/restart")
	s.awaitNewResults("up1", 3)

	if s.status().ScoringPaused {
		t.Fatal("autopaused: the stopped routine's stale failure was counted")
	}
	var events []map[string]any
	s.do(http.MethodGet, "/events?limit=500", "", nil, &events)
	if hasEvent(events, "mass_failure_autopause") {
		t.Fatalf("mass_failure_autopause logged although only 1 of 3 running services is failing: %v", events)
	}
}

// storedResults returns a service's whole check history straight from the
// store, newest first, with the round numbers the API does not show.
func (s *stack) storedResults(name string) []store.CheckResult {
	s.t.Helper()
	svc, ok, err := s.app.Store.GetServiceByName(s.app.Session.ID, name)
	if err != nil || !ok {
		s.t.Fatalf("GetServiceByName(%q): found=%v err=%v", name, ok, err)
	}
	results, err := s.app.Store.ListCheckResults(svc.ID, 1000, 0)
	if err != nil {
		s.t.Fatalf("ListCheckResults(%q): %v", name, err)
	}
	return results
}

// latestRound is the round of a service's newest stored result, 0 if none.
func (s *stack) latestRound(name string) int {
	s.t.Helper()
	if rs := s.storedResults(name); len(rs) > 0 {
		return rs[0].Round
	}
	return 0
}

// listen opens a TCP listener on a free local port that accepts and drops
// every connection, for a real tcp_connect check to pass against, and returns
// its port.
func listen(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// writeListenerConfig writes a config with 1-second rounds and one tcp_connect
// service per entry of ports, each pointed at its local port.
func writeListenerConfig(t *testing.T, path string, ports map[string]int) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "session_name: e2e-comp\nstarting_score: %d\nround_interval_seconds: 1\n", startingScore)
	fmt.Fprintf(&b, "admin:\n  username: %s\n  password_hash: ${%s}\nservices:\n", adminUser, hashEnvVar)
	for _, name := range slices.Sorted(maps.Keys(ports)) {
		fmt.Fprintf(&b, "  - name: %s\n    host: 127.0.0.1\n    check_type: tcp_connect\n"+
			"    points_per_failure: %d\n    params: {port: %d}\n", name, pointsPerFailure, ports[name])
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
}

// TestEndToEnd_ServicesCheckInLockstep runs two real tcp_connect checks for
// several rounds and checks that, once both are checking, every round holds
// exactly one result for each, stamped with the same instant: the services
// never drift apart.
func TestEndToEnd_ServicesCheckInLockstep(t *testing.T) {
	setAdminHash(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	writeListenerConfig(t, configPath, map[string]int{"web": listen(t), "ssh": listen(t)})

	s := newStackWithRegistry(t, filepath.Join(dir, "scoring.db"), configPath, checks.NewDefaultRegistry())
	s.admin(s.login(), "/engine/start")
	// 4, not 3: round 1 may hold only one service (see below), and this
	// still leaves at least two shared rounds to compare.
	waitFor(t, "4 rounds holding both services", func() bool {
		return min(s.latestRound("web"), s.latestRound("ssh")) >= 4
	})

	byRound := func(name string) map[int]store.CheckResult {
		out := make(map[int]store.CheckResult)
		for _, r := range s.storedResults(name) {
			if prev, dup := out[r.Round]; dup {
				t.Fatalf("%s has two results for round %d: #%d and #%d", name, r.Round, prev.ID, r.ID)
			}
			if !r.Success {
				t.Errorf("%s round %d failed (%q), want a pass against its listener", name, r.Round, r.Detail)
			}
			out[r.Round] = r
		}
		return out
	}
	web, ssh := byRound("web"), byRound("ssh")
	// Start enables services one at a time, so a round can begin between
	// the two and hold only one of them: compare from the first round both
	// services are in. The newest round may still be being written as we
	// read: compare up to the last one both services had finished.
	first := max(slices.Min(slices.Collect(maps.Keys(web))), slices.Min(slices.Collect(maps.Keys(ssh))))
	last := min(slices.Max(slices.Collect(maps.Keys(web))), slices.Max(slices.Collect(maps.Keys(ssh))))
	if last-first < 1 {
		t.Fatalf("only rounds %d..%d hold both services, want at least two to compare", first, last)
	}
	for round := first; round <= last; round++ {
		w, wok := web[round]
		h, hok := ssh[round]
		if !wok || !hok {
			t.Errorf("round %d: web present %v, ssh present %v; want both", round, wok, hok)
			continue
		}
		if !w.Timestamp.Equal(h.Timestamp) {
			t.Errorf("round %d: web stamped %v, ssh %v; want one shared round start", round, w.Timestamp, h.Timestamp)
		}
	}
}

// TestEndToEnd_RoundNumberResumesAcrossARestart checks that a restarted
// process carries on numbering rounds from where the last one stopped,
// rather than starting again at 1.
func TestEndToEnd_RoundNumberResumesAcrossARestart(t *testing.T) {
	setAdminHash(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "scoring.db")
	configPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, configPath, "svc1")

	s1 := newStack(t, dbPath, configPath)
	s1.admin(s1.login(), "/engine/start")
	waitFor(t, "2 rounds before the restart", func() bool { return s1.latestRound("svc1") >= 2 })
	s1.close()

	s2 := newStack(t, dbPath, configPath)
	// Read once the first process is gone, so no round of its can still land.
	before := s2.latestRound("svc1")
	s2.admin(s2.login(), "/engine/start")
	waitFor(t, "a round after the restart", func() bool { return s2.latestRound("svc1") > before })

	// History is newest first; the earliest round past before is the
	// restarted process's first.
	first := 0
	for _, r := range s2.storedResults("svc1") {
		if r.Round > before {
			first = r.Round
		}
	}
	if first != before+1 {
		t.Errorf("first round after the restart = %d, want %d (one past the last round before it)", first, before+1)
	}
}

// TestEndToEnd_BootAndShutdownEvents checks how the process itself shows up
// in the event log: one process_started per boot, the startup sync and the
// shutdown stop attributed to the system rather than an admin, and no
// config_reloaded noise from a boot whose config changed nothing.
func TestEndToEnd_BootAndShutdownEvents(t *testing.T) {
	setAdminHash(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "scoring.db")
	configPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, configPath, "svc1")

	s1 := newStack(t, dbPath, configPath)
	s1.admin(s1.login(), "/engine/start")
	s1.close() // shuts down a running engine

	s2 := newStack(t, dbPath, configPath) // same config: the startup sync changes nothing
	var events []map[string]any
	s2.do(http.MethodGet, "/events?limit=500", "", nil, &events)

	if n := countEvents(events, "process_started"); n != 2 {
		t.Fatalf("process_started events = %d, want one per boot (2): %v", n, events)
	}
	ps := firstEvent(events, "process_started")
	msg, _ := ps["message"].(string)
	if ps["actor"] != "system" || ps["level"] != "info" || !strings.Contains(msg, "e2e-comp") ||
		!strings.Contains(msg, "stopped") || !strings.Contains(msg, "paused") {
		t.Errorf("process_started = %v, want an info event by system naming the session and the stopped/paused state", ps)
	}
	if ev := firstEvent(events, "engine_stopped"); ev == nil || ev["actor"] != "system" {
		t.Errorf("shutdown engine_stopped = %v, want one by system", ev)
	}
	if ev := firstEvent(events, "engine_started"); ev == nil || ev["actor"] != "admin" {
		t.Errorf("engine_started = %v, want the admin's", ev)
	}
	// The first boot added svc1 and says so; the second changed nothing.
	if n := countEvents(events, "config_reloaded"); n != 1 {
		t.Errorf("config_reloaded events = %d, want 1 (only the first boot changed anything): %v", n, events)
	}
	if ev := firstEvent(events, "service_added"); ev == nil || ev["actor"] != "system" {
		t.Errorf("startup service_added = %v, want one by system", ev)
	}
}

// TestEndToEnd_ServesTheFrontendBesideTheAPI checks the WebDir wiring: the
// built frontend answers its own files and every client-side route, while
// the API's routes and JSON errors are untouched.
func TestEndToEnd_ServesTheFrontendBesideTheAPI(t *testing.T) {
	setAdminHash(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, configPath, "svc1")
	webDir := filepath.Join(dir, "dist")
	if err := os.MkdirAll(filepath.Join(webDir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{"index.html": "<html>app</html>", "assets/app.js": "app()"} {
		if err := os.WriteFile(filepath.Join(webDir, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a, err := app.New(app.Options{
		ConfigPath: configPath,
		DBPath:     filepath.Join(dir, "scoring.db"),
		WebDir:     webDir,
		Registry:   failingRegistry(),
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	s := &stack{t: t, app: a, server: httptest.NewServer(a.Handler)}
	t.Cleanup(s.close)

	get := func(method, path string) (int, string, string) {
		t.Helper()
		req, err := http.NewRequest(method, s.server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var body bytes.Buffer
		if _, err := body.ReadFrom(resp.Body); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header.Get("Content-Type"), body.String()
	}

	for path, want := range map[string]string{"/": "<html>app</html>", "/admin": "<html>app</html>", "/assets/app.js": "app()"} {
		if code, _, body := get(http.MethodGet, path); code != http.StatusOK || body != want {
			t.Errorf("GET %s = %d %q, want 200 %q", path, code, body, want)
		}
	}
	if st := s.status(); st.BlueScore != startingScore {
		t.Errorf("GET /status blue_score = %d, want %d", st.BlueScore, startingScore)
	}
	if code, ct, _ := get(http.MethodPost, "/admin"); code != http.StatusNotFound || ct != "application/json" {
		t.Errorf("POST /admin = %d %s, want the API's JSON 404", code, ct)
	}
}

// TestExampleConfigStarts keeps the documented config.example.yaml in step
// with the code: the real wiring must accept it as shipped.
func TestExampleConfigStarts(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	t.Setenv("ADMIN_PASSWORD_HASH", string(hash))
	s := newStack(t, filepath.Join(t.TempDir(), "scoring.db"), filepath.Join("..", "..", "config.example.yaml"))
	if st := s.status(); len(st.Services) == 0 {
		t.Fatal("config.example.yaml configures no services")
	}
	s.login()
}

// TestNew_FailsFastOnBadInputs checks that a misconfigured deployment is
// rejected up front with an error naming the problem.
func TestNew_FailsFastOnBadInputs(t *testing.T) {
	setAdminHash(t)
	dir := t.TempDir()
	goodConfig := filepath.Join(dir, "config.yaml")
	writeConfig(t, goodConfig, "svc1")

	cases := []struct {
		name       string
		configPath string
		dbPath     string
		wantErr    string
	}{
		{"missing config", filepath.Join(dir, "nope.yaml"), filepath.Join(dir, "a.db"), "nope.yaml"},
		{"missing db directory", goodConfig, filepath.Join(dir, "no-such-dir", "scoring.db"), "no-such-dir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := app.New(app.Options{ConfigPath: tc.configPath, DBPath: tc.dbPath})
			if err == nil {
				a.Close()
				t.Fatal("app.New succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}
