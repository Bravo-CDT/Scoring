// Package checks holds the service-check plugins the scoring engine runs
// against competitors' hosts, and the Registry that maps a configured check
// type name to its implementation.
//
// Plugins are deliberately ignorant of configuration, scoring and
// persistence: each one receives a host and its raw params map and reports a
// Result. Malformed params produce a failed Result whose Detail names the
// offending param; a plugin never panics on bad input.
//
// Each built-in plugin parses its params with one function, which both the
// plugin (at every check) and ValidateParams (at config load) call, so what
// the config accepts and what the plugin can run cannot drift apart.
package checks

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// builtin is one check type the engine ships: its implementation and the
// validator for its params, which wraps the same parser the implementation
// uses.
type builtin struct {
	check    CheckFunc
	validate func(params map[string]any) error
}

// builtins maps each shipped check type name to its plugin.
var builtins = map[string]builtin{
	"dns": {
		check:    DNS,
		validate: func(p map[string]any) error { _, err := parseDNSParams(p); return err },
	},
	"tcp_connect": {
		check:    TCPConnect,
		validate: func(p map[string]any) error { _, err := parseTCPConnectParams(p); return err },
	},
	"http": {
		check:    HTTP,
		validate: func(p map[string]any) error { _, err := parseHTTPParams(p); return err },
	},
	"http_exclusive": {
		check:    HTTPExclusive,
		validate: func(p map[string]any) error { _, err := parseHTTPExclusiveParams(p); return err },
	},
	"ldap": {
		check:    LDAP,
		validate: func(p map[string]any) error { _, err := parseLDAPParams(p); return err },
	},
	"postgres": {
		check:    Postgres,
		validate: func(p map[string]any) error { _, err := parsePostgresParams(p); return err },
	},
	"smb": {
		check:    SMB,
		validate: func(p map[string]any) error { _, err := parseSMBParams(p); return err },
	},
	"smtp": {
		check:    SMTP,
		validate: func(p map[string]any) error { _, err := parseSMTPParams(p); return err },
	},
}

// BuiltinTypes returns the names of the check types the engine ships, sorted.
// The config validates check_type against this list.
func BuiltinTypes() []string {
	return slices.Sorted(maps.Keys(builtins))
}

// ValidateParams reports whether params are valid for the built-in check type
// checkType: every required param present, every param of the right type and
// range, and no params the type does not know (a misspelled optional param
// would otherwise be silently ignored). The error names the offending param
// and what it must be; it does not name the service, which the caller adds.
func ValidateParams(checkType string, params map[string]any) error {
	b, ok := builtins[checkType]
	if !ok {
		return fmt.Errorf("unknown check type %q (known: %s)", checkType, strings.Join(BuiltinTypes(), ", "))
	}
	return b.validate(params)
}

// Result is the outcome of one check attempt.
type Result struct {
	Success bool
	Detail  string
}

// RoundResult is one service's outcome for a completed scoring round, as the
// Scheduler reports it to its ResultSink. It lives here, beside Result, so the
// scheduler and the scorer can share it without importing each other.
type RoundResult struct {
	ServiceID        int64
	PointsPerFailure int
	Result           Result
	Latency          time.Duration
}

// CheckFunc performs one check attempt. It must return promptly once ctx is
// done. The Scheduler does not depend on that for correctness — it stops
// waiting shortly after the deadline and scores the check as a failure — but
// a CheckFunc that ignores ctx leaks its goroutine for every round it overruns,
// until it eventually returns on its own.
type CheckFunc func(ctx context.Context, host string, params map[string]any) Result

// Registry maps check type names to their implementations. It is safe for
// concurrent use.
type Registry struct {
	mu    sync.RWMutex
	funcs map[string]CheckFunc
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{funcs: make(map[string]CheckFunc)}
}

// Register associates name with fn, replacing any earlier registration.
func (r *Registry) Register(name string, fn CheckFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.funcs[name] = fn
}

// Get returns the check registered under name, and whether there was one.
func (r *Registry) Get(name string) (CheckFunc, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn, ok := r.funcs[name]
	return fn, ok
}

// NewDefaultRegistry returns a Registry with every built-in check (see
// builtins) already registered.
func NewDefaultRegistry() *Registry {
	r := NewRegistry()
	for name, b := range builtins {
		r.Register(name, b.check)
	}
	return r
}

// fail builds a failed Result with a formatted Detail.
func fail(format string, args ...any) Result {
	return Result{Success: false, Detail: fmt.Sprintf(format, args...)}
}

// intParam reads an optional integer param. YAML decoding yields int; int64
// is accepted too for callers that build params by other means. ok reports
// whether the param was present at all.
func intParam(params map[string]any, key string) (n int64, ok bool, err error) {
	v, ok := params[key]
	if !ok {
		return 0, false, nil
	}
	switch n := v.(type) {
	case int:
		return int64(n), true, nil
	case int64:
		return n, true, nil
	default:
		return 0, true, fmt.Errorf("param %q must be an integer, got %T", key, v)
	}
}

// stringParam reads an optional string param. ok reports whether the param
// was present at all.
func stringParam(params map[string]any, key string) (s string, ok bool, err error) {
	v, ok := params[key]
	if !ok {
		return "", false, nil
	}
	s, isString := v.(string)
	if !isString {
		return "", true, fmt.Errorf("param %q must be a string, got %T", key, v)
	}
	return s, true, nil
}

// boolParam reads an optional boolean param. ok reports whether the param
// was present at all.
func boolParam(params map[string]any, key string) (b bool, ok bool, err error) {
	v, ok := params[key]
	if !ok {
		return false, false, nil
	}
	b, isBool := v.(bool)
	if !isBool {
		return false, true, fmt.Errorf("param %q must be a boolean, got %T", key, v)
	}
	return b, true, nil
}

// errMissingRequired builds the standard "missing required param" error.
func errMissingRequired(key string) error {
	return fmt.Errorf("missing required param %q", key)
}

// rejectUnknownParams fails if params has any key not in allowed.
func rejectUnknownParams(params map[string]any, allowed ...string) error {
	var unknown []string
	for key := range params {
		if !slices.Contains(allowed, key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	slices.Sort(unknown)
	quoted := make([]string, len(unknown))
	for i, key := range unknown {
		quoted[i] = fmt.Sprintf("%q", key)
	}
	return fmt.Errorf("unknown param %s (allowed: %s)", strings.Join(quoted, ", "), strings.Join(allowed, ", "))
}

// paramPort reads the required "port" param and checks it is a valid TCP
// port number.
func paramPort(params map[string]any) (int, error) {
	n, ok, err := intParam(params, "port")
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("missing required param %q", "port")
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("param %q must be between 1 and 65535, got %d", "port", n)
	}
	return int(n), nil
}

// checkHost rejects an empty host, which the dialer would otherwise treat as
// the local machine.
func checkHost(host string) error {
	if host == "" {
		return fmt.Errorf("missing host")
	}
	return nil
}
