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
ENV       ?= lab
TF_ENVS   ?= lab dev prod
GCP       := infra/scripts/gcp.sh

# Module names from modules.toml (first column of `xtask list`).
MODULES = $(shell $(XTASK) list | awk '{print $$1}')

.DEFAULT_GOAL := help

.PHONY: help fmt fmt-check vet lint vuln test race arch proptest fuzz-smoke bench verify \
        proto devpg-up devpg-down docker compose-up compose-down \
        tf-fmt tf-validate tf-test policy-test ci \
        gcp-up gcp-plan gcp-pause gcp-resume gcp-down gcp-cost gcp-status gcp-images \
        gcp-lab-up gcp-lab-plan gcp-lab-pause gcp-lab-resume gcp-lab-down gcp-lab-cost gcp-lab-status gcp-lab-images

help: ## list targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

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

tf-validate: ## terraform validate every environment (no backend)
	@set -e; for env in $(TF_ENVS); do \
		terraform -chdir=infra/envs/$$env init -backend=false -input=false >/dev/null; \
		terraform -chdir=infra/envs/$$env validate; \
	done

tf-test: ## offline mock-provider plan tests of every environment (terraform test)
	@set -e; for env in $(TF_ENVS); do \
		terraform -chdir=infra/envs/$$env init -backend=false -input=false >/dev/null; \
		terraform -chdir=infra/envs/$$env test; \
	done

policy-test: ## conftest unit tests for infra/policy
	conftest verify --policy infra/policy

ci: fmt-check vet lint arch race verify ## the fast CI stage locally

# --- GCP lifecycle and cost guardrails (infra/README.md, infra/COSTS.md).
# ENV is lab, dev or prod; the gcp-lab-* targets are ENV=lab. Every target
# needs terraform and an authenticated gcloud; nothing runs automatically.
gcp-up: ## terraform plan + policy check + apply for ENV (lab|dev|prod)
	$(GCP) $(ENV) up

gcp-plan: ## terraform plan + policy check for ENV
	$(GCP) $(ENV) plan

gcp-pause: ## stop what bills hourly in ENV (VM, Cloud SQL, warm instances, tick)
	$(GCP) $(ENV) pause

gcp-resume: ## undo gcp-pause for ENV
	$(GCP) $(ENV) resume

gcp-down: ## terraform destroy ENV (prod also needs CONFIRM=prod)
	$(GCP) $(ENV) down

gcp-cost: ## print what is billable in ENV and the free-tier assumptions
	$(GCP) $(ENV) cost

gcp-status: ## what is running in ENV now
	$(GCP) $(ENV) status

gcp-images: ## build and push every image for ENV, write images.auto.tfvars.json
	infra/scripts/push-images.sh $(ENV)

gcp-lab-up: ## lab: terraform apply (near-zero cost)
	$(GCP) lab up

gcp-lab-plan: ## lab: terraform plan + policy check
	$(GCP) lab plan

gcp-lab-pause: ## lab: stop the VM, pause the tick, close the gateway
	$(GCP) lab pause

gcp-lab-resume: ## lab: start the VM, resume the tick, reopen the gateway
	$(GCP) lab resume

gcp-lab-down: ## lab: terraform destroy
	$(GCP) lab down

gcp-lab-cost: ## lab: what is billable and the free-tier assumptions
	$(GCP) lab cost

gcp-lab-status: ## lab: what is running now
	$(GCP) lab status

gcp-lab-images: ## lab: build and push every image, write images.auto.tfvars.json
	infra/scripts/push-images.sh lab
