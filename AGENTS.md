# AGENTS.md

Repository-local guidance for coding agents working in `ptah-operator`.

## What this is

A Kubernetes operator for Ptah, scaffolded with Kubebuilder v4 under the group
`ptah.dev`. The API is `api/v1alpha1` — `PtahSchema`, `PtahSchemaPlan`,
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

## What a change to the API owes

- Regenerate. `make generate manifests`, and commit what they wrote — a
  hand-edited CRD is a contract nothing reproduces.
- Review the generated schema, not only the Go type. An `+optional` that does not
  reach the CRD, a list that lost its `listType`, a validation that silently
  widened: each is invisible in the Go diff and plain in the YAML one.
- Treat an existing object as a user. A field that was required and is now
  absent, an enum that lost a value, a default that changed — every one of them
  is a stored object that stops validating.

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

## Language

American English in code, comments, documentation, issue and PR text, and
program output. Plain international English: short sentences, concrete nouns,
active voice. Say what changed and why, once.

No AI attribution anywhere in git or on the forge — no co-author trailer, no
generated-with footer, no session or transcript reference.
