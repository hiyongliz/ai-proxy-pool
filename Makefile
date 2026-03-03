APP      := ai-proxy-pool
PKG      := ./...
GOFLAGS  := -trimpath

.PHONY: build run test lint clean install

build:
	go build $(GOFLAGS) -o $(APP) ./cmd/ai-proxy-pool

run: build
	./$(APP)

test:
	go test -race -cover $(PKG)

lint:
	go vet $(PKG)

clean:
	rm -f $(APP)

install:
	go install $(GOFLAGS) ./cmd/ai-proxy-pool
