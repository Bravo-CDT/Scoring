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
make hash PASSWORD=redpassword   # the red team's login, see Injects
ADMIN_PASSWORD_HASH='<admin hash>' BLUE_PASSWORD_HASH='<blue hash>' \
  RED_PASSWORD_HASH='<red hash>' make run
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

The blue team login is read at startup only, so adding the `blue_team`
section takes a restart before the page comes on; removing it and reloading
turns the page off at once. `allowed_users` applies on reload. Without the
section, every check uses the config's
credentials: blue team changes are kept in the database but not applied, and
apply again if the section comes back.

### Injects

Injects are the written tasks handed out during the competition. When the
config has a `blue_team` or `red_team` section, the scoreboard links to
`http://<host>:8080/injects`, where each team logs in with its own login
(`red_team.username` and the password behind `RED_PASSWORD_HASH` for red)
and sees only its own injects.

In the admin console's Injects section, an admin creates an inject for blue
or red with a deadline and uploads its instructions as a PDF. A new inject
starts closed; **Open** lets the team submit, until the deadline or until
it's closed again. Teams can see upcoming injects, but not the PDF until
the inject is first opened; from then on it stays available. The team
downloads the PDF and uploads one file (any
type, up to 50 MB) with a note saying what it's for; uploading again
replaces the earlier submission.

The grey team grades a submission out of 10 in the same section and can
change the grade until it's approved. **Approve** needs the inject closed
or past its deadline, and is final: it moves grade/10 × `inject_points`
(30000 unless the config says otherwise). A blue inject adds the points to
blue; a red inject moves them from blue to red. Grading doesn't need
scoring paused. **Delete** removes an inject and its files and takes back
any points it moved.

Clicking a team's score on the scoreboard shows where its points came from:
services, injects, manual adjustments, and the scavenger hunt. Points per inject show only on the team's own injects
page.

Submissions and PDFs are kept in an `uploads/` directory next to the
database. Backups don't include it, so copy it along with a backup if you
need the files. "Reset for a new round" keeps the injects but closes them and
clears every submission and grade, and closes every scavenger hunt and clears
its claim; with its
backup on, the submission files move into a `-uploads` directory beside the backup (e.g.
`backups/scoring-<timestamp>-uploads/`) instead of being deleted. To restore
that backup, move them back into `uploads/` too. Like the blue team's, the red team login
is read at startup only.

### Scavenger hunt

A scavenger hunt is a code hidden somewhere for a team to find. Teams see
their hunts on the same `/injects` page after logging in, and enter the code
to claim the points.

In the admin console's Scavenger hunt section, an admin creates a hunt for
blue, red, or either team, with a title, a description, the code, a point
value and a deadline. A new hunt starts closed; **Open** lets teams claim it,
until the deadline or until it's closed again. Teams never see the code.
It's matched exactly, including case and spaces.

The first correct claim wins and the hunt is done. A blue claim adds the
points to blue; a red claim moves them from blue to red, up to what blue has.
Five wrong codes in a row lock that team out of claiming for a minute; the
count is kept in memory, so a restart clears it. **Delete** removes a hunt
and takes back any points it moved.

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
`blue_team.password_hash` gets `BLUE_PASSWORD_HASH`, `red_team.password_hash`
`RED_PASSWORD_HASH`).

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
puts blue team credential changes back to the config's defaults, clears
inject submissions and grades and scavenger hunt claims (keeping the injects
and hunts), and restarts round numbering at 1.

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
