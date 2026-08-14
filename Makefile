.PHONY: build test test-race vet check clean

BINARY := build/subedit

build:
	@mkdir -p build
	go build -buildvcs=false -o $(BINARY) ./cmd/subedit

test:
	go test -buildvcs=false ./...

test-race:
	go test -buildvcs=false -race ./...

vet:
	go vet -buildvcs=false ./...

check: test vet

clean:
	go clean
	@rm -f $(BINARY)
