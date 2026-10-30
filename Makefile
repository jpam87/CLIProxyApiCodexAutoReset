GO ?= go
PLUGIN := dist/codex-autoreset.so
.PHONY: test build

test:
	$(GO) test ./...

build:
	mkdir -p dist
	$(GO) build -buildmode=c-shared -o $(PLUGIN) .
