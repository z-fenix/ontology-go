GO ?= go
COMPOSE ?= docker compose -f docker-compose.dev.yaml
TEST_DSN ?= postgres://onto:onto@localhost:5432/ontology?sslmode=disable

.PHONY: tidy fmt vet test test-integration build validate-example ddl-example lint dev-up dev-down dev-reset run-oms run-action publish-example run-action-fga fga-init fga-sync-once run-indexer run-query run-mcp sdk-example test-runtimes run-py-functions

tidy:
	$(GO) mod tidy

fmt:
	gofmt -w ./cmd ./internal

vet:
	$(GO) vet ./...
	$(GO) vet -tags integration ./...

test:
	$(GO) test ./... -cover

# Needs `make dev-up`. WARNING: drops and recreates schema "obj" in TEST_DSN.
# -p 1: the packages share one database and each resets its schemas, so they must not run
# concurrently. Without it, parallel CREATE TABLE on the same names races and fails with
# "duplicate key value violates unique constraint pg_type_typname_nsp_index". CI passes -p 1 too.
test-integration:
	ONTO_TEST_DSN='$(TEST_DSN)' ONTO_TEST_TEMPORAL=$${ONTO_TEST_TEMPORAL-localhost:7233} $(GO) test -tags integration -count=1 -p 1 ./internal/objstore/... ./internal/oms/... ./internal/action/... ./internal/indexer/... ./internal/authz/... ./internal/e2e/...

run-oms: build
	ONTO_DB_DSN='$(TEST_DSN)' ONTO_AUTH_DEV_HEADERS=true ./bin/oms

# Dev only: authorization disabled. Use ONTO_FGA_URL/ONTO_FGA_STORE_ID for real checks.
run-action: build
	ONTO_DB_DSN='$(TEST_DSN)' ONTO_OMS_URL=http://localhost:8080 ONTO_AUTHZ_ALLOW_ALL=true ONTO_AUTH_DEV_HEADERS=true \
	ONTO_TEMPORAL_HOSTPORT=localhost:7233 ONTO_WEBHOOK_ALLOWED_HOSTS=localhost,mro.internal ./bin/action-service --role=all

build:
	$(GO) build -trimpath -o bin/ontoctl ./cmd/ontoctl
	$(GO) build -trimpath -o bin/oms ./cmd/oms
	$(GO) build -trimpath -o bin/action-service ./cmd/action-service
	$(GO) build -trimpath -o bin/indexer ./cmd/indexer
	$(GO) build -trimpath -o bin/query-service ./cmd/query-service
	$(GO) build -trimpath -o bin/fga-sync ./cmd/fga-sync
	$(GO) build -trimpath -o bin/mcp-server ./cmd/mcp-server

validate-example: build
	./bin/ontoctl validate examples/ontology

ddl-example: build
	./bin/ontoctl ddl examples/ontology

lint:
	golangci-lint run ./...

dev-up:
	$(COMPOSE) up -d --wait postgres openfga temporal temporal-ui mock-mro
	$(COMPOSE) run --rm migrate
	$(MAKE) fga-init

# Idempotent: creates the OpenFGA store, uploads deploy/openfga/model.json if it changed, seeds dev
# tuples, and writes the IDs to .env.fga (source it, or let the run-* targets include it).
fga-init: build
	./bin/ontoctl fga init examples/ontology --fga-url http://localhost:8090 --seed deploy/openfga/dev-tuples.json > .env.fga
	@cat .env.fga

dev-down:
	$(COMPOSE) down

dev-reset:
	$(COMPOSE) down -v

# Publish the example ontology to a local OMS (make run-oms first).
publish-example: build
	./bin/ontoctl publish examples/ontology --oms http://localhost:8080 --git-commit $$(git rev-parse HEAD 2>/dev/null || echo dev) --actor dev@local

# One sync round against the dev database (source tables in schema public).
run-indexer: build
	ONTO_DB_DSN='$(TEST_DSN)' ONTO_SOURCE_MRO_DB_DSN='$(TEST_DSN)' ONTO_OMS_URL=http://localhost:8080 ./bin/indexer --once

# Dev only: authorization disabled, dev identity headers. Example:
#   curl -s -X POST localhost:8082/api/v1/ontologies/mro/objects/WorkOrder/search -H 'X-Actor: alice' -d '{"pageSize":10}'
run-query: build
	ONTO_DB_DSN='$(TEST_DSN)' ONTO_OMS_URL=http://localhost:8080 ONTO_AUTHZ_ALLOW_ALL=true ONTO_AUTH_DEV_HEADERS=true \
	ONTO_CLEARANCE_GROUPS=finance=CONFIDENTIAL ./bin/query-service

# Full OpenFGA reconcile against the dev stack (store id from `fga store create`).
fga-sync-once: build
	. ./.env.fga && ONTO_DB_DSN='$(TEST_DSN)' ONTO_OMS_URL=http://localhost:8080 ./bin/fga-sync --once

# Action service with real OpenFGA checks (needs `make dev-up` and `make fga-sync-once` first).
run-action-fga: build
	. ./.env.fga && ONTO_DB_DSN='$(TEST_DSN)' ONTO_OMS_URL=http://localhost:8080 ONTO_AUTH_DEV_HEADERS=true \
	ONTO_TEMPORAL_HOSTPORT=localhost:7233 ONTO_WEBHOOK_ALLOWED_HOSTS=localhost,mro.internal ./bin/action-service --role=all

# MCP endpoint for agents at http://127.0.0.1:8083/mcp (needs run-oms, run-query, run-action).
run-mcp: build
	ONTO_OMS_URL=http://localhost:8080 ONTO_AUTH_DEV_HEADERS=true \
	ONTO_MCP_CONFIRM_SECRET=$$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n') ./bin/mcp-server

# Generate the TypeScript SDK for the example ontology and type-check it (needs Node).
sdk-example: build
	./bin/ontoctl sdk generate examples/ontology --out examples/sdk/mro.ts
	npx --yes -p typescript@5.6 tsc -p examples/sdk/tsconfig.json

test-runtimes:
	python3 runtimes/python/test_runtime.py
	node --experimental-strip-types --test runtimes/typescript/runtime.test.ts

run-py-functions:
	python3 runtimes/python/example_functions.py
