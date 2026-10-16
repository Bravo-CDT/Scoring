# Manual testing walkthrough

A hands-on tour of the finished engine on one machine: three local services to
score, the binary, and `curl`. Every block is meant to be pasted, in order,
into **one** bash shell started at the repository root (the blocks share shell
variables and background jobs). Expected output is shown in comments; scores
and ids will differ slightly with timing.

Needs: Go, `python3`, OpenBSD-style `nc` (the Debian/Ubuntu default), `curl`,
`jq`. Uses ports 18080–18084 on 127.0.0.1. Linux `sed -i` syntax (on macOS use
`sed -i ''`).

## 1. Workspace, build, and the services to score

```sh
W=$(mktemp -d)                       # everything lives here; rm -rf it at the end
go build -o "$W/scoringengine" ./cmd/scoringengine

# web: HTTP with a page containing "Welcome"; api: plain HTTP; ssh: a TCP port.
mkdir -p "$W/www" && echo '<h1>Welcome to the blue team</h1>' > "$W/www/index.html"
python3 -m http.server 18081 --bind 127.0.0.1 --directory "$W/www" >"$W/web.log" 2>&1 & WEB_PID=$!
python3 -m http.server 18082 --bind 127.0.0.1 --directory "$W/www" >"$W/api.log" 2>&1 & API_PID=$!
nc -lk 127.0.0.1 18083 </dev/null >/dev/null 2>&1 & SSH_PID=$!
sleep 1
```

## 2. Config and admin password hash

Checks run every 3 seconds; the safety net auto-pauses scoring when 60% of
services fail at once.

```sh
cat > "$W/config.yaml" <<'EOF'
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

# bcrypt hash of the admin password, via the module's own bcrypt package
# (run from the repo root; htpasswd or python's bcrypt module work too).
mkdir -p "$W/hashpw" && cat > "$W/hashpw/main.go" <<'EOF'
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
go run "$W/hashpw/main.go" 'hunter2-admin' > "$W/hash.txt"
```

## 3. Start the engine and some helpers

```sh
start_engine() {
  ADMIN_PASSWORD_HASH="$(cat "$W/hash.txt")" "$W/scoringengine" \
    -config "$W/config.yaml" -db "$W/scoring.db" -addr 127.0.0.1:18080 >>"$W/engine.log" 2>&1 &
  ENGINE_PID=$!
  sleep 1
}
E=http://127.0.0.1:18080
st()  { curl -s $E/status | jq -c '{engine_state, scoring_paused, blue_score, red_score,
          services: [.services[] | {name, up, routine_enabled, scoring_paused}]}'; }
adm() { curl -s -X POST -H "Authorization: Bearer $TOKEN" "$E$1" -w '\n%{stderr}HTTP %{http_code}\n'; }   # body on stdout, HTTP code on stderr
evs() { curl -s "$E/events?limit=${1:-5}" | jq -c '.[] | {type, actor, message}'; }

start_engine
tail -3 "$W/engine.log"   # ... engine is STOPPED and scoring is PAUSED; to begin ...
st
# {"engine_state":"stopped","scoring_paused":true,"blue_score":1000,"red_score":0,"services":[... "up":false,"routine_enabled":false ...]}
```

## 4. Log in, start checking, resume scoring

```sh
TOKEN=$(curl -s -X POST $E/login -d '{"username":"admin","password":"hunter2-admin"}' | jq -r .token)
curl -s -X POST $E/login -d '{"username":"admin","password":"wrong"}' -w ' HTTP %{http_code}\n'
# {"error":"invalid credentials"} HTTP 401

adm /engine/start | jq -c '{engine_state, scoring_paused, failed}'
# {"engine_state":"running","scoring_paused":true,"failed":[]}    <- checks run, scoring still paused
# Each routine's first check is randomly delayed by up to 5s, and "up" is
# false until a service's first check, so give it a moment.
sleep 8; st                          # all three "up":true, blue_score still 1000

adm /scoring/resume >/dev/null
```

## 5. A service goes down

```sh
kill $API_PID; sleep 8
st     # api "up":false; blue_score has dropped by 5 per failed api check
evs 3  # {"type":"service_down","actor":"system","message":"api is down"} ...
```

## 6. Pause scoring: globally, then per service

```sh
adm /scoring/pause >/dev/null; sleep 1
B=$(curl -s $E/status | jq .blue_score); sleep 7
echo "before $B, after $(curl -s $E/status | jq .blue_score)"   # equal: frozen, checks continue

# Pause only api, then lift the global pause: api keeps failing but costs nothing.
adm /services/api/scoring/pause >/dev/null
adm /scoring/resume >/dev/null
B=$(curl -s $E/status | jq .blue_score); sleep 7
echo "before $B, after $(curl -s $E/status | jq .blue_score)"   # still equal
```

## 7. Edit the config: reload and restart

```sh
# Add a dns service with nothing listening, and make web cost 15 per failure.
cat >> "$W/config.yaml" <<'EOF'
  - name: dns
    host: 127.0.0.1
    check_type: tcp_connect
    interval_seconds: 3
    points_per_failure: 1
    params: {port: 18084}
EOF
sed -i 's/points_per_failure: 10/points_per_failure: 15/' "$W/config.yaml"

adm /reload                      # {"added":["dns"],"removed":[],"failed":[]} HTTP 200
adm /services/web/restart | jq -c '{failed}'   # {"failed":[]}  (web now re-read its entry)
sleep 4; st                      # dns listed, "up":false, losing 1 point per check
```

Now a typo. Config is validated as a whole, so the bad entry is refused
everywhere it would be loaded and reported in the response (running routines
are left alone):

```sh
sed -i 's/{port: 18084}/{prot: 18084}/' "$W/config.yaml"
adm /services/dns/restart
# {"error":"engine: dns routine failed to start: ... unknown param \"prot\" (allowed: port)","failed":[{"service":"dns","error":"..."}]} HTTP 500
adm /engine/start | jq -c '{engine_state, failed}'
# {"engine_state":"running","failed":[{"service":"dns","error":"..."}]}  HTTP 200: dns is enabled but not running
adm /reload
# {"added":[],"removed":[],"failed":[],"error":"engine: reloading config: ... unknown param \"prot\" ..."} HTTP 500

sed -i 's/{prot: 18084}/{port: 18084}/' "$W/config.yaml"
adm /services/dns/start | jq -c '{failed}'     # {"failed":[]}
```

## 8. Detail text is admin-only

```sh
curl -s "$E/services/api/results?limit=1" | jq -c '.[]'
# {"id":..,"timestamp":"..","success":false,"latency_ms":0}               <- no detail
curl -s -H "Authorization: Bearer $TOKEN" "$E/services/api/results?limit=1" | jq -c '.[]'
# {..., "detail":"requesting http://127.0.0.1:18082/: ... connection refused"}
curl -s "$E/events?limit=50" | jq -c '[.[] | select(.type=="service_down")][0]'                                   # no detail
curl -s -H "Authorization: Bearer $TOKEN" "$E/events?limit=50" | jq -c '[.[] | select(.type=="service_down")][0]'  # with detail
```

## 9. Mass-failure autopause

api and dns are down: 2 of 4 failing (50%, under the 60% threshold). Killing
web makes it 3 of 4:

```sh
kill $WEB_PID; sleep 8
st     # "scoring_paused":true
evs 10 | grep mass_failure
# {"type":"mass_failure_autopause","actor":"system","message":"3 of 4 checked services failing (threshold 60%); scoring auto-paused"}

# The admin decides it is genuine red-team work and resumes. The trigger is
# edge-based, so the same ongoing failure does not pause again:
adm /scoring/resume >/dev/null; sleep 8
st     # "scoring_paused":false, blue_score falling

# Recover below the threshold, then cross it again: it fires a second time.
python3 -m http.server 18081 --bind 127.0.0.1 --directory "$W/www" >"$W/web.log" 2>&1 & WEB_PID=$!
sleep 7; kill $WEB_PID; sleep 8
st     # "scoring_paused":true again
curl -s "$E/events?limit=200" | jq '[.[] | select(.type=="mass_failure_autopause")] | length'   # 2
```

## 10. Restart the process

```sh
adm /scoring/resume >/dev/null
st                                   # note blue_score, and api "scoring_paused":true
kill $ENGINE_PID; wait $ENGINE_PID   # SIGTERM: graceful shutdown
tail -2 "$W/engine.log"              # ... shutdown complete

start_engine
st
# "engine_state":"stopped","scoring_paused":true, same blue_score/red_score,
# api still "scoring_paused":true, every "routine_enabled":false
evs 2
# {"type":"process_started","actor":"system","message":"scoring engine process started for session \"hand-test\"; ..."}
# {"type":"engine_stopped","actor":"system","message":"engine stopped"}
adm /engine/start                   # {"error":"missing or invalid bearer token"}  HTTP 401: tokens do not survive a restart
```

Log in again and `POST /engine/start` and `/scoring/resume` to carry on.

## 11. Clean up

```sh
kill $ENGINE_PID $SSH_PID 2>/dev/null; wait 2>/dev/null
rm -rf "$W"
```
