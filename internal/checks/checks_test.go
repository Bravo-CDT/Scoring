package checks

import (
	"context"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// splitHostPortInt splits a "host:port" listener address into its host and
// integer port, failing the test on any malformed input.
func splitHostPortInt(t *testing.T, hostport string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		t.Fatalf("splitting %q: %v", hostport, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing port %q: %v", portStr, err)
	}
	return host, port
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	called := false
	r.Register("fake", func(ctx context.Context, host string, params map[string]any) Result {
		called = true
		return Result{Success: true}
	})

	fn, ok := r.Get("fake")
	if !ok {
		t.Fatal("expected fake check to be registered")
	}
	fn(context.Background(), "host", nil)
	if !called {
		t.Error("registered function was not the one returned by Get")
	}

	if _, ok := r.Get("does-not-exist"); ok {
		t.Error("expected ok=false for unregistered check type")
	}
}

func TestRegistry_RegisterReplaces(t *testing.T) {
	r := NewRegistry()
	r.Register("x", func(context.Context, string, map[string]any) Result { return Result{Detail: "first"} })
	r.Register("x", func(context.Context, string, map[string]any) Result { return Result{Detail: "second"} })

	fn, _ := r.Get("x")
	if got := fn(context.Background(), "", nil).Detail; got != "second" {
		t.Errorf("Detail = %q, want the later registration to win", got)
	}
}

func TestNewDefaultRegistry_HasBuiltins(t *testing.T) {
	r := NewDefaultRegistry()
	for _, name := range []string{"tcp_connect", "http", "http_exclusive", "postgres", "smb", "smtp", "dns"} {
		if _, ok := r.Get(name); !ok {
			t.Errorf("expected default registry to include %q", name)
		}
	}
}

func TestBuiltinTypes(t *testing.T) {
	got := BuiltinTypes()
	if want := []string{"dns", "http", "http_exclusive", "postgres", "smb", "smtp", "tcp_connect"}; !slices.Equal(got, want) {
		t.Errorf("BuiltinTypes() = %v, want %v", got, want)
	}
	// Every built-in type is in the default registry, so config validation and
	// what the scheduler can run never disagree.
	r := NewDefaultRegistry()
	for _, name := range got {
		if _, ok := r.Get(name); !ok {
			t.Errorf("built-in type %q missing from the default registry", name)
		}
	}
}

func TestValidateParams(t *testing.T) {
	tests := []struct {
		name      string
		checkType string
		params    map[string]any
		mention   []string // substrings the error must contain; nil means valid
	}{
		{"tcp valid", "tcp_connect", map[string]any{"port": 22}, nil},
		{"tcp valid int64", "tcp_connect", map[string]any{"port": int64(22)}, nil},
		{"tcp missing port", "tcp_connect", map[string]any{}, []string{`"port"`, "missing"}},
		{"tcp nil params", "tcp_connect", nil, []string{`"port"`}},
		{"tcp misspelled port", "tcp_connect", map[string]any{"prot": 22}, []string{`"prot"`, "unknown"}},
		{"tcp extra key", "tcp_connect", map[string]any{"port": 22, "path": "/"}, []string{`"path"`, "unknown"}},
		{"tcp port zero", "tcp_connect", map[string]any{"port": 0}, []string{`"port"`, "65535"}},
		{"tcp port too large", "tcp_connect", map[string]any{"port": 70000}, []string{`"port"`, "65535"}},
		{"tcp port string", "tcp_connect", map[string]any{"port": "22"}, []string{`"port"`, "integer"}},
		{"tcp port float", "tcp_connect", map[string]any{"port": 22.5}, []string{`"port"`, "integer"}},

		{"http minimal", "http", map[string]any{"port": 80}, nil},
		{"http full", "http", map[string]any{"port": 80, "path": "/login?x=1", "expect_status": 302, "expect_contains": "ok"}, nil},
		{"http missing port", "http", map[string]any{"path": "/"}, []string{`"port"`, "missing"}},
		{"http expect_contain typo", "http", map[string]any{"port": 80, "expect_contain": "Welcome"}, []string{`"expect_contain"`, "unknown"}},
		{"http port too large", "http", map[string]any{"port": 70000}, []string{`"port"`, "65535"}},
		{"http path relative", "http", map[string]any{"port": 80, "path": "index.html"}, []string{`"path"`, `"/"`}},
		{"http path wrong type", "http", map[string]any{"port": 80, "path": 5}, []string{`"path"`, "string"}},
		{"http expect_status wrong type", "http", map[string]any{"port": 80, "expect_status": "200"}, []string{`"expect_status"`, "integer"}},
		{"http expect_status too low", "http", map[string]any{"port": 80, "expect_status": 99}, []string{`"expect_status"`, "599"}},
		{"http expect_status too high", "http", map[string]any{"port": 80, "expect_status": 600}, []string{`"expect_status"`, "599"}},
		{"http expect_contains wrong type", "http", map[string]any{"port": 80, "expect_contains": 7}, []string{`"expect_contains"`, "string"}},

		{"http_exclusive valid", "http_exclusive", map[string]any{"port": 80, "expect_contains_ci": "wordpress", "closed_port": 8080}, nil},
		{"http_exclusive missing port", "http_exclusive", map[string]any{"expect_contains_ci": "wordpress", "closed_port": 8080}, []string{`"port"`, "missing"}},
		{"http_exclusive missing expect_contains_ci", "http_exclusive", map[string]any{"port": 80, "closed_port": 8080}, []string{`"expect_contains_ci"`, "missing"}},
		{"http_exclusive missing closed_port", "http_exclusive", map[string]any{"port": 80, "expect_contains_ci": "wordpress"}, []string{`"closed_port"`, "missing"}},
		{"http_exclusive closed_port out of range", "http_exclusive", map[string]any{"port": 80, "expect_contains_ci": "wordpress", "closed_port": 70000}, []string{`"closed_port"`, "65535"}},
		{"http_exclusive misspelled param", "http_exclusive", map[string]any{"port": 80, "expect_contains_ci": "wordpress", "closed_port": 8080, "expect_contain_ci": "x"}, []string{`"expect_contain_ci"`, "unknown"}},

		{"postgres valid", "postgres", map[string]any{"port": 5432, "user": "mrkrabs", "password": "Patty321!", "dbname": "KrustySafe"}, nil},
		{"postgres missing port", "postgres", map[string]any{"user": "mrkrabs", "password": "Patty321!", "dbname": "KrustySafe"}, []string{`"port"`, "missing"}},
		{"postgres missing user", "postgres", map[string]any{"port": 5432, "password": "Patty321!", "dbname": "KrustySafe"}, []string{`"user"`, "missing"}},
		{"postgres missing password", "postgres", map[string]any{"port": 5432, "user": "mrkrabs", "dbname": "KrustySafe"}, []string{`"password"`, "missing"}},
		{"postgres missing dbname", "postgres", map[string]any{"port": 5432, "user": "mrkrabs", "password": "Patty321!"}, []string{`"dbname"`, "missing"}},
		{"postgres misspelled param", "postgres", map[string]any{"port": 5432, "user": "mrkrabs", "password": "x", "dbname": "x", "dbanme": "x"}, []string{`"dbanme"`, "unknown"}},

		{"smb valid", "smb", map[string]any{"port": 445, "user": "mrkrabs", "password": "Patty321!", "share": "sambashare"}, nil},
		{"smb valid with optional params", "smb", map[string]any{"port": 445, "user": "mrkrabs", "password": "Patty321!", "domain": "KRUSTYKRAB", "share": "sambashare", "expect_share": "sambashare", "expect_file": "secretformula.txt"}, nil},
		{"smb missing port", "smb", map[string]any{"user": "mrkrabs", "password": "Patty321!", "share": "sambashare"}, []string{`"port"`, "missing"}},
		{"smb missing user", "smb", map[string]any{"port": 445, "password": "Patty321!", "share": "sambashare"}, []string{`"user"`, "missing"}},
		{"smb missing password", "smb", map[string]any{"port": 445, "user": "mrkrabs", "share": "sambashare"}, []string{`"password"`, "missing"}},
		{"smb missing share", "smb", map[string]any{"port": 445, "user": "mrkrabs", "password": "Patty321!"}, []string{`"share"`, "missing"}},
		{"smb misspelled param", "smb", map[string]any{"port": 445, "user": "mrkrabs", "password": "x", "share": "s", "shre": "s"}, []string{`"shre"`, "unknown"}},

		{"smtp valid", "smtp", map[string]any{"port": 587, "user": "mrkrabs", "password": "Patty321!"}, nil},
		{"smtp valid with insecure_skip_verify", "smtp", map[string]any{"port": 587, "user": "mrkrabs", "password": "Patty321!", "insecure_skip_verify": true}, nil},
		{"smtp missing port", "smtp", map[string]any{"user": "mrkrabs", "password": "Patty321!"}, []string{`"port"`, "missing"}},
		{"smtp missing user", "smtp", map[string]any{"port": 587, "password": "Patty321!"}, []string{`"user"`, "missing"}},
		{"smtp empty user", "smtp", map[string]any{"port": 587, "user": "", "password": "Patty321!"}, []string{`"user"`, "missing"}},
		{"smtp missing password", "smtp", map[string]any{"port": 587, "user": "mrkrabs"}, []string{`"password"`, "missing"}},
		{"smtp misspelled param", "smtp", map[string]any{"port": 587, "user": "mrkrabs", "password": "x", "pasword": "x"}, []string{`"pasword"`, "unknown"}},
		{"smtp insecure_skip_verify wrong type", "smtp", map[string]any{"port": 587, "user": "mrkrabs", "password": "x", "insecure_skip_verify": "yes"}, []string{`"insecure_skip_verify"`, "boolean"}},

		{"unknown check type", "ftp", map[string]any{"port": 21}, []string{`"ftp"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateParams(tt.checkType, tt.params)
			if tt.mention == nil {
				if err != nil {
					t.Fatalf("ValidateParams: unexpected error %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("ValidateParams: expected an error, got nil")
			}
			for _, m := range tt.mention {
				if !strings.Contains(err.Error(), m) {
					t.Errorf("error %q should mention %s", err, m)
				}
			}

			// The plugin parses its params with the same code at run time, so
			// it must reject them with the same message.
			fn, ok := NewDefaultRegistry().Get(tt.checkType)
			if !ok {
				return
			}
			res := fn(context.Background(), "127.0.0.1", tt.params)
			if res.Success || res.Detail != err.Error() {
				t.Errorf("plugin result = %+v, want failure with Detail %q", res, err)
			}
		})
	}
}

func TestBuiltins_RejectEmptyHost(t *testing.T) {
	for name, fn := range map[string]CheckFunc{"tcp_connect": TCPConnect, "http": HTTP, "http_exclusive": HTTPExclusive, "postgres": Postgres, "smb": SMB, "smtp": SMTP, "dns": DNS} {
		t.Run(name, func(t *testing.T) {
			res := fn(context.Background(), "", map[string]any{"port": 80, "user": "u", "password": "p", "dbname": "d", "share": "s", "name": "n"})
			if res.Success {
				t.Fatal("expected failure for an empty host")
			}
			if !strings.Contains(res.Detail, "host") {
				t.Errorf("Detail = %q, expected it to mention the host", res.Detail)
			}
		})
	}
}

func TestParamPort(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]any
		want    int
		wantErr bool
	}{
		{"int", map[string]any{"port": 80}, 80, false},
		{"int64", map[string]any{"port": int64(443)}, 443, false},
		{"max", map[string]any{"port": 65535}, 65535, false},
		{"nil params", nil, 0, true},
		{"missing", map[string]any{}, 0, true},
		{"string", map[string]any{"port": "80"}, 0, true},
		{"float", map[string]any{"port": 80.0}, 0, true},
		{"zero", map[string]any{"port": 0}, 0, true},
		{"negative", map[string]any{"port": -1}, 0, true},
		{"too large", map[string]any{"port": 65536}, 0, true},
		{"too large int64", map[string]any{"port": int64(1) << 40}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := paramPort(tt.params)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got port %d", got)
				}
				if !strings.Contains(err.Error(), "port") {
					t.Errorf("error %q should name the port param", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("port = %d, want %d", got, tt.want)
			}
		})
	}
}
