---
title: Release lifecycle
description: What install, upgrade, retirement and uninstall do, and what each refuses.
---

A release is installed, upgraded and removed by Helm hooks that refuse more
than they do. This page is what those hooks check and why a refusal reads the
way it does. Performing any of it is
[Operations](../../use/operations/#installation-and-upgrades), and nothing
here has to be read to follow a runbook there. What a release promises about
objects you have stored is
[API compatibility](../../support/api-compatibility/).

## The hooks and what each decides

`ptah-crd-manager` runs as Helm hooks. Each mode validates its preconditions
before proceeding:

| Mode | What it decides |
| --- | --- |
| `identity-probe` | Whether the live release identity matches the one being installed |
| `preflight` | Whether the install or upgrade may start at all: ownership, RBAC, namespaces, PriorityClass, resource quota |
| `reconcile` | The CRDs themselves, against the stored schema history |
| `verify`, `runtime-verify` | That what was applied is what runs |
| `teardown-retirement-probe-a`, `teardown-retirement-gate` | Whether a predecessor epoch may be retired |
| `teardown-quiesce`, `teardown`, `teardown-retirement-final` | Uninstall, privilege teardown and the final retirement record |

A refusal is written to the container's termination message as well as to
stderr, because Helm reports only that a hook Job failed: without that, an
operator is told an uninstall was refused and never why.

`ptah-cert-rotator` owns the webhook serving certificates. One reconciliation
at a time, serialized by a Lease of its own: it issues or renews the CA
through a staged transition, issues the serving certificate, repairs the trust
bundles on both webhook configurations, and probes every endpoint directly to
confirm the replacement is being served before it adopts it. A transition is
accepted only after every directly addressed API server observes both canary
webhooks continuously for a stability window. The manager never receives
permission to read the Secret this writes.

## What the CRD hook does

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
dedicated hook ServiceAccount can `get` and `update` only the six exact Ptah
CRD names. Separate read-only `list` grants for every kind that stores a
controller-state version exist solely for the downgrade preflight. Kubernetes
RBAC cannot restrict `create` by `resourceNames`, so the hook also receives a
temporary namespace-wide `create` grant for Deployments after the rollout
guard is installed. The guard admits that identity only for server-side
dry-run probes of the two fixed runtime Deployment names, rejects every
arbitrary name with a dedicated policy denial, and forbids its reserved probe
annotation from persistence. The hook proves both the exact-name and
arbitrary-name boundaries before using any broader rollout permission.
Hook RBAC is removed after either success or failure. Replacing only `spec` and
those two owned annotations preserves CRD UIDs, all other metadata, status,
and all custom resources. The manager and certificate-rotation Pods also run
the embedded verifier as an init container
with read-only, exact-name CRD and admission-configuration access. A partial
update, later schema drift, or admission singleton owned by another release
therefore cannot start either mutating process.

## Schema identity on a CRD

Every generated CRD carries a schema-identity pair: the positive decimal
`operator.ptah.run/crd-schema-version` rollback fence and
`operator.ptah.run/crd-schema-digest`, a lowercase SHA-256 digest of its
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

## The downgrade preflight

Before starting a manager, the init verifier scans every kind that stores a
controller-state version across the cluster: `PtahSchema`, `PtahSchemaPlan`,
`PtahSchemaApproval`, `PtahMigration`, `PtahMigrationPlan`, and
`PtahMigrationApproval`. It checks controller-state versions in
`PtahSchema.status.executionBinding`, `status.plan`, `status.applied`, and
`status.pendingObservation.plan`, in `PtahMigration.status.executionBinding`,
and in the immutable `spec.controllerStateVersion` carried by every plan and
approval.
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

## The credential phase

Release cutover also has a durable credential phase. The retained activation
ConfigMap moves monotonically from `{active=A, phase=active}` to
`{active=A, phase=draining, target=T, attempt=<full SHA-256>}` and only candidate
activation can return it to `{active=T, phase=active}`. While draining, the
ServiceAccount-origin guard denies both controller API writes and node-issued
TokenRequests for the candidate and predecessor controller identities. The
hook proves that exact draining tuple on every API server and quiesces the
runtime. The full 65-second credential grace is one uninterrupted joint
window: every sweep refreshes the API endpoint topology, repeats every direct
admission proof, verifies the exact stored admission tuple, and observes a
namespace-wide LIST-to-WATCH stream with no protected runtime Pod. Any endpoint
or Pod change, watch restart, stale response, or transient error resets the
whole window before the first grant moves. A failed
response at any transition is retried only for the same target and full attempt
digest; there is no cancellation or backward state transition. A fresh install
can skip the drain only when the activation state and complete preflight prove
that no predecessor, candidate ServiceAccount, candidate grant, protected Pod,
or prior drain exists.

For a predecessor cutover, the hook receives `bind` only on the stable
controller ClusterRole and the exact existing controller Roles in their
coordination, release, and discovery namespaces. A fresh install receives no
`bind` grant. The ServiceAccount-origin guard permits only the current reconcile
Pod to replace the predecessor subject with the candidate subject during that
attempt's exact draining state. Role references, binding identity and metadata,
and certificate subjects must remain unchanged. Other binding writes, including
granting a role to the hook itself, are denied. Uninstall includes every issued
`bind` grant in the direct authorization-revocation proof.

## The admission proof

Each release attempt also owns an immutable, sequence-keyed admission marker.
The chart inventories at most the active predecessor marker and current
candidate marker, rejects gaps, future or malformed markers, and refuses a
same-sequence marker bound to another full manager-image attempt. A final
ValidatingAdmissionPolicy and binding are installed after every retained
release guard. The final policy is itself a fail-closed credential fence: in
addition to its inert marker branch, it contains the complete protected
controller, certificate, hook, and cleanup caller and bound-TokenRequest origin
checks, including the controller phase ratchet. Seven credential and workload-
provenance guards expose mutually exclusive, content-versioned field-manager
branches on the same immutable marker: the ServiceAccount-origin guard plus
the six workload guards that constrain the executable identities behind those
credentials. For every
ready API-server address, the hook sends one unchanged dry-run update per
policy. Each request must return exactly one denial attributed to that exact
policy and binding; removing any guard, retaining an old attempt, or adding a
second cause makes the sweep inconclusive or fatal. The sentinel request also
returns one exact tuple-specific denial, proving its broad binding and current
activation fence. Each sweep re-reads and compares the complete stored policy,
binding, activation, and marker contracts, so an old cached denial cannot hide
a foreign stored replacement that may publish later. All endpoints must return
the complete denial bundle for one uninterrupted five-second window; topology
changes, stored-object changes, admitted dry-runs, stale tuple denials, and
transient discovery, transport, or server errors reset the window. Foreign
object shape or an unexpected denial fails
the attempt closed. The Kubernetes Service virtual IP is never used for this
proof. The sentinel policy/binding name versions this enforcement contract; a
future semantic change must use a new versioned identity so a cached older
pair cannot satisfy the proof.

Other retained functional guards are still compared exactly in shared storage,
but are outside this per-endpoint credential/provenance publication claim.

Controller and certificate init verification repeats the same direct endpoint
proof for the activated candidate before either process starts serving. The
optional missing-Secret recovery policy is installed later and is outside the
retained-marker claim. When enabled, certificate startup fences that pair
separately with exact attributed negative dry-runs on every API server, and the
rotator still verifies the pair's complete stored structure before using its
Secret `create` permission.

## A retry before activation

An interrupted upgrade is resumed by rerunning the identical candidate, and the
probe that resumption needs is not a restart.

During a retry before candidate activation, an enforcement probe may need a
dry-run copy of a stopped, candidate-stamped Deployment with the predecessor's
top-level identity and desired replica count. The original Pod template, UID,
and resource version remain unchanged. The replica count comes from the
preserved predecessor verifier arguments, or the certificate contract's fixed
single replica, never from the new candidate's controller settings. The full
retained admission rules must accept that baseline before the probe adds its
reserved annotation and requires an isolated denial. These requests are all
dry-run: they do not restart the predecessor or change its persisted identity.

## What the runtime verifier requires

Both fixed admission configurations record the owning release name and
namespace, the effective coordination namespace, the leader-election mode, and
the fixed leader-election ID in `operator.ptah.run/*` annotations. Connected
Helm rendering uses `lookup` and fails when either singleton is missing its
peer, lacks an annotation, or disagrees with the requested values. This blocks
a second release and blocks changes to `coordination.namespace` or
`leaderElection` before either the CRD hook or manager Deployment can mutate
cluster state. There is no value that bypasses this check.

The runtime verifier closes the remaining concurrent-install race in which two
clients could both render while the singleton was absent. New manager and
certificate-rotation Pods wait for both fixed admission configurations and
require their complete annotation tuple to match the Pod's release. They also
require exactly two mutating approval webhooks and four validating webhooks --
one approval pair for each resource family, plus operation-Pod intent and
controller writes -- with fail-closed policies, nonempty CA bundles, the exact Service, paths, port, rules, selectors,
match conditions, review version, side-effect and match policies, reinvocation
policy, and their bounded timeouts. Where certificate rotation is enabled, the
two candidate canary webhooks are required beside them; where it is not, they
must be absent. Nothing else in either configuration is accepted.
The verifier reads this complete contract again after its wait and rechecks the
CRDs immediately before allowing the process to start. A losing release or a
Pod launched while Helm is repairing a drifted singleton can neither reconcile
schemas nor patch the winning release's CA bundle.

## Uninstall as retirement

An uninstall returns the release activation ConfigMap to the state a fresh
install starts from and only then deletes it. Kubernetes keeps serving a
deleted policy parameter to the bindings that read it, so what it keeps serving
has to be the bootstrap state; otherwise a reinstall in the same namespace
meets guards reading the sequence the removed release last activated, and its
first hook cannot get a Deployment past them.

Uninstall is a fail-closed, ordered retirement protocol. Two release-stable
validating admission fences are ordinary chart resources and therefore exist
before an uninstall starts. Their narrow form protects the fixed bootstrap
ServiceAccount and the exact proof Jobs and Pods. Helm first replaces fence A
with the complete uninstall boundary while narrow fence B remains active, then
the bootstrap Job proves fence A directly on every advertised API server for an
uninterrupted five-second window. Helm can replace fence B only after that
proof; a second Job then proves A and B together. Neither publication delay nor
an interrupted replacement can expose an unfenced bootstrap credential.

The broad fences constrain the controller, certificate rotator, quiesce,
cleanup, and bootstrap identities; their bound TokenRequests; and the complete
Job and Pod execution contracts used by the remaining hooks. Job and Pod status
updates are restricted to exact Kubernetes controller, scheduler, or node
principals and to the fields those principals legitimately own. Deleting a
protected Job is allowed only to a principal with admission-management
authority and only after its authenticated status contains a terminal
`Complete=True` or `Failed=True` condition. This prevents a namespace writer
from turning Job deletion into hook success while preserving Helm's
deterministic cleanup and retry path for a genuinely finished Job.

The quiesce hook verifies the complete release, admission, RBAC,
ServiceAccount, and workload inventory before scaling the two exact runtime
Deployments to zero. A separate cleanup identity removes only the candidate
release's exact bindings and chart-created ServiceAccounts. Every chart
workload uses a Pod-bound projected ServiceAccount token. After runtime Pods
and retired chart-created ServiceAccounts disappear, the fences remain active
through an uninterrupted 65-second credential-revocation proof. Kubernetes may
continue accepting a bound credential for up to 60 seconds after its Pod or
ServiceAccount enters deletion; the additional five-second stable interval
prevents such a credential from outliving the boundary.

Each authorization and admission sweep addresses every ready, serving,
non-terminating endpoint advertised by the `default/kubernetes` Service
directly, while verifying the normal Kubernetes Service TLS name and cluster
CA. Discovery uses a complete, coherently paginated EndpointSlice inventory.
Membership, readiness, canonical address, stored contract, or probe-result
changes restart the ordinary convergence windows. Missing, malformed,
unreachable, or TLS-invalid inventory blocks uninstall.

Canonical SubjectAccessReviews bind every retired runtime and hook
ServiceAccount to the exact mutating permissions issued by this release. A
normal Kubernetes RBAC no-opinion result (`allowed=false`, `denied=false`) is
successful; an allow or evaluation error is not. SelfSubjectAccessReviews use
the cleanup Job's real bearer token, so its ServiceAccount UID, Pod and node
binding, and credential identifier come from authentication rather than a
synthetic identity. Stored RBAC, protected-Pod absence, direct authorization,
and topology evidence must agree for the same uninterrupted window.

Helm, not a Pod credential, performs every admission-policy mutation. While A
and B remain broad, Helm replaces each original validating policy and binding
with an exact inert marker-only form. A retry accepts only the narrowly defined
original, retired, or temporarily absent side states reachable from that
ordered replacement; foreign or ambiguous combinations fail closed. The final
Job verifies all stored retired pairs and their exact attributed denials on
every API endpoint before deleting the secondary convergence marker and exact
release activation object.

The final Job then deletes its own cleanup ServiceAccount with immutable UID
and resource-version preconditions. Before that deletion it freezes the direct
endpoint clients and starts an EndpointSlice watch from the inventory LIST
resourceVersion. While an endpoint still authenticates the deleted credential,
the release activation must remain absent and both broad fences must continue
returning their exact denials. Only an `Unauthorized` response counts as token
retirement; `Forbidden`, an admitted probe, a foreign phase, or a transport or
server error fails closed. Every frozen endpoint must remain continuously
unauthorized for five seconds after the last accepted authentication, and the
EndpointSlice watch must remain unchanged and healthy throughout. An endpoint
addition, removal, readiness change, watch closure, expired watch, or malformed
event aborts the hook so a retry creates a fresh ServiceAccount and snapshot.
After deleting its ServiceAccount, the manager performs no persistent API
mutation: an endpoint that still authenticates the token receives only the
exact dry-run fence probes, while an endpoint that has returned `Unauthorized`
receives read-only phase checks to detect an authentication regression.

The immutable retirement marker remains until the entire pre-delete event has
succeeded. Helm then removes that marker and the inert retirement pairs under
`hook-succeeded`; ordinary release deletion finally removes A and B by their
stable manifest identities. No essential cleanup depends on a post-delete
hook. Webhook-configuration cache retirement is outside this privilege proof;
a stale webhook entry after uninstall is an availability residue, not evidence
that a retired release credential still has authority. Because the protocol
contains three bounded credential windows, use a Helm uninstall timeout of at
least five minutes.

### The parameter informer anchor

An uninstall also leaves behind one cluster-scoped pair named
`ptah-operator-parameter-informer-anchor`: a ValidatingAdmissionPolicy and its
binding. They admit everything they match, name a ConfigMap nothing creates,
and exist for one reason. The API server keeps a single informer per admission
parameter kind, and when the last bound policy naming a built-in kind goes away
it cancels that informer and cannot start it again: the replacement comes from
the typed shared informer factory, which refuses to restart an informer it has
already started. The canceled informer still reports itself as synced, so
every later policy that reads a ConfigMap parameter resolves against a cache
frozen at the moment it stopped. Parameters written afterwards are invisible,
and a binding that denies on a missing parameter refuses every request it
matches. Without the anchor, uninstalling the operator would leave the next
install unable to run its own hooks until the API servers restarted.

The defect is upstream and open:
[kubernetes/kubernetes#133827](https://github.com/kubernetes/kubernetes/issues/133827)
reports it, and
[kubernetes/kubernetes#141015](https://github.com/kubernetes/kubernetes/pull/141015)
is the fix for this exact case. That fix reached master during the 1.37 code
freeze and is still unmerged, so no release in the supported window carries it.
A CRD parameter kind resolves through a different informer path and is not
affected.

