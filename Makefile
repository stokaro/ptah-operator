SHELL := /bin/sh

GO ?= go
CONTROLLER_GEN_VERSION ?= v0.21.0
CRD_SCHEMA_VERSION := 1
CONTROLLER_STATE_VERSION := 1
RACE_MUTATION_SHARDS ?= 8
RACE_MUTATION_SHARD ?=
override RACE_MUTATION_TESTS := TestVerifyE2EHarnessRejectsCriticalMutations|TestVerifyE2EDataPlaneRejectsCriticalMutations|TestVerifyFailedUpgradeEvidenceRejectsCriticalMutations|TestVerifyE2EChildScriptsRejectCriticalMutations
DOCKER_CONTEXT ?= remote-dev-container
IMG ?= ghcr.io/stokaro/ptah-operator:dev
REVISION ?= $(shell git rev-parse --verify HEAD 2>/dev/null)

.PHONY: all build test validate-race-shards test-race test-race-base test-race-mutation vet fmt-check generate manifests verify verify-source verify-crd-schema-history verify-kubernetes-support update-kubernetes-support verify-release docker-build e2e-static e2e

all: verify build

build:
	@# ./... rather than a list of commands: a command left out of the list
	@# is a binary nothing builds, and the list was already complete only by
	@# coincidence. Non-main packages type-check and write nothing.
	$(GO) build ./...

test:
	$(GO) test ./...

validate-race-shards:
	@case "$(RACE_MUTATION_SHARDS)" in \
		[1-8]) ;; \
		*) \
			printf '%s\n' 'RACE_MUTATION_SHARDS must be an integer between 1 and 8' >&2; \
			exit 1; \
			;; \
	esac

test-race-base: validate-race-shards
	$(GO) test -race -count=1 -timeout=10m -skip '^($(RACE_MUTATION_TESTS))$$' ./...

test-race-mutation: validate-race-shards
	@case "$(RACE_MUTATION_SHARD)" in \
		[0-7]) ;; \
		*) \
			printf '%s\n' 'RACE_MUTATION_SHARD must be an integer between 0 and 7' >&2; \
			exit 1; \
			;; \
	esac; \
	if [ "$(RACE_MUTATION_SHARD)" -ge "$(RACE_MUTATION_SHARDS)" ]; then \
		printf 'RACE_MUTATION_SHARD must be less than %s\n' "$(RACE_MUTATION_SHARDS)" >&2; \
		exit 1; \
	fi
	PTAH_MUTATION_TEST_SHARD="$(RACE_MUTATION_SHARD)/$(RACE_MUTATION_SHARDS)" \
		$(GO) test -race -count=1 -timeout=10m \
		-run '^($(RACE_MUTATION_TESTS))$$' ./hack

test-race: validate-race-shards test-race-base
	@# The mutation suites repeatedly inspect large shell fixtures. Partitioning
	@# every table by ordinal keeps complete race coverage within Go's bounded
	@# per-package timeout on low-core CI runners.
	@shard=0; \
	while [ "$$shard" -lt "$(RACE_MUTATION_SHARDS)" ]; do \
		printf 'race mutation shard %s/%s\n' "$$((shard + 1))" "$(RACE_MUTATION_SHARDS)"; \
		$(MAKE) --no-print-directory test-race-mutation \
			RACE_MUTATION_SHARD="$$shard" || exit $$?; \
		shard=$$((shard + 1)); \
	done

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
	GO="$(GO)" sh hack/stamp-crd-schema-version.sh \
		$(CRD_SCHEMA_VERSION) $(CONTROLLER_STATE_VERSION) config/crd/bases/*.yaml
	@mkdir -p charts/ptah-operator/crds
	@rm -f charts/ptah-operator/crds/*.yaml
	@cp config/crd/bases/*.yaml charts/ptah-operator/crds/
	@mkdir -p internal/crdupgrade/assets
	@rm -f internal/crdupgrade/assets/*.yaml
	@cp config/crd/bases/*.yaml internal/crdupgrade/assets/

verify: verify-source test-race

verify-source: fmt-check generate manifests verify-crd-schema-history verify-kubernetes-support verify-release e2e-static vet build test
	@git diff --exit-code -- api/v1alpha1/zz_generated.deepcopy.go config/crd/bases charts/ptah-operator/crds internal/crdupgrade/assets

verify-crd-schema-history: manifests
	$(GO) run ./hack/verifycrdschemahistory

verify-kubernetes-support:
	$(GO) run ./hack/verify-kubernetes-support.go

# This target performs live upstream discovery. Normal verification is offline.
update-kubernetes-support:
	$(GO) run ./hack/updatekubernetessupport

verify-release:
	$(GO) run ./hack/releaseverify

docker-build:
	@revision="$(REVISION)"; \
		head="$$(git rev-parse --verify HEAD 2>/dev/null)"; \
		if [ "$${#revision}" -ne 40 ] || \
			[ -n "$$(printf '%s' "$$revision" | tr -d '0-9a-f')" ]; then \
			printf '%s\n' 'REVISION must be an exact 40-character lowercase Git commit' >&2; \
			exit 1; \
		fi; \
		if [ "$${revision}" != "$${head}" ]; then \
			printf 'REVISION %s must equal current HEAD %s\n' "$$revision" "$$head" >&2; \
			exit 1; \
		fi; \
		if [ -n "$$(git status --porcelain --untracked-files=normal)" ]; then \
			printf '%s\n' 'docker-build source tree must exactly match HEAD' >&2; \
			exit 1; \
		fi
	docker --context "$(DOCKER_CONTEXT)" build \
		--build-arg "REVISION=$(REVISION)" \
		--tag "$(IMG)" .

e2e-static:
	./hack/e2e-static.sh

e2e:
	DOCKER_CONTEXT="$(DOCKER_CONTEXT)" ./hack/e2e-kind.sh
