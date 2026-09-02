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
existing Secret. Setting `webhook.existingSecret` completely disables built-in
certificate lifecycle resources. Disabling `certificateRotation.enabled`
therefore requires `webhook.existingSecret`; the chart refuses an unmanaged
generated certificate. A first interactive install can instead generate a
self-signed Secret and its built-in rotation Deployment. Argo CD and Flux
should depend on the CRDs and an externally managed TLS Secret before
synchronizing the Deployment and webhook configurations.

Install exactly one Helm release of the operator in a cluster. The manager
watches cluster-wide resources, and admission uses the singleton
`ptah-operator-admission` webhook configurations; a second release would create
an independent availability domain capable of blocking the first. The fixed
configuration names make an ordinary second Helm install fail ownership
validation. High availability is provided by `replicaCount` within the one
release, with leader election enabled. Helm rejects more than one replica when
`leaderElection=false`; disabling election is supported only for an isolated
cluster with one operator release and one replica.

`coordination.namespace` contains the fixed manager leader-election Lease and
the database target Leases. It defaults to the release namespace and may name
a separately administered namespace for the same release. Within that
namespace, every resource that can mutate one physical database must share the
exact `spec.target.coordinationKey`, even when connection URLs use different
aliases, proxies, or credentials.

The manager has exact `get`, `create`, and `update` access to Leases through
one namespace Role in `coordination.namespace`. When that namespace differs
from the release namespace, its RoleBinding names the manager ServiceAccount
across namespaces. The manager has no cluster-wide Lease rule and no Lease
access in unrelated namespaces. Certificate rotation uses a different
ServiceAccount and its exact release-namespace Lease grant.

### Kubernetes admission configuration

The operator persists a bounded snapshot of built-in Pod admission before it
dispatches an operation Job. Its final Pod validating webhook accepts the
exact snapshotted LimitRange resource defaults, ServiceAccount image pull
secrets, RuntimeClass scheduling and overhead, PriorityClass values, and the
configured admission-plugin mutations. It rejects other changes to resources,
tolerations, image pull policy, command, image, environment, volumes, or
security settings before the Pod can be scheduled.

For regular and init containers, Kubernetes copies an admitted limit into a
previously absent request after admission. The envelope accepts that API
default only when the request is exactly equal to the separately validated
limit. LimitRange API defaulting derives a missing container default limit
from `max`, then derives a missing default request from that limit, and finally
from `min`. The snapshot records those derived values explicitly. If more than
one bound LimitRange offers a different value for the same unset field, the
controller rejects dispatch instead of depending on list-order-dependent
first-wins behavior. At most 64 request and limit default entries are retained
across the complete UID- and resourceVersion-bound set.

The `admission` chart values must match the kube-apiserver admission chain:

- `defaultTolerationsEnabled` defaults to `true`. When enabled,
  `defaultNotReadyTolerationSeconds` and
  `defaultUnreachableTolerationSeconds` default to 300 and must match the API
  server flags. Zero means an exact zero-second toleration; it does not disable
  the plugin. When disabled, both automatically injected tolerations must be
  absent.
- `extendedResourceTolerationEnabled` defaults to `false`. Enable it only when
  kube-apiserver enables `ExtendedResourceToleration`.
- `alwaysPullImagesEnabled` defaults to `false`. Enable it only when
  kube-apiserver enables `AlwaysPullImages`.

A mismatch fails closed by rejecting the operation Pod. Updating a referenced
LimitRange, ServiceAccount, RuntimeClass, or PriorityClass after snapshot
persistence also rejects newly mutated Pods; retry reconciliation after the
desired admission objects are stable. The manager receives only read access to
those credential-free objects. It receives neither Secret read permission nor
Pod create or delete permission.

The webhook deliberately does not model `PodNodeSelector` or
`PodTolerationRestriction` defaults; clusters that inject either into operation
Pods are rejected rather than silently broadening scheduling. ServiceAccount
token projection is also rejected because every operation template sets
`automountServiceAccountToken=false`. Arbitrary mutating-webhook changes remain
outside the envelope. Priority, RuntimeClass, LimitRange, ServiceAccount image
pull secrets, DefaultTolerationSeconds, ExtendedResourceToleration, and
AlwaysPullImages are the explicitly modeled mutations.

`PodLevelResources` is enabled by default in the supported Kubernetes window,
but those releases apply LimitRange `default` and `defaultRequest` only to
`Container` items. Operation templates leave `spec.resources` unset, and the
controller rejects a non-nil Pod-level resource stanza before dispatch. A
future or customized API server that injects Pod-level resources fails closed
until that mutation has an explicit, versioned snapshot model.

Admission selects Pods carrying both
`app.kubernetes.io/managed-by=ptah-operator` and
`app.kubernetes.io/component=schema-operation`, then requires an exact Job
controller owner on create or on either side of an update. Kubernetes evaluates
the object selector against both the new and old object, so removing either
identity label from an existing operation Pod still invokes the webhook and is
denied. The same scope covers the `ephemeralcontainers` and `resize`
subresources. A foreign Job cannot gain admission by copying the labels: the
handler also binds the Job to the exact current PtahSchema, operation, and
persisted Pod intent.

While the webhook is unavailable, matching operation Pods fail closed, but
unrelated Job Pods remain available. The controller and certificate-rotation
Deployments remain recoverable because their Pods are ReplicaSet-owned. Run at
least two controller replicas and preserve the PodDisruptionBudget during
maintenance.

The object selector and Job-owner match condition are evaluated after mutating
admission. A cluster administrator who can install or change a cluster-scoped
mutating webhook is therefore inside the admission trust boundary: such a
webhook could remove the managed identity labels or Job owner reference before
validation. Ordinary workload creators cannot use copied labels or extra owner
references to gain operation admission; matching requests enter the rule, and
the handler rejects foreign or ambiguous ownership.

## Webhook certificate lifecycle

For a chart-generated webhook Secret, the chart stores `tls.crt`, `tls.key`,
`ca.crt`, and `ca.key` and schedules a separate certificate rotator. The
manager volume projects only `tls.crt` and `tls.key`; the CA certificate and CA
private key never enter manager Pods. The manager ClusterRole has no Secret
access. The rotator uses its own ServiceAccount with `get` and `update` limited
by `resourceNames` to the one generated Secret, one precreated coordination
Lease, and the exact mutating and validating webhook configurations. By
default, `certificateRotation.recreateMissingSecret=false`: the chart grants no
Secret `create`, renders no Secret-creation admission policy or binding, and
grants no read access to those policy types. A deleted Secret therefore makes
the rotator fail clearly and remain unready until an administrator restores it
through a controlled Helm or GitOps operation.

Set `certificateRotation.recreateMissingSecret=true` only when automatic
deletion recovery is required. Secret `create` cannot be restricted by
`resourceNames`, so this opt-in adds a namespace-wide RBAC verb plus a
fail-closed `ValidatingAdmissionPolicy` and binding that limit the rotator
ServiceAccount to one exact TLS Secret name, namespace, managed label, and four
nonempty data fields. The rotator requires the policy's current generation to
be type-checked, verifies the complete policy and binding structure, and runs
negative server-side dry runs before using `create`.

The single-release opt-in has a bootstrap tradeoff: Helm cannot atomically
establish the admission policy and grant RBAC. A namespace-wide `create` grant
can therefore exist briefly before policy enforcement is established. The
rotator will not use the grant during that interval, but that runtime check
cannot constrain a compromised ServiceAccount acting outside the rotator.
Leaving the default disabled removes both the broad verb and this ordering
window. The rotator can only list EndpointSlices in the release namespace
because Kubernetes assigns their names dynamically and RBAC cannot constrain a
list by label selector.

Serving certificates rotate before `certificateRotation.renewalThreshold`.
The CA rotates before its threshold, when it cannot safely issue a full-lived
serving certificate, when a legacy generated Secret has no `ca.key`, or when
`ca.crt` is missing, malformed, or unrelated to the serving leaf. Candidate
CAs from managed webhook entries are filtered certificate-by-certificate. A
candidate is retained only when it signs the exact persisted leaf for every
Service DNS name and every stable endpoint proves it is serving that byte-exact
leaf. Unrelated roots are never copied between entries. An expired certificate
follows the same recovery path; the rotator talks to the Kubernetes API and
probes webhook endpoints directly, so recovery never depends on a successful
call through the expired admission webhook.

CA replacement is fail-closed and restart-safe:

1. Every exact managed webhook entry retains its own parseable prior
   certificates and receives the next CA. A current signer may be shared only
   after its signature and exact live-leaf identity are independently proved;
   unauthenticated entry-local prior trust is never copied to another entry.
2. One atomic Secret update replaces the serving certificate, serving key, CA,
   and CA key.
3. The rotator lists the exact Service EndpointSlices, rejects empty, unready,
   terminating, malformed, or duplicate endpoint sets, and performs a TLS
   handshake with every ready Pod IP using the Service DNS name. It requires
   the exact new leaf certificate, then requires a second identical endpoint
   snapshot.
4. Only after that proof do both webhook configurations contract to the new CA.

If the rotator stops between steps, its replacement expands both configurations
independently from each entry's own trust, repeats proof for every stable
endpoint, and then contracts trust. A timeout leaves each entry's prior trust
plus the authoritative CA in place. Ordinary rotation never configures the API
server to trust neither the old nor the new serving certificate.
controller-runtime watches the projected `tls.crt` and `tls.key` files and
reloads them without restarting the manager Pods.

A generated Secret containing valid material with an empty or `Opaque` type is
normalized to `kubernetes.io/tls`. The no-rotation path updates only the Secret
type and the four managed material fields before repairing trust; rotation
paths normalize the type as part of their existing atomic material update.

With missing-Secret recreation enabled, the rotator first appends a newly
generated CA to each exact entry. It preserves every parseable certificate
candidate from that entry even when neighboring bytes are malformed, without
borrowing trust from another entry. It then recreates only the
policy-constrained Secret, accepts an uncertain or racing create only after
byte-exact read-back, proves the new leaf at every stable endpoint, and
contracts every entry to the new CA. Admission remains fail-closed while Pods
reload the recreated certificate. Manager readiness is tied to the webhook
server's started checker rather than a process-only ping.

The conservative defaults reconcile immediately at startup and every six
hours, bound each reconciliation to 15 minutes, and retry failures with jittered
exponential backoff from five seconds to five minutes. Liveness reports whether
the supervisor is running; readiness is true only after a successful complete
reconciliation and becomes false during retries. The Deployment exposes those
states on `/healthz` and `/readyz` without serving certificate or key material.
It renews 30 days before expiry, issues 90-day serving certificates and
three-year CAs, and allows five minutes for Secret projection plus endpoint
proof. `probeTimeout` must exceed `probeInterval`; serving validity must exceed
the renewal threshold; CA validity must exceed serving validity. The separately
authorized, ReplicaSet-owned Deployment is deliberately outside the all-Job
admission rule, so it can restart and repair trust when the webhook certificate
or CA bundle is already broken. Its precreated Lease serializes reconciliation.
To request an immediate reconciliation without changing the interval, restart
that Deployment:

```sh
kubectl -n <namespace> rollout restart \
  deployment/<certificate-rotation-deployment>
```

Never copy `ca.key` into diagnostics. Inspect Deployment status and structured
error messages; certificate and key bytes are intentionally absent from logs.

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
`ReadyToApply`, `AwaitingApproval`, and `Blocked` all retain a read-only refresh
deadline so the cadence survives controller restarts. An eligible Apply can be
reserved immediately before that deadline; once it is due, a fresh resolution
and observation take priority over automatic policy or approval consumption.
After policy and, when required, the exact recorded approval pass final
validation, the controller captures one timestamp, checks it against that
deadline, and stores the same value as the Apply operation's start time. That
durable Apply claim is the one-shot authorization boundary: dispatch, lock
contention, and controller restarts continue the claimed operation instead of
letting a later timer reinterpret it.

Plans also bind the manager's complete execution identity: Ptah version,
executor image digest, runner image digest, and runner protocol. If that
identity changes before a mutating Job is dispatched, the current plan is no
longer executable. A recorded approval becomes stale with reason
`ExecutionBindingChanged`. A claimed but undispatched Apply releases its old
authorization and database lock, the plan is cleared, and the replacement
manager runs Resolve, Verify, Observe, and Plan again. Approve only the
replacement plan; do not recreate an approval against the old UID or
fingerprint. A dispatched Apply is never recreated under the new binding; its
outcome is handled conservatively and requires post-Apply observation.

The audit-visible `status.executionBinding` records that component tuple plus
an opaque `epoch`. The epoch changes for every observed component transition,
including a rollback to a byte-identical tuple. A plan and approval carry the
epoch as `spec.executionBindingID`, so approval is one-shot for that exact
transition: it cannot become valid again after a later rollout or rollback,
even if all four component fields return to their previous values.

A normal rolling upgrade has a leader-handoff boundary: before the old manager
stops, it may still dispatch an approval that is valid for the complete old
execution binding. That Job remains internally consistent and is never changed
to the replacement binding. If an upgrade maintenance window requires that no
new Apply be dispatched after the window begins, first scale the manager
Deployment to zero and wait until all manager Pods have terminated. Then run
`helm upgrade --wait`; the chart restores the configured replica count and the
replacement manager invalidates every undispatched old-binding authorization
before it can mutate a database.

## Mutable tags and registry outages

Every reconciliation interval resolves the requested reference again. A moved
tag clears dependent plan and applied evidence, then repeats verification and
observation against the new digest. An old approval cannot authorize the new
plan. This cadence also applies while a plan is ready to apply, waiting for
approval, or `Blocked`: read-only verification, observation, and planning
continue, the same immutable plan returns as current when its inputs are
unchanged, and no `Apply` Job is created during the refresh. A moved tag or
database drift observed by a due refresh therefore invalidates stale plan or
approval evidence before mutation can begin.

Native verification always inspects the resolved digest. When the selected
policy requires `require_digest_pin`, the runner also evaluates the original
requested reference: a tag or implicit `latest` is refused even though it
resolved successfully, while an explicit digest remains eligible. Policies
that permit tags retain the refresh behavior above without weakening any later
digest binding.

Registry failures are fail-closed. During a failed refresh of a previously
resolved source, the last source, target, plan, applied record, and successful
reconciliation timestamp remain historical evidence; they are not a claim of
currentness. `ArtifactResolved=Unknown` with reason `RefreshFailed`, while
`PlanReady` and `InSync` become `Unknown` with reason
`SourceFreshnessUnknown`. No Verify, Observe, Plan, or Apply follows that
failed Resolve. After connectivity returns, the operator must resolve and
verify again, then observe and plan. A same-digest, no-drift recovery ends with
`PlanReady=False`/`NoChanges`, `InSync=True`/`ScopedConverged`, and no Apply.
Observe and plan also need to fetch the digest-pinned schema, so an outage can
delay database checks without causing mutation.

Every registry-authentication Secret must include `registry: <host[:port]>`,
matching the OCI client's effective request authority. This applies to both
environment-key and Docker config JSON representations; the latter retains its
standard `.dockerconfigjson` key alongside the fixed grant. A source under
`oci://docker.io/...` therefore grants `registry-1.docker.io`. Missing,
malformed, or mismatched authority grants stop the Job before Ptah can make a
registry request. Authenticated plain HTTP also requires
`allowPlainHTTP: "true"` in the authentication Secret. Rotate those values only
when intentionally changing the credential's authority or transport grant.
`clientCertificateFrom` is currently refused because the executor cannot scope
the certificate safely across cross-host redirects.

An authenticated source with `transport.caFrom` must also place
`caSHA256: sha256:<64 lowercase hex>` in that same registry Secret. Compute the
digest over the exact selected ConfigMap value, including its final newline if
present. Missing or changed grant bytes fail before the registry client starts.
The operator snapshots the selected CA once per Job, so a projected ConfigMap
update cannot change trust roots between validation and fetch. Rotate a custom
CA and its Secret-owned digest grant together; until both projections agree,
read-only source operations fail closed. Anonymous custom-CA sources do not
need a Secret grant but use the same per-Job snapshot boundary and 1 MiB limit.

Digest-pinned references reduce change ambiguity and are recommended for
production. Promote the same digest between environments rather than rebuilding
equivalent tags.

## External database targets

The operator does not provision a database. An operation Job only needs a
network route to the target and a namespace-local Secret containing the URL
selected by `spec.target.urlFrom`. That target may be a managed service, a
private endpoint, or a database outside Kubernetes. Keep the Secret scoped to
the schema namespace, make the selected key nonempty, and set one stable
`coordinationKey` for every URL alias that reaches the same physical database.

A selectorless Service plus a manually managed EndpointSlice is one way to
give an external address stable in-cluster DNS. Bind the EndpointSlice to the
exact Service UID, set a distinct `endpointslice.kubernetes.io/managed-by`
label for the component that owns it, publish only the database port, and
arrange lifecycle management for address changes; the operator deliberately
does not rewrite that route. NetworkPolicy, firewall rules, TLS, database
privileges, backups, and high availability remain the platform owner's
responsibility. The runtime role needs enough DDL authority for the requested
migrations but should not be a database superuser. Database ports do not need
to be exposed on a Kubernetes node or on the host running a kind test cluster.

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
