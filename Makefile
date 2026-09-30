VERSION ?= dev
BIN := bin/comfyvault

.PHONY: build run test vet fmt clean

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/comfyvault

run:
	go run ./cmd/comfyvault serve

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -rf bin
