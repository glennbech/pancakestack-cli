.PHONY: build test lint tidy install clean release-check

BINARY := pancakestack
LDFLAGS := -s -w -X main.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/pancakestack

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/pancakestack

test:
	go test -v ./...

lint:
	gofmt -d .
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -f $(BINARY)
	rm -rf dist/

# Local dry-run of a release (needs goreleaser: brew install goreleaser)
release-check:
	goreleaser check
	goreleaser release --snapshot --clean --skip=publish
