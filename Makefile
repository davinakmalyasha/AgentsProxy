# Makefile — D:\MyProject\AgentsProxy (Laragon dev, Go 1.27)
.PHONY: run build test vet tidy fmt docker redis-cli healthz

run:
	go run ./cmd/server

build:
	go build -ldflags="-s -w -X main.Version=$$(git rev-parse --short HEAD 2>NUL || echo dev)" -o bin/server ./cmd/server

# migrate runs embedded migrations against DATABASE_ADMIN_URL (or
# DATABASE_URL) without booting the server. The deploy step for
# serverless, where RUN_MIGRATIONS=false keeps them off the cold path.
migrate:
	go run ./cmd/migrate

test:
	# -p 1: integration tests share one Postgres; parallel package
	# execution races DELETE/INSERT on the same tables across packages
	# ("tuple concurrently updated" SQLSTATE XX000). Serial is correct
	# for a shared-DB suite (see ARCHITECTURE §11.5).
	go test -p 1 ./... -cover

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
