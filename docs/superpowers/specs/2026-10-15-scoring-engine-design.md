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

**Revision note:** this spec went through an adversarial review after the
first draft. That review found real gaps — no competition/session scoping
(score would mix across dry runs and real rounds), a floor/cap rule that
discarded the data needed to audit or reverse a bad deduction, a
read-then-write race on that same floor/cap, an exploit where a hung check
scores *less* downtime than a cleanly-failed one, and an events/check table
design that couldn't actually answer "why was this machine down" or "who
paused scoring." This revision closes all of those. See inline notes below
for what changed and why.

## Threat model

Red team is explicitly **out of scope** as an adversary against the
scoring engine itself — per the competition's own rules, attacking the
engine (vs. the ten scored services) is not something red team will do,
and is not something this design defends against. Concretely, this means:
no TLS requirement, no login rate-limiting, and no per-request source-IP
logging are built for this competition. This is a deliberate, accepted
tradeoff given the rules, not an oversight — **if this engine is ever run
in a context where that assumption doesn't hold, the Auth section's
"explicitly not hardened against" list below needs to be revisited.**

This does not excuse plain bugs that would cause a crash or data loss
regardless of adversary (e.g. an unprotected shared map panicking under
ordinary concurrent use) — those are fixed below regardless of threat
model.

Separately: the public API still avoids handing red team a *scoring*
advantage (e.g. the exact string a check expects to see, which would let
them deface a service while keeping it "passing"). That's not about
defending the engine — it's about keeping the score honest. See
[Viewer model](#viewer-model) and the public API surface below.

## Goals

- Score 10 real services deterministically and simply — correctness over
  scale.
- Let an operator change *which* services are scored, *how* they're
  checked, and *how many points* each is worth, by editing a config file —
  never by changing Go code. Adding or removing a service takes effect via
  `POST /reload` (see [Control plane](#control-plane)), without stopping
  the competition; changing `starting_score` or admin credentials still
  requires a full restart.
- Survive a process restart with no data loss and a safe default run
  state, and make that state (including "the engine is sitting stopped
  right now") visible, not just loggable.
- Allow an operator to stop/start/restart one misbehaving service's check
  routine without taking down the other nine, and to pause scoring
  (globally or for one service) without losing visibility into service
  state.
- Keep every point movement attributable and reversible: record the
  nominal amount owed alongside whatever was actually transferred, and
  keep enough identity (which service, which session, which check) that a
  dispute can be resolved from stored data instead of memory.
- Shape the data model so two features planned for later — a scavenger
  hunt / flag-capture score source, and hand-graded injects with file
  uploads — can be added without redesigning how score is computed or
  stored. (This spec is honest below about what those features will still
  need beyond this schema — see [Future work](#future-work).)

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
- TLS, login rate-limiting, and source-IP logging — see [Threat
  model](#threat-model).
- Multiple concurrent competition sessions running at once. Sessions (see
  [Data model](#data-model)) exist so history doesn't mix across a dry run
  and the real event, not to support running two competitions in parallel.

## Architecture

```
config.yaml ──► Config Loader (validate at startup; re-readable per-service, and on /reload)
                       │
                       ▼
                 Scheduler ──► Check Plugin Registry ──► [tcp_connect] [http] (+ more later)
                       │                                        │
                       │                                   check result (pass/fail, latency, detail)
                       ▼                                        │
                    Scorer ◄─────────────────────────────────────
                       │             (single serialized writer)
                       ▼
              SQLite Persistence, WAL mode (sessions, services, ledger, check results, events, run state)
                       │
                       ▼
             HTTP API: public read-only (coarse) + admin control (auth required)
```

## Components

- **Config Loader** — parses and validates `config.yaml` into typed
  structs. Called once at process startup (fail fast on any error — the
  process does not start with an invalid config), on-demand for a single
  service whenever that service's routine is (re)started, and on
  `POST /reload` (diffs the full file against currently-known services).
  A routine always runs against the freshest definition on disk.
- **Check Plugin Registry** — a small, fixed set of check implementations,
  each satisfying one interface: given a service's `params` and a
  deadline, return pass/fail plus a detail string (latency, error text, or
  response summary, capped — see [Data model](#data-model)). Plugins know
  nothing about scoring or persistence. Built now: `tcp_connect`
  (banner/connect check), `http` (GET request, status + content match).
- **Scheduler** — owns one controllable routine per configured service.
  Each routine can be started, stopped, or restarted independently, and is
  added/removed by `POST /reload`. **A routine is a self-paced loop, not a
  shared ticker**: run check (bounded by a deadline equal to that
  service's `interval_seconds`, so a hung check is force-failed before the
  next cycle) → record result → sleep for whatever remains of the
  interval → repeat. This guarantees exactly one check per interval per
  service by construction, with no overlapping ticks and no separate
  timeout field to configure. All routines are gated by the global
  `engine_state`: when `stopped`, no routine runs regardless of its
  individual state. Each routine's first check fires immediately on start
  (not after waiting a full interval), and each routine's startup is
  staggered by a small random delay (0–5s) so ten routines starting
  together don't all write at once.
- **Scorer** — the only component that knows scoring rules, and the only
  writer of ledger entries and check-derived events. Runs as a single
  serialized path (one goroutine consuming a channel of check results, or
  an equivalent mutex) so floor/cap arithmetic is never racing itself —
  see [Scoring semantics](#scoring-semantics). On every check result: if
  the result is a failure, and neither the global `scoring_paused` flag
  nor that service's own `scoring_paused` flag is set, it writes a ledger
  entry transferring (up to) `points_per_failure` from blue to red.
  Passing results never move points — this is a fixed-points-per-failed-
  interval model, not a transition-based one, and it's a point-in-time
  snapshot: a service that was down moments before or after a given check
  but passed *at* check time is simply scored as passing for that
  interval. The Scorer also watches for mass failures (see [Mass-failure
  safety net](#mass-failure-safety-net)).
- **Ledger** — a generic, append-only record of every point transfer,
  tagged by `source_type`. Only `service_check` is produced today;
  `flag_submission` and `inject` are reserved values for later score
  sources. Score is **never** stored as a standalone mutable value — blue
  and red score are always computed as a `SUM` over non-voided ledger
  entries, so the score can never drift from its own history and is
  trivially auditable.
- **Persistence (SQLite, WAL mode)** — durable store for sessions,
  services, check results, the ledger, the structured event log, and run
  state. A single writer connection serializes all writes; reads can use
  additional connections. See [Data model](#data-model).
- **Auth** — a single admin account (username + bcrypt password hash,
  defined in config, never plaintext). Login issues an in-memory bearer
  token (crypto/rand, held in a mutex-protected map); tokens are
  invalidated on restart. Protects every state-changing endpoint;
  read-only endpoints stay open. See [Threat model](#threat-model) for
  what this deliberately does not include.
- **HTTP API** — see [HTTP API](#http-api).

## Viewer model

There is exactly one authenticated role (admin) and one unauthenticated
role (public). Public/unauthenticated access — including the future
frontend's default view — is always treated as the blue-team perspective;
there is no separate authenticated red-team viewer role to design around
in this build or the next. If that changes later, it's a new role added
on top of this, not a rework of it.

The public API is intentionally coarser than the admin API: it exposes
pass/fail and timing, but not the raw `detail` text from a check (which
can contain the exact string/credential a check looks for — see
[HTTP API](#http-api)).

## Scoring semantics

- `starting_score` (config, e.g. `1000`) is the total point pool for the
  current session. Blue starts holding all of it; red starts at 0.
- Every failed check, for a service whose scoring is not paused (globally
  or individually), transfers that service's `points_per_failure` from
  blue to red. This happens on every failed interval the service is down,
  not just on the down-transition. A check's result reflects only the
  state at the moment it ran — there's no averaging or sub-interval
  tracking.
- **Floor/cap and the nominal-vs-actual split.** Each ledger entry records
  both `nominal_points` (the service's configured `points_per_failure` at
  the moment of this check — fixed at write time, immune to a later
  config edit) and `points` (the amount actually transferred, after
  floor/cap). If blue's current score is less than `nominal_points`, only
  the remaining balance is transferred; blue never goes below 0, red
  never exceeds `starting_score`. Recording both means a later admin
  correction (voiding an entry — see below) can reconstruct what *should*
  have happened to the entries after it, instead of losing that
  information at write time.
- **Voiding, not deleting.** `ledger_entries` are never deleted or
  mutated once written — `voided` (bool), `voided_at`, and `voided_reason`
  columns exist so a bad entry (e.g. a misconfigured check that
  wrongly dinged blue) can be excluded from the score going forward without
  destroying the audit trail. Score is `SUM(points) WHERE voided = 0`.
  **This build does not ship an admin endpoint to void an entry** — that's
  deliberately deferred (a manual SQL update is an acceptable stopgap for
  a first competition) — but the columns exist now because adding them
  after real competition data exists would mean a migration against rows
  with no way to distinguish "never needed voiding" from "voided before
  the column existed."
- **Concurrency.** All of the above happens on the Scorer's single
  serialized write path (see Components), so there is no read-then-write
  race between two services failing in the same instant — by the time the
  second entry's floor/cap is computed, the first has already committed.
- Score is derived, never stored directly. In general:
  `blue_score = starting_score - SUM(points where from_side='blue' AND NOT voided) +
  SUM(points where to_side='blue' AND NOT voided)`, and symmetrically for
  red. This build only ever produces blue-to-red entries, so it
  simplifies to `blue_score = starting_score - SUM(points)` and
  `red_score = SUM(points)` (both filtered to non-voided rows) — but the
  general formula is what future score sources need to plug into. Note
  for future work: an **inject awarded to a side isn't a transfer at all**
  (there's no losing side) — the `side` enum will need a third value
  (e.g. `pool`) before injects can post entries; this build does not add
  it yet, to avoid guessing at a value nothing produces or consumes.

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
  windows. Setting it while `engine_state=stopped` is allowed and simply
  takes effect the next time the engine starts.
- **Per-service `scoring_paused`** (bool, **persisted**, not runtime-only
  — see below): same effect as the global flag, scoped to one service.
  Intended for the window around fixing a misconfigured check — stop the
  routine, fix `config.yaml`, restart the routine, but keep that one
  service's scoring paused until it's confirmed healthy again, so a
  fix-in-progress never costs blue team points. `POST
  /services/{name}/restart` accepts an optional
  `{"pause_scoring": true}` body to stop the routine, flip this flag, and
  start the routine again as one atomic admin action — so "quarantine
  this check while I fix it" is a single call, not a race between two.
- **Per-service routine enabled/disabled** (runtime-only, resets on
  restart): start/stop/restart one service's check routine independently
  of all others. Restarting a routine always re-reads that service's
  entry from `config.yaml` fresh — this is how a config change (new port,
  new expected content, new point value) takes effect without restarting
  the whole engine.
- **`POST /reload`**: diffs `config.yaml` against the set of known
  services. New entries get a `services` row created and a routine
  started (subject to current `engine_state`); entries no longer present
  are marked inactive (their routine is stopped, their history stays
  queryable, they disappear from `/status`); entries still present are
  left running as-is (use `/services/{name}/restart` to pick up changed
  parameters for an existing service). This is the mechanism for the
  Goals section's promise that adding/removing a service doesn't require
  an outage. Changing `starting_score` or admin credentials is still
  restart-only, and is explicitly scoped out of `/reload`.

**Why per-service scoring-pause persists but routine-enabled doesn't:** a
stopped routine is a live, in-session troubleshooting state — if the
engine restarts, the operator presumably wants everything attempted again.
A paused *score*, though, is a deliberate "don't count this until I say
so" decision, and the whole point of the restart-safety default below is
that deliberate safety decisions should never silently reverse themselves
on an unplanned restart.

**Restart safety default:** on every process start, `engine_state` is
force-set to `stopped` and global `scoring_paused` to `true`, regardless
of what was persisted before the restart. An admin must explicitly start
the engine and resume scoring. Per-service routine-enabled always comes
back "enabled" on a fresh process start. Per-service `scoring_paused`, in
contrast, is loaded from its last persisted value — it does not reset.

## Mass-failure safety net

If the engine's own network connectivity fails (uplink drop, routing
issue, anything on the scoring engine's side rather than any one scored
host), every service fails every interval simultaneously, and the
per-service "don't distinguish cause" design (see [Error
handling](#error-handling)) would otherwise bleed the entire point pool
in minutes for a fault that has nothing to do with red team.

Config carries an optional `mass_failure_fraction` (e.g. `0.5`). If, in a
single pass of the Scheduler's routines, at least that fraction of
**active** services fail, the Scorer auto-sets global `scoring_paused =
true`, persists it (so it survives a restart, same as any other
deliberate pause), and writes a high-severity event explaining why. It
does not stop the routines themselves — checks keep running and stay
visible, exactly like a manual pause — and it does not auto-resume; an
admin must look, confirm it's a real problem (not simultaneous red-team
action across all ten boxes, which is a judgment call either way), and
call `POST /scoring/resume` once satisfied. Leaving `mass_failure_fraction`
unset disables this feature entirely.

The share counts only active services whose routine is running (a stopped
routine's last result is stale); a service with no result yet counts as
not failing. The trigger is edge-based: it fires when the share crosses
the threshold from below, so an admin's resume during the same ongoing
mass failure is not overridden — the share must first drop below the
threshold and cross again. A crossing while scoring is already paused
(a manual pause) is consumed the same way, without an event. A freshly started engine has seen no crossing
yet, so it fires only after observing the share below the threshold.

## Data model

### Config (`config.yaml`)

The only hand-edited source of truth for services, scoring parameters, and
admin credentials:

```yaml
session_name: "fall-2026-ccdc"   # changing this starts a fresh session (see below)
starting_score: 1000
mass_failure_fraction: 0.5        # optional; omit to disable the safety net

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
`points_per_failure` positive integers, `password_hash` present and
parses as a valid bcrypt hash (fail fast rather than silently denying
every login later), `mass_failure_fraction` (if set) between 0 and 1.

### Sessions

A **session** scopes all score/history data so a dry run, a practice
round, and the real competition never mix. `session_name` in config
identifies the active session: at startup, the engine looks up a
`sessions` row with that name. If found, **that row's persisted
`starting_score` governs** — editing `starting_score` in config has no
effect on an existing session, only on a session being created for the
first time. If not found, a new `sessions` row is created using config's
current `starting_score`. Switching to a new name for the real event
(e.g. `dry-run-oct18` → `competition-oct20`) starts a fresh score with
the old session's data left intact and queryable, never deleted. There's
no `POST /sessions` endpoint in this build — the config field is the only
control, consistent with "config governs behavior, restart applies it."

### SQLite tables

- **`sessions`** — `id, name, starting_score, created_at`.
- **`services`** — `id, session_id, name, active, created_at`. Surrogate
  `id` is what every other table references, but a service's identity is
  `(session_id, name)`: config has no other key to match on. Renaming a
  service in config is therefore a remove plus an add — the old name's
  history stays under the old (now inactive) row, the new name starts a
  fresh row, and a service whose scoring was paused comes back unpaused
  under its new name. Removing a service (via `/reload`) sets
  `active=false` but keeps the row and all its history; re-adding the
  same name reactivates that row.
- **`service_config_snapshots`** — `id, service_id, applied_at, host,
  check_type, interval_seconds, points_per_failure, params_json`. Written
  every time a routine (re)starts. Answers "what was this service's
  config at 11:45" precisely — the question a dispute always asks, and
  the one piece of data `config.yaml` itself can't answer once it's been
  edited again.
- **`check_results`** — `id, session_id, service_id, timestamp, success,
  latency_ms, detail`. Every check attempt, pass or fail. `latency_ms` on
  a failure is the elapsed time to the error/timeout, not zero.
  `detail` carries error/output text, **capped at 4KB** (truncated with a
  marker beyond that) and validated as UTF-8 before storage — this backs
  the future "click a machine to see why it's down" view, but only
  through the admin API (see [HTTP API](#http-api)); it is never exposed
  on the public endpoints.
- **`ledger_entries`** — `id, session_id, timestamp, source_type,
  source_ref, from_side, to_side, nominal_points, points, voided,
  voided_at, voided_reason, reason`. Every point transfer, from any
  source. `source_type` is `'service_check'` today; `'flag_submission'`
  and `'inject'` are reserved for later (see [Future
  work](#future-work) on what else those will need). `source_ref` is the
  integer id of the row in the table named by `source_type` (so
  `check_results.id` today) — `NOT NULL`, with a unique index on
  `(session_id, source_type, source_ref)` so the same check result can
  never produce two ledger entries. Score is always `SUM(points) WHERE
  NOT voided` over this table for the active session — never a separately
  stored value.
- **`events`** — `id, session_id, service_id (nullable), level, actor,
  type, message, detail`. Structured, human-readable log of every
  state-affecting occurrence (`service_down`, `service_up`,
  `engine_stopped`, `engine_started`, `scoring_paused`, `scoring_resumed`,
  `service_scoring_paused`, `routine_stopped`, `routine_started`,
  `mass_failure_autopause`, `process_started`, etc.) — for post-
  competition review and disputes. `level` is `info`/`warning`/`error`
  (closes the gap where the Error handling section promised "error-level"
  events the old schema had no column for). `actor` is `'admin'` for any
  admin-triggered action or `'system'` for an automatic one (mass-failure
  auto-pause, startup); with a single shared admin account this can't
  name an individual, but it does answer "did a person do this or did the
  engine do it," which is the actual dispute question. `service_down` /
  `service_up` transition events are derived by comparing each result to
  the **previous row for that `service_id` in `check_results`** (durable,
  not held in memory), so a restart can't cause a duplicate or missing
  transition event.
- **`engine_run_state`** — single row: `engine_state, scoring_paused`.
  Persisted, but always overwritten to the safe default
  (`stopped`/`true`) on process startup per [Control
  plane](#control-plane). (Per-service `scoring_paused` lives on the
  `services` row itself, and — unlike this table — is not overwritten at
  startup.)

Indexes: `check_results(session_id, service_id, timestamp DESC)`,
`ledger_entries(session_id, timestamp)`, `events(session_id, service_id,
timestamp DESC)`, the unique index on `ledger_entries` noted above.

Timestamps are stored as integer Unix-milliseconds UTC throughout, with
`id` as the tiebreaker for same-millisecond ordering — avoids any
ambiguity from timezone or clock-step issues on the competition network.

No `users` table: the single admin account's username and password hash
live in config.

## HTTP API

**Public, no auth:**

- `GET /status` — session name, `starting_score`, current blue/red score,
  global `engine_state` and `scoring_paused`, server time, and per-service
  state: name, up/down, routine enabled, per-service `scoring_paused`,
  last check time, latency, and consecutive-failure count (so "this
  service has never passed since it was last restarted" is visible at a
  glance). Deliberately **no** `detail` text here — see [Viewer
  model](#viewer-model).
- `GET /events?limit=N&before=<id>` — structured event log, newest first,
  default `limit=50`, max `limit=500`, `before` for cursor-style paging.
- `GET /services/{name}/results?limit=N&before=<id>` — recent check
  attempts for one service: timestamp, success, latency — **not**
  `detail`.

**Admin, requires `Authorization: Bearer <token>` from `/login`:**

- `POST /login` — `{username, password}` → `{token}`.
- `POST /engine/start`, `POST /engine/stop`.
- `POST /scoring/pause`, `POST /scoring/resume` — global.
- `POST /services/{name}/start`, `/stop` — per-service routine control.
- `POST /services/{name}/restart` — reloads that service's config entry
  fresh; accepts optional body `{"pause_scoring": true}` to atomically
  pause that service's scoring as part of the restart.
- `POST /services/{name}/scoring/pause`, `/scoring/resume` — per-service
  scoring pause, independent of routine run state. Persisted (see
  [Control plane](#control-plane)).
- `POST /reload` — diff `config.yaml` against known services; starts
  routines for new entries, deactivates removed ones.
- `GET /services/{name}/results?limit=N&before=<id>` *(admin variant)* —
  same as the public endpoint but includes full `detail` text per
  attempt.

All start/stop/pause/resume actions are idempotent: acting on an
already-running/already-stopped/already-paused/already-resumed target
just returns current state as a 200. Unknown service name → 404. Missing
or invalid token on an admin route → 401. Invalid request body → 400.

## Error handling

- **Startup config errors** (bad YAML, unknown `check_type`, duplicate
  service name, missing required field, invalid admin password hash) —
  fail fast, process exits with a clear message.
- **Single-service reload errors** (triggered by `/services/{name}/start`,
  `/restart`, or `/reload` for one entry) — only that service's action
  fails, with the reason in the response; the routine stays stopped; the
  rest of the engine is unaffected.
- **Check failures vs. plugin errors** — not distinguished at the scoring
  layer. Timeout, connection refused, bad banner, wrong content — all
  count as a failed check; the *why* lives in `check_results.detail`, not
  in separate scoring logic. Every check is bounded by its service's
  `interval_seconds` as described in the Scheduler's self-paced loop (see
  [Components](#components)) — no separate timeout config, no overlapping
  ticks by construction.
- **Mass failures** — see [Mass-failure safety net](#mass-failure-safety-net).
- **Persistence write failures** (e.g., disk full) — logged as an
  `error`-level event (see the `events.level` column); that interval's
  write is skipped and the engine keeps running rather than crashing over
  one bad write.
- **Oversized or invalid check output** — `detail` text is read through a
  bounded reader and capped at 4KB before storage (see [Data
  model](#data-model)), so a scored service returning a huge or malformed
  body can't fill the disk or break JSON encoding of the results
  endpoints.
- **API errors** — `400` invalid body, `401` missing/bad token on an admin
  route, `404` unknown service name, `500` unexpected.

## Testing strategy

- **Check plugins** — unit tests against real local listeners/servers
  (`httptest.Server` for `http`, `net.Listen` for `tcp_connect`): pass
  path, wrong-content/status, closed port, a deliberately slow/hanging
  responder to confirm it's force-failed at the deadline rather than
  hanging the routine.
- **Scheduler** — start/stop/restart lifecycle per service; a stopped
  routine never ticks; a restart picks up a changed temp config file
  (proves reload-via-restart); first check fires immediately on start;
  one check per interval with no overlap even when the check itself runs
  right up to the deadline.
- **Scorer** — table-driven tests over sequences of check results ×
  global/per-service pause combinations, asserting correct `nominal_points`
  vs. `points` and the floor-at-0 / cap-at-starting-total behavior,
  **including two services failing in the same instant** to exercise the
  serialized write path (run with `-race`). A voided-entry test confirms
  the score recomputes correctly excluding it.
- **Persistence** — round-trip tests for all tables; score correctly
  derived as `SUM(points) WHERE NOT voided`; `engine_run_state` always
  loads as stopped/paused regardless of what was last written, while a
  persisted per-service `scoring_paused` survives a simulated restart
  unchanged; the unique index rejects a duplicate `(session_id,
  source_type, source_ref)` insert; a concurrent-write stress test
  confirms no `SQLITE_BUSY` is silently swallowed as data loss.
- **API** — handler tests for every endpoint: auth enforcement,
  idempotency of repeated calls, 404s, full login→admin-action flow, and
  that the public results/status endpoints never include `detail` text
  while the admin variant does.
- **End-to-end smoke test** — real engine process, temp config + temp
  SQLite file, a couple of fake local targets; drive it through check
  failures → score drop → pause → no further drop → stop → no checks run
  → simulated restart → confirms safe-default run state, and confirms the
  persisted per-service pause from before the restart is still in effect.
- **Mass-failure test** — fail enough services in one pass to cross
  `mass_failure_fraction` and confirm global scoring auto-pauses and
  persists, without stopping the routines themselves.

## Future work (explicitly designed around, not built here)

- Remaining protocol-specific check plugins: SMB, SMTP, PostgreSQL, FTP,
  Telnet, Openfire/XMPP, CA.
- Scavenger hunt / flag-capture scoring: a new `source_type` in
  `ledger_entries` (e.g. `'flag_submission'`), large point transfers,
  either side submitting against the other. The ledger schema itself
  accommodates this, but it genuinely needs more than a new
  `source_type` value: a per-side (realistically per-user) authenticated
  submitter identity, a `flags`/`submissions` table, and
  replay/dedupe protection so the same flag can't be scored twice. None
  of that exists yet — the single shared admin account is not a stand-in
  for it.
- Injects: hand-graded tasks with file-upload drop boxes (PDF, text,
  screenshots). Grading produces a `source_type='inject'` ledger entry,
  but an inject *awarded* to a side (not taken from the other) needs the
  `side` enum extended with a non-team value (e.g. `pool`) first — see
  [Scoring semantics](#scoring-semantics). Deadlines/time windows, rubrics,
  grader identity, and file storage (filesystem or blob store) are all
  separate work not designed here.
- A separate password/credential gate specifically for changing a
  scorecheck's credentials, distinct from general admin login.
- TLS and login rate-limiting, if this is ever run somewhere the
  threat model in this spec no longer holds.
- Possibly moving config storage from a flat file into the database, and
  editing it from a future web UI — the config schema and the
  per-service-reload-on-restart/`/reload` mechanism are designed so this
  swap doesn't change how scoring or checking work, only where the config
  bytes come from.
