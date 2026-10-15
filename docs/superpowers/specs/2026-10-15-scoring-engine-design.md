# Scoring Engine — Core Design

## Overview

A Go-based scoring engine for a small CCDC-style competition: one blue team
(10 members) defending 10 hosts (5 Linux, 5 Windows), one service per host,
against one red team. The engine polls each service on an interval,
determines pass/fail, and moves points between a single blue score and a
single red score accordingly. Score, check history, and logs persist across
engine restarts.

This is **not** multi-team CCDC scoring (no N independently-scored teams) —
it is a single scoreboard, two sides, zero-sum.

This spec covers the scoring engine's backend only. A web frontend is a
separate, later project and is out of scope here except where its future
needs shape a decision below (noted explicitly).

## Goals

- Score 10 real services deterministically and simply — correctness over
  scale, and no anti-cheat hardening (not a concern for this competition).
- Let an operator change *which* services are scored, *how* they're
  checked, and *how many points* each is worth, by editing a config file —
  never by changing Go code, for the common case of reconfiguring existing
  services.
- Survive a process restart with no data loss and a safe default run
  state.
- Allow an operator to stop/start/restart one misbehaving service's check
  routine without taking down the other nine, and to pause scoring (globally
  or for one service) without losing visibility into service state.
- Shape the data model so two features planned for later — a scavenger
  hunt / flag-capture score source, and hand-graded injects with file
  uploads — can be added without redesigning how score is computed or
  stored.

## Non-goals (this build)

- Frontend/scoreboard UI (future project).
- Full set of protocol-specific check plugins. This build implements the
  plugin framework plus two general-purpose plugins (`tcp_connect`,
  `http`); the remaining plugins (SMB, SMTP, PostgreSQL, FTP, Telnet,
  Openfire/XMPP, CA) are fast-follow additions using the same interface.
- Scavenger hunt / flag submission scoring.
- Injects and their file-upload drop boxes.
- A separate credential-change gate for scorecheck credentials (distinct
  from general admin login).
- Distinct authenticated roles for red vs. blue viewers — see
  [Viewer model](#viewer-model).
- Live config file-watching / hot reload of the whole process. Reload
  granularity is per-service, triggered by an explicit stop+start of that
  service's routine (see [Control plane](#control-plane)).

## Architecture

```
config.yaml ──► Config Loader (validate at startup; re-readable per-service)
                       │
                       ▼
                 Scheduler ──► Check Plugin Registry ──► [tcp_connect] [http] (+ more later)
                       │                                        │
                       │                                   check result (pass/fail, latency, detail)
                       ▼                                        │
                    Scorer ◄─────────────────────────────────────
                       │
                       ▼
              SQLite Persistence (ledger, check results, events, run state)
                       │
                       ▼
             HTTP API: public read-only + admin control (auth required)
```

## Components

- **Config Loader** — parses and validates `config.yaml` into typed
  structs. Called once at process startup (fail fast on any error — the
  process does not start with an invalid config), and on-demand for a
  single service whenever that service's routine is (re)started, so a
  routine always runs against the freshest definition on disk.
- **Check Plugin Registry** — a small, fixed set of check implementations,
  each satisfying one interface: given a service's `params`, return
  pass/fail plus a detail string (latency, error text, or response
  summary). Plugins know nothing about scoring or persistence. Built now:
  `tcp_connect` (banner/connect check), `http` (GET request, status +
  content match).
- **Scheduler** — owns one controllable routine per configured service.
  Each routine can be started, stopped, or restarted independently. A
  running routine ticks on its own `interval_seconds` and invokes its
  check plugin. All routines are gated by the global `engine_state`: when
  `stopped`, no routine runs regardless of its individual state.
- **Scorer** — the only component that knows scoring rules. On every check
  result: if the result is a failure, and neither the global
  `scoring_paused` flag nor that service's own `scoring_paused` flag is
  set, it writes a ledger entry transferring `points_per_failure` from
  blue to red (points moved are floored so blue's score cannot go below 0,
  and capped so red cannot exceed the competition's starting total).
  Passing results never move points — this is a fixed-points-per-failed-
  interval model, not a transition-based one.
- **Ledger** — a generic, append-only record of every point transfer,
  tagged by `source_type`. Only `service_check` is produced today;
  `flag_submission` and `inject` are reserved values for later score
  sources. Score is **never** stored as a standalone mutable value — blue
  and red score are always computed as `SUM(ledger_entries)`, so the score
  can never drift from its own history and is trivially auditable.
- **Persistence (SQLite)** — durable store for check results, the ledger,
  the structured event log, and run state. See [Data model](#data-model).
- **Auth** — a single admin account (username + bcrypt password hash,
  defined in config, never plaintext). Login issues an in-memory bearer
  token; tokens are invalidated on restart. Protects every state-changing
  endpoint; read-only endpoints stay open.
- **HTTP API** — see [HTTP API](#http-api).

## Viewer model

There is exactly one authenticated role (admin) and one unauthenticated
role (public). Public/unauthenticated access — including the future
frontend's default view — is always treated as the blue-team perspective;
there is no separate authenticated red-team viewer role to design around
in this build or the next. If that changes later, it's a new role added
on top of this, not a rework of it.

## Scoring semantics

- `starting_score` (config, e.g. `1000`) is the total point pool. Blue
  starts holding all of it; red starts at 0.
- Every failed check, for a service whose scoring is not paused (globally
  or individually), transfers that service's `points_per_failure` from
  blue to red. This happens on every failed interval the service is down,
  not just on the down-transition.
- Transfers are floored/capped: if blue's current score is less than
  `points_per_failure`, only the remaining balance is transferred (the
  `points` value recorded on that ledger entry is the actual amount
  moved, never the nominal rate) — blue never goes below 0, red never
  exceeds `starting_score`.
- Score is derived, never stored directly. In general:
  `blue_score = starting_score - SUM(points where from_side='blue') +
  SUM(points where to_side='blue')`, and symmetrically for red. This
  build only ever produces blue-to-red entries, so it simplifies to
  `blue_score = starting_score - SUM(points)` and `red_score =
  SUM(points)` — but the general formula is what future score sources
  (flags moving points either direction, injects awarded to either side)
  plug into without any change to how score is computed.

## Control plane

Two independent global switches, plus per-service controls:

- **`engine_state`**: `stopped` | `running`. The hard switch for "the
  competition is not in session." When `stopped`, no check routine runs at
  all, overriding any per-routine state. This is a soft, in-process state
  (not killing the OS process) so the admin API remains reachable to turn
  it back on.
- **`scoring_paused`** (global, bool): meaningful only while
  `engine_state=running`. Routines keep ticking and check results keep
  being recorded (so service state stays visible), but the Scorer writes
  no ledger entries while this is true. Used for disputes/manual review
  windows.
- **Per-service `scoring_paused`** (bool, runtime-only): same effect as
  the global flag, scoped to one service. Intended for the window around
  fixing a misconfigured check — stop the routine, fix `config.yaml`,
  restart the routine, but keep that one service's scoring paused until
  it's confirmed healthy again, so a fix-in-progress never costs blue
  team points.
- **Per-service routine enabled/disabled** (runtime-only): start/stop/
  restart one service's check routine independently of all others.
  Restarting a routine always re-reads that service's entry from
  `config.yaml` fresh — this is how a config change (new port, new
  expected content, new point value) takes effect without restarting the
  whole engine.

**Restart safety default:** on every process start, `engine_state` is
force-set to `stopped` and `scoring_paused` to `true`, regardless of what
was persisted before the restart. An admin must explicitly start the
engine and resume scoring. Per-service routine-enabled and per-service
scoring-paused flags are runtime-only and always come back as "enabled /
not paused" on a fresh process start — a per-service stop or pause is
treated as a live, in-session fix, not a durable decision.

## Data model

### Config (`config.yaml`)

The only hand-edited source of truth for services, scoring parameters, and
admin credentials:

```yaml
starting_score: 1000

admin:
  username: admin
  password_hash: ${ADMIN_PASSWORD_HASH}   # bcrypt hash, injected via env var

services:
  - name: apache-web
    host: 10.0.1.10
    check_type: http
    interval_seconds: 60
    points_per_failure: 10
    params:
      port: 80
      path: /
      expect_status: 200
      expect_contains: "Welcome"

  - name: telnet-win
    host: 10.0.2.15
    check_type: tcp_connect
    interval_seconds: 60
    points_per_failure: 10
    params:
      port: 23
```

Validation at load time: unique service names, known `check_type`,
required params present for that check type, `interval_seconds` and
`points_per_failure` positive integers.

### SQLite tables

- **`check_results`** — `id, service_name, timestamp, success, latency_ms,
  detail`. Every check attempt, pass or fail. `detail` carries the full
  error/output text and backs the future "click a machine to see why it's
  down" view.
- **`ledger_entries`** — `id, timestamp, source_type, source_ref,
  from_side, to_side, points, reason`. Every point transfer, from any
  source. `source_type` is `'service_check'` today; `'flag_submission'`
  and `'inject'` are reserved for later. Score is always `SUM` over this
  table — never a separately stored value.
- **`events`** — `id, timestamp, type, message, detail`. Structured,
  human-readable log of every state-affecting occurrence (`service_down`,
  `service_up`, `engine_stopped`, `engine_started`, `scoring_paused`,
  `scoring_resumed`, `service_scoring_paused`, `routine_stopped`,
  `routine_started`, etc.) — for post-competition review and disputes.
- **`engine_run_state`** — single row: `engine_state, scoring_paused`.
  Persisted, but always overwritten to the safe default
  (`stopped`/`true`) on process startup per [Control
  plane](#control-plane).

No `users` table: the single admin account's username and password hash
live in config.

## HTTP API

**Public, no auth:**

- `GET /status` — current blue/red score, per-service state (up/down,
  last check time, latency).
- `GET /events` — paginated structured event log.
- `GET /services/{name}/results?limit=N` — recent check attempts for one
  service, including full `detail` text.

**Admin, requires `Authorization: Bearer <token>` from `/login`:**

- `POST /login` — `{username, password}` → `{token}`.
- `POST /engine/start`, `POST /engine/stop`.
- `POST /scoring/pause`, `POST /scoring/resume` — global.
- `POST /services/{name}/start`, `/stop`, `/restart` — per-service
  routine control; start/restart always reloads that service's config
  entry fresh.
- `POST /services/{name}/scoring/pause`, `/scoring/resume` — per-service
  scoring pause, independent of routine run state.

All start/stop/pause/resume actions are idempotent: acting on an
already-running/already-stopped/already-paused/already-resumed target
just returns current state as a 200. Unknown service name → 404. Missing
or invalid token on an admin route → 401. Invalid request body → 400.

## Error handling

- **Startup config errors** (bad YAML, unknown `check_type`, duplicate
  service name, missing required field) — fail fast, process exits with a
  clear message.
- **Single-service reload errors** (triggered by `/services/{name}/start`
  or `/restart`) — only that call fails, with the reason in the response;
  the routine stays stopped; the rest of the engine is unaffected.
- **Check failures vs. plugin errors** — not distinguished at the scoring
  layer. Timeout, connection refused, bad banner, wrong content — all
  count as a failed check; the *why* lives in `check_results.detail`, not
  in separate scoring logic. Every check runs under a timeout so a hung
  check can't block its own goroutine forever; other services are
  unaffected regardless, since each service has its own goroutine.
- **Persistence write failures** (e.g., disk full) — logged as an
  error-level event; that interval's write is skipped and the engine
  keeps running rather than crashing over one bad write.
- **API errors** — `400` invalid body, `401` missing/bad token on an admin
  route, `404` unknown service name, `500` unexpected.

## Testing strategy

- **Check plugins** — unit tests against real local listeners/servers
  (`httptest.Server` for `http`, `net.Listen` for `tcp_connect`): pass
  path, wrong-content/status, closed port, timeout.
- **Scheduler** — start/stop/restart lifecycle per service; a stopped
  routine never ticks; a restart picks up a changed temp config file
  (proves reload-via-restart).
- **Scorer** — table-driven tests over sequences of check results ×
  global/per-service pause combinations, asserting correct ledger entries
  and the floor-at-0 / cap-at-starting-total behavior.
- **Persistence** — round-trip tests for all four tables; score correctly
  derived as `SUM(ledger_entries)`; `engine_run_state` always loads as
  stopped/paused regardless of what was last written.
- **API** — handler tests for every endpoint: auth enforcement,
  idempotency of repeated calls, 404s, full login→admin-action flow.
- **End-to-end smoke test** — real engine process, temp config + temp
  SQLite file, a couple of fake local targets; drive it through check
  failures → score drop → pause → no further drop → stop → no checks run
  → simulated restart → confirms safe-default run state.

## Future work (explicitly designed around, not built here)

- Remaining protocol-specific check plugins: SMB, SMTP, PostgreSQL, FTP,
  Telnet, Openfire/XMPP, CA.
- Scavenger hunt / flag-capture scoring: a new `source_type` in
  `ledger_entries` (e.g. `'flag_submission'`), large point transfers,
  either side submitting against the other. Fits the existing ledger
  without schema changes to scoring itself.
- Injects: hand-graded tasks with file-upload drop boxes (PDF, text,
  screenshots). Grading produces a `source_type='inject'` ledger entry;
  file storage is a separate concern (filesystem or blob store) not
  designed here.
- A separate password/credential gate specifically for changing a
  scorecheck's credentials, distinct from general admin login.
- Possibly moving config storage from a flat file into the database, and
  editing it from a future web UI — the config schema and the
  per-service-reload-on-restart mechanism are designed so this swap
  doesn't change how scoring or checking work, only where the config
  bytes come from.
