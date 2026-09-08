# Agent skills

Instructions an agent loads when the task calls for them. Three are vendored
from upstream at a pinned commit; one is written here.

A vendored copy is deliberate. A skill is instructions an agent will follow, so
it deserves the review a dependency gets — and pinning is what makes "the rules
changed under us" visible as a diff rather than as a behavior change.

| Skill | Origin | Pinned at | License |
| --- | --- | --- | --- |
| `kubebuilder-api-design` | [ConfigButler/skills](https://github.com/ConfigButler/skills) | `f4f7463` | Apache-2.0 |
| `k8s-crd-design-review` | [ConfigButler/skills](https://github.com/ConfigButler/skills) | `f4f7463` | Apache-2.0 |
| `kubernetes-skill` | [LukasNiessen/kubernetes-skill](https://github.com/LukasNiessen/kubernetes-skill) | `f85547f` | MIT |
| `operator-correctness` | written here | — | — |

## What each is for

**`kubebuilder-api-design`** shapes Go API types and `+kubebuilder` markers so
generation emits the CRD schema that was intended. It scopes itself to the API
and hands the generated YAML to the review skill below; controller logic is
explicitly out of its scope.

**`k8s-crd-design-review`** reads a generated CRD as a compiled API contract:
compatibility of a change, CEL validation, list semantics under Server-Side
Apply, references to other objects, and versioning.

**`kubernetes-skill`** covers manifests, Helm and Kustomize by failure mode
rather than by resource — insecure workload defaults, resource starvation,
network exposure, privilege sprawl, fragile rollouts, API drift — and names the
validation to run. It is for `charts/`, `config/` and the RBAC, not for the
reconciler.

**`operator-correctness`** is the half the vendored three leave out: the
reconcile return contract, requeue semantics, what a status write does to
events, cached reads and conflicts, ownership and finalizers, and what envtest
can prove. Its facts are read out of the `controller-runtime` version this
repository pins.

## What was reviewed, and what was rejected

Two widely-starred operator skills were read and not vendored. Both carry
technical claims that are wrong against `sigs.k8s.io/controller-runtime v0.24.1`,
which this repository pins:

- one instructs the agent to return `ctrl.Result{Requeue: true}, err`, and ships
  a scripted check that flags code which does not. The error branch in
  `pkg/internal/controller/controller.go` requeues with the rate limiter and
  never reads `result`, and `Requeue` carries `Deprecated: Use RequeueAfter
  instead` in `pkg/reconcile/reconcile.go`. The rule asks for a deprecated field
  in a position where it is ignored;
- both state that a status subresource prevents a reconcile after a status
  write. It prevents the `metadata.generation` bump, not the update event; the
  skip comes from `GenerationChangedPredicate`, whose own documentation says so
  and adds two caveats worth knowing.

A skill is not a library, but it fails the same way: quietly, by being followed.

## Updating a vendored skill

Fetch the upstream files at the new commit, read the diff, and update the pinned
commit in the table above in the same change. A vendored copy whose pin no
longer matches what was reviewed is worse than no pin at all.
