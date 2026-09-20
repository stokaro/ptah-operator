# AGENTS.md

Repository-local guidance for coding agents working in `ptah-operator`.

## What this is

A Kubernetes operator for Ptah, scaffolded with Kubebuilder v4 under the group
`ptah.run`. The API is `api/v1alpha1`, and it serves two families of three
kinds: `PtahSchema`, `PtahSchemaPlan` and `PtahSchemaApproval` for a declared
schema, `PtahMigration`, `PtahMigrationPlan` and `PtahMigrationApproval` for a
versioned sequence. The work happens in `internal/`, which holds the
controllers, admission, certificate rotation, the CRD upgrade path, the plan
store and the runner. Five programs ship from `cmd/`: the manager, the
certificate rotator, the CRD manager, the runner and the `kubectl ptah` plugin.

[The architecture page](docs/site/src/content/docs/reference/architecture.md)
says how those pieces fit together and which invariant each one holds.

Read the versions out of `go.mod` rather than out of prose here. At the time of
writing they are `sigs.k8s.io/controller-runtime v0.24.1`, `k8s.io/api v0.36.1`,
Go `1.26` with toolchain `1.27`.

## Skills

`.agents/skills/` holds instructions to load when the task calls for them, and
[`.agents/skills/README.md`](.agents/skills/README.md) says where each came from
and what was rejected. Three are vendored from upstream at a pinned commit; one
is written here.

`.claude/skills` is a relative symlink to that directory, so an agent that
discovers skills under `.claude/` finds the same four without a second copy.
There is one set of files; the link is the only thing that knows about both
paths.

| Working on | Read |
| --- | --- |
| Go API types, `+kubebuilder` markers, generation | `.agents/skills/kubebuilder-api-design/SKILL.md` |
| A generated CRD as an API contract: compatibility, CEL, list semantics, versioning | `.agents/skills/k8s-crd-design-review/SKILL.md` |
| Reconcilers, webhooks, finalizers, ownership, controller tests | `.agents/skills/operator-correctness/SKILL.md` |
| `charts/`, `config/`, RBAC, anything a cluster applies | `.agents/skills/kubernetes-skill/SKILL.md` |

An API change is two reviews, not one: the Go types, and then the CRD the
generator emitted from them. The first skill hands the second its output on
purpose.

## Building and testing

```bash
make build          # the five binaries
make test           # unit contour
make generate       # deepcopy
make manifests      # CRDs and RBAC from the markers
make verify         # the checks CI runs, including the CRD schema history
make e2e            # the suite against kind
```

`make verify` is the one that refuses a change the generators did not produce:
`verify-source`, `verify-crd-schema-history` and `verify-kubernetes-support` are
separate targets under it, so run it after touching `api/` or any marker.

## What a green master says

CI runs the complete set on every push to `master`: no job is skipped because
the commit is on `master`. A newer run cancels the older one on every ref,
`master` included, and this is the intended policy. Do not narrow
`cancel-in-progress` back to pull requests; `hack/verify-kubernetes-support.go`
accepts `true` and nothing else.

The run that counts is the one for the newest commit. The tip is what the next
change builds on and what a release is cut from, and an older run proves a tree
nobody builds on again. One run spends about twelve hours of runner time and
reaches its verdict in about an hour and twenty minutes (measured on run
35495669973: fifteen acceptance jobs between twelve minutes and an hour and a
quarter, the race detector at six minutes, and one image build at six), so
letting a superseded run finish only holds the newer one behind it. This repository used
to spare a started `master` run, and the queue that built up held the commit
that fixed a known lifecycle failure for hours while the run ahead of it
re-proved that failure.

The cost is taken on purpose. A commit followed by another merge before its run
finishes carries no verdict of its own, and a commit with no check reads exactly
like one nothing objected to. So "master is green" is a statement about the tip
whose run finished, a bisect cannot assume a commit it lands on was ever built,
and a change that needs its own verdict — a release candidate, or a change to
the lifecycle path itself — goes through a pull request and is merged after its
run finishes. The pull request fans out over the same three minors and the same
five suites.

## The acceptance suites

Acceptance runs as one job per Kubernetes minor and suite.
[`support/e2e-suites.json`](support/e2e-suites.json) is where the suites and
their phases live, and it is the only place they are written down:
`hack/verify-kubernetes-support.go` builds the CI matrix from it and refuses it
unless every phase the driver runs belongs to exactly one suite.

```bash
make e2e                                   # every phase, in the driver's order, as before
E2E_SUITE=data-plane make e2e              # one suite, against a cluster of its own
E2E_SUITE=migrations-postgresql make e2e   # one engine's migration rows and reference data
```

The partition follows the dependencies rather than the clock, so phases that
share mutable state stay in one suite: the CRD upgrade and the uninstall that
follows it, the data plane and the fault injection inside it, the migration rows
and the reference data that runs in the same namespace.

Where the clock decides is between engines, which share nothing but the
namespace a suite stands up for itself. PostgreSQL and MySQL in one job made
that suite the longest stage of the matrix -- fifty-five minutes of the
migration path and twenty-two of the reference data, measured on run
35299742747 -- so each engine's phases are a suite of their own and the two run
at once. The engine is a phase input rather than a default: the driver names it,
and a phase asked to run with none refuses instead of covering one engine and
reporting two.

That last pair needs the namespace the data plane stands up — its registry
Service, its databases, its admission fixtures — so the migrations suite runs
the data-plane phase in preparation mode (`E2E_DATAPLANE_MODE=prepare`), which
creates those prerequisites and executes none of its own acceptance. Preparation
is not coverage: a phase counts as covered only where a suite lists it under
`phases`, and the catalog check counts it there and nowhere else.

`hack/e2e-suites-selftest.sh` measures the selection — that a suite runs the
phases it claims, that it runs no other suite's phase, and that the preparation
boundary sits before the data plane's own acceptance — and `make e2e-static`
runs it.

## Where the task images come from

The four images a run needs — the operator under test, the synthetic next
release, the isolated fixture and the Ptah executor — depend on one commit and
one catalog pin, and on no Kubernetes minor. CI builds them once:

```bash
E2E_STOP_AFTER=images E2E_IMAGE_EXPORT_DIR=/tmp/task-images make e2e
```

That is the same driver a lifecycle runs, stopped where the images exist and no
cluster does. Beside them it writes `images.json`: the commit they were built
from, the Ptah commit the executor carries, the two release sequences, and each
image's own identity. A lifecycle reads them back:

```bash
E2E_PREBUILT_IMAGE_DIR=/tmp/task-images make e2e
```

and refuses anything that is not its own inputs — a manifest from another
commit, an executor built from a Ptah the catalog does not pin, a next release
of the wrong sequence, an image whose identity or role label disagrees with the
manifest it travelled with. The content audits still run over whatever was
loaded. `make e2e` on its own builds for itself, so there is one build path
rather than a CI-only one, and `hack/e2e-shared-images-selftest.sh` measures
every refusal.

## Where a run's time went

A lifecycle records one row per stage: the bootstrap steps, each phase, and the
scenarios inside the four longest phases. The rows land in the ledger
`E2E_TIMING_LEDGER` names, the run's identity in `E2E_TIMING_CONTEXT`, and
`hack/e2etiming` joins them into a report and a Markdown summary:

```bash
go run ./hack/e2etiming \
  -ledger "$WORK_DIR/timings.jsonl" \
  -context "$WORK_DIR/timing-context.json" \
  -summary -
```

CI names both files outside the work directory, so a run that passed keeps its
measurements; it publishes the summary on the job, and uploads the pair together
with the samples `hack/e2e-resource-samples.sh` took while the lifecycle ran.
The samples are what separate a stage that was slow from one that was starved,
and the job records are what separate waiting for a runner from working on it.

Nothing in the stopwatch can decide a run: the outcome comes from the caller
that already knows it, and the call that closes a stage returns the status it
was given. `hack/e2e-timing-selftest.sh` is what keeps that true, and
`make e2e-static` runs it.

## What a change to the API owes

- Regenerate. `make generate manifests`, and commit what they wrote — a
  hand-edited CRD is a contract nothing reproduces.
- Review the generated schema, not only the Go type. An `+optional` that does not
  reach the CRD, a list that lost its `listType`, a validation that silently
  widened: each is invisible in the Go diff and plain in the YAML one.
- Treat an existing object as a user. A field that was required and is now
  absent, an enum that lost a value, a default that changed — every one of them
  is a stored object that stops validating.
- Give the field a caller before it ships. A setting nothing exercises is found
  first by whoever needs it, which is the worst place to find it. Unit tests are
  not that caller: they measure the plumbing, and the question is whether the
  path works. One row that sets the field and reaches the state it is for is
  enough, and it has to reach that state — a resource that stops at the gate
  proves the value was carried and says nothing about what it did.
- Give the reference an example. `docs/reference-examples/<kind>.md` is spliced
  into the generated page ahead of the field tables, and `make docs-reference`
  refuses a kind that has none. `hack/reference_examples_test.go` validates
  every example in them against the CRD the API server enforces, so an example
  cannot quietly go on naming a field the API dropped or a value it stopped
  accepting.
- Prefer no default to a plausible one. An unset field that changes nothing
  keeps every stored object running exactly as it ran, and leaves the choice
  with whoever knows their database. A default is a silent edit to every
  resource already in a cluster, and it pins this API to a value the thing it
  configures is free to move.

## What a change to a controller owes

`.agents/skills/operator-correctness/SKILL.md` carries the detail; the two rules
worth stating here because no gate catches them:

- **A non-nil error requeues by itself.** `return ctrl.Result{}, err` is the
  shape. `Requeue` is deprecated in the pinned controller-runtime and the error
  branch never reads `Result` at all, so pairing them is dead code that reads as
  intent.
- **envtest runs no built-in controllers.** Nothing there reconciles a
  Deployment into Pods or garbage-collects by owner reference, so a green
  envtest is evidence about this controller and about the API contract, and
  about nothing Kubernetes would have done. That belongs in `test/e2e`, against
  kind.

## What a proof owes

A lifecycle costs about ninety minutes on each of three Kubernetes minors, and
the migration phase stops at its first failure, so a proof that measures the
wrong thing hides every proof behind it and costs a day to find out. Three rules,
learned by paying that:

- **Assert the condition, never the phase it passes through.** A resource that
  has stopped still resolves, verifies and reads at its interval, so it is
  legitimately out of `Blocked` for part of every cycle while the refusal stays
  true throughout. A proof that demands the phase on every poll fails on the
  poll that lands mid-cycle, and reports the harness rather than the operator.
- **Assert your claim, not everything the status happened to say.** A count, a
  version or a reason that is true when the proof is written but belongs to
  something else's bookkeeping makes the proof fail on a change that never
  touched what it measures. `pendingCount` moved when the operator started
  reading Ptah's own selection; the row that had pinned it broke, and it was not
  about pending work at all.
- **If a phase has to be asserted, assert the document that matched it.** A
  loop that polls until a phase appears already holds the status that satisfied
  it; asserting against that document has no window at all, while re-reading
  afterwards reopens one. The window is usually small -- a resource waiting for
  a person sits there for a whole interval -- but it is the difference between
  a proof that cannot race and one that merely usually does not.
- **A filter has to be shown to refuse something.** Reading it again catches a
  reasoning error and misses the one that matters: a filter that passes its
  author's intent and measures something else reads correctly. `testdata/e2e/*.jq`
  hold the ones that earn a file, and `hack/migration-refusal-filter-selftest.sh`
  runs each against one reading it must accept and several it must refuse, which
  are the mistakes that were actually made.

## Language

American English in code, comments, documentation, issue and PR text, and
program output. Plain international English: short sentences, concrete nouns,
active voice. Say what changed and why, once.

`README.ja.md` is the Japanese translation of `README.md`, and
`docs/site/scripts/check-translations.mjs` holds the two together: the fenced
blocks match in order, so do the link, image and badge addresses, each file
links to the other, and the product name carries its reading once --
`Ptah（プタハ）` at the first mention, `Ptah` after it. Translate the prose and
leave the commands and the addresses byte for byte; a flag or a badge URL that
differs between the two files is a defect. The prose itself is review's, because
no gate can read it. stokaro/ptah carries the same gate over its own README as a
second copy, so a rule changed in one is changed in the other by hand.

No AI attribution anywhere in git or on the forge — no co-author trailer, no
generated-with footer, no session or transcript reference.
