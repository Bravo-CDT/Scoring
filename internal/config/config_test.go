package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/Bravo-CDT/Scoring/internal/checks"
)

// hashEnvVar is the environment variable the test fixtures interpolate the
// admin password hash from, exercising ${VAR} expansion the way a real
// deployment does.
const hashEnvVar = "TEST_ADMIN_HASH"

// realHash is a genuine bcrypt hash (cheapest cost, since nothing here ever
// verifies a password against it).
var realHash = func() string {
	h, err := bcrypt.GenerateFromPassword([]byte("competition"), bcrypt.MinCost)
	if err != nil {
		panic("generating test bcrypt hash: " + err.Error())
	}
	return string(h)
}()

const validYAML = `
session_name: "test-session"
starting_score: 1000
round_interval_seconds: 30

admin:
  username: admin
  password_hash: ${TEST_ADMIN_HASH}

services:
  - name: apache-web
    host: 10.0.1.10
    check_type: http
    points_per_failure: 10
    params:
      port: 80
      path: /
      expect_status: 200
      expect_contains: "Welcome"

  - name: telnet-win
    host: 10.0.2.15
    check_type: tcp_connect
    points_per_failure: 5
    params:
      port: 23
`

// minimalYAML is the smallest valid config: one
// service with only its required param.
const minimalYAML = `
session_name: "minimal"
starting_score: 500
round_interval_seconds: 60
admin:
  username: admin
  password_hash: ${TEST_ADMIN_HASH}
services:
  - name: svc1
    host: 127.0.0.1
    check_type: tcp_connect
    points_per_failure: 10
    params: {port: 22}
`

// writeTemp writes contents to a config.yaml in a per-test temp dir and
// returns its path.
func writeTemp(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

// setHash points the fixtures' ${TEST_ADMIN_HASH} at a usable hash for the
// duration of the test.
func setHash(t *testing.T) {
	t.Helper()
	t.Setenv(hashEnvVar, realHash)
}

func TestParse_SameBehaviorAsLoad(t *testing.T) {
	setHash(t)
	if _, err := Parse([]byte(validYAML)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestHash_SameBytesSameHash(t *testing.T) {
	a := Hash([]byte("x"))
	b := Hash([]byte("x"))
	c := Hash([]byte("y"))
	if a != b || a == c {
		t.Fatalf("Hash(%q) = %q, Hash(%q) = %q; want equal hashes for equal input, different for different input", "x", a, "y", c)
	}
}

func TestLoad_Valid(t *testing.T) {
	setHash(t)
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.SessionName != "test-session" {
		t.Errorf("SessionName = %q, want test-session", cfg.SessionName)
	}
	if cfg.StartingScore != 1000 {
		t.Errorf("StartingScore = %d, want 1000", cfg.StartingScore)
	}
	if cfg.RoundIntervalSeconds != 30 {
		t.Errorf("RoundIntervalSeconds = %d, want 30", cfg.RoundIntervalSeconds)
	}
	if cfg.Admin.Username != "admin" {
		t.Errorf("Admin.Username = %q, want admin", cfg.Admin.Username)
	}
	if cfg.Admin.PasswordHash != realHash {
		t.Errorf("Admin.PasswordHash = %q, want it expanded from $%s (%q)", cfg.Admin.PasswordHash, hashEnvVar, realHash)
	}

	if len(cfg.Services) != 2 {
		t.Fatalf("len(Services) = %d, want 2", len(cfg.Services))
	}
	web := cfg.Services[0]
	if web.Name != "apache-web" {
		t.Errorf("Services[0].Name = %q, want apache-web", web.Name)
	}
	if web.Host != "10.0.1.10" {
		t.Errorf("Services[0].Host = %q, want 10.0.1.10", web.Host)
	}
	if web.CheckType != "http" {
		t.Errorf("Services[0].CheckType = %q, want http", web.CheckType)
	}
	if web.PointsPerFailure != 10 {
		t.Errorf("Services[0].PointsPerFailure = %d, want 10", web.PointsPerFailure)
	}

	telnet := cfg.Services[1]
	if telnet.Name != "telnet-win" || telnet.CheckType != "tcp_connect" {
		t.Errorf("Services[1] = %+v, want telnet-win/tcp_connect", telnet)
	}
	if telnet.PointsPerFailure != 5 {
		t.Errorf("Services[1].PointsPerFailure = %d, want 5", telnet.PointsPerFailure)
	}
}

// TestLoad_ParamTypes pins down the dynamic types the check plugins will see
// when they read values out of Params: YAML integers must arrive as Go int
// (not int64 or float64), and quoted scalars as string.
func TestLoad_ParamTypes(t *testing.T) {
	setHash(t)
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	params := cfg.Services[0].Params

	port, ok := params["port"].(int)
	if !ok {
		t.Fatalf("Params[port] has type %T, want int", params["port"])
	}
	if port != 80 {
		t.Errorf("Params[port] = %d, want 80", port)
	}
	status, ok := params["expect_status"].(int)
	if !ok {
		t.Fatalf("Params[expect_status] has type %T, want int", params["expect_status"])
	}
	if status != 200 {
		t.Errorf("Params[expect_status] = %d, want 200", status)
	}
	if path, ok := params["path"].(string); !ok || path != "/" {
		t.Errorf("Params[path] = %#v, want string \"/\"", params["path"])
	}
	if contains, ok := params["expect_contains"].(string); !ok || contains != "Welcome" {
		t.Errorf("Params[expect_contains] = %#v, want string \"Welcome\"", params["expect_contains"])
	}
}

func TestLoad_MinimalConfig(t *testing.T) {
	setHash(t)
	cfg, err := Load(writeTemp(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if port, _ := cfg.Services[0].Params["port"].(int); port != 22 || len(cfg.Services[0].Params) != 1 {
		t.Errorf("Params = %v, want just port 22", cfg.Services[0].Params)
	}
}

// Every shipped check type requires a param today, so an omitted params block
// cannot pass validation; normalize still guarantees consumers a non-nil map.
func TestNormalize_OmittedParamsBecomeEmptyMap(t *testing.T) {
	cfg := Config{Services: []ServiceConfig{{Name: "svc1"}}}
	normalize(&cfg)
	if cfg.Services[0].Params == nil || len(cfg.Services[0].Params) != 0 {
		t.Errorf("Params = %#v, want a non-nil empty map", cfg.Services[0].Params)
	}
}

// The config accepts exactly the check types the checks package ships.
func TestKnownCheckTypesMatchChecksPackage(t *testing.T) {
	if got, want := knownCheckTypes(), checks.BuiltinTypes(); !slices.Equal(got, want) {
		t.Errorf("knownCheckTypes() = %v, want %v", got, want)
	}
}

// Check params are validated at load, per check type, so a typo fails at
// startup (or on /reload) rather than as a failing check that costs the blue
// team points. Each error names the service and the param.
func TestLoad_InvalidParams(t *testing.T) {
	// httpYAML is minimalYAML with svc1 turned into an http check carrying
	// the given params block.
	httpYAML := func(params string) string {
		y := strings.Replace(minimalYAML, "check_type: tcp_connect", "check_type: http", 1)
		return strings.Replace(y, "params: {port: 22}", "params: "+params, 1)
	}
	tcpYAML := func(params string) string {
		return strings.Replace(minimalYAML, "params: {port: 22}", "params: "+params, 1)
	}
	tests := []struct {
		name    string
		yaml    string
		mention []string
	}{
		{"params omitted", strings.Replace(minimalYAML, "    params: {port: 22}\n", "", 1), []string{`"port"`, "missing"}},
		{"missing port", tcpYAML("{}"), []string{`"port"`, "missing"}},
		{"misspelled port", tcpYAML("{prot: 22}"), []string{`"prot"`, "unknown"}},
		{"port zero", tcpYAML("{port: 0}"), []string{`"port"`, "between 1 and 65535"}},
		{"port too large", tcpYAML("{port: 70000}"), []string{`"port"`, "between 1 and 65535"}},
		{"port not an int", tcpYAML(`{port: "22"}`), []string{`"port"`, "integer", "string"}},
		{"port is a float", tcpYAML("{port: 22.5}"), []string{`"port"`, "integer"}},
		{"http expect_contain typo", httpYAML("{port: 80, expect_contain: Welcome}"), []string{`"expect_contain"`, "unknown"}},
		{"http missing port", httpYAML("{path: /}"), []string{`"port"`, "missing"}},
		{"http relative path", httpYAML("{port: 80, path: index.html}"), []string{`"path"`, `"/"`}},
		{"http path not a string", httpYAML("{port: 80, path: 5}"), []string{`"path"`, "string"}},
		{"http expect_status out of range", httpYAML("{port: 80, expect_status: 700}"), []string{`"expect_status"`, "between 100 and 599"}},
		{"http expect_status not an int", httpYAML(`{port: 80, expect_status: "200"}`), []string{`"expect_status"`, "integer"}},
		{"http expect_contains not a string", httpYAML("{port: 80, expect_contains: 7}"), []string{`"expect_contains"`, "string"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setHash(t)
			_, err := Load(writeTemp(t, tt.yaml))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			for _, m := range append([]string{`service "svc1"`}, tt.mention...) {
				if !strings.Contains(err.Error(), m) {
					t.Errorf("error = %q, want it to mention %s", err, m)
				}
			}
		})
	}
}

// LoadService validates the whole file, so one service's bad params make every
// lookup fail rather than letting the scheduler start the others on a config
// that startup would have refused.
func TestLoadService_RejectsInvalidParams(t *testing.T) {
	setHash(t)
	bad := validYAML + `
  - name: broken
    host: 10.0.3.1
    check_type: tcp_connect
    points_per_failure: 5
    params: {prot: 23}
`
	_, err := LoadService(writeTemp(t, bad), "telnet-win")
	if err == nil || !strings.Contains(err.Error(), `service "broken"`) || !strings.Contains(err.Error(), `"prot"`) {
		t.Fatalf("LoadService error = %v, want one naming service broken and param prot", err)
	}
	if errors.Is(err, ErrServiceNotFound) {
		t.Errorf("error = %v, want a validation error distinct from ErrServiceNotFound", err)
	}
}

// TestLoad_AcceptsHashFixture guards the synthetic hash literal that the rest
// of the engine's tests embed in their config fixtures: it is not a real
// bcrypt digest, but it must parse as one or every downstream test fails here.
func TestLoad_AcceptsHashFixture(t *testing.T) {
	t.Setenv(hashEnvVar, "$2a$10$abcdefghijklmnopqrstuvabcdefghijklmnopqrstuvwxyz123456")
	if _, err := Load(writeTemp(t, validYAML)); err != nil {
		t.Fatalf("Load returned error for synthetic bcrypt fixture: %v", err)
	}
}

// Expansion is not special-cased to the admin hash: any ${VAR} anywhere in
// the file is substituted, which lets a deployment parameterize hosts too.
func TestLoad_ExpandsEnvVarsAnywhere(t *testing.T) {
	setHash(t)
	t.Setenv("TEST_SVC_HOST", "192.0.2.7")
	yaml := strings.Replace(minimalYAML, "host: 127.0.0.1", "host: ${TEST_SVC_HOST}", 1)

	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Services[0].Host != "192.0.2.7" {
		t.Errorf("Host = %q, want 192.0.2.7", cfg.Services[0].Host)
	}
}

func TestLoad_MissingEnvVar(t *testing.T) {
	setHash(t)
	os.Unsetenv(hashEnvVar) // t.Setenv restores the original value afterwards.

	_, err := Load(writeTemp(t, validYAML))
	if err == nil {
		t.Fatal("expected error when admin hash env var is unset, got nil")
	}
	if !strings.Contains(err.Error(), hashEnvVar) {
		t.Errorf("error %q does not name the missing variable %s", err, hashEnvVar)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.yaml")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want it to wrap os.ErrNotExist", err)
	}
}

func TestLoad_InvalidConfigs(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string // substring the error must mention
	}{
		{
			name:    "malformed yaml",
			yaml:    "session_name: \"unterminated\nstarting_score: [1, 2",
			wantErr: "yaml",
		},
		{
			name: "unknown top-level field (the removed mass_failure_fraction too)",
			yaml: `
session_name: "s"
starting_score: 1000
round_interval_seconds: 60
mass_failure_fraction: 0.8
admin:
  username: admin
  password_hash: ${TEST_ADMIN_HASH}
services:
  - name: svc1
    host: 127.0.0.1
    check_type: http
    points_per_failure: 10
`,
			wantErr: "mass_failure_fraction",
		},
		{
			// Strictness is recursive: a typo inside a service entry is
			// rejected too, instead of leaving the field at its zero value.
			name:    "unknown service field",
			yaml:    strings.Replace(minimalYAML, "points_per_failure: 10", "points_per_falure: 10", 1),
			wantErr: "points_per_falure",
		},
		{
			// The pre-rounds per-service interval is gone; a config still
			// carrying one must fail loudly rather than have it silently
			// ignored in favour of the shared round interval.
			name:    "leftover per-service interval_seconds",
			yaml:    strings.Replace(minimalYAML, "    points_per_failure: 10\n", "    points_per_failure: 10\n    interval_seconds: 60\n", 1),
			wantErr: "interval_seconds",
		},
		{
			name: "missing session_name",
			yaml: `
starting_score: 1000
round_interval_seconds: 60
admin:
  username: admin
  password_hash: ${TEST_ADMIN_HASH}
services:
  - name: svc1
    host: 127.0.0.1
    check_type: http
    points_per_failure: 10
`,
			wantErr: "session_name",
		},
		{
			name:    "zero starting_score",
			yaml:    strings.Replace(minimalYAML, "starting_score: 500", "starting_score: 0", 1),
			wantErr: "starting_score",
		},
		{
			name:    "negative starting_score",
			yaml:    strings.Replace(minimalYAML, "starting_score: 500", "starting_score: -1", 1),
			wantErr: "starting_score",
		},
		{
			name:    "missing admin username",
			yaml:    strings.Replace(minimalYAML, "  username: admin\n", "", 1),
			wantErr: "admin.username",
		},
		{
			name:    "invalid bcrypt hash",
			yaml:    strings.Replace(minimalYAML, "${TEST_ADMIN_HASH}", "not-a-bcrypt-hash", 1),
			wantErr: "password_hash",
		},
		{
			name: "duplicate service name",
			yaml: validYAML + `
  - name: apache-web
    host: 10.0.1.99
    check_type: http
    points_per_failure: 10
    params:
      port: 80
`,
			wantErr: `duplicate service name "apache-web"`,
		},
		{
			name:    "unknown check_type",
			yaml:    strings.Replace(minimalYAML, "check_type: tcp_connect", "check_type: not_a_real_check", 1),
			wantErr: "not_a_real_check",
		},
		{
			name: "missing service name",
			yaml: `
session_name: "s"
starting_score: 1000
round_interval_seconds: 60
admin:
  username: admin
  password_hash: ${TEST_ADMIN_HASH}
services:
  - host: 127.0.0.1
    check_type: http
    points_per_failure: 10
`,
			wantErr: "services[0].name",
		},
		{
			name:    "missing service host",
			yaml:    strings.Replace(minimalYAML, "    host: 127.0.0.1\n", "", 1),
			wantErr: "host",
		},
		{
			name:    "negative points_per_failure",
			yaml:    strings.Replace(minimalYAML, "points_per_failure: 10", "points_per_failure: -5", 1),
			wantErr: "points_per_failure",
		},
		{
			name:    "network cidr that isn't one",
			yaml:    minimalYAML + "networks:\n  - cidr: 10.111.1.0\n",
			wantErr: "networks[0].cidr",
		},
		{
			name:    "host listed twice",
			yaml:    minimalYAML + "hosts:\n  - address: 10.0.0.1\n  - address: 10.0.0.1\n",
			wantErr: "duplicate host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setHash(t)
			_, err := Load(writeTemp(t, tt.yaml))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoad_RoundIntervalSecondsRequired(t *testing.T) {
	setHash(t)
	_, err := Load(writeTemp(t, strings.Replace(minimalYAML, "round_interval_seconds: 60\n", "", 1)))
	if err == nil || !strings.Contains(err.Error(), "round_interval_seconds") {
		t.Fatalf("Load error = %v, want one naming round_interval_seconds", err)
	}
}

func TestLoad_RoundIntervalSecondsMustBePositive(t *testing.T) {
	for _, n := range []string{"0", "-5"} {
		t.Run(n, func(t *testing.T) {
			setHash(t)
			_, err := Load(writeTemp(t, strings.Replace(minimalYAML, "round_interval_seconds: 60", "round_interval_seconds: "+n, 1)))
			if err == nil || !strings.Contains(err.Error(), "round_interval_seconds must be a positive integer") {
				t.Fatalf("Load error = %v, want the positive-integer error", err)
			}
		})
	}
}

func TestLoadService_Found(t *testing.T) {
	setHash(t)
	svc, err := LoadService(writeTemp(t, validYAML), "telnet-win")
	if err != nil {
		t.Fatalf("LoadService returned error: %v", err)
	}
	if svc.Name != "telnet-win" {
		t.Errorf("Name = %q, want telnet-win", svc.Name)
	}
	if svc.Host != "10.0.2.15" {
		t.Errorf("Host = %q, want 10.0.2.15", svc.Host)
	}
	if svc.PointsPerFailure != 5 {
		t.Errorf("PointsPerFailure = %d, want 5", svc.PointsPerFailure)
	}
	if port, _ := svc.Params["port"].(int); port != 23 {
		t.Errorf("Params[port] = %#v, want 23", svc.Params["port"])
	}
}

func TestLoadService_NotFound(t *testing.T) {
	setHash(t)
	_, err := LoadService(writeTemp(t, validYAML), "does-not-exist")
	if !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("error = %v, want it to wrap ErrServiceNotFound", err)
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error = %q, want it to name the missing service", err)
	}
}

// A parse or validation failure must surface as itself, not be mistaken for a
// removed service: Task 7's scheduler keys "service was deleted from config"
// handling on ErrServiceNotFound alone.
func TestLoadService_InvalidConfigIsNotServiceNotFound(t *testing.T) {
	setHash(t)
	bad := strings.Replace(minimalYAML, "check_type: tcp_connect", "check_type: not_a_real_check", 1)
	_, err := LoadService(writeTemp(t, bad), "svc1")
	if err == nil {
		t.Fatal("expected error for invalid config, got nil")
	}
	if !strings.Contains(err.Error(), "not_a_real_check") {
		t.Errorf("error = %v, want it to name the unknown check type", err)
	}
	if errors.Is(err, ErrServiceNotFound) {
		t.Errorf("error = %v, want a validation error distinct from ErrServiceNotFound", err)
	}
}

// The check command must run without the admin hash in its environment: an
// unset ${VAR} is reported, not fatal, and the admin section is not checked.
// Services are still validated exactly as Load validates them.
func TestLoadForChecks_ToleratesUnsetAdminHash(t *testing.T) {
	const unsetVar = "SOME_UNSET_VAR_XYZ"
	os.Unsetenv(unsetVar)
	yml := strings.Replace(minimalYAML, "${TEST_ADMIN_HASH}", "${"+unsetVar+"}", 1)

	cfg, unset, err := LoadForChecks(writeTemp(t, yml))
	if err != nil {
		t.Fatalf("LoadForChecks: %v", err)
	}
	if !slices.Equal(unset, []string{unsetVar}) {
		t.Errorf("unsetVars = %v, want [%s]", unset, unsetVar)
	}
	if len(cfg.Services) != 1 || cfg.Services[0].Name != "svc1" {
		t.Errorf("services = %+v, want the one svc1", cfg.Services)
	}

	bad := strings.Replace(yml, "check_type: tcp_connect", "check_type: not_a_real_check", 1)
	if _, _, err := LoadForChecks(writeTemp(t, bad)); err == nil || !strings.Contains(err.Error(), "not_a_real_check") {
		t.Errorf("LoadForChecks with a bad check_type: err = %v, want it to name the unknown check type", err)
	}
}
