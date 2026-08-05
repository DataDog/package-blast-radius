BIN := bin/blast-radius

.PHONY: all build test vet clean

all: build

build:
	go build -o $(BIN) ./cmd/blast-radius

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin
