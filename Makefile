.PHONY: $(MAKECMDGOALS)

.DEFAULT_GOAL := help
SHELL := /bin/bash

# Внешний порт приложения (см. docker-compose.yml). Переопределяется:
#   make up GOTCHA_PORT=59081
GOTCHA_PORT ?= 59080
export GOTCHA_PORT

### Docker commands ###

build: ## Build containers
	docker compose build

rebuild: ## ReBuild containers without cache
	docker compose build --no-cache

up: ## Up containers
	docker compose up -d

up-rebuild: ## Up containers with force recreate and build
	docker compose up -d --force-recreate --build

up-alone: ## Up containers from current project and remove others
	docker compose up -d --remove-orphans

down: ## Down containers in current project
	docker compose down

down-all: ## Down containers in current project and others
	docker compose down --remove-orphans

down-v: ## Down containers with data in volumes (полный сброс баз)
	docker compose down -v

restart: ## Restart containers
	docker compose restart

ps: ## Show containers status
	docker compose ps

logs: ## Follow app logs
	docker compose logs -f gotcha

logs-all: ## Follow logs of all containers
	docker compose logs -f

app-connect: ## Shell into the app container
	docker compose exec -i -t gotcha sh

db-connect: ## psql into PostgreSQL
	docker compose exec -i -t postgres psql -U gotcha -d gotcha

ch-connect: ## clickhouse-client into ClickHouse
	docker compose exec -i -t clickhouse clickhouse-client --user gotcha --password gotcha --database gotcha

health: ## Check /healthz of the running app
	@curl -sf http://localhost:$(GOTCHA_PORT)/healthz && echo || (echo "app is down"; exit 1)

open: ## Print the app URL
	@echo "http://localhost:$(GOTCHA_PORT)"

### Go commands ###

run: ## Run the app locally (нужны поднятые postgres+clickhouse: make up)
	go run ./cmd/gotcha

go-build: ## Build the binary into ./gotcha
	go build -o gotcha ./cmd/gotcha

templ: ## Regenerate templ templates (*_templ.go)
	go run github.com/a-h/templ/cmd/templ@$$(go list -m -f '{{.Version}}' github.com/a-h/templ) generate

fmt: ## gofmt all sources
	gofmt -w ./cmd ./internal

vet: ## go vet
	go vet ./...

tidy: ## go mod tidy
	go mod tidy

test: ## Run all tests (нужен docker: интеграционные поднимают контейнеры)
	go test ./... -count=1 -timeout 1800s

test-short: ## Run unit tests only (без docker, быстро)
	go test ./... -short -count=1

test-race: ## Run unit tests with race detector
	go test ./... -short -race -count=1

check: fmt vet test-short ## fmt + vet + быстрые тесты (перед коммитом)

### HELP commands ###

help: ## Show current help message
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' ./Makefile | sort | \
	awk 'BEGIN {FS = ":.*?## "}; {printf "\033[32m%-30s\033[0m %s\n", $$1, $$2}'
