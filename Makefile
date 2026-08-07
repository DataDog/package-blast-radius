BIN := bin/blast-radius

# -s -w drop the symbol table and DWARF data (~13MB); panic tracebacks still
# resolve because the pclntab is kept.
LDFLAGS := -s -w

.PHONY: all build test vet clean

all: build

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/blast-radius

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin
