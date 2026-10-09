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

# Regenerate the GitHub Pages setup pages from the agent prompt.
.PHONY: docs
docs:
	go test ./internal/fin -run TestSetupPagesMatchThePrompt -update

# Re-render the README gallery from docs/examples/index.html.
.PHONY: gallery
gallery:
	./scripts/gallery.sh

.PHONY: e2e
e2e:
	./scripts/sandbox-e2e.sh
