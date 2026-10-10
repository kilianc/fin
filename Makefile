.DEFAULT_GOAL := build

VERSION ?= dev

# Universal macOS binary at bin/fin (Apple Silicon and Intel, macOS 12+).
.PHONY: build
build:
	./scripts/build.sh $(VERSION)

# Test, build, tag and upload to GitHub Releases: make release VERSION=0.2.0
.PHONY: release
release:
	./scripts/release.sh $(VERSION)

.PHONY: install
install:
	go install ./cmd/fin

.PHONY: test
test:
	go vet ./...
	go test -race ./...

# Regenerate the GitHub Pages setup and try pages from their prompts.
.PHONY: docs
docs:
	go test ./internal/fin -run 'Test(SetupPages|TryPages|PrivacyPage|SitePages)' -update

# Re-render the README gallery from docs/examples/index.html.
.PHONY: gallery
gallery:
	./scripts/gallery.sh

# Rebuild terminal artwork from the approved circular logo (Python 3 + Pillow).
.PHONY: mascot
mascot:
	python3 scripts/generate-mascot.py

.PHONY: e2e
e2e:
	./scripts/sandbox-e2e.sh
