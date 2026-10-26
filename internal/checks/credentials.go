package checks

import (
	"maps"
	"strings"
)

// Credential is a username and password a check logs in with: what the blue
// team can change for a credentialed service.
type Credential struct {
	Username string
	Password string
}

// credentialParams names, per check type that logs in to something, the
// param holding its username and the one holding its password. A script's
// credential lives in its env instead; see scriptCredentialEnv.
var credentialParams = map[string]struct{ user, password string }{
	"postgres": {"user", "password"},
	"smb":      {"user", "password"},
	"smtp":     {"user", "password"},
	"openfire": {"user", "password"},
	"ldap":     {"bind_user", "password"},
}

// The env variables a script check reads its credential from. A script is
// credentialed only if its env sets both.
const (
	scriptUserEnv     = "CHECK_USER"
	scriptPasswordEnv = "CHECK_PASSWORD"
)

// DefaultUsername returns the username params configure for a check of
// checkType, without an ldap UPN's @domain, and whether the service is
// credentialed at all.
func DefaultUsername(checkType string, params map[string]any) (string, bool) {
	if checkType == "script" {
		env, ok := scriptCredentialEnv(params)
		if !ok {
			return "", false
		}
		user, _ := env[scriptUserEnv].(string)
		return user, true
	}
	keys, ok := credentialParams[checkType]
	if !ok {
		return "", false
	}
	user, _ := params[keys.user].(string)
	name, _, _ := strings.Cut(user, "@")
	return name, true
}

// WithCredential returns a copy of params with c's username and password in
// place of the configured ones, and true; or params itself and false if a
// check of checkType has no credential to replace. An ldap bind_user keeps
// the configured @domain, so "patrick" binds as patrick@krusty.krab. The
// input is never modified.
func WithCredential(checkType string, params map[string]any, c Credential) (map[string]any, bool) {
	if checkType == "script" {
		env, ok := scriptCredentialEnv(params)
		if !ok {
			return params, false
		}
		env = maps.Clone(env)
		env[scriptUserEnv] = c.Username
		env[scriptPasswordEnv] = c.Password
		out := maps.Clone(params)
		out["env"] = env
		return out, true
	}
	keys, ok := credentialParams[checkType]
	if !ok {
		return params, false
	}
	user := c.Username
	if configured, _ := params[keys.user].(string); strings.Contains(configured, "@") {
		_, domain, _ := strings.Cut(configured, "@")
		user += "@" + domain
	}
	out := maps.Clone(params)
	out[keys.user] = user
	out[keys.password] = c.Password
	return out, true
}

// scriptCredentialEnv returns a script check's env map if it sets both the
// user and password variables.
func scriptCredentialEnv(params map[string]any) (map[string]any, bool) {
	env, ok := params["env"].(map[string]any)
	if !ok {
		return nil, false
	}
	_, hasUser := env[scriptUserEnv]
	_, hasPassword := env[scriptPasswordEnv]
	return env, hasUser && hasPassword
}
