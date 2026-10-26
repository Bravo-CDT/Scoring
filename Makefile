.PHONY: help build build-web dev run hash check check-deps test lint demo-targets stop-demo-targets clean reset-db

BIN    := bin/scoringengine
CONFIG ?= config.yaml
DB     ?= scoring.db
ADDR   ?= :8080
WEB    ?= web/dist

# npm writes this on every install, so it is newer than package-lock.json
# exactly when node_modules is up to date with it.
NODE_MODULES := web/node_modules/.package-lock.json

help:
	@echo "Building and running:"
	@echo "  make build                 build the frontend to $(WEB) and the engine to $(BIN)"
	@echo "  make build-web             build just the frontend to $(WEB)"
	@echo "  make run                   build, then run against CONFIG=$(CONFIG) DB=$(DB) ADDR=$(ADDR)"
	@echo "                             (needs ADMIN_PASSWORD_HASH set in the environment)"
	@echo "  make hash PASSWORD=x       print a bcrypt hash of x for ADMIN_PASSWORD_HASH"
	@echo "                             (or BLUE_PASSWORD_HASH, the blue team's login)"
	@echo
	@echo "Checking services:"
	@echo "  make check                 run every check in CONFIG once and print the results"
	@echo "                             (SERVICES=\"a b\" for just those; run from the repo root)"
	@echo "  make check-deps            apt-install the tools the checks/ scripts need"
	@echo "                             (run once per scoring box)"
	@echo
	@echo "Development:"
	@echo "  make dev                   run the frontend's hot-reloading dev server, proxying"
	@echo "                             API requests to an engine started with 'make run'"
	@echo "                             (API_URL=http://host:port if it isn't on :8080)"
	@echo "  make test                  run the Go and frontend test suites"
	@echo "  make lint                  go vet, plus oxlint and a type check on the frontend"
	@echo "  make demo-targets          start three fake local services to point demo checks at"
	@echo "  make stop-demo-targets     stop them"
	@echo
	@echo "Cleaning up:"
	@echo "  make clean                 remove the built binary, frontend build, and"
	@echo "                             demo-target state (never touches the database)"
	@echo "  make reset-db              DESTROYS the database at DB=$(DB) -- all scores"
	@echo "                             and history, no undo"
	@echo
	@echo "CONFIG, DB, ADDR and WEB can be set on any target, e.g. make run CONFIG=config.lab.yaml"
	@echo
	@echo "First run: cp config.example.yaml config.yaml (edit it), or"
	@echo "cp config.demo.yaml config.yaml && make demo-targets to try it out locally."

build: build-web
	go build -o $(BIN) ./cmd/scoringengine

build-web: $(NODE_MODULES)
	cd web && npm run build

$(NODE_MODULES): web/package.json web/package-lock.json
	cd web && npm ci

dev: $(NODE_MODULES)
	@echo "Vite proxies API requests to http://127.0.0.1:8080; in another terminal, start"
	@echo "the engine with 'make run' (or 'make demo-targets' and config.demo.yaml first)."
	cd web && npm run dev

run: build
	@test -f "$(CONFIG)" || { echo "no $(CONFIG) -- copy config.example.yaml or config.demo.yaml to $(CONFIG) first"; exit 1; }
	@test -n "$$ADMIN_PASSWORD_HASH" || { echo "ADMIN_PASSWORD_HASH is not set -- see 'make hash'"; exit 1; }
	$(BIN) -config "$(CONFIG)" -db "$(DB)" -addr "$(ADDR)" -web "$(WEB)"

hash:
	@test -n "$(PASSWORD)" || { echo "usage: make hash PASSWORD=yourpassword"; exit 1; }
	@go run ./scripts/hashpw "$(PASSWORD)"

check:
	go run ./cmd/scoringengine check -config "$(CONFIG)" $(SERVICES)

check-deps:
	sudo apt-get install -y $$(grep -v '^\s*#' checks/apt-packages.txt)

test: $(NODE_MODULES)
	go test ./...
	cd web && npm test

lint: $(NODE_MODULES)
	go vet ./...
	cd web && npm run lint && npx tsc -b

demo-targets:
	@scripts/demo-targets.sh up

stop-demo-targets:
	@scripts/demo-targets.sh down

clean:
	@scripts/demo-targets.sh down >/dev/null 2>&1 || true
	rm -rf bin $(WEB) .demo-targets

reset-db:
	rm -f "$(DB)" "$(DB)-wal" "$(DB)-shm"
