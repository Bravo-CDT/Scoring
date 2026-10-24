package checks

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// ScriptDir is the directory every script check's executable must resolve
// into, symlinks followed. A relative ScriptDir resolves against the working
// directory at check time, as -web does, so under `make run` it is the repo's
// top-level checks/ directory. It is a variable only so tests can point it
// elsewhere.
//
// The confinement matters because the admin console can save and live-reload
// the config: without it, anyone holding the admin password could make the
// engine run any program on the scoring box every round.
var ScriptDir = "checks"

const (
	// maxScriptDetail caps the length of a script's Detail line, in bytes.
	maxScriptDetail = 200
	// maxScriptOutput caps how much of a script's combined output is kept;
	// anything after it is read and discarded.
	maxScriptOutput = 64 * 1024
	// scriptWaitDelay is how long Script waits, after the script is killed
	// or exits, for its output pipes to close before giving up on them.
	scriptWaitDelay = time.Second
)

// Script runs an executable from ScriptDir with host as its one argument.
// Exit status 0 passes; anything else fails. The last non-empty line of the
// script's combined stdout and stderr, trimmed and capped at maxScriptDetail
// bytes, is the Detail, pass or fail, so a script writes its own one-line
// diagnosis. That line is public, so a script must never print a credential.
//
// Params:
//   - script (string, required): path to the executable. A relative path
//     resolves against the working directory. Once symlinks are followed it
//     must name an executable file inside ScriptDir.
//   - env (map, optional): extra environment variables, values string, int
//     or bool. Every name must match ^CHECK_[A-Z0-9_]+$ (see scriptEnvName).
//
// Any other param is rejected.
//
// Credentials go in through env, never argv, so they stay out of ps. The
// script's environment is only PATH and HOME, copied from the engine's, plus
// env; nothing else of the engine's environment (ADMIN_PASSWORD_HASH, say)
// reaches it. The script runs in its own process group, and when ctx is done
// the whole group is killed, so children it spawned (expect's telnet, say)
// die with it.
func Script(ctx context.Context, host string, params map[string]any) Result {
	if err := checkHost(host); err != nil {
		return fail("%v", err)
	}
	p, err := parseScriptParams(params)
	if err != nil {
		return fail("%v", err)
	}

	var out cappedBuffer
	cmd := exec.CommandContext(ctx, p.path, host)
	cmd.Env = p.env
	for _, key := range []string{"HOME", "PATH"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// The negative pid names the script's whole process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = scriptWaitDelay

	err = cmd.Run()
	last := lastLine(out.Bytes())
	state := cmd.ProcessState
	switch {
	case state == nil:
		return fail("running %s: %v", p.rel, err)
	case ctx.Err() != nil && !state.Success():
		if last != "" {
			return fail("script timed out: %s", last)
		}
		return fail("script timed out")
	case state.Success():
		if last == "" {
			last = "script exited 0"
		}
		return Result{Success: true, Detail: last}
	case last != "":
		return fail("%s", last)
	case state.ExitCode() >= 0:
		return fail("script exited with status %d", state.ExitCode())
	default:
		return fail("script ended: %s", state)
	}
}

// lastLine returns the last non-empty line of out, trimmed, made valid UTF-8
// and capped at maxScriptDetail bytes.
func lastLine(out []byte) string {
	lines := strings.Split(strings.ToValidUTF8(string(out), "�"), "\n")
	for _, line := range slices.Backward(lines) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > maxScriptDetail {
			cut := maxScriptDetail
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			line = line[:cut]
		}
		return line
	}
	return ""
}

// cappedBuffer keeps the first maxScriptOutput bytes written to it and
// silently discards the rest, so a chatty script can neither exhaust memory
// nor block on a full pipe.
type cappedBuffer struct {
	buf bytes.Buffer
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := maxScriptOutput - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// Bytes returns what was kept.
func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }

// scriptParams is Script's parsed params.
type scriptParams struct {
	path string   // absolute, symlinks resolved
	rel  string   // path relative to ScriptDir, for messages
	env  []string // "K=V", sorted
}

// parseScriptParams parses and validates Script's params. It is the only
// place they are checked, for ValidateParams and the check alike, so the
// ScriptDir confinement and the env restrictions are enforced at config load
// and again before every run.
func parseScriptParams(params map[string]any) (scriptParams, error) {
	if err := rejectUnknownParams(params, "script", "env"); err != nil {
		return scriptParams{}, err
	}
	script, ok, err := stringParam(params, "script")
	if err != nil {
		return scriptParams{}, err
	}
	if !ok || script == "" {
		return scriptParams{}, errMissingRequired("script")
	}
	path, err := resolvePath(script)
	if err != nil {
		return scriptParams{}, fmt.Errorf("param %q: %v", "script", err)
	}
	dir, err := resolvePath(ScriptDir)
	if err != nil {
		return scriptParams{}, fmt.Errorf("param %q: script directory: %v", "script", err)
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return scriptParams{}, fmt.Errorf("param %q: %q must be a file inside the script directory %s", "script", script, ScriptDir)
	}
	info, err := os.Stat(path)
	if err != nil {
		return scriptParams{}, fmt.Errorf("param %q: %v", "script", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return scriptParams{}, fmt.Errorf("param %q: %s must be an executable file", "script", rel)
	}

	env, err := parseScriptEnv(params)
	if err != nil {
		return scriptParams{}, err
	}
	return scriptParams{path: path, rel: rel, env: env}, nil
}

// resolvePath makes path absolute and follows every symlink in it. The path
// must exist.
func resolvePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// scriptEnvName is the allowlist for env names. It is an allowlist, not a
// deny-list, because many ordinary names turn their value into code run by
// the script's interpreter before the script itself: BASH_ENV and
// BASH_FUNC_*%% for bash, ENV for sh, PYTHONSTARTUP, PERL5OPT, NODE_OPTIONS,
// TCLLIBPATH for expect, LD_PRELOAD for the dynamic linker, and so on. With a
// deny-list, a config edit through the admin console could still run any
// code on the scoring box; no interpreter gives a CHECK_ name meaning.
var scriptEnvName = regexp.MustCompile(`^CHECK_[A-Z0-9_]+$`)

// parseScriptEnv reads the optional "env" param as sorted "K=V" pairs. Errors
// name the offending key but never a value, which may be a credential.
func parseScriptEnv(params map[string]any) ([]string, error) {
	v, ok := params["env"]
	if !ok {
		return nil, nil
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		return nil, fmt.Errorf("param %q must be a map, got %T", "env", v)
	}
	env := make([]string, 0, len(m))
	for key, val := range m {
		if !scriptEnvName.MatchString(key) {
			return nil, fmt.Errorf("param %q: variable name %q must match %s", "env", key, scriptEnvName)
		}
		switch val.(type) {
		case string, int, int64, bool:
		default:
			return nil, fmt.Errorf("param %q: %q must be a string, integer or boolean, got %T", "env", key, val)
		}
		s := fmt.Sprint(val)
		if strings.ContainsRune(s, 0) {
			return nil, fmt.Errorf("param %q: %q must not contain a NUL byte", "env", key)
		}
		env = append(env, key+"="+s)
	}
	slices.Sort(env)
	return env, nil
}
