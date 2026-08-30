# Operations

## Installation and upgrades

Install CRDs and the controller through the Helm chart. Supply digest-pinned
executor and runner images; pinning the manager image with `image.digest` is
also recommended. The chart refuses execution images that are tags.

CRDs are stored under the chart's `crds/` directory and are installed before
templated resources. Review CRD schema changes before an upgrade because Helm
does not upgrade CRDs automatically. Apply the new CRDs explicitly, wait for
them to become Established, then upgrade the release.

For deterministic GitOps rendering, provision the webhook TLS Secret outside
the chart and set both `webhook.existingSecret` and the PEM-encoded
`webhook.caBundle`. A connected Helm install can instead reuse `ca.crt` from an
existing Secret; a first interactive install can generate a self-signed Secret.
Argo CD and Flux should depend on the CRDs and TLS Secret before synchronizing
the Deployment and webhook configurations.

Every operator instance that can manage the same databases must use the same
`coordination.namespace`. The chart defaults it to the release namespace.

## Normal status progression

Use the phase as a summary and Conditions as the stable machine interface:

```sh
kubectl -n <namespace> get ptahschema <name>
kubectl -n <namespace> get ptahschema <name> -o yaml
kubectl -n <namespace> get ptahschemaplan
kubectl -n <namespace> get events --field-selector involvedObject.name=<name>
```

`Ready=True` and `InSync=True` mean a read-only observation matched the verified
artifact. They are not inferred from an apply exit code. `ReadyToApply` means
the `Always` policy accepted a non-destructive plan without claiming that an
approval is needed; `AwaitingApproval` means an exact approval is required.
`Blocked` distinguishes deliberate policy refusal from an execution failure.

## Mutable tags and registry outages

Every reconciliation interval resolves the requested reference again. A moved
tag clears dependent plan and applied evidence, then repeats verification and
observation against the new digest. An old approval cannot authorize the new
plan.

Registry failures are fail-closed. The last resolved digest remains visible as
evidence, but the controller does not silently reinterpret a tag or apply a
plan whose verification inputs cannot be revalidated. Observe and plan also
need to fetch the digest-pinned schema, so a registry outage can delay database
checks without causing mutation.

Digest-pinned references reduce change ambiguity and are recommended for
production. Promote the same digest between environments rather than rebuilding
equivalent tags.

## Suspension and deletion

Setting `spec.suspend=true` prevents new Jobs. If an apply is already active,
the controller continues observing that Job until its outcome is safe to
classify; it does not delete the Pod mid-mutation.

Deleting a `PtahSchema` never runs SQL. The transient finalizer exists only to
observe an already active operation and release coordination safely. Once no
operation is active, deletion removes Kubernetes-owned plans and Jobs through
normal garbage collection; database objects remain untouched.

## Failure recovery

- Read-only Resolve, Verify, Observe, and Plan failures retry after the bounded
  failure interval with a new deterministic Job attempt.
- A changed generation, policy, artifact, target identity, or observed state
  discards the stale operation or plan before apply.
- Once a mutating child may have been dispatched, missing output, timeout,
  cancellation, or malformed output is `OutcomeUnknown`. The controller
  releases its Lease and performs a fresh observation. It never blindly
  replays the plan.
- A controller restart resumes the persisted operation and existing Job UID.
  Replacement or unrelated Jobs are rejected.
- A successful apply transitions to `VerifyingConvergence`; only a later
  observation can record `status.applied` and `InSync=True`.

The operator does not perform automatic rollback. Repair the desired artifact
or database deliberately, then let observation produce a new plan.

## Plan retention

Plans and chunks are owned by the schema. Old plan objects may remain useful as
audit evidence until Kubernetes garbage collection removes the owning schema;
only the exact UID and fingerprint in `status.plan` are current. Completed Jobs
receive a short cleanup TTL after their framed result has been harvested.

## Kubernetes versions

The supported minor window and update procedure are defined in
[Kubernetes support](kubernetes-support.md). A support-window change adds the
new minor and removes the oldest minor atomically, after the entire real-cluster
matrix succeeds.
