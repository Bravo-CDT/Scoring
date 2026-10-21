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
cp config.lab.yaml config.yaml   # real hosts/services; no secrets in the file itself
make hash PASSWORD=yourpassword
ADMIN_PASSWORD_HASH='<hash>' PG_PASSWORD='...' SMB_PASSWORD='...' SMTP_PASSWORD='...' make run
```

Once it's running, open `http://<host>:8080/` for the public scoreboard.
Log in at `http://<host>:8080/admin` and use its controls to start the
engine and resume scoring (or `POST /login` then `POST /engine/start` and
`POST /scoring/resume` directly). See `config.example.yaml` for the full
config reference.

To work on the frontend, run the engine as above in one terminal and
`make dev` in another: it starts Vite's hot-reloading dev server (it prints
its URL), which proxies API requests to the engine on `127.0.0.1:8080`. If
the engine listens elsewhere, set `API_URL`, e.g.
`API_URL=http://127.0.0.1:9090 make dev`.
