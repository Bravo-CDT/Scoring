#!/usr/bin/env bash
# Driver for docs/manual-testing.md. Each function below is one section of
# that walkthrough; `make <target>` in the repo root calls one function here.
# State (pids, token, hash, config) lives in .manual-test/ so steps can be run
# as separate `make` invocations, in order, the same way the doc's shell
# blocks are meant to be pasted one after another.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
W="$ROOT/.manual-test"
BIN="$W/scoringengine"
E="http://127.0.0.1:18080"
ADMIN_PASSWORD="hunter2-admin"

mkdir -p "$W"

save_pid() { echo "$2" >"$W/$1.pid"; }
pid_of()   { [ -f "$W/$1.pid" ] && cat "$W/$1.pid" || true; }
alive()    { local p; p=$(pid_of "$1"); [ -n "$p" ] && kill -0 "$p" 2>/dev/null; }
reap()     { local p; p=$(pid_of "$1"); [ -n "$p" ] && kill "$p" 2>/dev/null || true; rm -f "$W/$1.pid"; }

token()    { cat "$W/token.txt" 2>/dev/null || true; }

wait_up() {
  for _ in $(seq 1 20); do
    curl -s -o /dev/null "$E/status" && return 0
    sleep 0.5
  done
  echo "engine never came up on $E; check $W/engine.log" >&2
  return 1
}

st() {
  curl -s "$E/status" | jq -c '{engine_state, scoring_paused, blue_score, red_score,
    services: [.services[] | {name, up, routine_enabled, scoring_paused}]}'
}

evs() {
  curl -s "$E/events?limit=${1:-5}" | jq -c '.[] | {type, actor, message}'
}

adm() {
  curl -s -X POST -H "Authorization: Bearer $(token)" "$E$1" -w '\nHTTP %{http_code}\n'
}

require_running() {
  if ! alive engine; then
    echo "engine is not running; run 'make boot' (and 'make up'/'make config' before it) first." >&2
    exit 1
  fi
}

# --- section 1: workspace, build, and the services to score ----------------
cmd_up() {
  echo "==> building scoringengine"
  go build -o "$BIN" ./cmd/scoringengine

  mkdir -p "$W/www"
  echo '<h1>Welcome to the blue team</h1>' >"$W/www/index.html"

  if alive web; then
    echo "web already running"
  else
    python3 -m http.server 18081 --bind 127.0.0.1 --directory "$W/www" >"$W/web.log" 2>&1 &
    save_pid web $!
    echo "web (http.server:18081) started: pid $(pid_of web)"
  fi

  if alive api; then
    echo "api already running"
  else
    python3 -m http.server 18082 --bind 127.0.0.1 --directory "$W/www" >"$W/api.log" 2>&1 &
    save_pid api $!
    echo "api (http.server:18082) started: pid $(pid_of api)"
  fi

  if alive ssh; then
    echo "ssh already running"
  else
    nc -lk 127.0.0.1 18083 </dev/null >/dev/null 2>&1 &
    save_pid ssh $!
    echo "ssh (nc:18083) started: pid $(pid_of ssh)"
  fi

  sleep 1
}

# --- section 2: config and admin password hash -----------------------------
cmd_config() {
  cat >"$W/config.yaml" <<'EOF'
session_name: "hand-test"
starting_score: 1000
mass_failure_fraction: 0.6
admin:
  username: admin
  password_hash: ${ADMIN_PASSWORD_HASH}
services:
  - name: web
    host: 127.0.0.1
    check_type: http
    interval_seconds: 3
    points_per_failure: 10
    params: {port: 18081, path: /, expect_status: 200, expect_contains: "Welcome"}
  - name: api
    host: 127.0.0.1
    check_type: http
    interval_seconds: 3
    points_per_failure: 5
    params: {port: 18082}
  - name: ssh
    host: 127.0.0.1
    check_type: tcp_connect
    interval_seconds: 3
    points_per_failure: 20
    params: {port: 18083}
EOF

  mkdir -p "$W/hashpw"
  cat >"$W/hashpw/main.go" <<'EOF'
package main

import (
	"fmt"
	"os"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	h, err := bcrypt.GenerateFromPassword([]byte(os.Args[1]), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(h))
}
EOF
  (cd "$ROOT" && go run "$W/hashpw/main.go" "$ADMIN_PASSWORD") >"$W/hash.txt"
  echo "==> wrote $W/config.yaml and $W/hash.txt"
}

# --- section 3: start the engine --------------------------------------------
cmd_boot() {
  if alive engine; then
    echo "engine already running (pid $(pid_of engine))"
  else
    ADMIN_PASSWORD_HASH="$(cat "$W/hash.txt")" "$BIN" \
      -config "$W/config.yaml" -db "$W/scoring.db" -addr 127.0.0.1:18080 >>"$W/engine.log" 2>&1 &
    save_pid engine $!
    wait_up
    echo "engine started: pid $(pid_of engine)"
  fi
  tail -3 "$W/engine.log"
  st
}

# --- section 4: log in, start checking, resume scoring ----------------------
cmd_login() {
  require_running
  curl -s -X POST "$E/login" -d "{\"username\":\"admin\",\"password\":\"$ADMIN_PASSWORD\"}" \
    | jq -r .token >"$W/token.txt"
  echo "==> token saved to $W/token.txt"
  echo "--- a wrong password, for comparison ---"
  curl -s -X POST "$E/login" -d '{"username":"admin","password":"wrong"}' -w ' HTTP %{http_code}\n'
}

cmd_go() {
  require_running
  adm /engine/start | jq -c '{engine_state, scoring_paused, failed}' 2>/dev/null || adm /engine/start
  echo "waiting for first checks (routines stagger up to 5s)..."
  sleep 8
  st
  adm /scoring/resume >/dev/null
  echo "==> scoring resumed"
  st
}

# --- section 5: a service goes down -----------------------------------------
cmd_kill_api() {
  reap api
  echo "==> api killed; waiting for checks to notice"
  sleep 8
  st
  evs 3
}

# --- section 6: pause scoring globally, then per service --------------------
cmd_pause() {
  require_running
  adm /scoring/pause >/dev/null
  echo "==> scoring paused globally"
  B=$(curl -s "$E/status" | jq .blue_score)
  sleep 7
  echo "before $B, after $(curl -s "$E/status" | jq .blue_score)   # equal: frozen, checks still ran"
}

cmd_pause_api() {
  require_running
  adm /services/api/scoring/pause >/dev/null
  adm /scoring/resume >/dev/null
  echo "==> global scoring resumed, api's own scoring still paused"
  B=$(curl -s "$E/status" | jq .blue_score)
  sleep 7
  echo "before $B, after $(curl -s "$E/status" | jq .blue_score)   # still equal: api losing nothing"
}

# --- section 7: edit the config: reload and restart -------------------------
cmd_reload() {
  require_running
  if ! grep -q 'name: dns' "$W/config.yaml"; then
    cat >>"$W/config.yaml" <<'EOF'
  - name: dns
    host: 127.0.0.1
    check_type: tcp_connect
    interval_seconds: 3
    points_per_failure: 1
    params: {port: 18084}
EOF
  fi
  sed -i 's/points_per_failure: 10/points_per_failure: 15/' "$W/config.yaml"
  echo "==> added dns, repriced web to 15/failure"
  adm /reload
  adm /services/web/restart | jq -c '{failed}' 2>/dev/null || adm /services/web/restart
  sleep 4
  st
}

cmd_break_dns() {
  require_running
  sed -i 's/{port: 18084}/{prot: 18084}/' "$W/config.yaml"
  echo "==> typo'd dns's config (port -> prot)"
  echo "--- restart dns (fails, dns's own routine stops) ---"
  adm /services/dns/restart || true
  echo "--- engine/start (fails only for dns; everything else unaffected) ---"
  adm /engine/start | jq -c '{engine_state, failed}' 2>/dev/null || adm /engine/start
  echo "--- reload (refused whole-config; nothing changes) ---"
  adm /reload || true
}

cmd_fix_dns() {
  sed -i 's/{prot: 18084}/{port: 18084}/' "$W/config.yaml"
  echo "==> fixed the typo"
  adm /services/dns/start | jq -c '{failed}' 2>/dev/null || adm /services/dns/start
}

# --- section 8: detail text is admin-only -----------------------------------
cmd_detail() {
  require_running
  echo "--- public: no detail ---"
  curl -s "$E/services/api/results?limit=1" | jq -c '.[]'
  echo "--- admin: with detail ---"
  curl -s -H "Authorization: Bearer $(token)" "$E/services/api/results?limit=1" | jq -c '.[]'
  echo "--- public event, no detail ---"
  curl -s "$E/events?limit=50" | jq -c '[.[] | select(.type=="service_down")][0]'
  echo "--- admin event, with detail ---"
  curl -s -H "Authorization: Bearer $(token)" "$E/events?limit=50" | jq -c '[.[] | select(.type=="service_down")][0]'
}

# --- section 9: mass-failure autopause --------------------------------------
cmd_mass_failure() {
  require_running
  reap web
  echo "==> web killed too; api/dns were already down -> 3 of 4 failing"
  sleep 8
  st
  evs 10 | grep mass_failure || echo "(no mass_failure event yet; try again in a moment)"

  adm /scoring/resume >/dev/null
  echo "==> admin resumed (edge-triggered: the ongoing failure won't re-pause it)"
  sleep 8
  st

  mkdir -p "$W/www"
  python3 -m http.server 18081 --bind 127.0.0.1 --directory "$W/www" >"$W/web.log" 2>&1 &
  save_pid web $!
  echo "==> web back up; waiting, then killing it again to re-cross the threshold"
  sleep 7
  reap web
  sleep 8
  st
  echo "mass_failure_autopause events so far:"
  curl -s "$E/events?limit=200" | jq '[.[] | select(.type=="mass_failure_autopause")] | length'
}

# --- section 10: restart the process ----------------------------------------
cmd_restart_engine() {
  require_running
  adm /scoring/resume >/dev/null
  st
  echo "==> SIGTERM to the engine (graceful shutdown)"
  P=$(pid_of engine)
  kill "$P"
  wait "$P" 2>/dev/null || true
  rm -f "$W/engine.pid"
  tail -2 "$W/engine.log"

  cmd_boot
  evs 2
  echo "--- the old token no longer works ---"
  adm /engine/start || true
  echo "==> log in again (make login) to carry on"
}

# --- helpers -----------------------------------------------------------------
cmd_status() { st; }
cmd_events() { evs "${1:-5}"; }

cmd_demo() {
  cmd_up
  cmd_config
  cmd_boot
  cmd_login
  cmd_go
  cmd_kill_api
  cmd_pause
  cmd_pause_api
  cmd_reload
  cmd_break_dns
  cmd_fix_dns
  cmd_detail
  cmd_mass_failure
  cmd_restart_engine
  echo
  echo "==> demo done. Engine is running at $E; services/web is down (killed in the"
  echo "    mass-failure step and not restarted). 'make login' again to get a fresh"
  echo "    token, or 'make clean' to tear everything down."
}

# --- section 11: clean up ----------------------------------------------------
cmd_clean() {
  reap engine
  reap web
  reap api
  reap ssh
  rm -rf "$W"
  echo "==> stopped everything and removed $W"
}

cmd="${1:-}"
shift || true
case "$cmd" in
  up)              cmd_up ;;
  config)          cmd_config ;;
  boot)            cmd_boot ;;
  login)           cmd_login ;;
  go)              cmd_go ;;
  status)          cmd_status ;;
  events)          cmd_events "${1:-5}" ;;
  kill-api)        cmd_kill_api ;;
  pause)           cmd_pause ;;
  pause-api)       cmd_pause_api ;;
  reload)          cmd_reload ;;
  break-dns)       cmd_break_dns ;;
  fix-dns)         cmd_fix_dns ;;
  detail)          cmd_detail ;;
  mass-failure)    cmd_mass_failure ;;
  restart-engine)  cmd_restart_engine ;;
  demo)            cmd_demo ;;
  clean)           cmd_clean ;;
  *)
    echo "usage: $0 {up|config|boot|login|go|status|events|kill-api|pause|pause-api|reload|break-dns|fix-dns|detail|mass-failure|restart-engine|demo|clean}" >&2
    exit 1
    ;;
esac
