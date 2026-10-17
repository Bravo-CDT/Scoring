.PHONY: help build run hash demo-targets stop-demo-targets clean reset-db

BIN    := bin/scoringengine
CONFIG ?= config.yaml
DB     ?= scoring.db
ADDR   ?= :8080

help:
	@echo "  make build                 build the engine to $(BIN)"
	@echo "  make run                   run it against CONFIG=$(CONFIG) DB=$(DB) ADDR=$(ADDR)"
	@echo "                             (needs ADMIN_PASSWORD_HASH set in the environment)"
	@echo "  make hash PASSWORD=x       print a bcrypt hash of x for admin.password_hash"
	@echo "  make demo-targets          start three fake local services to point demo checks at"
	@echo "  make stop-demo-targets     stop them"
	@echo "  make clean                 remove the built binary and demo-target state"
	@echo "                             (never touches the database)"
	@echo "  make reset-db              DESTROYS the database at DB=$(DB) -- all scores"
	@echo "                             and history, no undo"
	@echo
	@echo "First run: cp config.example.yaml config.yaml (edit it), or"
	@echo "cp config.demo.yaml config.yaml && make demo-targets to try it out locally."

build:
	go build -o $(BIN) ./cmd/scoringengine

run: build
	@test -f "$(CONFIG)" || { echo "no $(CONFIG) -- copy config.example.yaml or config.demo.yaml to $(CONFIG) first"; exit 1; }
	@test -n "$$ADMIN_PASSWORD_HASH" || { echo "ADMIN_PASSWORD_HASH is not set -- see 'make hash'"; exit 1; }
	$(BIN) -config "$(CONFIG)" -db "$(DB)" -addr "$(ADDR)"

hash:
	@test -n "$(PASSWORD)" || { echo "usage: make hash PASSWORD=yourpassword"; exit 1; }
	@go run ./scripts/hashpw "$(PASSWORD)"

demo-targets:
	@scripts/demo-targets.sh up

stop-demo-targets:
	@scripts/demo-targets.sh down

clean:
	@scripts/demo-targets.sh down >/dev/null 2>&1 || true
	rm -rf bin .demo-targets

reset-db:
	rm -f "$(DB)" "$(DB)-wal" "$(DB)-shm"
