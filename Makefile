# SPDX-License-Identifier: AGPL-3.0-or-later

GO ?= go
VERSION ?= dev

.PHONY: build test vet fmt check integration image clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/nuno ./cmd/nuno

# What CI gates on: unit and contract tests, with the race detector.
test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

check: vet test
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "not gofmt clean:"; echo "$$unformatted"; exit 1; fi

# The real stack. Not a merge blocker, and it needs the services up:
#   docker compose -f docker-compose.test.yml up -d --wait
integration:
	$(GO) test -tags integration -count 1 -timeout 20m ./tests/integration/...

image:
	docker build --build-arg VERSION=$(VERSION) -t nuno:$(VERSION) .

clean:
	rm -rf bin
