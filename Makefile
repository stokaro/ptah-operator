SHELL := /bin/sh

GO ?= go
CONTROLLER_GEN_VERSION ?= v0.21.0
DOCKER_CONTEXT ?= remote-dev-container
IMG ?= ghcr.io/stokaro/ptah-operator:dev

.PHONY: all build test test-race vet fmt-check generate manifests verify verify-kubernetes-support update-kubernetes-support verify-release docker-build e2e-static e2e

all: verify build

build:
	$(GO) build ./cmd/manager ./cmd/ptah-runner ./cmd/ptah-cert-rotator

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
	@mkdir -p charts/ptah-operator/crds
	@cp config/crd/bases/*.yaml charts/ptah-operator/crds/

verify: fmt-check generate manifests verify-kubernetes-support verify-release e2e-static vet test
	@git diff --exit-code -- api/v1alpha1/zz_generated.deepcopy.go config/crd/bases charts/ptah-operator/crds

verify-kubernetes-support:
	$(GO) run ./hack/verify-kubernetes-support.go

# This target performs live upstream discovery. Normal verification is offline.
update-kubernetes-support:
	$(GO) run ./hack/updatekubernetessupport

verify-release:
	$(GO) run ./hack/releaseverify

docker-build:
	docker --context $(DOCKER_CONTEXT) build --tag $(IMG) .

e2e-static:
	./hack/e2e-static.sh

e2e:
	DOCKER_CONTEXT="$(DOCKER_CONTEXT)" ./hack/e2e-kind.sh
