# Scoring
Scoring engine for our competition

## Running it

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

Once it's running: `POST /login`, then `POST /engine/start` and
`POST /scoring/resume` as admin to begin checking and scoring. See
`config.example.yaml` for the full config reference.
