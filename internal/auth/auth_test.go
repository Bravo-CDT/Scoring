package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func newTestAuth(t *testing.T, password string) *Auth {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	return New("admin", string(hash))
}

func login(t *testing.T, a *Auth) string {
	t.Helper()
	token, ok := a.Login("admin", "secret")
	if !ok {
		t.Fatal("Login(admin, secret) failed")
	}
	return token
}

func TestLogin_CorrectCredentials(t *testing.T) {
	a := newTestAuth(t, "secret")
	token, ok := a.Login("admin", "secret")
	if !ok || token == "" {
		t.Fatalf("Login(admin, secret) = %q, %v; want non-empty token, true", token, ok)
	}
	if len(token) != 64 {
		t.Errorf("token length = %d, want 64 hex chars (256 bits)", len(token))
	}
	if !a.Valid(token) {
		t.Error("expected freshly issued token to be valid")
	}
}

func TestLogin_IssuesDistinctTokens(t *testing.T) {
	a := newTestAuth(t, "secret")
	t1, t2 := login(t, a), login(t, a)
	if t1 == t2 {
		t.Fatal("two logins returned the same token")
	}
	if !a.Valid(t1) || !a.Valid(t2) {
		t.Error("both tokens should remain valid")
	}
}

func TestLogin_Rejections(t *testing.T) {
	a := newTestAuth(t, "secret")
	cases := []struct{ name, user, pass string }{
		{"wrong password", "admin", "wrong"},
		{"wrong username", "nobody", "secret"},
		{"username case differs", "Admin", "secret"},
		{"username prefix", "admi", "secret"},
		{"empty username", "", "secret"},
		{"empty password", "admin", ""},
		{"both empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, ok := a.Login(tc.user, tc.pass)
			if ok || token != "" {
				t.Errorf("Login(%q, %q) = %q, %v; want \"\", false", tc.user, tc.pass, token, ok)
			}
		})
	}
}

func TestLogin_InvalidStoredHashFailsClosed(t *testing.T) {
	for _, hash := range []string{"", "not-a-bcrypt-hash", "$2a$10$tooshort", "secret"} {
		a := New("admin", hash)
		for _, pass := range []string{"", "secret", hash} {
			if token, ok := a.Login("admin", pass); ok || token != "" {
				t.Errorf("hash %q: Login(admin, %q) = %q, %v; want \"\", false", hash, pass, token, ok)
			}
		}
	}
}

func TestValid_UnknownTokenRejected(t *testing.T) {
	a := newTestAuth(t, "secret")
	token := login(t, a)
	for _, bad := range []string{"", "not-a-real-token", token[:len(token)-1], token + "0", " " + token} {
		if a.Valid(bad) {
			t.Errorf("Valid(%q) = true; want false", bad)
		}
	}
}

func TestValid_EmptyTokenOnFreshAuth(t *testing.T) {
	a := newTestAuth(t, "secret")
	if a.Valid("") {
		t.Error("empty token must never be valid")
	}
}

func TestValid_TokensNotSharedAcrossInstances(t *testing.T) {
	// Tokens live in memory only, so a new Auth (e.g. after a restart)
	// must not accept tokens issued by an old one.
	a := newTestAuth(t, "secret")
	token := login(t, a)
	b := newTestAuth(t, "secret")
	if b.Valid(token) {
		t.Error("token from one Auth instance accepted by another")
	}
}

func TestRequireAdmin_Rejects(t *testing.T) {
	a := newTestAuth(t, "secret")
	token := login(t, a)
	cases := []struct{ name, header string }{
		{"missing header", ""},
		{"scheme only", "Bearer"},
		{"scheme with empty token", "Bearer "},
		{"unknown token", "Bearer nope"},
		{"wrong scheme", "Basic " + token},
		{"no scheme", token},
		{"no separator", "Bearer" + token},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := a.RequireAdmin(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

			req := httptest.NewRequest(http.MethodPost, "/engine/stop", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if called {
				t.Error("wrapped handler must not be called")
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
			}
			if body["error"] == "" {
				t.Errorf("body %q missing error field", rec.Body.String())
			}
		})
	}
}

func TestRequireAdmin_ValidToken(t *testing.T) {
	a := newTestAuth(t, "secret")
	token := login(t, a)
	for _, header := range []string{"Bearer " + token, "bearer " + token, "BEARER " + token, "Bearer   " + token + " "} {
		t.Run(header, func(t *testing.T) {
			called := false
			h := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodPost, "/engine/stop", nil)
			req.Header.Set("Authorization", header)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK || !called {
				t.Errorf("status = %d, called = %v; want 200, true", rec.Code, called)
			}
		})
	}
}

func TestIsAuthenticated(t *testing.T) {
	a := newTestAuth(t, "secret")
	token := login(t, a)

	cases := []struct {
		name, header string
		want         bool
	}{
		{"no header", "", false},
		{"invalid token", "Bearer nope", false},
		{"wrong scheme", "Basic " + token, false},
		{"valid token", "Bearer " + token, true},
		{"lowercase scheme", "bearer " + token, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/services/x/results", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if got := a.IsAuthenticated(req); got != tc.want {
				t.Errorf("IsAuthenticated = %v, want %v", got, tc.want)
			}
		})
	}
}

// Without -race available, this test still exercises concurrent writers
// (Login) against concurrent readers (Valid) and then verifies the token set
// is intact: every issued token is distinct and valid. Lost writes or map
// corruption (Go's runtime detects concurrent map writes and aborts) would
// fail it.
func TestConcurrentLoginAndValidate(t *testing.T) {
	a := newTestAuth(t, "secret")
	seed := login(t, a)

	const workers = 64
	tokens := make([]string, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range workers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			token, ok := a.Login("admin", "secret")
			if !ok || !a.Valid(token) {
				t.Errorf("worker %d: concurrent login/validate failed", i)
			}
			tokens[i] = token
		}()
		go func() {
			defer wg.Done()
			<-start
			for range 200 {
				if !a.Valid(seed) {
					t.Errorf("seed token became invalid during concurrent logins")
					return
				}
				if a.Valid("bogus") {
					t.Errorf("bogus token accepted during concurrent logins")
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	seen := make(map[string]bool, workers)
	for i, tok := range tokens {
		if tok == "" {
			continue // already reported by the worker
		}
		if seen[tok] {
			t.Errorf("worker %d: duplicate token issued", i)
		}
		seen[tok] = true
		if !a.Valid(tok) {
			t.Errorf("worker %d: token lost after concurrent logins", i)
		}
	}
}
