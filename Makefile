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

.PHONY: all build test validate-race-shards test-race test-race-base test-race-mutation vet fmt-check generate manifests verify verify-source verify-crd-schema-history verify-kubernetes-support verify-ptah-support update-kubernetes-support verify-release docker-build e2e-static e2e demo demo-up demo-record demo-serve demo-test demo-down

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

verify-source: fmt-check generate manifests verify-crd-schema-history verify-kubernetes-support verify-ptah-support verify-release e2e-static vet build test
	@git diff --exit-code -- api/v1alpha1/zz_generated.deepcopy.go config/crd/bases charts/ptah-operator/crds internal/crdupgrade/assets

verify-crd-schema-history: manifests
	$(GO) run ./hack/verifycrdschemahistory

verify-kubernetes-support:
	$(GO) run ./hack/verify-kubernetes-support.go

verify-ptah-support:
	$(GO) run ./hack/verifyptahsupport

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

# ---------------------------------------------------------------------------
# The demonstration lab.
#
# demo/ holds three parts: the scenarios, the recorder that runs them against a
# live cluster, and the player on the documentation site. `demo` does the whole
# round trip; the targets under it are the steps, so a scenario can be
# re-recorded without rebuilding the cluster.
#
# The lab is the end-to-end harness stopped after its bootstrap. There is no
# second cluster, no second chart install and no second set of pinned images:
# what the suite proves is what the demonstration runs on.

DEMO_ENVIRONMENT := demo/.lab/environment
DEMO_KUBERNETES_VERSION ?= 1.37.0
DEMO_RUN_ID ?= demo

demo: demo-up demo-record
	@printf 'demo: recorded. Read it with: make demo-serve\n'

# The bootstrap is skipped when a lab is already up; the namespace fixtures are
# applied either way, because they are idempotent and because a lab that was
# brought up before a fixture changed would otherwise keep the old one.
demo-up:
	@mkdir -p demo/.lab
	@if [ -f "$(DEMO_ENVIRONMENT)" ]; then \
		printf 'demo: a lab is already up (%s); remove it with make demo-down\n' "$(DEMO_ENVIRONMENT)"; \
	else \
		K8S_VERSION="$(DEMO_KUBERNETES_VERSION)" \
		E2E_STOP_AFTER=bootstrap \
		E2E_ENVIRONMENT_FILE="$(CURDIR)/$(DEMO_ENVIRONMENT)" \
		E2E_RUN_ID="$(DEMO_RUN_ID)" \
		DOCKER_CONTEXT="$(DOCKER_CONTEXT)" \
		./hack/e2e-kind.sh; \
	fi
	./demo/bin/lab prepare

demo-record:
	@[ -f "$(DEMO_ENVIRONMENT)" ] || { \
		printf 'demo: no lab. Bring one up with: make demo-up\n' >&2; \
		exit 1; \
	}
	$(GO) run ./demo/cmd/record -root .

demo-serve:
	cd docs/site && npm ci && npm run dev

# What runs without a cluster: the scenarios parse and state an expectation for
# every step, the recorder's own units, and the site builds from the recording
# that is committed.
demo-test:
	$(GO) test ./demo/...
	$(GO) run ./demo/cmd/record -root . -check
	cd docs/site && npm ci && \
		npm run check:demo:selftest && npm run check:demo-page:selftest && \
		npm run check:demo && npm run build && \
		npm run check:links && npm run check:navigation && npm run check:demo-page

demo-down:
	@[ -f "$(DEMO_ENVIRONMENT)" ] || { printf 'demo: no lab to remove\n'; exit 0; }
	@set -a; . "./$(DEMO_ENVIRONMENT)"; set +a; \
		export DOCKER_CONFIG="$$E2E_DOCKER_CONFIG"; \
		printf 'demo: removing cluster %s\n' "$$E2E_KIND_CLUSTER_NAME"; \
		kind delete cluster --name "$$E2E_KIND_CLUSTER_NAME" >/dev/null 2>&1 || true; \
		for container in "$$E2E_EXTERNAL_POSTGRES_CONTAINER_ID" "$$E2E_REGISTRY_CONTAINER_ID"; do \
			[ -n "$$container" ] || continue; \
			docker --context "$$E2E_DOCKER_CONTEXT" container rm -fv "$$container" >/dev/null 2>&1 || true; \
		done
	rm -rf demo/.lab
