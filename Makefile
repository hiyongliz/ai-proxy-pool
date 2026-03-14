APP      := appool
PKG      := ./...
GOFLAGS  := -trimpath
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILT_BY ?= local
GOBIN    ?= $(shell sh -c 'if [ -n "$$GOBIN" ]; then printf %s "$$GOBIN"; else printf %s "$$(go env GOPATH)/bin"; fi')
LDFLAGS  := -X 'main.version=$(VERSION)' -X 'main.commit=$(COMMIT)' -X 'main.buildDate=$(DATE)' -X 'main.builtBy=$(BUILT_BY)'

.PHONY: build run test fmt lint coverage clean install

build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(APP) ./cmd/ai-proxy-pool

run: build
	./$(APP)

test:
	go test -race -cover $(PKG)

fmt:
	@command -v goimports >/dev/null 2>&1 && goimports -w . || gofmt -w .

lint:
	go vet $(PKG)
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || echo "golangci-lint not installed, skipping"

coverage:
	go test -race -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out

clean:
	rm -f $(APP)

install:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(GOBIN)/$(APP) ./cmd/ai-proxy-pool
