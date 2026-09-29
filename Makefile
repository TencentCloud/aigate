GO ?= go
BIN := bin/aigate

.PHONY: build run test test-e2e vet fmt tidy clean

build:
	$(GO) build -o $(BIN) ./cmd/aigate

run: build
	$(BIN) -db-type sqlite -db-dsn aigate.db -init-admin-token dev-admin-token

test:
	$(GO) test ./...

test-e2e:
	bash test/e2e/run.sh

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin
