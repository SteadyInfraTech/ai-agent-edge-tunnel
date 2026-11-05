NAME = ai-agent-edge-tunnel
VERSION ?= 0.1.0

.PHONY: all build test vet clean manifests docker-build install uninstall deploy undeploy

all: test build

build:
	go build -o bin/manager cmd/main.go

vet:
	go vet ./...

test: vet
	go test -v -race ./...

IMG ?= ghcr.io/steadyinfratech/ai-agent-edge-tunnel:$(VERSION)

docker-build:
	docker build -t $(IMG) .

install:
	kubectl apply -k config/crd

uninstall:
	kubectl delete -k config/crd

deploy:
	kubectl apply -k config/default

undeploy:
	kubectl delete -k config/default

manifests:
	@echo "CRD manifests located in config/crd/bases/"

clean:
	rm -rf bin/
