# Developer entry points. CI runs the same commands (.github/workflows).
#
#   make verify          per-module contract for every module in modules.toml
#   make docker SERVICE=engine

GO        ?= go
SERVICE   ?= gateway
FUZZTIME  ?= 60s
BENCHTIME ?= 1s
IMAGE     ?= flowforge/$(SERVICE):dev
COMPOSE   ?= docker compose -f deploy/docker-compose.yml
XTASK     := $(GO) run ./cmd/xtask

# Module names from modules.toml (first column of `xtask list`).
MODULES = $(shell $(XTASK) list | awk '{print $$1}')

.DEFAULT_GOAL := help

.PHONY: help fmt fmt-check vet lint vuln test race arch proptest fuzz-smoke bench verify \
        proto devpg-up devpg-down docker compose-up compose-down \
        tf-fmt tf-validate policy-test ci

help: ## list targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

fmt: ## gofmt -s -w every tracked Go file
	git ls-files -z '*.go' | xargs -0 -r gofmt -s -w

fmt-check: ## fail if any Go file needs gofmt
	@out="$$(git ls-files -z '*.go' | xargs -0 -r gofmt -s -l)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet: ## go vet
	$(GO) vet ./...

lint: ## staticcheck
	$(GO) run honnef.co/go/tools/cmd/staticcheck@latest ./...

vuln: ## govulncheck
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

test: ## unit and conformance tests
	$(GO) test -count=1 ./...

race: ## tests with the race detector
	$(GO) test -race -count=1 ./...

arch: ## architecture / layering rules (spec 24)
	$(GO) test -count=1 ./internal/archtest

proptest: ## property tests at FF_PROPTEST_CASES (default 1000)
	FF_PROPTEST_CASES=$${FF_PROPTEST_CASES:-1000} $(GO) test -count=1 -run 'Prop_' ./...

fuzz-smoke: ## every fuzz target for FUZZTIME (default 60s)
	$(XTASK) fuzz-smoke -fuzztime $(FUZZTIME)

bench: ## benchmarks for every module, recorded under bench/results/
	@set -e; for m in $(MODULES); do \
		echo "== bench-module $$m"; \
		$(XTASK) bench-module -benchtime $(BENCHTIME) $$m; \
	done

verify: ## xtask verify-module for every module listed by xtask list
	@set -e; mods="$(MODULES)"; \
	if [ -z "$$mods" ]; then echo "xtask list returned no modules" >&2; exit 1; fi; \
	for m in $$mods; do \
		echo "== verify-module $$m"; \
		$(XTASK) verify-module $$m; \
	done

proto: ## regenerate gRPC stubs into gen/
	scripts/genproto.sh

devpg-up: ## start a throwaway local Postgres (prints the FF_TEST_PG_DSN export)
	scripts/devpg.sh start

devpg-down: ## stop the local Postgres
	scripts/devpg.sh stop

docker: ## build deploy/Dockerfile for SERVICE (default gateway)
	docker build -f deploy/Dockerfile --build-arg SERVICE=$(SERVICE) -t $(IMAGE) .

compose-up: ## start Postgres 16, Redis 7 and the Pub/Sub emulator
	$(COMPOSE) up -d --wait
	@echo "export FF_TEST_PG_DSN='postgres://postgres:postgres@127.0.0.1:5432/flowforge?sslmode=disable'"
	@echo "export FF_TEST_REDIS_ADDR='127.0.0.1:6379' PUBSUB_EMULATOR_HOST='127.0.0.1:8085'"

compose-down: ## stop the compose stack and delete its volumes
	$(COMPOSE) down -v

tf-fmt: ## terraform fmt check
	terraform fmt -recursive -check infra

tf-validate: ## terraform validate both environments (no backend)
	@set -e; for env in dev prod; do \
		terraform -chdir=infra/envs/$$env init -backend=false -input=false >/dev/null; \
		terraform -chdir=infra/envs/$$env validate; \
	done

policy-test: ## conftest unit tests for infra/policy
	conftest verify --policy infra/policy

ci: fmt-check vet lint arch race verify ## the fast CI stage locally
