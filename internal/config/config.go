// Package config loads and validates the scoring engine's config.yaml — the
// hand-edited source of truth for the competition's services, scoring
// parameters, and admin credentials.
//
// Loading is cheap and side-effect free, so callers re-read the file whenever
// they need current values: the engine validates the whole file at startup (it
// must not run on an invalid config) and later re-reads a single service entry
// with LoadService when a service is (re)started or /reload diffs the file.
//
// Any ${VAR} in the file is replaced with that environment variable's value
// before parsing, which is how secrets such as the admin bcrypt hash stay out
// of the file. A missing variable is an error rather than an empty string
// (except under LoadForChecks, which only reports it).
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"slices"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"

	"github.com/Bravo-CDT/Scoring/internal/checks"
)

// ErrServiceNotFound is returned by LoadService, wrapped, when the config
// contains no service with the requested name. Callers use errors.Is to tell
// "this service was removed from config" apart from a parse or validation
// failure, which must not be treated as a deletion.
var ErrServiceNotFound = errors.New("config: service not found")

// knownCheckTypes returns the check types the engine ships, straight from
// internal/checks so the two lists cannot diverge. Validating here means a
// typo'd check_type fails at startup instead of at the first tick.
func knownCheckTypes() []string { return checks.BuiltinTypes() }

// ServiceConfig is one scored service. It has no interval of its own: every
// service is checked once per shared round (Config.RoundIntervalSeconds).
type ServiceConfig struct {
	Name             string         `yaml:"name"`
	Host             string         `yaml:"host"`
	CheckType        string         `yaml:"check_type"`
	PointsPerFailure int            `yaml:"points_per_failure"`
	Params           map[string]any `yaml:"params"`
}

// AdminConfig holds the single administrator's credentials. The password is
// only ever present as a bcrypt hash.
type AdminConfig struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
}

// NetworkConfig names a subnet on the scoreboard's network diagram. It has
// no effect on checks.
type NetworkConfig struct {
	CIDR string `yaml:"cidr"`
	Name string `yaml:"name"`
}

// HostConfig describes a checked machine for the scoreboard's network
// diagram: what it's called and what it runs. It has no effect on checks;
// services are matched to it by their host.
type HostConfig struct {
	Address string `yaml:"address"`
	Name    string `yaml:"name"`
	OS      string `yaml:"os"`
}

// Config is a whole config.yaml.
type Config struct {
	SessionName   string `yaml:"session_name"`
	StartingScore int    `yaml:"starting_score"`
	// RoundIntervalSeconds is the length of one scoring round: every enabled
	// service is checked once per round, all at the same instant. It is read
	// once at startup, so changing it takes a restart; /reload does not
	// apply it.
	RoundIntervalSeconds int             `yaml:"round_interval_seconds"`
	Admin                AdminConfig     `yaml:"admin"`
	Services             []ServiceConfig `yaml:"services"`
	// Networks and Hosts only label the scoreboard's network diagram; both
	// are optional and are applied on /reload.
	Networks []NetworkConfig `yaml:"networks"`
	Hosts    []HostConfig    `yaml:"hosts"`
}

// Hash returns the sha256 hex digest of raw config file bytes, used to
// detect whether the file on disk has changed since it was last loaded.
func Hash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Load reads, expands, parses, and validates the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, nil
}

// LoadService loads path and returns the service entry called name. It returns
// an error wrapping ErrServiceNotFound if no such service is configured, and
// the underlying load error if the file is unreadable or invalid.
func LoadService(path, name string) (*ServiceConfig, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	for i := range cfg.Services {
		if cfg.Services[i].Name == name {
			svc := cfg.Services[i]
			return &svc, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrServiceNotFound, name)
}

// LoadForChecks loads path for running checks by hand (scoringengine check),
// which needs only the services. Unlike Load, an unset ${VAR} is left in
// place as literal text and reported in unsetVars rather than failing, and the
// admin section is not validated, so the scoring box need not hold the admin
// hash just to run checks. Everything else, services included, is validated
// exactly as Load validates it.
func LoadForChecks(path string) (cfg *Config, unsetVars []string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	expanded, unsetVars := expandEnv(raw)
	cfg, err = decode(expanded, false)
	if err != nil {
		return nil, nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, unsetVars, nil
}

// Parse expands, parses, and validates raw config file bytes. Load is
// os.ReadFile plus Parse; callers that already have the bytes in hand (an
// admin's config-editor save, say) call Parse directly.
func Parse(raw []byte) (*Config, error) {
	expanded, missing := expandEnv(raw)
	if len(missing) > 0 {
		return nil, fmt.Errorf("environment variable(s) not set: %v", missing)
	}
	return decode(expanded, true)
}

// decode parses and validates expanded config bytes, validating the admin
// section only if withAdmin is set.
func decode(expanded []byte, withAdmin bool) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	// Reject unrecognized keys: a misspelled field would otherwise be silently
	// ignored, and an optional one would fail quietly by doing nothing at all.
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing yaml: %w", err)
	}
	if err := validate(&cfg, withAdmin); err != nil {
		return nil, err
	}
	normalize(&cfg)
	return &cfg, nil
}

var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces every ${VAR} reference with that environment variable's
// value. An unset variable's reference is left as is, and all of them are
// returned at once so a misconfigured deployment needs one fix-and-retry cycle
// rather than one per secret.
func expandEnv(raw []byte) (expanded []byte, missing []string) {
	expanded = envVarPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		name := string(envVarPattern.FindSubmatch(match)[1])
		val, ok := os.LookupEnv(name)
		if !ok {
			if !slices.Contains(missing, name) {
				missing = append(missing, name)
			}
			return match
		}
		return []byte(val)
	})
	return expanded, missing
}

// validate checks cfg, including its admin section only if withAdmin is set.
func validate(cfg *Config, withAdmin bool) error {
	if cfg.SessionName == "" {
		return errors.New("session_name is required")
	}
	if cfg.StartingScore <= 0 {
		return fmt.Errorf("starting_score must be a positive integer, got %d", cfg.StartingScore)
	}
	if cfg.RoundIntervalSeconds <= 0 {
		return fmt.Errorf("round_interval_seconds must be a positive integer, got %d", cfg.RoundIntervalSeconds)
	}
	if withAdmin {
		if cfg.Admin.Username == "" {
			return errors.New("admin.username is required")
		}
		if _, err := bcrypt.Cost([]byte(cfg.Admin.PasswordHash)); err != nil {
			// Fail now rather than silently rejecting every login later.
			return fmt.Errorf("admin.password_hash is not a valid bcrypt hash: %w", err)
		}
	}
	if err := validateServices(cfg.Services); err != nil {
		return err
	}
	return validateLayout(cfg.Networks, cfg.Hosts)
}

// validateLayout checks the diagram labels: every network a valid CIDR and
// every host an address, with neither listed twice.
func validateLayout(networks []NetworkConfig, hosts []HostConfig) error {
	seenNets := make(map[netip.Prefix]struct{}, len(networks))
	for i, n := range networks {
		prefix, err := netip.ParsePrefix(n.CIDR)
		if err != nil {
			return fmt.Errorf("networks[%d].cidr: %w", i, err)
		}
		if _, dup := seenNets[prefix.Masked()]; dup {
			return fmt.Errorf("duplicate network %q", n.CIDR)
		}
		seenNets[prefix.Masked()] = struct{}{}
	}
	seenHosts := make(map[string]struct{}, len(hosts))
	for i, h := range hosts {
		if h.Address == "" {
			return fmt.Errorf("hosts[%d].address is required", i)
		}
		if _, dup := seenHosts[h.Address]; dup {
			return fmt.Errorf("duplicate host %q", h.Address)
		}
		seenHosts[h.Address] = struct{}{}
	}
	return nil
}

// validateServices checks every service entry: unique names, a host, a known
// check type whose params that check accepts, and a positive penalty.
func validateServices(services []ServiceConfig) error {
	seen := make(map[string]struct{}, len(services))
	for i, svc := range services {
		if svc.Name == "" {
			return fmt.Errorf("services[%d].name is required", i)
		}
		if _, dup := seen[svc.Name]; dup {
			return fmt.Errorf("duplicate service name %q", svc.Name)
		}
		seen[svc.Name] = struct{}{}

		// Beyond here the service has a name, so identify it by name.
		if svc.Host == "" {
			return fmt.Errorf("service %q: host is required", svc.Name)
		}
		if known := knownCheckTypes(); !slices.Contains(known, svc.CheckType) {
			return fmt.Errorf("service %q: unknown check_type %q (known: %v)", svc.Name, svc.CheckType, known)
		}
		// The same parser the check runs with, so a param the check would
		// reject at every tick is refused here instead.
		if err := checks.ValidateParams(svc.CheckType, svc.Params); err != nil {
			return fmt.Errorf("service %q: params: %w", svc.Name, err)
		}
		if svc.PointsPerFailure <= 0 {
			return fmt.Errorf("service %q: points_per_failure must be a positive integer, got %d", svc.Name, svc.PointsPerFailure)
		}
	}
	return nil
}

// normalize fills in representations that spare every consumer a nil check:
// an omitted params block becomes an empty map, so check plugins and the
// config-snapshot JSON see {} instead of null.
func normalize(cfg *Config) {
	for i := range cfg.Services {
		if cfg.Services[i].Params == nil {
			cfg.Services[i].Params = map[string]any{}
		}
	}
}
