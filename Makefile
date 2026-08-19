DATABASE_URL ?= postgres://taskboard:taskboard@localhost:54329/taskboard?sslmode=disable

.PHONY: db-up up server worker seed test test-integration test-concurrency prod-check prod-up prod-down

db-up:
	docker compose up -d db

up:
	docker compose up --build -d

server:
	DATABASE_URL='$(DATABASE_URL)' go run ./cmd/server

worker:
	DATABASE_URL='$(DATABASE_URL)' go run ./cmd/worker -step-delay 15s

seed:
	DATABASE_URL='$(DATABASE_URL)' go run ./cmd/seed

test:
	go test ./...

test-integration: db-up
	TEST_DATABASE_URL='$(DATABASE_URL)' go test -count=1 -p 1 ./...

test-concurrency: db-up
	go run ./cmd/claim-race -database-url '$(DATABASE_URL)' -workers 10 -tasks 250

prod-check:
	docker compose --env-file .env.production -f compose.production.yml config --quiet

prod-up: prod-check
	docker compose --env-file .env.production -f compose.production.yml up -d --build --scale worker=2

prod-down:
	docker compose --env-file .env.production -f compose.production.yml down
