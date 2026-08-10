BIN := bin/blast-radius

# Strip Go metadata and let the native linker discard unused DuckDB sections.
UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
EXTLDFLAGS := -Wl,-dead_strip
else ifeq ($(UNAME_S),Linux)
EXTLDFLAGS := -Wl,--gc-sections
endif

ifneq ($(EXTLDFLAGS),)
LDFLAGS := -s -w -linkmode=external -extldflags=$(EXTLDFLAGS)
else
LDFLAGS := -s -w
endif

STRIP ?= strip

.PHONY: all build test vet clean

all: build

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/blast-radius
	$(STRIP) $(BIN)

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin
