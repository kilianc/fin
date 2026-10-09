.DEFAULT_GOAL := build

.PHONY: build
build:
	@mkdir -p bin
	go build -o bin/fin ./cmd/fin

.PHONY: install
install:
	go install ./cmd/fin

.PHONY: test
test:
	go vet ./...
	go test -race ./...

.PHONY: e2e
e2e:
	./scripts/sandbox-e2e.sh
