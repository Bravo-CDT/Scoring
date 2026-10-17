.PHONY: help up config boot login go status events kill-api pause pause-api \
        reload break-dns fix-dns detail mass-failure restart-engine demo clean

SCRIPT := scripts/manual-test.sh
N ?= 5

help:
	@echo "Manual testing targets for docs/manual-testing.md (run in this order,"
	@echo "or just 'make demo' to run the whole walkthrough non-interactively):"
	@echo ""
	@echo "  make up             1. build the engine; start fake web/api/ssh services"
	@echo "  make config         2. write config.yaml and the admin password hash"
	@echo "  make boot           3. start the scoring engine process (stopped+paused)"
	@echo "  make login          4a. log in as admin, save the bearer token"
	@echo "  make go             4b. start check routines, then resume scoring"
	@echo "  make kill-api       5. kill the api fake service"
	@echo "  make pause          6a. pause scoring globally, prove the score freezes"
	@echo "  make pause-api      6b. pause only api's scoring, then resume globally"
	@echo "  make reload         7a. add a dns service + reprice web; hot-reload"
	@echo "  make break-dns      7b. introduce a config typo, see it rejected"
	@echo "  make fix-dns        7c. fix the typo, start dns"
	@echo "  make detail         8. show check/event detail is admin-only"
	@echo "  make mass-failure   9. cross the autopause threshold twice"
	@echo "  make restart-engine 10. SIGTERM + restart the process, state survives"
	@echo "  make clean          11. kill everything, remove .manual-test/"
	@echo ""
	@echo "  make status         pretty-print GET /status any time"
	@echo "  make events N=20    pretty-print the last N events (default 5)"
	@echo "  make demo           run steps 1-10 automatically back to back"

up:
	@$(SCRIPT) up

config:
	@$(SCRIPT) config

boot:
	@$(SCRIPT) boot

login:
	@$(SCRIPT) login

go:
	@$(SCRIPT) go

status:
	@$(SCRIPT) status

events:
	@$(SCRIPT) events $(N)

kill-api:
	@$(SCRIPT) kill-api

pause:
	@$(SCRIPT) pause

pause-api:
	@$(SCRIPT) pause-api

reload:
	@$(SCRIPT) reload

break-dns:
	@$(SCRIPT) break-dns

fix-dns:
	@$(SCRIPT) fix-dns

detail:
	@$(SCRIPT) detail

mass-failure:
	@$(SCRIPT) mass-failure

restart-engine:
	@$(SCRIPT) restart-engine

demo:
	@$(SCRIPT) demo

clean:
	@$(SCRIPT) clean
