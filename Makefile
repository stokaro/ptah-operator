SHELL := /bin/sh

GO ?= go
CONTROLLER_GEN_VERSION ?= v0.22.0
SETUP_ENVTEST_VERSION ?= v0.25.1
ACTIONLINT_VERSION ?= v1.7.12
ENVTEST_KUBERNETES_VERSION ?= 1.37.0
ENVTEST_INDEX ?= https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/1031496fc98a4f51010c3bdfdeb57b5d67bea7bd/envtest-releases.yaml
ENVTEST_BIN_DIR ?=
CRD_SCHEMA_VERSION := 30
CONTROLLER_STATE_VERSION := 2
override RACE_MUTATION_TESTS := TestVerifyE2EHarnessRejectsCriticalMutations|TestVerifyE2EDataPlaneRejectsCriticalMutations|TestVerifyFailedUpgradeEvidenceRejectsCriticalMutations|TestVerifyE2EChildScriptsRejectCriticalMutations
DOCKER_CONTEXT ?= remote-dev-container
IMG ?= ghcr.io/stokaro/ptah-operator:dev
REVISION ?= $(shell git rev-parse --verify HEAD 2>/dev/null)

.PHONY: all build test test-envtest test-race vet fmt-check generate manifests chart-policies verify verify-source verify-crd-schema-history verify-kubernetes-support verify-ptah-support update-kubernetes-support verify-runner-protocol verify-release docker-build acceptance-coverage acceptance-record acceptance-issue-map scan-vulnerabilities e2e-static e2e

# A second declaration rather than a longer first one: the lifecycle targets
# above are audited as one line, and appending to it is a change to that audit
# for the sake of a demonstration.
.PHONY: demo demo-up demo-record demo-serve demo-test demo-reproduce demo-down

all: verify build

build:
	@# ./... rather than a list of commands: a command left out of the list is
	@# a binary nothing builds. Non-main packages type-check and write nothing.
	$(GO) build ./...

# ./hack took 331 and 338 seconds in the verify job on acd17c4 and 93b209b, and
# a loaded machine runs it past go test's default of ten minutes. The shell
# mutation suites in it run nowhere else.
test:
	$(GO) test -timeout=30m ./...

# The suites under test/envtest run against a real kube-apiserver and etcd and
# nothing else: the CRD schemas and their CEL, the chart's admission policies
# and bindings, and the manager's webhooks, decided by the API server itself.
# setup-envtest is pinned by version and reads an index pinned by commit, which
# carries the digest of every archive it downloads, so a run here and a run in
# CI start the same binaries. hack/verify-kubernetes-support.go holds the
# Kubernetes version inside the support window.
#
# -count=1 because the suites read the chart through helm, a child process the
# test cache cannot see: a cached pass would survive a policy edit. The
# slowest package took under three minutes on a laptop; -timeout is what the
# verify job's limit is budgeted against, so a hung suite prints its goroutines
# before the job is canceled.
# ENVTEST_BIN_DIR empty keeps setup-envtest's own store, shared with other
# checkouts; CI names a directory it caches.
test-envtest:
	@assets="$$($(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION) \
		use $(ENVTEST_KUBERNETES_VERSION) --index "$(ENVTEST_INDEX)" \
		$(if $(ENVTEST_BIN_DIR),--bin-dir "$(ENVTEST_BIN_DIR)") -p path)" || exit $$?; \
	KUBEBUILDER_ASSETS="$$assets" PTAH_REQUIRE_ENVTEST=1 $(GO) test -count=1 -timeout=10m ./test/envtest/...

test-race:
	@# The skipped suites are the shell mutation tables. Each row rewrites a
	@# fixture and runs the static verifier over it, which other ./hack tests
	@# already do under the detector. The test target, and so verify-source,
	@# runs them without it.
	$(GO) test -race -count=1 -timeout=10m -skip '^($(RACE_MUTATION_TESTS))$$' ./...

# The acceptance phases under test/e2e build only with the e2e tag, so a plain
# go test ./... never reaches for a cluster. Vetting them with the tag is what
# compiles them here rather than on a kind cluster an hour into a lifecycle.
vet:
	$(GO) vet ./...
	$(GO) vet -tags e2e ./test/e2e/...

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

# The chart's admission policies are written once, in Go, and the templates
# that ship them are generated from those definitions: hack/chartpolicies
# writes each into charts/ptah-operator/templates with the release values left
# as Helm expressions. verify-source runs this and refuses a template the
# generator did not write, the way it refuses a hand-edited CRD.
chart-policies:
	$(GO) run ./hack/chartpolicies -chart charts/ptah-operator

.PHONY: docs-reference docs-reference-check

# The field reference the site publishes, generated from the API types.
#
# The shipped CRDs drop descriptions (crd:maxDescLen=0 above), so this runs the
# same generator once more into a directory nothing keeps, reads the schemas
# with their doc comments, and writes the pages. -write updates them;
# docs-reference-check refuses a page the API has moved past.
docs-reference:
	@tmp=$$(mktemp -d); \
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		crd paths=./api/... output:crd:artifacts:config=$$tmp; \
	$(GO) run ./hack/crdreference -crds $$tmp \
		-examples docs/reference-examples \
		-out docs/site/src/content/docs/reference -write; \
	status=$$?; rm -rf $$tmp; exit $$status

docs-reference-check:
	@tmp=$$(mktemp -d); \
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		crd paths=./api/... output:crd:artifacts:config=$$tmp; \
	$(GO) run ./hack/crdreference -crds $$tmp \
		-examples docs/reference-examples \
		-out docs/site/src/content/docs/reference -require-descriptions; \
	status=$$?; rm -rf $$tmp; exit $$status

.PHONY: lint-workflows

# actionlint reads every workflow's expressions, job graph and action inputs,
# and runs shellcheck over each run block when shellcheck is on PATH, which it
# is on GitHub's runners. CODEOWNERS is what makes a workflow change reviewed;
# this catches what a reviewer reads past.
lint-workflows:
	$(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

verify: verify-source test-race

verify-source: fmt-check lint-workflows generate manifests chart-policies verify-crd-schema-history verify-kubernetes-support verify-ptah-support verify-runner-protocol verify-release e2e-static vet build test test-envtest
	@git diff --exit-code -- api/v1alpha1/zz_generated.deepcopy.go config/crd/bases charts/ptah-operator/crds internal/crdupgrade/assets \
		charts/ptah-operator/templates/controller-object-guard.yaml \
		charts/ptah-operator/templates/controller-write-guard.yaml \
		charts/ptah-operator/templates/certificate-secret-guard.yaml

verify-crd-schema-history: manifests
	$(GO) run ./hack/verifycrdschemahistory

verify-kubernetes-support:
	$(GO) run ./hack/verify-kubernetes-support.go

verify-ptah-support:
	$(GO) run ./hack/verifyptahsupport

# A plan and its approval bind the runner protocol version, not the runner
# image, so the version has to move when the runner's source does. This refuses
# a source change the version did not follow, unless support/runner-protocol.json
# declares why the contract stayed the same.
verify-runner-protocol:
	$(GO) run ./hack/verifyrunnerprotocol

# The PA-01 coverage table for an acceptance record, derived from the two
# support catalogs and the driver rather than typed beside them. The table's
# completeness is a test under `go test ./hack/...`; this prints it.
acceptance-coverage:
	@$(GO) run ./hack/acceptancecoverage

# The whole acceptance record #242 asks for: the candidate identity the tree
# can answer, a disposition for every requirement, and the coverage table. It
# awards no pass; what a build or a deployment decides is left blank and named.
#
# ACCEPTANCE_PROFILE points at a declared profile -- the digests, the installed
# values, the operating and recovery targets, the run the evidence comes from,
# and a disposition per requirement. The record fills itself from it and is
# refused rather than printed when the profile would overstate it: a verdict
# with no evidence, an accepted requirement beside an unfilled target, a tag
# where a digest belongs.
acceptance-record:
ifeq ($(strip $(ACCEPTANCE_PROFILE)),)
	@$(GO) run ./hack/acceptancecoverage -record
else
	@$(GO) run ./hack/acceptancecoverage -record -profile $(ACCEPTANCE_PROFILE)
endif

# Reachable vulnerabilities in what this module builds, read against the
# current advisory database.
#
# Deliberately not pinned, unlike ShellCheck in support/tools.json. A pinned
# scanner and a pinned database would make this reproducible and useless: PA-11
# asks for "a current vulnerability database" and for reassessment as new
# advisories appear, so a run that agrees with last month's is the wrong kind
# of stable. It is a live-discovery target for the same reason
# update-kubernetes-support is, and normal verification stays offline.
#
# This is the source half. Scanning the shipped bytes -- the published image
# digests, with their own toolchain -- is the release half, and neither
# substitutes for the other: a rebuild from the same source needs its own
# artifact verification.
scan-vulnerabilities:
	@command -v govulncheck >/dev/null || { \
		echo "govulncheck is required: go install golang.org/x/vuln/cmd/govulncheck@latest" >&2; \
		exit 1; \
	}
	govulncheck ./...

# The state of every issue the #242 review map links, per requirement, read
# when it runs. The map deliberately stores no states, so this reads them and
# stores none either. It awards nothing: a closed issue is not evidence about a
# candidate.
#
# This target performs live forge discovery. Normal verification is offline.
acceptance-issue-map:
	@./hack/acceptance-issue-map.sh

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
# Empty by default: demo/bin/lab reads the newest release out of
# support/kubernetes.json. A version written here is a second answer to which
# releases are supported, and the harness refuses one outside the window, so
# this target would stop working the day the window moves.
DEMO_KUBERNETES_VERSION ?=
DEMO_RUN_ID ?= demo

demo: demo-up demo-record
	@printf 'demo: recorded. Read it with: make demo-serve\n'

# The bootstrap is skipped when a lab is already up; the namespace fixtures are
# applied either way, because they are idempotent and because a lab that was
# brought up before a fixture changed would otherwise keep the old one. Both
# decisions live in demo/bin/lab, where the shell they need is shell.
demo-up:
	LAB_ENVIRONMENT="$(CURDIR)/$(DEMO_ENVIRONMENT)" \
	LAB_KUBERNETES_VERSION="$(DEMO_KUBERNETES_VERSION)" \
	LAB_RUN_ID="$(DEMO_RUN_ID)" \
	DOCKER_CONTEXT="$(DOCKER_CONTEXT)" \
		./demo/bin/lab up

demo-record:
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

# The acceptance criterion the demonstration is written against: after the
# environment is standing, a scenario can be repeated from what the pages
# publish, with no script of ours reachable, by an account that may not create
# a Job. It needs a live lab.
demo-reproduce:
	LAB_ENVIRONMENT="$(CURDIR)/$(DEMO_ENVIRONMENT)" ./demo/acceptance/reproduce.sh

demo-down:
	LAB_ENVIRONMENT="$(CURDIR)/$(DEMO_ENVIRONMENT)" ./demo/bin/lab down
