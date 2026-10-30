GO ?= go

.PHONY: all build test vet fmt fmt-check demo clean

all: fmt-check vet test

build:
	$(GO) build -o bin/ ./cmd/...

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

# Closed vs open model against a demo server that freezes periodically.
demo:
	./scripts/demo.sh

clean:
	rm -rf bin out
