# Makefile — D:\MyProject\AgentsProxy (Laragon dev, Go 1.27)
.PHONY: run build test vet tidy fmt docker redis-cli healthz

run:
	@if [ -f .env ]; then set -a; . ./.env; set +a; fi; go run ./cmd/server

build:
	go build -ldflags="-s -w -X main.Version=$$(git rev-parse --short HEAD 2>NUL || echo dev)" -o bin/server ./cmd/server

# migrate runs embedded migrations against DATABASE_ADMIN_URL (or
# DATABASE_URL) without booting the server. The deploy step for
# serverless, where RUN_MIGRATIONS=false keeps them off the cold path.
migrate:
	@if [ -f .env ]; then set -a; . ./.env; set +a; fi; go run ./cmd/migrate

test:
	# DB-backed tests read TEST_DATABASE_* from .env.test (see
	# .env.test.example). They never read the runtime DATABASE_URL.
	bash scripts/test-local.sh

vet:
	go vet ./...

fmt:
	go fmt ./...

tidy:
	go mod tidy

docker:
	docker build -t agents-proxy .

healthz:
	curl -s http://localhost:8080/healthz | python -m json.tool

redis-cli:
	redis-cli -h localhost -p 6379

# Prod-only docker compose (Railway/VPS) — NOT for Laragon daily dev
compose-up:
	docker compose up --build -d

compose-down:
	docker compose down
