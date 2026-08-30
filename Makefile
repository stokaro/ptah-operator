SHELL := /bin/sh

GO ?= go
CONTROLLER_GEN_VERSION ?= v0.21.0
DOCKER_CONTEXT ?= remote-dev-container
IMG ?= ghcr.io/stokaro/ptah-operator:dev

.PHONY: all build test test-race vet fmt-check generate manifests verify docker-build

all: verify build

build:
	$(GO) build ./cmd/manager ./cmd/ptah-runner

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./internal/...

vet:
	$(GO) vet ./...

fmt-check:
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))" || \
		{ gofmt -l $$(find . -name '*.go' -not -path './vendor/*'); exit 1; }

generate:
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		object:headerFile=hack/boilerplate.go.txt paths=./api/...

manifests:
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		crd:maxDescLen=0 paths=./api/... output:crd:artifacts:config=config/crd/bases

verify: fmt-check generate manifests vet test
	@git diff --exit-code -- api/v1alpha1/zz_generated.deepcopy.go config/crd/bases

docker-build:
	docker --context $(DOCKER_CONTEXT) build --tag $(IMG) .
