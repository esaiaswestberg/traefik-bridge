GO ?= go
BIN_DIR ?= bin

.PHONY: build build-linux check fmt generate lint test integration-docker-provider docker-build docker-build-master docker-build-slave docker-build-proxy

build:
	$(GO) build ./cmd/...

build-linux:
	mkdir -p $(BIN_DIR)/linux-amd64 $(BIN_DIR)/linux-arm64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -o $(BIN_DIR)/linux-amd64/bridge-master ./cmd/bridge-master
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -o $(BIN_DIR)/linux-amd64/bridge-slave ./cmd/bridge-slave
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -o $(BIN_DIR)/linux-amd64/bridge-proxy ./cmd/bridge-proxy
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -o $(BIN_DIR)/linux-amd64/bridge-provision ./cmd/bridge-provision
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -o $(BIN_DIR)/linux-arm64/bridge-master ./cmd/bridge-master
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -o $(BIN_DIR)/linux-arm64/bridge-slave ./cmd/bridge-slave
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -o $(BIN_DIR)/linux-arm64/bridge-proxy ./cmd/bridge-proxy
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -o $(BIN_DIR)/linux-arm64/bridge-provision ./cmd/bridge-provision

check: fmt lint test

generate:
	buf generate

fmt:
	$(GO) fmt ./...

lint:
	golangci-lint run

test:
	$(GO) test ./...

integration-docker-provider:
	./tests/integration/docker-provider/run.sh

docker-build: docker-build-master docker-build-slave docker-build-proxy

docker-build-master:
	docker build -f deploy/docker/Dockerfile.master -t traefik-bridge-master:local .

docker-build-slave:
	docker build -f deploy/docker/Dockerfile.slave -t traefik-bridge-slave:local .

docker-build-proxy:
	docker build -f deploy/docker/Dockerfile.proxy -t traefik-bridge-proxy:local .
