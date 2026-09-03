# Operations

## Installation and upgrades

Install CRDs and the controller through the Helm chart. Supply digest-pinned
manager, executor, and runner images. The chart refuses all three when only a
tag is supplied. `image.allowMutableTag=true` exists solely for isolated test
clusters that load a locally built image and is never a production setting. It
also requires `image.testIdentityDigest` to equal that loaded image's exact
lowercase `sha256:` Docker image ID; the manager records
`<repository>@<image-ID>` as its content identity while the Pod still pulls the
test tag. Production `image.digest` mode forbids `image.testIdentityDigest`.

CRDs are stored under the chart's `crds/` directory and are installed before
templated resources. The candidate operator image embeds the exact same
generated CRDs. The same reconciliation hook runs for `pre-install` and
`pre-upgrade`, which makes a fresh installation over CRDs retained by an
earlier uninstall safe and usable. It first scans every durable
controller-state version described below. Only after that downgrade preflight
succeeds does it dry-run every required schema change, update only the `spec`
and the two owned schema-identity annotations of each existing CRD, and wait
for both `Established=True` and `NamesAccepted=True`. Helm does not roll the
manager Deployment until that hook succeeds.

The hook never creates, deletes, or force-applies a CRD. A missing CRD, an API
identity conflict, a stored version absent from the candidate, a rejected
dry-run, or an incompatible schema identity introduced concurrently makes the
Helm operation fail. The
dedicated hook ServiceAccount can `get` and `update` only the three exact Ptah
CRD names. Separate read-only `list` grants for `PtahSchema`, `PtahSchemaPlan`,
and `PtahSchemaApproval` are the only non-exact-name hook permissions and exist
solely for the downgrade preflight.
Hook RBAC is removed after either success or failure. Replacing only `spec` and
those two owned annotations preserves CRD UIDs, all other metadata, status,
and all custom resources. The manager and certificate-rotation Pods also run
the embedded verifier as an init container
with read-only, exact-name CRD and admission-configuration access. A partial
update, later schema drift, or admission singleton owned by another release
therefore cannot start either mutating process.

Every generated CRD carries a schema-identity pair: the positive decimal
`operator.ptah.dev/crd-schema-version` rollback fence and
`operator.ptah.dev/crd-schema-digest`, a lowercase SHA-256 digest of its
normalized `spec`. Every deliberate generated CRD schema change must increase
`CRD_SCHEMA_VERSION` in the Makefile and regenerate the base, chart, and
embedded copies together; the generator derives the digest. Before any dry-run
or real update, the hook checks all three live versions and refuses an existing
version newer than the candidate. It also refuses two different digests bound
to the same version. This machine-enforced binding prevents an older image from
narrowing a newer schema even if a developer forgot to bump the version.

Repository verification independently recomputes every annotated digest and
compares the complete generated CRD set with an exact Git baseline. A changed
normalized `spec` requires one shared schema version that is strictly newer;
an unchanged set rejects a version bump, rollback, and added or removed CRDs.
Pull-request CI uses the exact base commit, a default-branch push uses the
event's previous commit, and scheduled or manual CI uses the candidate's
parent. CI refuses a missing or invalid baseline. Locally, uncommitted generated
changes compare with `HEAD`, while a clean just-committed tree compares with
`HEAD^`; `CRD_SCHEMA_BASELINE_REF` selects an explicit baseline and
`CRD_SCHEMA_REQUIRE_EXPLICIT_BASELINE=true` disables that local convenience.
The sole initial transition accepts either a repository baseline with no CRD
files or a complete baseline carrying neither owned annotation, and only when
the candidate is the complete generated set at shared schema version 1. A
partial baseline remains a set-integrity failure rather than a bootstrap.

A missing version or digest is accepted only when the live normalized CRD
`spec` already matches the candidate exactly; the hook then performs an
annotation-only legacy identity adoption. A malformed annotation, an incomplete
identity plus any schema difference, or a same-version digest collision fails
before all CRD mutations. For such an installation, keep the managers offline
and restore or select the historical operator release whose generated schema
exactly matches the live CRD. Let that release adopt the complete identity,
then upgrade through versioned schemas. If no matching release can be
identified, back up every CRD and custom resource and perform a separately
reviewed offline schema migration. The operator intentionally provides no
value that labels an unknown schema as trusted.

Before starting a manager, the init verifier scans every `PtahSchema`,
`PtahSchemaPlan`, and `PtahSchemaApproval` across the cluster. It checks
controller-state versions in `PtahSchema.status.executionBinding`,
`status.plan`, `status.applied`, and `status.pendingObservation.plan`, plus the
immutable `spec.controllerStateVersion` carried by every plan and approval.
Every kind is read through exhaustive pagination anchored to its own single
collection `resourceVersion`. A nonzero version newer than the binary's
supported controller-state version blocks the rollout, even if another stored
location is absent or still legacy. During a Helm downgrade, the pre-upgrade
hook fails before Helm changes the Deployment, so the newer ready manager Pods
remain in place and the older candidate never gets an opportunity to
reinterpret or rewrite future state.
Missing or zero versions remain readable as legacy state and are upgraded only
by ordinary reconciliation under the current manager. A malformed or negative
stored version also blocks startup. The upgrade hook repeats this state scan
after all server-side dry-runs, immediately before release cutover. After the
old runtime Pods have stopped, it performs a third scan immediately before the
first real CRD update. The controller init verifier repeats both CRD and state
checks after the admission singleton becomes ready. A missing client for any
of the three durable resource collections fails closed.

CRD updates are necessarily separate Kubernetes API transactions. The complete
dry-run prevents predictable partial upgrades, but an API failure or concurrent
administrator change can still interrupt the real update sequence. In that
case, leave the old running manager in place, resolve the API or policy failure,
and rerun the same candidate upgrade. Do not edit the remaining CRDs to imitate
the candidate and do not use server-side apply conflict forcing. A rollback to
an image whose embedded schemas differ also remains blocked by its init
verifier; select a manager version compatible with the schemas already stored.

Helm retains CRDs and their custom resources on uninstall. Back them up before
schema work anyway; uninstalling the release removes the controller and
admission resources, not the database changes previously executed by Ptah.

Uninstall is a fail-closed, two-phase operation. A read/update-only hook first
verifies the complete release, admission, RBAC, ServiceAccount, and workload
inventory before scaling the two exact runtime Deployments to zero. A separate
cleanup identity then removes only the candidate release's exact bindings and
chart-created ServiceAccounts. It self-revokes that temporary delete authority
before any admission guard is removed. The remaining identity can only read
the teardown inventory, submit SubjectAccessReviews and
SelfSubjectAccessReviews, and delete the exact admission and activation objects
compiled into the candidate.

Every chart workload uses a Pod-bound projected ServiceAccount token. After
the runtime Pods are gone and the retired chart-created ServiceAccounts have
been deleted, cleanup retains the admission boundary for another 65 seconds.
Kubernetes invalidates a bound credential no later than 60 seconds after the
bound Pod or ServiceAccount enters deletion, including when finalizers delay
object removal. The five-second margin prevents a formerly mounted credential
from outliving the admission guards. The cleanup Job's own bound credential is
still live and is evaluated separately as described below. This delay relies
on the documented Kubernetes authenticator contract; the authorization probes
that follow prove revocation of chart-issued permissions, not bearer-token
rejection.

Before removing admission, the cleanup hook sends every revocation probe
directly to every ready and serving address advertised by the
`default/kubernetes` Service. Each connection verifies the normal Kubernetes
Service TLS name and cluster CA. Discovery is repeated before every sweep. An
added or removed address, or any discovery error, restarts the stable interval;
a non-terminating address that is not ready and serving blocks completion.
Terminating addresses may leave the set, but that smaller set must itself stay
stable for the full interval. This proves every continuously advertised address
rather than assuming that an EndpointSlice address maps one-to-one to an API
server process.

Canonical SubjectAccessReviews bind every retired runtime and hook
ServiceAccount to only the exact mutating permissions that its deleted
release bindings issued; permissions issued exclusively to another identity
are not cross-probed. A normal Kubernetes RBAC no-opinion result
(`allowed=false`, `denied=false`) is successful; an allow or evaluation error
is not. The cleanup Job is checked through SelfSubjectAccessReview using its
actual bearer token on every address, so its ServiceAccount UID, Pod and node
binding, and credential identifier come from the authenticator rather than a
synthetic identity. The stable interval starts only after all of those results
are not allowed. Missing, malformed, changing, unreachable, or TLS-invalid
endpoint inventory blocks uninstall instead of assuming authorization cache
convergence.

If `serviceAccount.create=false`, the named controller ServiceAccount must be
dedicated to this release. Any additional direct RoleBinding or
ClusterRoleBinding for that identity, including a binding in another
namespace, blocks uninstall because the operator cannot prove safe privilege
retirement without taking ownership of user RBAC. Remove or migrate that
binding and retry. The user-created ServiceAccount itself is never deleted.
Its use by the controller must remain limited to the chart's Pod-bound token;
unbound credentials or grants from an external authorizer are outside the
enumerable Kubernetes RBAC contract and must be retired by the administrator
before uninstall.

A failed preflight leaves the runtime unchanged. Helm deletes the failed hook
Job immediately, while the failed release revision retains the exact hook
name, weight, event, phase, and timestamps used by the support-window proof.
An API failure during the
subsequent two-Deployment quiesce can leave one exact Deployment at zero, but
the admission boundary remains and the same operation is safe to retry. A
later cleanup failure likewise leaves admission in place and retains the
failed Job and Pod for diagnostics. After correcting the reported conflict or
API reachability problem, rerun the same `helm uninstall`; the hooks revalidate
live state and resume idempotently from the exact stored identities. Do not
delete the remaining guards by hand merely to make an uninstall pass.

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
cluster with one operator release and one replica. The manager Deployment uses
`Recreate` for both modes. Every ready replica serves admission traffic even
when it does not hold the reconciliation Lease, so an old and a new revision
must never overlap behind the webhook Service. Upgrades are briefly unavailable
and fail closed; after rollout, multiple elected replicas again provide
steady-state availability and single-writer reconciliation.

Before quiescing the old runtime, the upgrade hook projects the exact candidate
Pod requests and limits through every synchronized Pod ResourceQuota in the
release namespace. After the old Pods disappear, it waits through stale quota
status and transient API failures until a resource-version-anchored
ResourceQuota/Pod/ResourceQuota observation shows capacity for the candidate.
This check cannot reserve capacity against unrelated namespace writers. If
another workload consumes that capacity after the observation, Kubernetes may
temporarily report `FailedCreate` for the replacement Deployment. Admission
remains fail closed; free the required quota and the Deployment controller will
retry candidate Pod creation. Treat a Helm `--wait` timeout in that state as a
capacity incident, not as permission to remove the rollout guards.

Both fixed admission configurations record the owning release name and
namespace, the effective coordination namespace, the leader-election mode, and
the fixed leader-election ID in `operator.ptah.dev/*` annotations. Connected
Helm rendering uses `lookup` and fails when either singleton is missing its
peer, lacks an annotation, or disagrees with the requested values. This blocks
a second release and blocks changes to `coordination.namespace` or
`leaderElection` before either the CRD hook or manager Deployment can mutate
cluster state. There is no value that bypasses this check.

The runtime verifier closes the remaining concurrent-install race in which two
clients could both render while the singleton was absent. New manager and
certificate-rotation Pods wait for both fixed admission configurations and
require their complete annotation tuple to match the Pod's release. They also
require exactly one mutating approval webhook and three validating webhooks for
approval, operation-Pod intent, and controller writes, with fail-closed
policies, nonempty CA bundles, the exact Service, paths, port, rules, selectors,
match conditions, review version, side-effect and match policies, reinvocation
policy, and their bounded timeouts.
The verifier reads this complete contract again after its wait and rechecks the
CRDs immediately before allowing the process to start. A losing release or a
Pod launched while Helm is repairing a drifted singleton can neither reconcile
schemas nor patch the winning release's CA bundle.

### Offline singleton migration

Do not change singleton annotations merely to make an online upgrade pass. To
adopt a legacy installation that predates the annotations, first establish that
the fixed configurations are owned by the one intended release. Suspend every
`PtahSchema`, wait for every operation Job to finish, place the databases in a
maintenance window, and scale the manager and certificate-rotation Deployments
to zero. Annotate both `ptah-operator-admission` configurations with the actual
release name, release namespace, effective coordination namespace,
leader-election setting, and the fixed ID
`ptah-operator.operator.ptah.dev`. Upgrade that same release, verify its CRDs
and Pods, then restore replicas and resume reconciliation.

Changing an established coordination namespace or leader-election mode is a
full offline migration, not an upgrade. After the same suspension, job drain,
database maintenance, and scale-to-zero sequence, record the old coordination
Leases and back up all Ptah custom resources. Uninstall the release; verify that
the three CRDs and their objects remain. Install exactly one release with the
new invariant values, verify its admission annotations and manager readiness,
then end maintenance and resume the schemas. Retain the old Leases as audit
evidence until no interrupted operation can refer to them.

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
Lease, and the exact mutating and validating webhook configurations. RBAC
cannot limit which fields an `update` changes, so two retained, parameterless,
fail-closed admission policies type-check the mutating and validating
configurations separately. They require the exact rotator ServiceAccount,
singleton name, unchanged ordered webhook-entry inventory, caller-controlled
metadata, and webhook behavior. Kube-apiserver-managed generation and
managed-fields bookkeeping are the only metadata exceptions; generation must
track an actual webhook-list change. Kubernetes field management can rewrite
`metadata.managedFields` before admission and cannot distinguish that rewrite
from a caller-requested ownership reset, so the guard does not treat field
ownership bookkeeping as a security boundary. Among behavioral and
caller-controlled fields, only a nonempty per-entry `caBundle` of at most 256
KiB may differ. Because the policy preserves the live entry inventory
instead of hard-coding one release's list, a failed preflight before quiescence
cannot lock the still-running predecessor rotator out of its existing CA-only
updates. The rotator treats its configured webhook names as required identity
anchors, then rotates every additional entry targeting the exact release
Service. This also lets an already-running predecessor carry a same-Service
entry observed in a narrow partial-apply race; URL and foreign-Service entries
remain untouched. It does not make a quiesced predecessor restartable after
candidate activation. The release and image ratchets intentionally block that
rollback, and recovery after quiescence is to retry the same candidate. Helm
installs and binds these policies before granting certificate update access,
and every hook and runtime init verifier requires the observed policy
generations to have no CEL warnings. By default,
`certificateRotation.recreateMissingSecret=false`: the chart grants no
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

The complete stable condition-reason vocabulary is cataloged in
[Condition reasons](condition-reasons.md). Automation should compare the
`type`, `status`, and `reason` tuple and require `observedGeneration` to match
the resource generation; condition messages are diagnostic text, not an API.

`status.target.driftFindings` is a bounded, canonical summary of the most
recent raw Observe result. Entries are ordered by descending severity and then
category, contain no object names or SQL, and are limited to 64 categories.
`driftFindingCount` covers the complete report rather than the displayed list;
when `driftFindingsTruncated` is true, undisplayed categories contributed to
that total. A subsequent scoped Plan deliberately does not replace this raw
observation summary: the `DriftDetected` condition describes the authoritative
managed scope, while `status.target` remains evidence for the observation
identified by `driftReportDigest` and `lastObservedAt`.

`EngineSupported=False` with reason `UnsupportedEngine` is an explicit
non-authorizing state, not a reconciliation crash. It creates no operation Job,
plan, or approval and does not read the database credential. The controller
finishes any already-dispatched Apply and its mandatory read-only proof before
entering that state, while an undispatched Apply claim loses authorization
without creating a Job. Selecting a supported engine later starts again at
Resolve rather than reusing the old plan or approval.

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

Current plan contract v3 also binds the manager's complete execution identity:
manager image digest, controller source revision, controller state version,
Ptah version, executor image digest, runner image digest, and runner protocol.
If that identity changes before a mutating Job is dispatched, the current plan
is no longer executable. A recorded approval becomes stale with reason
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
even if all seven component fields return to their previous values.

A normal chart upgrade has a hard revision boundary: the `Recreate` strategy
terminates every old manager Pod before any replacement manager Pod starts.
Before termination, the old manager may still dispatch an approval that is
valid for the complete old execution binding. That Job remains internally
consistent and is never changed to the replacement binding. If an upgrade
maintenance window requires that no new Apply be dispatched after the window
begins, first scale the manager Deployment to zero and wait until all manager
Pods have terminated. Then run `helm upgrade --wait`; the chart restores the
configured replica count and the replacement manager invalidates every
undispatched old-binding authorization before it can mutate a database.

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
failure to schedule that TTL leaves the operation retryable. The exact current
Apply may receive the same TTL while still running when the controller must
persist an uncertain outcome; the countdown starts only after the Job finishes.
During schema deletion, a terminal Job that cannot satisfy the exact cleanup
admission contract is left to owner garbage collection instead of blocking the
finalizer on a weaker write.

An executable plan is limited to 8 MiB, including its trailing newline. The
runner rejects a larger native plan before publication. Accepted bytes are
stored in immutable 512 KiB binary ConfigMap chunks; that chunk size leaves
headroom below the Kubernetes object-size limit after API JSON base64 encoding.

## Kubernetes versions

The supported minor window and update procedure are defined in
[Kubernetes support](kubernetes-support.md). A support-window change adds the
new minor and removes the oldest minor atomically, after the entire real-cluster
matrix succeeds.
