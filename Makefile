NAME = ai-agent-edge-tunnel
VERSION ?= 0.1.0

.PHONY: all build test vet clean manifests docker-build install uninstall deploy undeploy

all: test build

build:
	go build -o bin/manager ./cmd

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

# deploy applies config/default with the manager image set to $(IMG), so the
# image you built and pushed is the one that runs. It works on a copy of
# config/ and leaves the checked-in manifests untouched.
deploy:
	rm -rf .deploy && mkdir .deploy && cp -r config .deploy/config
	cd .deploy/config/manager && kustomize edit set image controller=$(IMG)
	kubectl apply -k .deploy/config/default
	rm -rf .deploy

# undeploy deletes every AgentEdgeTunnel first and waits for the manager to
# finish their finalizers (removing the Cloudflare resources). Only then are
# the manager and the CRD removed; deleting them together would stop the
# manager before it could clean up and leave the objects stuck.
undeploy:
	kubectl delete agentedgetunnels --all --all-namespaces --wait=true --timeout=10m
	kubectl delete -k config/default

manifests:
	@echo "CRD manifests located in config/crd/bases/"

clean:
	rm -rf bin/ .deploy/
