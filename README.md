# Scoring
Scoring engine for our competition

## Running it

Building needs Go and Node.js (20.19+ or 22.12+, with npm): `make build` builds the
frontend in `web/` to `web/dist`, then the engine binary, which serves the
frontend and the API from one port. The binary reads the frontend from disk
at runtime (`-web`, default `./web/dist`), so a plain `go build` works too,
serving only the API until the frontend is built.

```sh
cp config.example.yaml config.yaml   # edit it: hosts, services, admin username
make hash PASSWORD=yourpassword      # put the printed hash in config.yaml's
                                      # admin.password_hash, or export it as
                                      # ADMIN_PASSWORD_HASH instead
ADMIN_PASSWORD_HASH='<hash>' make run
```

To try it out locally before you have real hosts to check:

```sh
make demo-targets
cp config.demo.yaml config.yaml
ADMIN_PASSWORD_HASH='<hash>' make run
```

To run it against the actual lab network:

```sh
cp config.lab.yaml config.yaml   # real hosts/services, with the comp's known
                                  # default service password baked in
make hash PASSWORD=yourpassword
ADMIN_PASSWORD_HASH='<hash>' make run
```

Once it's running, open `http://<host>:8080/` for the public scoreboard.
Log in at `http://<host>:8080/admin` and use its controls to start the
engine and resume scoring (or `POST /login` then `POST /engine/start` and
`POST /scoring/resume` directly). See `config.example.yaml` for the full
config reference.

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
run alongside a running engine. Run it (and the engine) from the repo root:
script paths in the config are relative to the working directory, and
every script must live in `checks/` -- the engine refuses to run one from
anywhere else.

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
database file (e.g. `backups/scoring-20261022T190000Z.db` next to
`scoring.db`), safe to copy elsewhere at any time.

There is no restore button; restoring one is a manual, three-step process:

```sh
# Stop the running engine first (Ctrl-C, or however it's being run).
cp backups/scoring-<timestamp>.db scoring.db   # overwrite the live -db path
ADMIN_PASSWORD_HASH='<hash>' make run          # start it again
```

To work on the frontend, run the engine as above in one terminal and
`make dev` in another: it starts Vite's hot-reloading dev server (it prints
its URL), which proxies API requests to the engine on `127.0.0.1:8080`. If
the engine listens elsewhere, set `API_URL`, e.g.
`API_URL=http://127.0.0.1:9090 make dev`.
