# remote-adapter — top-level build.
#
# One pure-Go binary: rca. No native artifacts, no cgo, no embedding step.

BIN := bin
GO  := go

.PHONY: all go test fmt vet clean

all: go

## Build rca into ./bin
go:
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/rca ./cmd/rca

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf $(BIN)
