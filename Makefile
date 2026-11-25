GO ?= go
COMPOSE ?= docker compose -f docker-compose.dev.yaml
TEST_DSN ?= postgres://onto:onto@localhost:5432/ontology?sslmode=disable

.PHONY: tidy fmt vet test test-integration build validate-example ddl-example lint dev-up dev-down dev-reset run-oms run-action publish-example

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
test-integration:
	ONTO_TEST_DSN='$(TEST_DSN)' $(GO) test -tags integration -count=1 ./internal/objstore/... ./internal/oms/... ./internal/action/...

run-oms: build
	ONTO_DB_DSN='$(TEST_DSN)' ./bin/oms

# Dev only: authorization disabled. Use ONTO_FGA_URL/ONTO_FGA_STORE_ID for real checks.
run-action: build
	ONTO_DB_DSN='$(TEST_DSN)' ONTO_OMS_URL=http://localhost:8080 ONTO_AUTHZ_ALLOW_ALL=true \
	ONTO_TEMPORAL_HOSTPORT=localhost:7233 ONTO_WEBHOOK_ALLOWED_HOSTS=localhost,mro.internal ./bin/action-service --role=all

build:
	$(GO) build -trimpath -o bin/ontoctl ./cmd/ontoctl
	$(GO) build -trimpath -o bin/oms ./cmd/oms
	$(GO) build -trimpath -o bin/action-service ./cmd/action-service

validate-example: build
	./bin/ontoctl validate examples/ontology

ddl-example: build
	./bin/ontoctl ddl examples/ontology

lint:
	golangci-lint run ./...

dev-up:
	$(COMPOSE) up -d --wait postgres openfga temporal temporal-ui mock-mro
	$(COMPOSE) run --rm migrate

dev-down:
	$(COMPOSE) down

dev-reset:
	$(COMPOSE) down -v

# Publish the example ontology to a local OMS (make run-oms first).
publish-example: build
	./bin/ontoctl publish examples/ontology --oms http://localhost:8080 --git-commit $$(git rev-parse HEAD 2>/dev/null || echo dev) --actor dev@local
