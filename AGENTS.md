# AGENTS.md

Repository-local guidance for coding agents working in `ptah-operator`.

## What this is

A Kubernetes operator for Ptah, scaffolded with Kubebuilder v4 under the group
`ptah.run`. The API is `api/v1alpha1` — `PtahSchema`, `PtahSchemaPlan`,
`PtahSchemaApproval` — and the work happens in `internal/`, which holds the
controllers, admission, certificate rotation, the CRD upgrade path, the plan
store and the runner. Four programs ship from `cmd/`: the manager, the
certificate rotator, the CRD manager and the runner.

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
make build          # the four binaries
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

CI runs on every push to `master`, and `cancel-in-progress` is true only for a
pull request: a master run that has started is never cancelled. What a batch of
merges loses is the run that had not started yet. The concurrency group holds
one pending run, so while one commit's lifecycle occupies the group, the next
merge queues and the merge after that cancels the queued one. Measured on
`43534c4`: its run was created at 05:56:15 and cancelled fifteen seconds later,
when `17565ba` arrived. The commit then carries no check at all, which reads
exactly like a commit nothing objected to.

That is the trade, taken on purpose. One run spends about seven hours of runner
time (three kind lifecycles near two hours apiece, plus the race detector at
forty minutes, measured on `75387d4`), and a commit in the middle of a batch
would spend it re-proving what the tip proves.

So "master is green" is a statement about the commits whose runs survived, not
about every commit, and a bisect cannot assume a commit it lands on was ever
built. When one commit has to carry its own verdict, a release candidate or a
change to the lifecycle path itself, put it on a branch and let the pull request
run: it fans out over the same three minors.

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

No AI attribution anywhere in git or on the forge — no co-author trailer, no
generated-with footer, no session or transcript reference.
