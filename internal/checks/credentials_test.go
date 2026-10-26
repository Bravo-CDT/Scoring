package checks

import (
	"maps"
	"reflect"
	"testing"
)

func TestWithCredential(t *testing.T) {
	c := Credential{Username: "patrick", Password: "pw2"}
	tests := []struct {
		name      string
		checkType string
		params    map[string]any
		want      map[string]any // nil: not credentialed
	}{
		{"postgres", "postgres",
			map[string]any{"port": 5432, "user": "mrkrabs", "password": "pw1", "dbname": "db"},
			map[string]any{"port": 5432, "user": "patrick", "password": "pw2", "dbname": "db"}},
		{"smb", "smb",
			map[string]any{"port": 445, "user": "mrkrabs", "password": "pw1", "share": "s"},
			map[string]any{"port": 445, "user": "patrick", "password": "pw2", "share": "s"}},
		{"smtp", "smtp",
			map[string]any{"port": 587, "user": "mrkrabs", "password": "pw1"},
			map[string]any{"port": 587, "user": "patrick", "password": "pw2"}},
		{"openfire", "openfire",
			map[string]any{"port": 9090, "user": "sandy", "password": "pw1"},
			map[string]any{"port": 9090, "user": "patrick", "password": "pw2"}},
		{"ldap keeps the UPN's domain", "ldap",
			map[string]any{"port": 389, "bind_user": "spongebob@krusty.krab", "password": "pw1", "search_user": "spongebob"},
			map[string]any{"port": 389, "bind_user": "patrick@krusty.krab", "password": "pw2", "search_user": "spongebob"}},
		{"ldap without a domain", "ldap",
			map[string]any{"port": 389, "bind_user": "admin", "password": "pw1"},
			map[string]any{"port": 389, "bind_user": "patrick", "password": "pw2"}},
		{"script with user and password env", "script",
			map[string]any{"script": "checks/telnet.exp", "env": map[string]any{"CHECK_USER": "sandy", "CHECK_PASSWORD": "pw1", "CHECK_PORT": 23}},
			map[string]any{"script": "checks/telnet.exp", "env": map[string]any{"CHECK_USER": "patrick", "CHECK_PASSWORD": "pw2", "CHECK_PORT": 23}}},
		{"script without a password env", "script",
			map[string]any{"script": "checks/ca_rpc.sh", "env": map[string]any{"CHECK_USER": "sandy"}},
			nil},
		{"script without env", "script",
			map[string]any{"script": "checks/ca_rpc.sh"},
			nil},
		{"http", "http",
			map[string]any{"port": 80},
			nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := deepCopy(tt.params)
			got, ok := WithCredential(tt.checkType, tt.params, c)
			if ok != (tt.want != nil) {
				t.Fatalf("ok = %v, want %v", ok, tt.want != nil)
			}
			if ok && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("params = %v, want %v", got, tt.want)
			}
			if !reflect.DeepEqual(tt.params, before) {
				t.Errorf("input params changed to %v", tt.params)
			}
		})
	}
}

func TestDefaultUsername(t *testing.T) {
	tests := []struct {
		checkType string
		params    map[string]any
		want      string
		wantOK    bool
	}{
		{"postgres", map[string]any{"user": "mrkrabs", "password": "x"}, "mrkrabs", true},
		{"ldap", map[string]any{"bind_user": "spongebob@krusty.krab", "password": "x"}, "spongebob", true},
		{"script", map[string]any{"env": map[string]any{"CHECK_USER": "sandy", "CHECK_PASSWORD": "x"}}, "sandy", true},
		{"script", map[string]any{"script": "checks/ca_rpc.sh"}, "", false},
		{"dns", map[string]any{"port": 53}, "", false},
	}
	for _, tt := range tests {
		got, ok := DefaultUsername(tt.checkType, tt.params)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("DefaultUsername(%s, %v) = %q, %v; want %q, %v", tt.checkType, tt.params, got, ok, tt.want, tt.wantOK)
		}
	}
}

// deepCopy copies params and any nested env map, for checking the input was
// left alone.
func deepCopy(params map[string]any) map[string]any {
	out := maps.Clone(params)
	if env, ok := out["env"].(map[string]any); ok {
		out["env"] = maps.Clone(env)
	}
	return out
}
