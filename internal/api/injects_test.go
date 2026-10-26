package api

import (
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/auth"
	"github.com/Bravo-CDT/Scoring/internal/engine"
)

const redUsername = "red"

// newInjectsHarness serves the API with blue and red team logins on, both
// with testPassword, starting blue at 100000.
func newInjectsHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarnessWithOptions(t, okCheck, []Option{
		WithBlueTeam(auth.New(blueUsername, testHash)),
		WithRedTeam(auth.New(redUsername, testHash)),
	}, "svc1")
	h.writeConfig(strings.Replace(configYAML("svc1"), "starting_score: 1000", "starting_score: 100000", 1) + fmt.Sprintf(`
blue_team:
  username: %s
  password_hash: %q
  allowed_users: [mrkrabs]
red_team:
  username: %s
  password_hash: %q
`, blueUsername, testHash, redUsername, testHash))
	if _, err := h.e.Reload(""); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	h.e.EnableBlueTeamLogin()
	h.e.EnableRedTeamLogin()
	h.e.SetUploadsDir(t.TempDir())
	return h
}

func (h *harness) teamLogin(username string) (token, team string) {
	h.t.Helper()
	r := h.do(http.MethodPost, "/team/login", "", fmt.Sprintf(`{"username":%q,"password":%q}`, username, testPassword))
	h.mustStatus(r, http.StatusOK, "team login as "+username)
	out := r.object(h.t)
	token, _ = out["token"].(string)
	team, _ = out["team"].(string)
	return token, team
}

// createInject creates an inject as the admin, open if open, due in an hour.
func (h *harness) createInject(admin, team string, open bool) int64 {
	h.t.Helper()
	deadline := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	r := h.do(http.MethodPost, "/admin/injects", admin, fmt.Sprintf(`{"title":"report","team":%q,"deadline":%q}`, team, deadline))
	h.mustStatus(r, http.StatusOK, "create inject")
	id := int64(r.object(h.t)["id"].(float64))
	if open {
		r = h.do(http.MethodPut, fmt.Sprintf("/admin/injects/%d", id), admin, fmt.Sprintf(`{"title":"report","deadline":%q,"open":true}`, deadline))
		h.mustStatus(r, http.StatusOK, "open inject")
	}
	return id
}

// upload sends a multipart body with a note and a file of content, streamed,
// and returns the status and body without requiring JSON.
func (h *harness) upload(method, path, token, note string, content io.Reader) (int, string) {
	h.t.Helper()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		mw.WriteField("note", note)
		fw, _ := mw.CreateFormFile("file", "answer.txt")
		_, err := io.Copy(fw, content)
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	req, _ := http.NewRequest(method, h.url+path, pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// get fetches path without requiring a JSON response.
func (h *harness) get(path, token string) (int, http.Header, string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.url+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(body)
}

func TestTeamLogin_SaysWhichTeam(t *testing.T) {
	h := newInjectsHarness(t)
	if _, team := h.teamLogin(blueUsername); team != "blue" {
		t.Errorf("blue login team = %q", team)
	}
	if _, team := h.teamLogin(redUsername); team != "red" {
		t.Errorf("red login team = %q", team)
	}
	r := h.do(http.MethodPost, "/team/login", "", fmt.Sprintf(`{"username":%q,"password":"wrong"}`, redUsername))
	h.mustStatus(r, http.StatusUnauthorized, "wrong password")
}

func TestInjects_RedCannotSeeBlue(t *testing.T) {
	h := newInjectsHarness(t)
	admin := h.login()
	id := h.createInject(admin, "blue", true)
	red, _ := h.teamLogin(redUsername)

	r := h.do(http.MethodGet, "/team/injects", red, "")
	h.mustStatus(r, http.StatusOK, "red list")
	if got := r.array(t); len(got) != 0 {
		t.Errorf("red sees %v, want nothing", got)
	}
	for _, path := range []string{"pdf", "submission"} {
		if status, _, _ := h.get(fmt.Sprintf("/team/injects/%d/%s", id, path), red); status != http.StatusNotFound {
			t.Errorf("red GET blue's %s = %d, want 404", path, status)
		}
	}
	if status, _ := h.upload(http.MethodPut, fmt.Sprintf("/team/injects/%d/submission", id), red, "", strings.NewReader("x")); status != http.StatusNotFound {
		t.Errorf("red PUT to blue's inject = %d, want 404", status)
	}
}

func TestInjects_SubmitGradeApprove(t *testing.T) {
	h := newInjectsHarness(t)
	admin := h.login()
	id := h.createInject(admin, "red", true)
	red, _ := h.teamLogin(redUsername)

	status, body := h.upload(http.MethodPut, fmt.Sprintf("/team/injects/%d/submission", id), red, "our answer", strings.NewReader("hello"))
	if status != http.StatusOK || !strings.Contains(body, `"our answer"`) {
		t.Fatalf("submit = %d %s", status, body)
	}
	status, header, file := h.get(fmt.Sprintf("/team/injects/%d/submission", id), admin)
	if status != http.StatusOK || file != "hello" || !strings.Contains(header.Get("Content-Disposition"), "answer.txt") {
		t.Fatalf("admin download = %d %q %v", status, file, header)
	}

	path := fmt.Sprintf("/admin/injects/%d", id)
	h.mustStatus(h.do(http.MethodPut, path+"/grade", admin, `{"grade":7}`), http.StatusOK, "grade")
	h.mustStatus(h.do(http.MethodPost, path+"/approve", admin, ""), http.StatusBadRequest, "approve while open")
	deadline := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	h.mustStatus(h.do(http.MethodPut, path, admin, fmt.Sprintf(`{"title":"report","deadline":%q,"open":false}`, deadline)), http.StatusOK, "close")
	h.mustStatus(h.do(http.MethodPost, path+"/approve", admin, ""), http.StatusOK, "approve")

	if got := h.do(http.MethodGet, "/status", "", "").object(t)["red_score"]; got != 21000.0 {
		t.Errorf("red_score = %v, want 21000", got)
	}
	bd := h.do(http.MethodGet, "/score/breakdown", "", "").object(t)
	if got := bd["red"].(map[string]any)["injects"]; got != 21000.0 {
		t.Errorf("breakdown red injects = %v, want 21000", got)
	}
	h.mustStatus(h.do(http.MethodPost, path+"/approve", admin, ""), http.StatusConflict, "second approve")
}

func TestInjects_UploadTooLarge(t *testing.T) {
	h := newInjectsHarness(t)
	admin := h.login()
	id := h.createInject(admin, "blue", true)
	blue, _ := h.teamLogin(blueUsername)

	big := io.LimitReader(zeroReader{}, engine.MaxUploadBytes+1)
	if status, body := h.upload(http.MethodPut, fmt.Sprintf("/team/injects/%d/submission", id), blue, "", big); status != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d %s, want 413", status, body)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestScoreBreakdown_Public(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	r := h.do(http.MethodGet, "/score/breakdown", "", "")
	h.mustStatus(r, http.StatusOK, "GET /score/breakdown")
	if got := r.object(t)["blue"].(map[string]any); got["scavenger_hunt"] != 0.0 || got["total"] != 1000.0 {
		t.Errorf("blue = %v, want scavenger_hunt 0 and total 1000", got)
	}
}

func TestEvents_PublicMessagesHideInjects(t *testing.T) {
	h := newInjectsHarness(t)
	admin := h.login()
	h.createInject(admin, "red", true)

	public := string(h.do(http.MethodGet, "/events", "", "").body)
	if strings.Contains(public, "report") || strings.Contains(public, "red,") {
		t.Errorf("public events show the inject: %s", public)
	}
	if adminView := string(h.do(http.MethodGet, "/events", admin, "").body); !strings.Contains(adminView, "report") {
		t.Errorf("admin events lack the inject's title: %s", adminView)
	}
}

// slowReader hands out a few bytes at a time, pausing before each.
type slowReader struct{ left int }

func (r *slowReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	time.Sleep(50 * time.Millisecond)
	r.left--
	p[0] = 'x'
	return 1, nil
}

// An upload slower than the server's ReadTimeout still goes through: the
// upload routes give themselves longer.
func TestInjects_SlowUploadOutlivesReadTimeout(t *testing.T) {
	h := newInjectsHarness(t)
	id := h.createInject(h.login(), "blue", true)
	blueAuth := auth.New(blueUsername, testHash)
	srv := httptest.NewUnstartedServer(newServer(h.e, h.st, h.sess.ID, auth.New(testUsername, testHash), h.configPath,
		log.New(h.logs, "", 0), WithBlueTeam(blueAuth)))
	srv.Config.ReadTimeout = 100 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	token, _ := blueAuth.Login(blueUsername, testPassword)

	slow := *h
	slow.url = srv.URL
	if status, body := slow.upload(http.MethodPut, fmt.Sprintf("/team/injects/%d/submission", id), token, "", &slowReader{left: 8}); status != http.StatusOK {
		t.Errorf("slow upload = %d %s, want 200", status, body)
	}
}
