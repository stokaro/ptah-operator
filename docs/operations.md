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
certificate lifecycle resources. A first interactive install can instead
generate a self-signed Secret and its built-in rotation Deployment. Argo CD and
Flux should depend on the CRDs and an externally managed TLS Secret before
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

Admission matches exact Job-controlled Pods on create and update, including
the `ephemeralcontainers` and `resize` subresources. It does not use mutable
labels as a selector. While the webhook is unavailable, every Job Pod creation
is fail-closed and may be delayed. The controller and certificate-rotation
Deployments remain recoverable because their Pods are ReplicaSet-owned. Run at
least two controller replicas and preserve the PodDisruptionBudget during
maintenance.

The Job-owner match condition is evaluated after mutating admission. A cluster
administrator who can install or change a cluster-scoped mutating webhook is
therefore inside the admission trust boundary: such a webhook could remove the
Job owner reference before validation. Ordinary workload creators cannot use
labels or extra owner references to bypass validation; any Job controller
reference enters the rule, and the handler rejects ambiguous ownership.

## Webhook certificate lifecycle

For a chart-generated webhook Secret, the chart stores `tls.crt`, `tls.key`,
`ca.crt`, and `ca.key` and schedules a separate certificate rotator. The
manager volume projects only `tls.crt` and `tls.key`; the CA certificate and CA
private key never enter manager Pods. The manager ClusterRole has no Secret
access. The rotator uses its own ServiceAccount with `get` and `update` limited
by `resourceNames` to the one generated Secret, one precreated coordination
Lease, and the exact mutating and validating webhook configurations. It can
only list EndpointSlices in the release namespace because Kubernetes assigns
their names dynamically and RBAC cannot constrain a list by label selector.

Serving certificates rotate before `certificateRotation.renewalThreshold`.
The CA rotates before its threshold, when it cannot safely issue a full-lived
serving certificate, or when a legacy generated Secret has no `ca.key`. An
expired certificate follows the same recovery path; the rotator talks to the
Kubernetes API and probes webhook endpoints directly, so recovery never
depends on a successful call through the expired admission webhook.

CA replacement is fail-closed and restart-safe:

1. Both webhook configurations receive an overlapping old-plus-new CA bundle.
2. One atomic Secret update replaces the serving certificate, serving key, CA,
   and CA key.
3. The rotator lists the exact Service EndpointSlices, rejects empty, unready,
   terminating, malformed, or duplicate endpoint sets, and performs a TLS
   handshake with every ready Pod IP using the Service DNS name. It requires
   the exact new leaf certificate, then requires a second identical endpoint
   snapshot.
4. Only after that proof do both webhook configurations contract to the new CA.

If the rotator stops between steps, its replacement expands both configurations
to a common overlap, repeats proof for every stable endpoint, and then contracts
trust. A timeout leaves overlapping trust in place. There is no interval in
which the API server is configured to trust neither the old nor the new
serving certificate. controller-runtime watches the projected `tls.crt` and
`tls.key` files and reloads them without restarting the manager Pods.

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
