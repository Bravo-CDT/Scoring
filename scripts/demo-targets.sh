#!/usr/bin/env bash
# Starts (or stops) a couple of fake local services to point demo score
# checks at, for trying the engine out before you have real blue-team hosts
# to check. See config.demo.yaml for a config that matches these ports.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
D="$ROOT/.demo-targets"
mkdir -p "$D/www"

save_pid() { echo "$2" >"$D/$1.pid"; }
pid_of()   { [ -f "$D/$1.pid" ] && cat "$D/$1.pid" || true; }
alive()    { local p; p=$(pid_of "$1"); [ -n "$p" ] && kill -0 "$p" 2>/dev/null; }
reap()     { local p; p=$(pid_of "$1"); [ -n "$p" ] && kill "$p" 2>/dev/null || true; rm -f "$D/$1.pid"; }

up() {
  echo '<h1>demo target</h1>' >"$D/www/index.html"

  if alive web; then echo "web already running"; else
    python3 -m http.server 18081 --bind 127.0.0.1 --directory "$D/www" >"$D/web.log" 2>&1 &
    save_pid web $!
    echo "web (http.server:18081) started"
  fi
  if alive api; then echo "api already running"; else
    python3 -m http.server 18082 --bind 127.0.0.1 --directory "$D/www" >"$D/api.log" 2>&1 &
    save_pid api $!
    echo "api (http.server:18082) started"
  fi
  if alive ssh; then echo "ssh already running"; else
    nc -lk 127.0.0.1 18083 </dev/null >/dev/null 2>&1 &
    save_pid ssh $!
    echo "ssh (nc:18083) started"
  fi
}

down() {
  reap web
  reap api
  reap ssh
  rm -rf "$D"
  echo "demo targets stopped"
}

case "${1:-}" in
  up)   up ;;
  down) down ;;
  *) echo "usage: $0 {up|down}" >&2; exit 1 ;;
esac
