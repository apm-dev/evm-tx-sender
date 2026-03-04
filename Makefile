# tx-sender Makefile

.PHONY: run build test vet generate tidy docker-up docker-down

run:
	set -a && source .env && set +a && go run cmd/tx-sender/*

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

generate:
	go generate ./internal/domain/

tidy:
	go mod tidy

docker-up:
	docker compose up --build

docker-down:
	docker compose down
