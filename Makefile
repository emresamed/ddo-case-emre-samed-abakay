# SahneBilet — common commands. Everything runs in Docker; only `make test`
# needs a local Go toolchain.
COMPOSE ?= docker compose

.PHONY: up down logs verify load rush reset-demo test cover chaos env

env:
	@test -f .env || cp .env.example .env

up: env
	$(COMPOSE) up --build -d
	@echo "web: http://localhost:3000  api: http://localhost:8080"

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f --tail=100

# End-to-end check of every F-x / T-x item; prints a PASS/FAIL table and
# exits non-zero on failure. Brings the stack up if needed.
verify: env
	$(COMPOSE) up --build -d --wait db api web
	$(COMPOSE) --profile verify run --rm --build verify

# k6 seat rush: K6_VUS virtual users (default 2000) race for one showtime.
load: env
	$(COMPOSE) up --build -d --wait db api
	$(COMPOSE) --profile load run --rm k6

# One premiere rush from the simulator (SIM_RUSH_USERS customers, default
# 2000) with a no-double-sale check at the end.
rush: env
	$(COMPOSE) up --build -d --wait db api
	$(COMPOSE) run --rm --build -e SIM_MODE=rush simulator

# Back to the initial seed (same SEED => same data).
reset-demo: env
	$(COMPOSE) up -d --wait db api
	$(COMPOSE) exec api /app/api reset-demo

test:
	cd backend && go test -race -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

cover:
	cd backend && go test -coverprofile=coverage.out ./internal/domain/... && go tool cover -func=coverage.out

# Fault injection: stop the database, watch the API degrade, start it again,
# watch it recover.
chaos: env
	sh scripts/chaos.sh
