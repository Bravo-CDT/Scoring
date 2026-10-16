// Package checks holds the service-check plugins the scoring engine runs
// against competitors' hosts, and the Registry that maps a configured check
// type name to its implementation.
//
// Plugins are deliberately ignorant of configuration, scoring and
// persistence: each one receives a host and its raw params map and reports a
// Result. Malformed params produce a failed Result whose Detail names the
// offending param; a plugin never panics on bad input.
package checks

import (
	"context"
	"fmt"
	"sync"
)

// Result is the outcome of one check attempt.
type Result struct {
	Success bool
	Detail  string
}

// CheckFunc performs one check attempt. It must return promptly once ctx is
// done — callers (the Scheduler) rely on this to bound a hung check at the
// service's interval.
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

// NewDefaultRegistry returns a Registry with the built-in checks
// (tcp_connect and http) already registered.
func NewDefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register("tcp_connect", TCPConnect)
	r.Register("http", HTTP)
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
