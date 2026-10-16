// Package auth provides single-admin authentication: a bcrypt-checked login
// that issues random bearer tokens, plus HTTP helpers that check them.
//
// Tokens are held only in memory, so every token is invalidated when the
// process restarts. There is no expiry, rate limiting or TLS handling; the
// engine's threat model does not include attacks on the engine itself.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// tokenBytes is the amount of randomness in each token (256 bits).
const tokenBytes = 32

// Auth checks the single admin account and tracks issued tokens. It is safe
// for concurrent use.
type Auth struct {
	username     string
	passwordHash []byte

	mu sync.RWMutex
	// tokens holds the SHA-256 digest of each issued token, not the token
	// itself. Looking up a digest means map-lookup timing reveals nothing
	// about how much of a guessed token matches a real one.
	tokens map[[sha256.Size]byte]struct{}
}

// New returns an Auth for the given admin username and bcrypt password hash.
// An unparseable hash is not an error here; it simply makes every Login fail.
func New(username, passwordHash string) *Auth {
	return &Auth{
		username:     username,
		passwordHash: []byte(passwordHash),
		tokens:       make(map[[sha256.Size]byte]struct{}),
	}
}

// Login checks the credentials and, if they match, issues a new token. It
// fails closed: any error (bad hash, mismatched password, entropy failure)
// yields ("", false).
func (a *Auth) Login(username, password string) (token string, ok bool) {
	// Always run the bcrypt comparison, even for a wrong username, so the
	// response time does not reveal whether the username was right.
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(a.username)) == 1
	passOK := bcrypt.CompareHashAndPassword(a.passwordHash, []byte(password)) == nil
	if !userOK || !passOK {
		return "", false
	}

	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", false
	}
	token = hex.EncodeToString(buf)

	a.mu.Lock()
	a.tokens[sha256.Sum256([]byte(token))] = struct{}{}
	a.mu.Unlock()
	return token, true
}

// Valid reports whether token was issued by Login on this Auth. The empty
// token is never valid.
func (a *Auth) Valid(token string) bool {
	if token == "" {
		return false
	}
	digest := sha256.Sum256([]byte(token))
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.tokens[digest]
	return ok
}

// RequireAdmin wraps a handler, responding 401 if the request's
// Authorization: Bearer <token> header is missing or invalid.
func (a *Auth) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.IsAuthenticated(r) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"missing or invalid bearer token"}` + "\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IsAuthenticated reports whether the request carries a currently valid
// token, without rejecting the request if not — used by the results
// endpoint to decide whether to include detail text.
func (a *Auth) IsAuthenticated(r *http.Request) bool {
	return a.Valid(bearerToken(r))
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header, matching the scheme case-insensitively. It returns "" if the
// header is absent or uses another scheme.
func bearerToken(r *http.Request) string {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}
