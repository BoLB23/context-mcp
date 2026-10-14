.PHONY: build run test validate

build:
	go build -o bin/contextd ./cmd/contextd

run:
	go run ./cmd/contextd

test:
	go test ./...

validate:
	go run ./cmd/contextd -check
