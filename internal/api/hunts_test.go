package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// createHunt creates a hunt as the admin, open if open, due in an hour, worth
// 500 points.
func (h *harness) createHunt(admin, team, code string, open bool) int64 {
	h.t.Helper()
	deadline := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body := fmt.Sprintf(`{"title":"flag","team":%q,"description":"find it","code":%q,"points":500,"deadline":%q}`, team, code, deadline)
	r := h.do(http.MethodPost, "/admin/hunts", admin, body)
	h.mustStatus(r, http.StatusOK, "create hunt")
	id := int64(r.object(h.t)["id"].(float64))
	if open {
		body = fmt.Sprintf(`{"title":"flag","description":"find it","code":%q,"points":500,"deadline":%q,"open":true}`, code, deadline)
		r = h.do(http.MethodPut, fmt.Sprintf("/admin/hunts/%d", id), admin, body)
		h.mustStatus(r, http.StatusOK, "open hunt")
	}
	return id
}

func claim(h *harness, token string, id int64, code string) response {
	return h.do(http.MethodPost, fmt.Sprintf("/team/hunts/%d/claim", id), token, fmt.Sprintf(`{"code":%q}`, code))
}

func TestHunts_ClaimFlow(t *testing.T) {
	h := newInjectsHarness(t)
	admin := h.login()
	blue, _ := h.teamLogin(blueUsername)
	red, _ := h.teamLogin(redUsername)
	id := h.createHunt(admin, "either", "Flag{x}", true)

	h.mustStatus(claim(h, blue, id, "flag{x}"), http.StatusBadRequest, "wrong case")
	r := claim(h, blue, id, "Flag{x}")
	h.mustStatus(r, http.StatusOK, "blue claim")
	if got := r.object(t)["claimed_by"]; got != "blue" {
		t.Errorf("claimed_by = %v, want blue", got)
	}
	h.mustStatus(claim(h, red, id, "Flag{x}"), http.StatusConflict, "red after blue")

	r = h.do(http.MethodGet, "/score/breakdown", "", "")
	h.mustStatus(r, http.StatusOK, "breakdown")
	if got := r.object(t)["blue"].(map[string]any)["scavenger_hunt"]; got != 500.0 {
		t.Errorf("blue scavenger_hunt = %v, want 500", got)
	}
}

func TestHunts_TeamNeverSeesCode(t *testing.T) {
	h := newInjectsHarness(t)
	admin := h.login()
	blue, _ := h.teamLogin(blueUsername)
	id := h.createHunt(admin, "blue", "Flag{secret}", true)

	list := h.do(http.MethodGet, "/team/hunts", blue, "")
	h.mustStatus(list, http.StatusOK, "team list")
	if strings.Contains(string(list.body), "Flag{secret}") {
		t.Errorf("team list leaks the code: %s", list.body)
	}
	r := claim(h, blue, id, "Flag{secret}")
	h.mustStatus(r, http.StatusOK, "claim")
	if strings.Contains(string(r.body), "Flag{secret}") {
		t.Errorf("claim response leaks the code: %s", r.body)
	}
	list = h.do(http.MethodGet, "/team/hunts", admin, "")
	h.mustStatus(list, http.StatusOK, "admin list")
	if !strings.Contains(string(list.body), "Flag{secret}") {
		t.Errorf("admin list lacks the code: %s", list.body)
	}
}

func TestHunts_RedCannotClaimBlue(t *testing.T) {
	h := newInjectsHarness(t)
	id := h.createHunt(h.login(), "blue", "c", true)
	red, _ := h.teamLogin(redUsername)
	h.mustStatus(claim(h, red, id, "c"), http.StatusNotFound, "red claims blue hunt")
}

func TestHunts_CooldownIs429(t *testing.T) {
	h := newInjectsHarness(t)
	id := h.createHunt(h.login(), "either", "c", true)
	blue, _ := h.teamLogin(blueUsername)
	for range 5 {
		h.mustStatus(claim(h, blue, id, "bad"), http.StatusBadRequest, "wrong code")
	}
	r := claim(h, blue, id, "bad")
	h.mustStatus(r, http.StatusTooManyRequests, "locked")
	if r.header.Get("Retry-After") == "" {
		t.Error("no Retry-After header")
	}
}

func TestHunts_NeedsTeamToken(t *testing.T) {
	h := newInjectsHarness(t)
	admin := h.login()
	id := h.createHunt(admin, "either", "c", true)
	h.mustStatus(claim(h, "", id, "c"), http.StatusUnauthorized, "no token")
	h.mustStatus(claim(h, admin, id, "c"), http.StatusUnauthorized, "admin token")
}
