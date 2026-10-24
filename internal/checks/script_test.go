package checks

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// useScriptDir points ScriptDir at a fresh temporary directory for the rest of
// the test and returns it. Tests that call it must not run in parallel.
func useScriptDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := ScriptDir
	ScriptDir = dir
	t.Cleanup(func() { ScriptDir = old })
	return dir
}

// writeScript writes an executable #!/bin/sh script named name into dir and
// returns its path.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScript_PassUsesLastLine(t *testing.T) {
	path := writeScript(t, useScriptDir(t), "pass.sh", `printf 'a\nok: all good\n\n'`)
	res := Script(context.Background(), "10.1.2.3", map[string]any{"script": path})
	if !res.Success || res.Detail != "ok: all good" {
		t.Errorf("result = %+v, want success with Detail %q", res, "ok: all good")
	}
}

func TestScript_FailUsesLastLine(t *testing.T) {
	path := writeScript(t, useScriptDir(t), "fail.sh", "echo 'login rejected for sandy' >&2\nexit 1")
	res := Script(context.Background(), "10.1.2.3", map[string]any{"script": path})
	if res.Success || res.Detail != "login rejected for sandy" {
		t.Errorf("result = %+v, want failure with Detail %q", res, "login rejected for sandy")
	}
}

func TestScript_NoOutput(t *testing.T) {
	path := writeScript(t, useScriptDir(t), "quiet.sh", "exit 3")
	res := Script(context.Background(), "10.1.2.3", map[string]any{"script": path})
	if res.Success || !strings.Contains(res.Detail, "status 3") {
		t.Errorf("result = %+v, want failure mentioning status 3", res)
	}
}

func TestScript_GetsHostAndEnvOnly(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD_HASH", "secret")
	path := writeScript(t, useScriptDir(t), "env.sh", `echo "$1 $CHECK_FOO ${ADMIN_PASSWORD_HASH:-unset}"`)
	res := Script(context.Background(), "10.1.2.3", map[string]any{"script": path, "env": map[string]any{"CHECK_FOO": "bar"}})
	if !res.Success || res.Detail != "10.1.2.3 bar unset" {
		t.Errorf("result = %+v, want success with Detail %q", res, "10.1.2.3 bar unset")
	}
}

func TestScript_KillsProcessGroupOnTimeout(t *testing.T) {
	dir := useScriptDir(t)
	pidFile := filepath.Join(dir, "sleep.pid")
	path := writeScript(t, dir, "hang.sh", "sleep 30 &\necho $! > "+pidFile+"\nwait")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := Script(ctx, "10.1.2.3", map[string]any{"script": path})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Script returned after %v, want within 2s of a 200ms deadline", elapsed)
	}
	if res.Success {
		t.Errorf("result = %+v, want failure", res)
	}

	// The backgrounded sleep is in the script's process group, so it must
	// have been killed too, not left running.
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the script's child (pid %d) survived the deadline", pid)
		}
	}
}

func TestScript_CapsDetail(t *testing.T) {
	path := writeScript(t, useScriptDir(t), "long.sh", `printf '%010000d\n' 0`)
	res := Script(context.Background(), "10.1.2.3", map[string]any{"script": path})
	if !res.Success || len(res.Detail) > maxScriptDetail || res.Detail == "" {
		t.Errorf("result = success %v, Detail length %d, want success with a non-empty Detail of at most %d bytes", res.Success, len(res.Detail), maxScriptDetail)
	}
}

func TestScript_Params(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "checks")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := ScriptDir
	ScriptDir = dir
	t.Cleanup(func() { ScriptDir = old })
	t.Chdir(dir)

	ok := writeScript(t, dir, "ok.sh", "exit 0")
	outside := writeScript(t, root, "x.sh", "exit 0")
	if err := os.Symlink(outside, filepath.Join(dir, "link.sh")); err != nil {
		t.Fatal(err)
	}

	valid := []map[string]any{
		{"script": ok},
		{"script": "ok.sh"},
		{"script": ok, "env": map[string]any{"CHECK_USER": "sandy", "CHECK_N": 3, "CHECK_B": true}},
	}
	for _, params := range valid {
		if err := ValidateParams("script", params); err != nil {
			t.Errorf("ValidateParams(%v): unexpected error %v", params, err)
		}
	}

	tests := []struct {
		name    string
		params  map[string]any
		mention string
	}{
		{"missing script", map[string]any{}, `"script"`},
		{"script wrong type", map[string]any{"script": 5}, `"script"`},
		{"relative escape", map[string]any{"script": "../x.sh"}, "inside"},
		{"absolute outside", map[string]any{"script": outside}, "inside"},
		{"symlink outside", map[string]any{"script": filepath.Join(dir, "link.sh")}, "inside"},
		{"the directory itself", map[string]any{"script": dir}, `"script"`},
		{"nonexistent", map[string]any{"script": filepath.Join(dir, "nope.sh")}, `"script"`},
		{"env PATH", map[string]any{"script": ok, "env": map[string]any{"PATH": "x"}}, `"PATH"`},
		{"env HOME", map[string]any{"script": ok, "env": map[string]any{"HOME": "x"}}, `"HOME"`},
		{"env LD_PRELOAD", map[string]any{"script": ok, "env": map[string]any{"LD_PRELOAD": "x"}}, `"LD_PRELOAD"`},
		{"env BASH_ENV", map[string]any{"script": ok, "env": map[string]any{"BASH_ENV": "x"}}, `"BASH_ENV"`},
		{"env BASH_FUNC", map[string]any{"script": ok, "env": map[string]any{"BASH_FUNC_hostname%%": "() { :; }"}}, `"BASH_FUNC_hostname%%"`},
		{"env PYTHONSTARTUP", map[string]any{"script": ok, "env": map[string]any{"PYTHONSTARTUP": "x"}}, `"PYTHONSTARTUP"`},
		{"env lowercase check_", map[string]any{"script": ok, "env": map[string]any{"check_foo": "x"}}, "CHECK_"},
		{"env bare CHECK_", map[string]any{"script": ok, "env": map[string]any{"CHECK_": "x"}}, "CHECK_"},
		{"env not a map", map[string]any{"script": ok, "env": "notamap"}, `"env"`},
		{"env value wrong type", map[string]any{"script": ok, "env": map[string]any{"CHECK_X": 1.5}}, `"CHECK_X"`},
		{"unknown param", map[string]any{"script": ok, "args": "x"}, `"args"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateParams("script", tt.params)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.mention) {
				t.Errorf("error %q should mention %s", err, tt.mention)
			}
			res := Script(context.Background(), "10.1.2.3", tt.params)
			if res.Success || res.Detail != err.Error() {
				t.Errorf("Script result = %+v, want failure with Detail %q", res, err)
			}
		})
	}
}

func TestScript_Deleted(t *testing.T) {
	path := writeScript(t, useScriptDir(t), "gone.sh", "exit 0")
	params := map[string]any{"script": path}
	if err := ValidateParams("script", params); err != nil {
		t.Fatalf("ValidateParams: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	res := Script(context.Background(), "10.1.2.3", params)
	if res.Success || !strings.Contains(res.Detail, "gone.sh") {
		t.Errorf("result = %+v, want failure naming the missing script", res)
	}
}

func TestScript_MissingInterpreter(t *testing.T) {
	path := filepath.Join(useScriptDir(t), "interp.sh")
	if err := os.WriteFile(path, []byte("#!/nonexistent/interp\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := Script(context.Background(), "10.1.2.3", map[string]any{"script": path})
	if res.Success || !strings.Contains(res.Detail, "interpreter not found") {
		t.Errorf("result = %+v, want failure mentioning the missing interpreter", res)
	}
}

func TestScript_RejectsOptionLikeHost(t *testing.T) {
	dir := useScriptDir(t)
	ran := filepath.Join(dir, "ran")
	path := writeScript(t, dir, "ok.sh", "touch "+ran)
	res := Script(context.Background(), "-n/tmp/scoring.db", map[string]any{"script": path})
	if res.Success || res.Detail != `host must not start with "-"` {
		t.Errorf("result = %+v, want failure %q", res, `host must not start with "-"`)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the script ran despite the option-like host")
	}
}
