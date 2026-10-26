# Scoring
Scoring engine for our competition. It checks the blue team's services once
per round, takes points off for every service that's down, and shows the
result on a public scoreboard. An admin console controls the engine during
the competition.

`make help` lists every make target.

## Requirements

- Go (see `go.mod` for the version)
- Node.js 20.19+ or 22.12+, with npm, to build the frontend
- On the scoring box only: the tools the script checks need, installed with
  `make check-deps` (see [Checking services by hand](#checking-services-by-hand))

## Running it

`make build` builds the frontend in `web/` to `web/dist`, then the engine
binary to `bin/scoringengine`, which serves the frontend and the API from one
port. The binary reads the frontend from disk at runtime (`-web`, default
`./web/dist`), so a plain `go build` works too, serving only the API until
the frontend is built.

```sh
cp config.example.yaml config.yaml   # edit it: hosts, services, admin username
make hash PASSWORD=yourpassword      # prints a bcrypt hash
ADMIN_PASSWORD_HASH='<hash>' make run
```

Single-quote the hash: it contains `$` characters. `make run` builds first,
and takes `CONFIG`, `DB` and `ADDR` (defaults `config.yaml`, `scoring.db`,
`:8080`), e.g. `make run CONFIG=config.lab.yaml DB=comp.db`.

To try it out locally before you have real hosts to check:

```sh
make demo-targets                    # fake web/api/ssh services on 127.0.0.1
cp config.demo.yaml config.yaml
ADMIN_PASSWORD_HASH='<hash>' make run
make stop-demo-targets               # when you're done
```

To run it against the actual lab network (from the scoring box, the only
machine that can reach it):

```sh
cp config.lab.yaml config.yaml   # real hosts/services, with the comp's known
                                  # default service password baked in
make hash PASSWORD=adminpassword
make hash PASSWORD=bluepassword  # the blue team's login, see below
ADMIN_PASSWORD_HASH='<admin hash>' BLUE_PASSWORD_HASH='<blue hash>' make run
```

### During the competition

Open `http://<host>:8080/` for the public scoreboard. Log in at
`http://<host>:8080/admin`. The engine always starts stopped with scoring
paused, including after a crash or restart:

- **Start engine** runs the first round of checks immediately and times every
  later round from that moment. Checks run, but nothing costs points yet.
- **Resume scoring** makes failures start costing points. Pausing and
  resuming scoring doesn't change round timing.
- Services can be started, stopped, restarted, and have their scoring paused
  one at a time.
- **Edit config** and **Reload** apply config changes without a restart. A
  reload adds and removes services, restarts running services whose entry
  changed, and applies a changed `starting_score`. `round_interval_seconds`
  and `session_name` are only read at startup.
- The score adjustment form adds or removes points from either team by hand,
  with a reason.
- **Countdown** shows a timer on the scoreboard.
- **Back up now** and **Reset for a new round** are covered under
  [Backups](#backups).

A service with no check results yet shows as "Unregistered" rather than up or
down.

### Blue team credentials

When the config has a `blue_team` section (the lab config does), the
scoreboard links to `http://<host>:8080/credentials`. The blue team logs in
there with `blue_team.username` and the password behind `BLUE_PASSWORD_HASH`,
and sees every check that logs in to something (postgres, smb, smtp, ldap,
openfire, and scripts with `CHECK_USER`/`CHECK_PASSWORD`), with the user it
logs in as. When they rotate a password on a box, they pick the user from
`blue_team.allowed_users` and enter the new password; checks use it from the
next round. Passwords are never shown back.

Their changes are kept in the database, not the config, so they survive
restarts and `config.yaml` keeps the defaults. The admin console's
Credentials section shows what's been changed and resets a service to the
config's user and password, e.g. after its box is rebuilt. "Reset for a new
round" resets them all. For ldap, the user keeps the config's `@domain`:
picking `patrick` binds as `patrick@krusty.krab`.

The blue team login is read at startup only, so adding or removing the
`blue_team` section takes a restart to change the login; `allowed_users`
applies on reload. Without the section, every check uses the config's
credentials: blue team changes are kept in the database but not applied, and
apply again if the section comes back.

Everything the console does is also a plain HTTP API: `POST /login` with
`{"username","password"}` returns a token, sent as
`Authorization: Bearer <token>` on the admin routes (`POST /engine/start`,
`POST /scoring/resume`, and so on; see `internal/api/server.go` for the full
list). Tokens don't survive a restart.

## Configuring it

`config.example.yaml` is the full config reference: every field, and every
check type with its params. The config files in the repo:

| File | What it's for |
|------|---------------|
| `config.example.yaml` | Reference and starting point for a new config |
| `config.demo.yaml` | Points at `make demo-targets` for trying it locally |
| `config.lab.yaml` | The real lab network |

Check types: `tcp_connect`, `http`, `http_exclusive`, `postgres`, `smb`,
`smtp`, `dns`, `ldap`, `openfire`, and `script` (runs an executable from
`checks/`, for anything else). Unknown keys and bad params are rejected at
startup and on reload, naming the service and the param.

The scoreboard draws the services as a network diagram, grouped by host and
by subnet. The optional `hosts:` section gives each host address a name and an
operating system (which picks its logo), and `networks:` names subnets; see
the end of `config.example.yaml`. Neither affects checks.

`${VAR}` anywhere in the config is replaced with that environment variable,
which is how `admin.password_hash` gets `ADMIN_PASSWORD_HASH` (and
`blue_team.password_hash` gets `BLUE_PASSWORD_HASH`).

## Checking services by hand

Some checks (`ca`, `telnet` in the lab config) are scripts in `checks/` that
shell out to tools like `impacket-rpcdump` and `expect`. Install those once
on the scoring box:

```sh
make check-deps                      # apt-installs checks/apt-packages.txt
```

To run every configured check once and print the results, without starting
the engine:

```sh
make check CONFIG=config.lab.yaml                      # every service
make check CONFIG=config.lab.yaml SERVICES="iis telnet" # just these
```

This needs no `ADMIN_PASSWORD_HASH`, touches no database, and is safe to
run alongside a running engine. Because it reads no database, it checks with
the config's credentials, not ones the blue team has changed. Run it (and the engine) from the repo root:
script paths in the config are relative to the working directory, and
every script must live in `checks/` -- the engine refuses to run one from
anywhere else. The built binary does the same thing:
`bin/scoringengine check -config config.lab.yaml [service ...]`.

A script can also be run directly. It takes the host as its only argument
and its settings from `CHECK_*` environment variables; exit status 0 means
up, and the last line it prints is the public result detail:

```sh
CHECK_USER=sandy CHECK_PASSWORD='Patty321!' CHECK_EXPECT_HOSTNAME=kelpdelivery \
  checks/telnet.exp 10.111.2.5
checks/ca_rpc.sh 10.111.2.2
```

## Backups

The admin console has a "Back up now" button (and "Reset for a new round"
takes one by default too), both requiring the engine stopped first. Each
backup is a complete snapshot written to a `backups/` directory next to the
database file (e.g. `backups/scoring-20261022T190000.123Z.db` next to
`scoring.db`), safe to copy elsewhere at any time.

"Reset for a new round" clears scores and history for the current session,
puts blue team credential changes back to the config's defaults, and restarts
round numbering at 1.

There is no restore button; restoring one is a manual, three-step process:

```sh
# Stop the running engine first (Ctrl-C, or however it's being run).
cp backups/scoring-<timestamp>.db scoring.db   # overwrite the live -db path
ADMIN_PASSWORD_HASH='<hash>' make run          # start it again
```

`make reset-db` deletes the database outright (every session, no backup, no
undo).

## Development

```sh
make test     # Go tests, then the frontend's vitest suite
make lint     # go vet, oxlint, and a TypeScript type check
```

To work on the frontend, run the engine as above in one terminal and
`make dev` in another: it starts Vite's hot-reloading dev server (it prints
its URL), which proxies API requests to the engine on `127.0.0.1:8080`. If
the engine listens elsewhere, set `API_URL`, e.g.
`API_URL=http://127.0.0.1:9090 make dev`.

Layout:

| Path | What's there |
|------|--------------|
| `cmd/scoringengine` | The binary: `main.go` (the server) and `check.go` (the `check` command) |
| `internal/config` | Loading and validating the config |
| `internal/checks` | The check types |
| `internal/scheduler` | The round loop that runs checks |
| `internal/engine` | Engine and scoring control: start/stop, reload, reset |
| `internal/scorer`, `internal/store` | The point ledger and the SQLite database |
| `internal/api`, `internal/auth` | The HTTP API and admin login |
| `internal/app`, `internal/e2e` | Wiring everything together, and the end-to-end tests |
| `web/` | The React frontend (scoreboard and admin console) |
| `checks/` | Script checks and the apt packages they need |
| `scripts/` | `make hash` and `make demo-targets` helpers |

A new check type is a function registered by name in `builtins` in
`internal/checks/checks.go`, with a `parse<Type>Params` function that both
config validation and the check use.
