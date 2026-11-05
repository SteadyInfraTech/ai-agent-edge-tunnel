NAME = ai-agent-edge-tunnel
VERSION ?= 0.1.0

.PHONY: all build test clean manifests

all: test build

build:
	go build -o bin/manager cmd/main.go

test:
	go test -v ./...

manifests:
	@echo "CRD manifests located in config/crd/bases/"

clean:
	rm -rf bin/
