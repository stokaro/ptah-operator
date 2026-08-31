# Operations

## Installation and upgrades

Install CRDs and the controller through the Helm chart. Supply digest-pinned
manager, executor, and runner images. The chart refuses all three when only a
tag is supplied. `image.allowMutableTag=true` exists solely for isolated test
clusters that load a locally built image and is never a production setting.

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
Within that namespace, every resource that can mutate one physical database
must also share the exact `spec.target.coordinationKey`, even when connection
URLs use different aliases, proxies, or credentials.

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

If an Apply outcome still needs convergence proof, suspension retains and
renews that operation's database-realm Lease. This deliberately blocks later
mutations in the same realm until the resource is resumed and the read-only
proof completes, or until the resource is deleted. Releasing the Lease while
retaining unresolved proof would let an intervening Apply contaminate the
audit result.

Deleting a `PtahSchema` never runs SQL. The transient finalizer exists only to
observe an already active operation and release coordination safely. Once no
operation is active, deletion removes Kubernetes-owned plans and Jobs through
normal garbage collection; database objects remain untouched.

## Failure recovery

- Read-only Resolve, Verify, Observe, and Plan failures retry after the bounded
  failure interval with a new deterministic Job attempt.
- A changed generation, policy, artifact, coordination key, route identity, or
  observed state discards the stale operation or plan before apply.
- Once a mutating child may have been dispatched, missing output, timeout,
  cancellation, or malformed output is `OutcomeUnknown`. The controller
  preserves the immutable Apply holder and permits only a fresh observation.
  If Job creation or identity is uncertain, observation waits for the complete
  possible mutation deadline while the holder is renewed. The plan is never
  blindly replayed.
- A native stale-plan refusal is accepted as pre-mutation only when its exact,
  untruncated diagnostic names the source fingerprint in the reconstructed
  immutable plan. The plan is cleared, its recorded approval becomes stale,
  and reconciliation observes the database again.
- A controller restart resumes the persisted operation and existing Job UID.
  Replacement or unrelated Jobs are rejected.
- A successful apply transitions to `VerifyingConvergence`; only a later
  same-target observation can record `status.applied` and `InSync=True`.

The operator does not perform automatic rollback. Repair the desired artifact
or database deliberately, then let observation produce a new plan.

## Observability

The chart exposes the controller-runtime Prometheus endpoint through the
optional metrics Service (`metrics.service.enabled=true` by default). The
operator metrics use only closed, bounded label sets; object names, artifact
digests, database URLs, SQL, and error strings are never labels.

- `ptah_operator_reconciliations_total{result}` counts successful and failed
  reconciliations.
- `ptah_operator_drift_observations_total{engine,outcome}` distinguishes
  detected drift from in-sync observations.
- `ptah_operator_plans_total{engine,destructive}` counts published immutable
  plans and exposes their conservative destructive classification.
- `ptah_operator_approvals_total{outcome}` counts required, accepted, and stale
  approval transitions.
- `ptah_operator_applies_total{outcome}` counts started, completed, uncertain,
  and stale Apply transitions.
- `ptah_operator_operation_duration_seconds{operation,outcome}` measures
  completed logical operations, including uncertain and stale outcomes.
- `ptah_operator_failures_total{stage,category}` separates infrastructure,
  configuration, operation, policy-change, stale-input, and uncertain-outcome
  failures by bounded state-machine stage.

Production logs are structured. Controller-runtime request context supplies the
managed object identity; lifecycle records add the closed `operation`, numeric
`attempt`, and `phase` fields. Reconciliation completion records include
`requeue` and `requeueAfter`. Executor stdout and stderr are treated as
untrusted secret-bearing data: only a strictly validated Plan may transport
native stdout. Resolve and Verify emit bounded typed evidence, Observe emits
only summaries, and Apply emits no native output. Native stderr never crosses
those operation boundaries; controller-facing failures are generic and typed.

Kubernetes Events provide the object-scoped audit trail for operation claims,
completion, policy refusal, approval changes, and failures. Alert on a sustained
increase in `ptah_operator_failures_total`, any increase in
`ptah_operator_applies_total{outcome="uncertain"}`, and missing successful
observations for longer than the configured reconciliation interval plus the
operation deadline. Use status Conditions and Events to identify the affected
object instead of adding unbounded identity labels to metrics.

## Plan retention

Plans and chunks are owned by the schema. Old plan objects may remain useful as
audit evidence until Kubernetes garbage collection removes the owning schema;
only the exact UID and fingerprint in `status.plan` are current. Completed Jobs
receive a short cleanup TTL before the controller clears the active operation;
failure to schedule that TTL leaves the operation retryable.

An executable plan is limited to 8 MiB, including its trailing newline. The
runner rejects a larger native plan before publication. Accepted bytes are
stored in immutable 512 KiB binary ConfigMap chunks; that chunk size leaves
headroom below the Kubernetes object-size limit after API JSON base64 encoding.

## Kubernetes versions

The supported minor window and update procedure are defined in
[Kubernetes support](kubernetes-support.md). A support-window change adds the
new minor and removes the oldest minor atomically, after the entire real-cluster
matrix succeeds.
