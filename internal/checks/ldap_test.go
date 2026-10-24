package checks

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// validLDAPParams returns a full, valid set of ldap params for port.
func validLDAPParams(port int) map[string]any {
	return map[string]any{
		"port":        port,
		"bind_user":   "spongebob@krusty.krab",
		"password":    "Patty321!",
		"base_dn":     "dc=krusty,dc=krab",
		"search_user": "spongebob",
	}
}

func TestLDAP_Params(t *testing.T) {
	for _, key := range []string{"port", "bind_user", "password", "base_dn", "search_user"} {
		t.Run("missing "+key, func(t *testing.T) {
			params := validLDAPParams(389)
			delete(params, key)
			err := ValidateParams("ldap", params)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), `"`+key+`"`) {
				t.Errorf("error %q should mention %q", err, key)
			}
		})
	}
	for _, key := range []string{"bind_user", "password", "base_dn", "search_user"} {
		t.Run("empty "+key, func(t *testing.T) {
			params := validLDAPParams(389)
			params[key] = ""
			err := ValidateParams("ldap", params)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), `"`+key+`"`) {
				t.Errorf("error %q should mention %q", err, key)
			}
		})
	}
	t.Run("unknown param", func(t *testing.T) {
		params := validLDAPParams(389)
		params["bogus"] = 1
		err := ValidateParams("ldap", params)
		if err == nil || !strings.Contains(err.Error(), `"bogus"`) {
			t.Fatalf("expected error mentioning \"bogus\", got %v", err)
		}
	})
	if err := ValidateParams("ldap", validLDAPParams(389)); err != nil {
		t.Errorf("valid params rejected: %v", err)
	}
}

func TestLDAP_ConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res := LDAP(ctx, "127.0.0.1", validLDAPParams(port))
	if res.Success {
		t.Fatalf("expected failure, got %+v", res)
	}
	if !strings.HasPrefix(res.Detail, "connecting to") {
		t.Errorf("Detail %q should start with \"connecting to\"", res.Detail)
	}
	if strings.Contains(res.Detail, "Patty321!") {
		t.Errorf("Detail %q must not contain the password", res.Detail)
	}
}
