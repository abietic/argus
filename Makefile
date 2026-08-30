.PHONY: build test fmt fmt-check vet docs-check verify clean pi-install pi-format pi-check pi-test pi-build pi-smoke pi-verify pi-review

SHELL := /bin/bash
.SHELLFLAGS := -o pipefail -c

GO ?= go
GOFMT ?= gofmt
NPM ?= npm
BUILD_DIR := build
PI_REVIEW_DIR := runtime/pi-review
VERSION ?= dev
GO_FILES := $(shell find . -type f -name '*.go' -not -path './build/*')

build:
	@mkdir -p $(BUILD_DIR)
	$(GO) build -trimpath -ldflags "-X main.version=$(VERSION)" -o $(BUILD_DIR)/argus ./cmd/argus

test: pi-build
	$(GO) test -count=1 ./...

fmt:
	$(GOFMT) -w $(GO_FILES)
	$(NPM) --prefix $(PI_REVIEW_DIR) run format

fmt-check:
	@files="$$($(GOFMT) -l $(GO_FILES))"; \
	if [[ -n "$$files" ]]; then \
		echo "The following files need gofmt:" >&2; \
		echo "$$files" >&2; \
		exit 1; \
	fi
	$(NPM) --prefix $(PI_REVIEW_DIR) run check

vet:
	$(GO) vet ./...

docs-check:
	$(GO) run ./scripts/docs_check

pi-install:
	$(NPM) --prefix $(PI_REVIEW_DIR) ci

pi-format:
	$(NPM) --prefix $(PI_REVIEW_DIR) run format

pi-check:
	$(NPM) --prefix $(PI_REVIEW_DIR) run check

pi-test:
	$(NPM) --prefix $(PI_REVIEW_DIR) test

pi-build:
	$(NPM) --prefix $(PI_REVIEW_DIR) run build

pi-smoke: pi-build
	ARGUS_PI_WORKER_SMOKE=1 $(GO) test -count=1 ./internal/agentshadow -run '^TestAgentReviewWorkerCrossLanguageCanceledRoundTrip$$'

pi-verify: pi-check pi-test pi-smoke

pi-review:
	@$(NPM) --silent --prefix $(PI_REVIEW_DIR) run review -- $(ARGS)

verify: fmt-check vet test docs-check build pi-test pi-smoke

clean:
	rm -rf $(BUILD_DIR)
	$(NPM) --prefix $(PI_REVIEW_DIR) run clean
