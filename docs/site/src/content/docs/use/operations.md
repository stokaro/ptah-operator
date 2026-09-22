---
title: Operations
description: Installing, upgrading, watching and recovering a running operator.
---

## Start here {#start-here}

This page is long because the contracts are. If something is wrong now, the
row that matches is where to go.

| What you are looking at | Where it is answered |
| --- | --- |
| A first installation | [Install](../../start/install/), then [Install the operator](#install) |
| An upgrade to run | [Upgrade to a new release](#upgrade). A candidate refused before any CRD is touched leaves the active release running. |
| An upgrade that stopped partway | [Re-run an upgrade that stopped partway](#retry-upgrade) |
| A release with neither runtime Deployment left | [Repair a release that lost its runtime](#repair-runtime) |
| Removing the operator from a cluster | [Uninstall the release](#uninstall) |
| A resource that is not converging, and no obvious refusal | [Finding a resource that has stopped converging](#finding-a-resource-that-has-stopped-converging) |
| A resource in `Blocked`, or a condition you do not recognize | [Condition reasons](../../troubleshoot/condition-reasons/) |
| A migration whose Apply ended `Partial` or `Unknown` | [A migration run nobody accounted for](#a-migration-run-nobody-accounted-for) |
| Writes failing because the webhook cannot be reached | [Webhook certificate lifecycle](#webhook-certificate-lifecycle) |
| A write the admission refused, with a message | The message names the guard; [Security model](../security/) says what each one protects |
| The cluster or the namespace is gone and the database is not | [Recover operator state](../recovery/) |
| Storage growing with every plan | [Plan retention](#plan-retention), then [Pruning stored plans](#pruning-stored-plans) |
| Whether the operator can run SQL nobody approved | [Execution guarantees](../../reference/guarantees/) |

Before anything else, do not delete a `PtahMigration` to clear a state you do
not like. Its record of an Apply nobody accounted for goes with it, and that
record is what a person needs to find out whether the database was changed.

## Installation and upgrades

Every task here names what has to be true before it starts, the commands, what
proves it finished, where to stop, and what to do when it fails. The hooks
check a great deal more than any of these tasks say, and what each refusal
means is [Release lifecycle](../../reference/release-lifecycle/).

Helm 4 or newer is required, and every lifecycle contour is verified against
it. Helm 3 is not supported: it reaches end of life before this operator's
first release, and it applies client-side, so the release's objects carry no
server-side apply ownership for a later upgrade to take back.

### Install the operator {#install}

[Install](../../start/install/) carries a first installation end to end: the
values, the digests, and the readings that settle whether the manager is
serving. What follows is what any installation has to satisfy, including the
ones a first install never has to think about.

#### Before you start {#install-before}

You need `cluster-admin`: the CRDs, the admission policies and their
bindings are all cluster-scoped, and the install hook proves them against
every API server.

Install exactly one Helm release of the operator in a cluster. The manager
watches cluster-wide resources, and admission uses the singleton
`ptah-operator-admission` webhook configurations, so a second release would
create an independent availability domain capable of blocking the first. The
fixed configuration names make an ordinary second Helm install fail ownership
validation. High availability is `replicaCount` within the one release, with
leader election enabled. Helm rejects more than one replica when
`leaderElection=false`; disabling election is supported only for an isolated
cluster with one operator release and one replica. The manager Deployment uses
`Recreate` for both modes. Every ready replica serves admission traffic even
when it does not hold the reconciliation Lease, so an old and a new revision
must never overlap behind the webhook Service.

Supply digest-pinned manager, executor and runner images. The chart refuses all
three when only a tag is supplied, and there is no local-tag mode:
`image.allowMutableTag` and `image.testIdentityDigest` are not chart values, so
a values file that still names either fails schema validation, and a render
that skips schema validation refuses them by name. Manager Pods, hooks and
controller identity all use the same `image.repository@image.digest` reference.

Hold exclusive administrative control of the release namespace until Helm
reports success. Before the v2 retained hook-progress admission policies have
reached every API server, no in-chart workload can prove its own Job or Pod
status and deletion integrity against a principal that already has write access
to those resources in the namespace, so no untrusted principal may create,
update or delete hook Jobs or Pods, or update their status subresources, for
the duration. A dedicated release namespace is how to satisfy that. The same
boundary applies to the first upgrade from a release that did not install those
policies, and ends once they and their direct per-API-server proofs have
converged; upgrades and uninstalls between v2-aware releases protect hook
creation, status and deletion without relying on namespace-writer exclusion.
Downgrading to a release that does not understand v2 removes that guarantee and
is unsupported. Cluster administrators and principals that can change admission
policy remain inside the trust boundary.

With `serviceAccount.create=false`, `serviceAccount.name` is an identity base
rather than a complete Kubernetes object name. Create the dedicated
ServiceAccount `<name>-v<N>` in the release namespace before installing release
sequence `N`, and keep the base and the external-management mode unchanged for
every later sequence. The epoch suffix is what stops a new controller from
reusing the UID or credentials of a retired one, and the chart refuses a
same-name cutover. The base must be a DNS subdomain of at most 241 characters,
which reserves room for every positive 32-bit release sequence.

For deterministic GitOps rendering, provision the webhook TLS Secret outside
the chart and set both `webhook.existingSecret` and the PEM-encoded
`webhook.caBundle`. A connected Helm install can instead reuse `ca.crt` from an
existing Secret. Setting `webhook.existingSecret` completely disables the
built-in certificate lifecycle resources, so disabling
`certificateRotation.enabled` requires it; the chart refuses an unmanaged
generated certificate. A first interactive install can instead generate a
self-signed Secret and its built-in rotation Deployment. Argo CD and Flux
should depend on the CRDs and an externally managed TLS Secret before
synchronizing the Deployment and webhook configurations.

For local development, push the built image to a registry every cluster node
can reach and use that registry's manifest digest in `image.digest`, with
`image.repository` naming its repository. A Docker image ID is not a manifest
digest. For kind, its
[local registry setup](https://kind.sigs.k8s.io/docs/user/local-registry/)
configures node access. Where the registry needs authentication, create the
release namespace and the image pull Secret before running Helm and select the
Secret through `imagePullSecrets`, so installation hooks can pull their image
too.

#### Run it {#install-run}

```sh
helm install <release> <chart> --values <values>
```

Installing over CRDs an earlier release left behind needs `--force-conflicts`
when anything else has edited them. Helm applies the chart's CRDs server-side
on install, so a field a `kubectl patch` or another controller owns is a
conflict until the install is told to take it back:

```sh
helm install <release> <chart> --values <values> --force-conflicts
```

The flag makes the chart's CRDs win over whoever edited them. Use it when that
is what you mean.

#### What proves it worked {#install-evidence}

Helm reporting success means its hooks completed, which is not the same as the
manager serving.
[Confirm it installed](../../start/install/#confirm-it-installed) carries the
two readings that settle it: six CRDs at `Established=True`, and the manager
and certificate-rotator Deployments available.

#### Where to stop {#install-stop}

A second release in the same cluster is not a supported configuration, and
neither is a values file reaching for a mutable tag. Both are refused rather
than warned about, and there is no value that accepts them. An installation
whose CRDs carry no schema identity is a third: the operator intentionally
provides no value that labels an unknown schema as trusted.

#### If it fails {#install-recovery}

A failed preflight leaves the runtime unchanged, and the refusal it reports is
the whole problem. Correct that and rerun the same command. A failed or
still-running hook Job is not deleted as a shortcut to success: a terminal
failed Job stays available for diagnostics, and the next exact
`before-hook-creation` retry removes it.

### Upgrade to a new release {#upgrade}

Upgrades are supported from the first published release onward.

#### Before you start {#upgrade-before}

You need `cluster-admin`, and enough visibility to find a RoleBinding in any
namespace -- one naming a retired epoch blocks the upgrade wherever it lives.

Every release since the first stamps the CRDs with the schema version, schema
digest and controller-state version, and the admission singletons and
parent-origin policies with their release identity. An installation that
carries none of that is not an upgrade source: the chart refuses the upgrade
before any change, and
[Offline singleton migration](#offline-singleton-migration) is the way forward.

Changing an established coordination namespace or leader-election mode is not
an upgrade either. Both are pinned by the admission singletons, and connected
Helm rendering fails when the requested values disagree with what they record.
That is the same offline migration.

Remove or migrate any RoleBinding or ClusterRoleBinding of your own that names
an active or retained epoch, in any namespace. One that remains blocks the
upgrade, because the operator cannot prove safe privilege retirement without
taking ownership of user RBAC. Grants from an external authorizer are outside
the enumerable Kubernetes RBAC contract and have to be retired by hand first.

Free the quota the candidate Pods need. Before quiescing the old runtime the
upgrade hook projects the exact candidate requests and limits through every
synchronized Pod ResourceQuota in the release namespace, and after the old Pods
disappear it waits for a resource-version-anchored observation that shows
capacity. That check cannot reserve capacity against unrelated namespace
writers.

#### Run it {#upgrade-run}

An upgrade that moves the release to a new sequence needs `--force-conflicts`:

```sh
helm upgrade <release> <chart> --values <values> --force-conflicts
```

Such an upgrade stops the runtime in its pre-upgrade hook, which leaves
`.spec.replicas` at zero under the hook's own field manager, and the apply that
follows has to raise it again. Server-side apply reports that as a conflict.
Every other field the hook writes it writes to the value the chart applies, and
an equal value is never a conflict, so the force is confined to the replica
count the cutover moved. An upgrade that keeps the release sequence does not
stop the runtime and does not need the flag:

```sh
helm upgrade <release> <chart> --values <values>
```

#### What proves it worked {#upgrade-evidence}

The readings are the ones an install ends with, against the new digests:
`Established=True` on six CRDs, both Deployments available, and manager Pods
whose image is the candidate's.

An upgrade rerun against the release that is already active, the shape a GitOps
re-sync or a values-only change produces, leaves the runtime running. The
preflight and reconcile hooks verify the retained guards and the durable
activation parameter as on any upgrade, find both runtime Deployments carrying
the active release's identity with their replicas up, and report that there is
no stop transition to perform; the retained runtime guard admits a stop only
toward a newer release. Helm then applies the unchanged manifests. That is what
a converged release looks like.

#### Where to stop {#upgrade-stop}

Do not edit the remaining CRDs to imitate the candidate, do not force
server-side apply conflicts on them, and do not change singleton annotations to
make an upgrade pass. A rollback to an image whose embedded schemas differ is
blocked by its own init verifier; select a manager version compatible with the
schemas already stored rather than working around the refusal.

A Helm `--wait` timeout while the namespace is short of quota is a capacity
incident, not permission to remove the rollout guards. Admission remains fail
closed, and the Deployment controller retries candidate Pod creation once the
quota is free.

#### If it fails {#upgrade-recovery}

A failed preflight leaves the runtime unchanged. Correct the reported conflict
or API reachability problem and rerun the identical candidate chart, image and
values.

A CRD that lacks the schema version or the schema digest is refused before any
CRD mutation, even when its live normalized `spec` matches the candidate
exactly. A malformed annotation, an incomplete identity plus any schema
difference, and a same-version digest collision are refused the same way. For
such an installation, keep the managers offline and restore the identity the
release that created the CRD stamped on it, or reinstall from the first
published release after backing up every CRD and custom resource.

An interruption after the real update sequence started is a different
situation, and
[Re-run an upgrade that stopped partway](#retry-upgrade) is the runbook for it.

### Re-run an upgrade that stopped partway {#retry-upgrade}

CRD updates are necessarily separate Kubernetes API transactions. The complete
dry-run prevents predictable partial upgrades, but an API failure or a
concurrent administrator change can still interrupt the real update sequence.

#### Before you start {#retry-before}

You need `cluster-admin`, as for the upgrade this is resuming.

Resolve the API or policy failure that interrupted the sequence. No step of
this runbook makes progress while it stands.

Find out which side of credential draining the release is on, because it
decides what is still reversible. Before draining begins, the predecessor can
remain running. Once the retained activation parameter records `phase=draining`
its controller identity is fenced and its grants may already belong to the
candidate, even while `active-release-sequence` still names the predecessor.
Restoring old Deployment snapshots cannot reverse that state or make the
predecessor ready.

Have the identical candidate to hand. A retry is accepted only for the same
target and the same full attempt digest; there is no cancellation and no
backward state transition.

#### Run it {#retry-run}

Rerun the identical candidate chart, image and values:

```sh
helm upgrade <release> <chart> --values <values> --force-conflicts
```

The hooks revalidate live state and resume the transition. Their Deployments
are still stopped or still stamped with the older release, which is what
separates a retry from the no-op an already-active release produces.

#### What proves it worked {#retry-evidence}

The same readings an upgrade ends with: `Established=True` on six CRDs, both
Deployments available, and manager Pods carrying the candidate image. The
activation parameter back at `{active=T, phase=active}` is what says the
credential phase completed rather than stalled.

#### Where to stop {#retry-stop}

Do not manually reset the activation parameter or the controller bindings. Let
the same candidate retry complete the forward transition; a hand-written
rollback of that state has no supported path back.

A [post-activation recovery gap](https://github.com/stokaro/ptah-operator/issues/22)
remains when activation has advanced but Helm has not replaced the stopped
predecessor Pod template. The pre-activation probe correction does not cover
that boundary, and resetting activation state or controller bindings is not the
way around it.

#### If it fails {#retry-recovery}

The refusal names the hook that produced it, and
[Release lifecycle](../../reference/release-lifecycle/) says what that hook
proves and therefore what has to change. Correct that and rerun the same
candidate again; each attempt revalidates live state, so repeating it costs
nothing and changes nothing on its own.

### Repair a release that lost its runtime {#repair-runtime}

A GitOps prune or a namespace-wide delete that spared the release's other
objects can leave a release with neither runtime Deployment.

#### Before you start {#repair-before}

You need `cluster-admin`, as for any upgrade: the hooks prove the retained
cluster-scoped guards before Helm applies anything.

Find the chart version that is installed, and use that one. A newer chart pins
a contract the retained guards were not created for, so an upgrade is the
second step rather than the repair.

#### Run it {#repair-run}

```sh
helm upgrade <release> <chart-at-the-installed-version> --values <values>
```

#### What proves it worked {#repair-evidence}

Helm recreates both Deployments, and the hooks prove both retained guards
before anything is applied. With nothing in the cluster carrying the active
runtime identity there is no object those guards would accept, so each is
proven by a denial only it can produce: the rollout guard refuses a Deployment
created outside the two fixed names, and the runtime guard refuses an identity
it cannot account for. Neither probe is ever persisted. After that, the
readings an install ends with.

#### Where to stop {#repair-stop}

Do not repair with a newer chart, and do not recreate the Deployments by hand.
Restore the release at its installed version first and upgrade afterwards.

#### If it fails {#repair-recovery}

A refusal here names the guard that refused, and
[Release lifecycle](../../reference/release-lifecycle/) says what that guard
holds. Where the admission singletons no longer carry this release's identity,
the release is not repairable in place and
[Offline singleton migration](#offline-singleton-migration) is the path.

### Uninstall the release {#uninstall}

Uninstall is a fail-closed, ordered retirement protocol rather than a delete,
and it has three bounded credential windows to sit through.

#### Before you start {#uninstall-before}

You need `cluster-admin`. The retirement protocol replaces and then removes
cluster-scoped admission policies, and proves each removal against every API
server.

Back up the CRDs and their custom resources. Helm retains both, and an
uninstall removes the controller and admission resources rather than the
database changes Ptah previously executed, but a backup is what makes the next
install a decision rather than a hope.

Remove or migrate any RoleBinding or ClusterRoleBinding of your own that names
an active or retained epoch, in any namespace, for the same reason an upgrade
needs it gone. A ServiceAccount you created is never deleted by the chart, not
even after its epoch is retired, and any credential or grant issued outside
Kubernetes RBAC has to be retired by hand first.

#### Run it {#uninstall-run}

Allow at least five minutes, because the protocol waits out three bounded
credential windows:

```sh
helm uninstall <release> --timeout 5m
```

#### What proves it worked {#uninstall-evidence}

Helm reports success only after the final Job verified every retired pair and
its exact attributed denial on every API endpoint, deleted its own cleanup
ServiceAccount, and watched every frozen endpoint answer `Unauthorized` for an
uninterrupted five seconds.

What remains afterwards is deliberate: the six CRDs with their custom
resources, and one cluster-scoped pair named
`ptah-operator-parameter-informer-anchor` that exists so the next install can
run its own hooks. [Release lifecycle](../../reference/release-lifecycle/)
carries the upstream defect it works around.

#### Where to stop {#uninstall-stop}

Do not delete the remaining guards by hand to make an uninstall pass, and do
not delete a failed or still-running hook Job as a shortcut to success.

Delete the informer anchor only while another bound ConfigMap-parameter policy
exists, or before the API servers restart. Reinstalling the chart recreates it.

#### If it fails {#uninstall-recovery}

Every stage of the protocol fails toward the boundary staying in place. An API
failure during the two-Deployment quiesce can leave one exact Deployment at
zero, and at least one broad fence remains active. A later cleanup or
token-retirement failure likewise leaves the admission boundary standing.

Correct the reported conflict or API reachability problem and rerun the same
`helm uninstall`. The hooks revalidate live state and resume idempotently from
the exact reachable identities.

### Offline singleton migration {#offline-singleton-migration}

This is the way past an installation the chart will not adopt, and the way to
change a value the admission singletons pin.

#### Before you start {#offline-before}

You need `cluster-admin`, write access to the release namespace, and a
maintenance window on every database the suspended resources address.

Do not change singleton annotations merely to make an online upgrade pass. An
installation whose `ptah-operator-admission` configurations carry no release
identity predates the first published release, and the chart does not adopt it.

Changing an established coordination namespace or leader-election mode is the
same procedure. Record the old coordination Leases before starting, and retain
them as audit evidence until no interrupted operation can refer to them.

A migration suspended mid-sequence keeps `status.history` and, if one was left,
`status.unresolvedRun`. Both have to survive the uninstall: the first is what
says how far the sequence got, and the second is the record that stops a
replacement Apply from running a migration that may already have executed.

#### Run it {#offline-run}

In this order:

1. Suspend every `PtahSchema` and every `PtahMigration`.
2. Wait for every operation Job of both families to finish.
3. Place the databases in a maintenance window.
4. Scale the manager and certificate-rotation Deployments to zero.
5. Back up all Ptah custom resources.
6. Uninstall the release, and verify that the six CRDs and their objects
   remain.
7. Install exactly one release of the first published version or newer, with
   the new invariant values where those are what is changing.
8. Verify its CRDs, its admission annotations and manager readiness.
9. End maintenance and resume both kinds.

#### What proves it worked {#offline-evidence}

The six CRDs and their objects present after the uninstall, before anything is
installed over them. Then the new release's admission annotations carrying its
own identity, manager readiness, and both kinds converging again.

#### Where to stop {#offline-stop}

Do not start with a migration still running: a migration left running is a
writer this procedure does not stop. Do not install over the uninstalled
release if the six CRDs or their objects did not survive it, and do not treat a
resource whose `status.unresolvedRun` went missing as one that has nothing
outstanding.

#### If it fails {#offline-recovery}

Nothing here is time-bounded, so a failed step is repeated rather than worked
around: the databases are in maintenance and both kinds are suspended, which is
the state the procedure is safe to sit in. Restore the backed-up custom
resources into the six retained CRDs before resuming either kind.

`coordination.namespace` contains the fixed manager leader-election Lease and
the database target Leases. It defaults to the release namespace and may name
a separately administered namespace for the same release. Within that
namespace, every resource that can mutate one physical database must share the
exact `spec.target.coordinationKey`, even when connection URLs use different
aliases, proxies, or credentials.

### One database, one manager

Serialization is not ownership. Two resources that share a coordination key
never run at the same time, and they can still undo each other's work by taking
turns: one converges the database to a declared schema, the other applies a
migration that changes it back, and each reports success.

So a database more than one resource claims is refused rather than queued.
Every claimant goes `Blocked` with reason `RealmConflict`, no Job runs, and the
status says how many resources claim the database and how many of them have not
declared the claim shared.

Sharing one database between a `PtahSchema` and a `PtahMigration`, or between
two of either, is a statement each of them makes for itself:

```yaml
spec:
  target:
    coordinationKey: prod/application/orders-primary
    sharedRealm: true
```

One claimant that leaves it `false` blocks all of them, itself included. That
is what makes the declaration mutual without any resource naming another: no
resource can widen its own permission alone.

The operator verifies the declaration, not the disjointness. Nothing can tell
whether two sets of arbitrary SQL touch the same rows, so `sharedRealm: true`
means a person decided the areas do not overlap.

A resource that runs nothing claims nothing. Deleting one leaves the realm, and
so does `spec.suspend: true`, which is how a resource steps aside without being
deleted — the usual way to hand one database to the other claimant without
declaring anything shared. Resuming it puts it back, and the refusal returns
before any Job runs.

A declaration on one claimant does not wake the others, so a contested realm is
re-examined on a bounded cadence: the shorter of the resource's `spec.interval`
and one minute. Every claimant leaves `Blocked` within a minute of the last one
declaring, whatever interval it runs on.

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

Admission does not trust an `objectSelector`, because a Pod creator controls
those labels. Its match condition selects either side of an update when the Pod
has the complete managed identity or a controlling Job whose name has one of
the reserved operation prefixes. A label-less clone of a real operation Pod
therefore still reaches the webhook, while unrelated non-operation Job Pods
remain outside its outage scope. The handler then requires an exact Job
controller owner and binds that Job to the current PtahSchema, operation, and
persisted Pod intent. The same scope covers the `ephemeralcontainers` and
`resize` subresources.

On create, the webhook also requires the built-in Kubernetes Job-controller
identity. The Pod must preserve the full `<job-name>-` `generateName`, while
its concrete name must use the API server's effective prefix (at most 58
characters) followed by exactly five lowercase alphanumeric characters. This
prevents a namespace Pod creator from cloning an active Job's owner UID,
labels, and executable template. The controller independently rechecks the
same generated-name chain before accepting terminal evidence or recovery
state. Only the Job controller may remove its tracking finalizer, and that
exception permits no other Pod mutation.

While the webhook is unavailable, managed Pods and Pods controlled by Jobs
using a reserved operation prefix fail closed, but unrelated non-operation Job
Pods remain available. The controller and certificate-rotation Deployments
remain recoverable because their Pods are ReplicaSet-owned. Run at least two
controller replicas and preserve the PodDisruptionBudget during maintenance.

The managed-identity and Job-owner match condition is evaluated after mutating
admission. A cluster administrator who can install or change a cluster-scoped
mutating webhook is therefore inside the admission trust boundary: such a
webhook could rewrite both the managed identity and reserved Job owner before
validation. Ordinary workload creators cannot use missing or copied labels or
extra owner references to gain operation admission; matching requests enter
the rule, and the handler rejects foreign or ambiguous ownership.

## Webhook certificate lifecycle

For a chart-generated webhook Secret, the chart stores `tls.crt`, `tls.key`,
`ca.crt`, and `ca.key` and schedules a separate certificate rotator. The
manager volume projects only `tls.crt` and `tls.key`; the CA certificate and CA
private key never enter manager Pods. The manager ClusterRole has no Secret
access. The rotator uses its own ServiceAccount with `get` and `update` limited
by `resourceNames` to the generated Secret and a separate precreated `Opaque`
staging Secret, plus one precreated coordination Lease and the exact mutating
and validating webhook configurations. Both managed Secrets carry their one
purpose label plus the exact Helm ownership label and the two exact Helm
release annotations. The staging Secret has no owner references, finalizers,
or `stringData`; every read and write requires that exact shape, a live UID and
resource version, and an unchanged UID after update. Its chart manifest omits
`data`, so pending private material is never copied into Helm release state.
RBAC
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
KiB may differ. A mutable entry must target the exact production Service, or it
must be the one typed certificate canary entry that targets the exact candidate
Service. Because the policy preserves the live entry inventory instead of
hard-coding one release's list, a failed preflight before quiescence cannot lock
the still-running predecessor rotator out of its existing CA-only updates. The
rotator treats its configured production webhook names as required identity
anchors, then rotates every additional entry targeting the exact production
Service plus the exact canary entry. URL and foreign-Service entries remain
untouched. This does not make a predecessor restartable once credential draining
begins, including after a failure before candidate activation. The credential,
release, and image ratchets intentionally block backward recovery; retry the
same candidate to finish the interrupted transition. Helm
installs and binds these policies before granting certificate update access.
Every hook and runtime init verifier requires their observed generations to
have no CEL warnings and proves their exact denials through every directly
addressed API server before a certificate rotator can start. By default,
`certificateRotation.recreateMissingSecret=false`: the chart grants no
Secret `create`, renders no Secret-creation admission policy or binding, and
grants no read access to those policy types. A deleted Secret therefore makes
the rotator fail clearly and remain unready until an administrator restores it
through a controlled Helm or GitOps operation.

Set `certificateRotation.recreateMissingSecret=true` only when automatic
deletion recovery is required. Secret `create` cannot be restricted by
`resourceNames`, so this opt-in adds a namespace-wide RBAC verb plus a
fail-closed `ValidatingAdmissionPolicy` and binding that limit the rotator
ServiceAccount to one exact TLS Secret name, namespace, two labels, two release
annotations, and four nonempty data fields. A recovered Secret therefore stays
owned by the same Helm release instead of becoming an unmanaged replacement.
The rotator requires the policy's current generation to
be type-checked, verifies the complete policy and binding structure, and runs
negative server-side dry runs before using `create`.

The single-release opt-in has a bootstrap tradeoff: Helm cannot atomically
establish the admission policy and grant RBAC. A namespace-wide `create` grant
can therefore exist briefly before policy enforcement is established. The
rotator will not use the grant during that interval, but that runtime check
cannot constrain a compromised ServiceAccount acting outside the rotator.
Leaving the default disabled removes both the broad verb and this ordering
window. Direct API-server and webhook endpoint discovery uses separate,
read-only EndpointSlice `list` grants in the `default` namespace and the
release namespace. Kubernetes assigns slice names dynamically and RBAC cannot
constrain a list by label selector, so the rotator validates each namespaced
inventory against the exact Kubernetes or release Service identity before
opening direct connections.

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
call through the expired production admission webhook.

The rotator also exposes an isolated TLS listener through a dedicated
`publishNotReadyAddresses` Service. Two permanently installed, fail-closed
canary entries select only one immutable marker ConfigMap and only an exact,
unchanged, server-side dry-run update made by the rotator's ServiceAccount and
typed field manager. The listener validates the complete AdmissionReview and
returns a deterministic typed denial; it never admits or mutates an object.
For each observation, the rotator opens a fresh HTTP/1.1 connection directly
to every API server advertised by the default Kubernetes Service. It disables
connection reuse and TLS session resumption, requires both mutating and
validating typed denials, and then re-reads an identical endpoint inventory.
`certificateRotation.admissionConvergence` controls the continuous stability
window, poll interval, and timeout for one complete endpoint observation (the
marker GET plus both denial probes). This prevents a load-balanced or cached
success from standing in for convergence across all control-plane members.

CA replacement is fail-closed and restart-safe:

1. Before changing trust, the rotator atomically persists one versioned pending
   record in the staging Secret. The record binds the source Secret UID and a
   canonical digest of its four managed fields to the new CA and key, the next
   primary leaf and key, a candidate-CA listener leaf and key, and an
   independent proof CA certificate with its listener leaf and key. The proof
   CA private key is discarded after issuing that leaf and is never persisted.
   Missing-Secret recovery records the absence explicitly instead of inventing
   a source identity.
2. The rotator serves the candidate-CA listener certificate, expands each
   production entry from its own authenticated prior trust to include the new
   CA, publishes the mutating and validating singletons separately, and proves
   their combined canary state through every API server. A current signer may
   be shared only after its signature and exact live-leaf identity are
   independently proved; unauthenticated entry-local prior trust is never
   copied to another entry.
3. One atomic Secret update replaces the serving certificate, serving key, CA,
   and CA key. A lost response is accepted only after byte-exact readback. The
   rotator then lists the exact production Service EndpointSlices, rejects
   empty, unready, terminating, malformed, or duplicate endpoint sets, and
   performs a TLS handshake with every ready Pod IP using the Service DNS name.
   It requires the exact new leaf certificate and a second identical endpoint
   snapshot.
4. The rotator switches the isolated listener to the independent proof CA,
   contracts every production entry exactly to the new CA while both canary
   entries trust only that proof CA, and again publishes the mutating and
   validating singletons separately before the combined all-API-server
   barrier. Any API server retaining the expansion state rejects this proof
   certificate, so a stale cache cannot authorize contraction.
5. The listener switches back to the candidate-CA leaf, both canaries are
   parked on the active CA through the same ordered publication and combined
   barrier, and only then are the staging Secret and in-memory listener
   credential cleared.

If the rotator stops between steps, its replacement validates and reloads the
same byte-exact pending credentials, classifies the primary Secret against the
stored source UID and digest, and replays every live readback, endpoint proof,
and combined admission barrier. The persisted phase is a monotonic audit cursor
only; it never permits a successor to skip evidence. The rotator never
generates a second candidate while a temporally usable pending record exists.
It validates durable cryptographic identity separately from certificate time,
then classifies the exact primary relationship before acting. If related staged
material has expired or is not yet valid, the rotator atomically clears only
that record, confirms the clear by exact readback, and retries from the
authoritative primary state; an unrelated record remains untouched and
fail-closed. A timeout leaves the durable record and a stage-specific
recoverable trust state in place.
Ordinary rotation never configures the API server to trust neither the old nor
the new serving certificate.
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

The binary accepts those timings only when the operation deadline strictly
exceeds Lease acquisition,
two possible production endpoint-probe windows, and three admission stability
barriers, each rounded up to a whole poll interval. The per-API-server endpoint
observation timeout must also remain below the operation deadline. Invalid or
overflowing timing combinations fail at process startup instead of creating a
permanently unready rotator.

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
[Condition reasons](../../troubleshoot/condition-reasons/). Automation should compare the
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

Those categories describe structure. A reference row that someone changed in
the database is real drift and is reconciled like any other, but it appears in
the plan rather than in `driftFindings`: the drift report has no managed-data
section to read, and the operator will not infer one by matching SQL, because a
component that must never read row values may not start reading them to
classify them. So for managed rows the plan is the authority — an unconverged
row keeps `InSync` off and produces a non-empty plan even when no DDL changed.
stokaro/ptah#3250 tracks the drift report growing a section the operator can
publish instead.

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

Deleting a `PtahMigration` follows the same rule, and an Apply is where it is
visible. The Job is owned by the resource, so releasing the finalizer under a
running Apply hands a live executor to cascading deletion and stops it between
statements. So `kubectl delete ptahmigration` blocks while the dispatched Job
is unfinished or a Pod it owns has not stopped. Once nothing can write, a claim
that dispatched something records the run `Unknown`, because nothing read what
it did; a claim that only reserved a Job name records nothing, because nothing
ran. Both hand the database Lease back and then release the resource. A
read-only operation is not waited on: its Job reads and reports, and deletion
discards the claim and goes.

Use the default propagation. `kubectl delete --cascade=foreground` asks
Kubernetes to remove the resource's dependents before the resource, and the
Apply Job is one of them, so the garbage collector takes the Job and its Pod
before the operator is reconciled at all. The wait above cannot prevent that:
the finalizer holds the owner, and the dependent goes first. Deleting a
`PtahMigration` with a running Apply that way stops the executor between
statements and leaves the database in a state only a person can account for.

How long that block lasts is bounded for the Job and not for the Pod.
`spec.execution.activeDeadlineSeconds` is what turns an executor that hangs
into a terminal Job, so a run that is merely slow ends on its own deadline. A
Pod on a node the API server cannot reach is a different case: it stays
`Running` until the node object goes or the Pod is force-deleted, and the
deletion waits with it for as long as that takes. Nothing the operator holds
ends that wait. The database Lease is renewed for the whole of it, so no other
resource takes that database while a Pod that may still be executing SQL is
unaccounted for -- which is the reason the wait is worth its cost. Recover the node or force-delete the Pod; removing the
`operator.ptah.run/migration-operation` finalizer by hand is the last resort,
and it is a statement that nothing is executing against that database any more.

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

## A migration run nobody accounted for

A `PtahMigration` whose Apply ended `Partial` or `Unknown` records the run in
`status.unresolvedRun` and stops. The record is what a person needs in order to
go and look, and it is deliberately somewhere a condition cannot reach: any
later refusal rewrites the reason on `Blocked`, and a rewritten reason says
nothing about whether the mutation was ever accounted for.

```sh
kubectl get ptahmigration orders -o jsonpath='{.status.unresolvedRun}' | jq
```

A record this manager wrote names the attempt (`operationID`), the plan it was
carrying out, and the credential-free identity of a database. It names the Job
by name and UID wherever the manager established that one existed, and the Job
and its logs may be gone by then, which is why the record carries their
identity rather than pointing at them.

Which database `targetIdentityDigest` names depends on what the run managed to
say. A readable result frame reports the database the executor opened, and that
is the one the run reached. Without one -- an Apply whose create was never
confirmed, a Pod that wrote nothing a reader could use -- the record falls back
to the database the plan was computed against, which is the last one this
resource read. The record does not say which of the two it is, and the Secret
behind a reference can rotate between a reading and a run, so treat the digest
as where to start looking rather than as a statement of where the run went.
Establish that before repairing anything: a reader who inspects the wrong
database finds nothing wrong and clears a record that was telling the truth.

`jobName` and `jobUID` are empty on one of those records as well: a create
whose outcome the API server never confirmed. The name that claim reserved is
not evidence that anything ran under it, so the record says nothing rather than
sending a reader after a Job that may never have existed. The attempt and the
plan are still there. The deadline that claim carried is not: it lived on
`status.activeOperation`, which is cleared with the claim, and neither the
record nor the plan copies it. What is observable instead is the Lease, which
this resource keeps for as long as a dispatched Apply could still be writing.

A record **adopted** on upgrade names less, because less was kept. A manager
older than this record held the state in a condition, and the claim that ran
was gone long before the upgrade read it -- so `operationID` and `planRef` are
empty, and what survives is the outcome, the Job the last run recorded, and the
database the last reading named. Those are what to search for in that case;
there is no attempt or plan to look up, and looking is wasted time.

While the record stands this resource publishes no plan and dispatches no
Apply, including the migration that run was applying. It goes on reading unless
something else has stopped it first: resolve, verify and the history read
continue at the resource's interval, and that history read is how the record
clears. Four gates are answered before any operation is claimed, and a resource
held at one of them never reaches the reading that would clear its record:
suspension, an engine this operator does not support, a database realm another
resource claims, and stored state written by a newer manager than the one
running.

Passing those gates is not the same as getting the reading. Resolve, verify and
the history read are retried for as long as they keep failing, with no attempt
at which the operator gives up, so a resource failing one of them never reaches
the history read either. If the record is not clearing after the database was
repaired, read `status.activeOperation` first: the operation it names and the
attempt it is on say whether the chain is stuck rather than the record, and an
attempt climbing quickly is a Job failing as fast as it can be recreated.

Other resources may also be working against the same database if every claimant
declares a shared realm. So the record stops this resource from changing the
database; it is not a promise that the database is idle, and a repair should
not assume one.

The run that caused the record is part of that. Retiring the claim does not
stop a Pod: where the Apply may still be executing, this resource keeps the
realm Lease until its dispatch and execution deadlines have passed, so the
Lease in the coordination namespace is what says whether the run that is being
accounted for could still be writing while it is accounted for.

### How it clears

On its own, from a read-only reading of that same database with nothing of the
artifact left to apply. That happens after a person repairs whatever the run
left half-done -- the recovery for a partial migration is to undo what it
committed, drop the unfinished revision, and publish the sequence without it --
and the operator settles on its next reading with no spec edit.

A reading of a different database does not clear a record that names one, and a
record this manager wrote always names one: the plan that run was carrying out
was computed from a reading, and a reading that named no database is refused
before it is stored. Neither does an artifact that ends before the database
does: an artifact pointed at a shorter sequence says nothing about a run that
went past it.

### Clearing it by hand {#clear-unresolved-run}

#### Before you start {#clear-before}

You need write access to the `PtahMigration` and its `status` subresource in
its namespace. Nothing cluster-scoped is touched.

Establish what the run did. The record is the operator saying it cannot tell,
so removing it without answering that question hands the next Apply a database
in a state nobody checked.

Clear whatever else is refusing the resource first. A manager older than this
record held the same state in the `Blocked` condition, and the upgrade path
reads that condition before anything else in a pass, so anything that keeps
writing `Blocked` -- a realm another resource still claims, an engine this
operator does not support, a dirty revision row, an applied migration that no
longer matches its file, a migration waiting below the current version --
rebuilds the record on the pass after this one. The command below clears the
refusal that is standing; it cannot clear one that keeps coming back.

One standing refusal does not rebuild it: a database ahead of the artifact this
resource resolves. That reading has nothing of the artifact left to apply,
nothing dirty and nothing modified, so the upgrade path counts it as the
account the run was owed and does not write the record again.

That is not the same as the reading clearing it, and
[How it clears](#how-it-clears) says why: a record still standing survives such
a reading, because the refusal is answered before the branch that removes a
record is reached. So the refusal has to be repaired either way, by publishing
an artifact that carries the versions the database already applied.

#### Run it {#clear-run}

The refusal and the record go in one write. Removing the record alone is not
enough even with nothing else refusing: the condition outlives it by a pass,
and the upgrade path reads the condition.

```sh
kubectl get ptahmigration orders -o json \
  | jq 'del(.status.unresolvedRun)
        | .status.conditions = [
            .status.conditions[] | select(.type != "Blocked")
          ]' \
  | kubectl replace --subresource=status -f -
```

The condition is removed rather than set to `False`, because setting it leaves
`lastTransitionTime` describing the moment it became `True`: the operator's
next reading sees a condition already at the value it wants and keeps that
timestamp, so `Blocked=False` would go on claiming it became false at the
instant it became true. Removing it lets the next reading write the condition
whole.

#### What proves it worked {#clear-evidence}

The operator rewrites the conditions on its next reading, so the resource comes
back carrying a `Blocked` condition written whole and no
`status.unresolvedRun`. That is also why this is one write rather than two: a
resource left blocked between them is a resource whose record comes back.

#### Where to stop {#clear-stop}

Do not clear the record to make a resource move again. Do not remove it without
the refusal beside it, and do not set `Blocked=False` in place of removing the
condition. Do not reach for `kubectl delete` either;
[Deleting the resource discards it](#deleting-the-resource-discards-it) says
what that costs.

#### If it fails {#clear-recovery}

A record that is back on the next pass means a refusal is still standing, and
the write cleared the one that happened to be current rather than the one that
keeps returning. Repair the refusal the condition names, then clear the record
again.

### Runs adopted by an upgrade {#adopted-runs}

A manager older than this record held the same state in the `Blocked`
condition's reason, and an upgrade adopts those runs so the defect that rewrote
the reason cannot lose them.

It leaves alone a run the stored reading still accounts for. A reading taken
after that run finished, with nothing of the artifact left to apply, no dirty
revision and nothing modified, is the evidence the record would have been, and
a resource carrying one is not latched however it is blocked now.

So the runs that arrive this way are the ones no surviving reading settles: a
resource that has read again and found work pending, or a dirty row, or one
that has not read since. If such a resource is blocked for an unrelated reason
at the moment of the upgrade, its old run is adopted with it. Establish what
the run did, or that a later reading already accounted for it, and then
[clear the record](#clear-unresolved-run).

### Deleting the resource discards it

`kubectl delete ptahmigration` removes the record with the object. The operator
does not refuse the deletion: only a person clears this record, so a refusal
would be one the operator could never lift, and a resource nobody can remove is
worse than a record that ends in the event stream.

It does refuse to lose it quietly. The pass that removes the finalizer emits a
Warning Event, `UnresolvedRunDiscarded`, naming the outcome, the Job the run
ran as, the plan it was carrying out and the database it addressed, and logs
the same. Those are what remain for whoever later finds a change in that
database nobody can account for, so capture them before the Event's retention
window closes:

```sh
kubectl -n "$NAMESPACE" get events --field-selector reason=UnresolvedRunDiscarded \
  -o custom-columns=WHEN:.lastTimestamp,OBJECT:.involvedObject.name,MESSAGE:.message
```

Establishing what the run did before deleting the resource is the better order.
The record names everything needed for that, and it is the last reading of it.

## Observability

The chart exposes the controller-runtime Prometheus endpoint through the
optional metrics Service (`metrics.service.enabled=true` by default). The
operator metrics use only closed, bounded label sets; object names, artifact
digests, database URLs, SQL, and error strings are never labels.

Both resource families report through the same metrics, and `family` says
which: `schema` for `PtahSchema` and `migration` for `PtahMigration`. An alert
written without that label covers both, which is what an alert on an uncertain
apply wants; add `family="migration"` to ask about one.

- `ptah_operator_reconciliations_total{family,result}` counts successful and
  failed reconciliations.
- `ptah_operator_drift_observations_total{engine,outcome}` distinguishes
  detected drift from in-sync observations. It carries no family: a migration
  reads its own recorded history rather than observing the live database.
- `ptah_operator_plans_total{family,engine,destructive}` counts published
  immutable plans. `destructive` is the schema planner's conservative
  classification; a migration plan reports `unknown`, because nothing examines
  hand-written migrations for data loss.
- `ptah_operator_approvals_total{family,outcome}` counts approval transitions.
  `required` is a plan that asked for a decision, `accepted` is a decision the
  controller consumed at the dispatch boundary, and `stale` is a decision that
  stopped being usable because the plan it named is no longer current. A policy
  of `Always` waives the requirement and counts nothing.
- `ptah_operator_applies_total{family,outcome}` counts started, completed,
  uncertain, and stale Apply transitions. A migration run that ended `Partial`
  or `Unknown` is `uncertain`: neither may be retried, and both leave a record
  only a person can clear.
- `ptah_operator_operation_duration_seconds{family,operation,outcome}` measures
  completed logical operations, including uncertain and stale outcomes.
  `operation` is the union of both families: `resolve`, `verify`, `observe`,
  `plan`, `apply`, and `history`, the last of which only a migration performs.
- `ptah_operator_failures_total{family,stage,category}` separates
  infrastructure, configuration, operation, policy-change, stale-input, and
  uncertain-outcome failures by bounded state-machine stage. The stages are the
  operations plus `controller`, which is the operator's own failure rather than
  an operation's.

Every counter is incremented on a transition, not from the status a
reconciliation read. A resource waiting for a person is reconciled on its
interval for as long as it waits, so a counter read from its status would climb
with the reconciliation cadence and report decisions nobody made.

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
increase in `ptah_operator_failures_total` and on any increase in
`ptah_operator_applies_total{outcome="uncertain"}`. Use status Conditions and
Events to identify the affected object instead of adding unbounded identity
labels to metrics.

### Finding a resource that has stopped converging

The counters above cannot answer this one. They are aggregates over every
resource the operator manages, so a cluster where one schema stopped reading
two days ago and forty others are converging normally shows a healthy rate and
nothing else. There is no per-object series to query, on purpose: an object
label is unbounded, and a metric that carries one grows with the cluster.

What each resource does carry is its own answer, in its status:

```sh
kubectl get ptahschemas,ptahmigrations -A -o custom-columns=\
KIND:.kind,NS:.metadata.namespace,NAME:.metadata.name,\
PHASE:.status.phase,READY:'.status.conditions[?(@.type=="Ready")].status',\
NEXT:.status.nextReconciliationTime
```

`status.nextReconciliationTime` is when the controller intends to look again.
A value in the past by more than one operation deadline is a resource nothing
is working on, and the phase and conditions on the same row say why. Run it
from a cron job, a `kubectl` plugin, or a script beside your alerting, and page
on rows rather than on rates.

Turning that into a Prometheus series needs an exporter that reads object
status and emits per-object gauges; `kube-state-metrics` does exactly this for
custom resources, and its cardinality is then your choice rather than the
operator's. Durable state and freshness gauges shipped by the operator itself
are tracked in
[issue #226](https://github.com/stokaro/ptah-operator/issues/226).

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

The chunks are storage, not a reading interface. `kubectl ptah plan <schema>`
reads a stored plan back the way the operator does -- every chunk against its
digest and size, joined in index order, the whole document against its content
digest -- and `--applied` reads the one the last confirmed apply ran. See
[Read a plan](../read-a-plan/), which carries the
[install](../read-a-plan/#install).

## Pruning stored plans

A plan is never overwritten. A new fingerprint publishes a new
`PtahSchemaPlan` or `PtahMigrationPlan` and its chunks, and the old ones stay
until their owner is deleted. Storage therefore grows with every distinct plan:
at the 8 MiB ceiling, a hundred retained plans hold 800 MiB of SQL before
metadata. That is arithmetic about the ceiling, not a measurement of any
installation.

The operator prunes nothing on its own. How much history to keep is a decision
about your audit requirements and your etcd budget, and neither is something
this project can choose for you. What it can say exactly is which plans are not
safe to delete.

### Which plans are pinned

A plan is pinned while any of these names it. Each is a field on a live object,
so the set is checkable with `kubectl` rather than inferred:

| Field | Why the plan must stay |
| --- | --- |
| `ptahschema.status.plan` | published and awaiting approval, or being applied |
| `ptahschema.status.pendingObservation.plan` | the post-Apply verification owed for it has not finished |
| `ptahschema.status.applied.planRef` | the evidence of what the last confirmed apply ran |
| `ptahschemaapproval.spec.planRef` | a person authorized these exact bytes |
| `ptahmigrationapproval.spec.planRef` | the same, for a migration sequence |
| `ptahmigration.status.plan` | published and awaiting approval, or being applied |
| `ptahmigration.status.activeOperation.planRef` | an Apply is in flight against it |
| `ptahmigration.status.unresolvedRun.planRef` | a run nobody accounted for may have executed it |

The last row is the one that matters most and is easiest to miss. An
unresolved run is a migration that may already have changed the database, and
its plan is what a person reads to find out what it would have done. Deleting
it destroys the only record of the work in question.

### Prune plans that nothing pins {#prune-plans}

#### Before you start {#prune-before}

You need read access to both families and their approvals across the
namespaces you are pruning, and delete access to plan objects and ConfigMaps
in them. Nothing cluster-scoped is touched.

Read [Which plans are pinned](#which-plans-are-pinned) first. Every pin is a
field on a live object, so the set is checkable rather than inferred, and the
row easiest to miss is the one that matters most: a plan named by
`ptahmigration.status.unresolvedRun.planRef` is the only record of work nobody
has accounted for.

Do this while no operation is in flight for the resources involved. A plan that
is not pinned when you list it can become pinned a moment later, which is the
same race any external pruner has and the reason the window matters.

Export anything you intend to keep as evidence before deleting it. Nothing
reconstructs those bytes once the chunks are gone, and
[Export a plan before deleting it](#export-a-plan-before-deleting-it) carries
the commands and the digest check that says the export is the plan.

#### Run it {#prune-run}

Collect the pinned names first, then delete plans outside that set:

```sh
kubectl get ptahschemas,ptahmigrations -A -o json |
  jq -r '.items[] | .status as $s |
    ($s.plan.name // empty),
    ($s.pendingObservation.plan.name // empty),
    ($s.applied.planRef.name // empty),
    ($s.activeOperation.planRef.name // empty),
    ($s.unresolvedRun.planRef.name // empty)' |
  sort -u > pinned.txt
kubectl get ptahschemaapprovals,ptahmigrationapprovals -A \
  -o jsonpath='{range .items[*]}{.spec.planRef.name}{"\n"}{end}' |
  sort -u >> pinned.txt
```

Delete a plan only if its name is absent from `pinned.txt`, and delete its
chunk ConfigMaps with it -- they are selected by
`operator.ptah.run/plan=<plan-name>`.

#### What proves it worked {#prune-evidence}

The plans you chose are gone with their chunks, and the collection above,
re-run, still resolves every name it returns. A name that no longer resolves is
the finding: a pinned plan was deleted, and the resource that pinned it now
names bytes nothing holds.

#### Where to stop {#prune-stop}

Absence from `pinned.txt` is the only permission to delete a plan. Stop if the
plan is named by `status.unresolvedRun.planRef`, whatever else is true of it:
that is the record of a run that may already have changed a database, and
deleting it destroys the only description of the work in question. Stop if an
export's digest disagrees with what the plan records -- a chunk is already
missing or was tampered with, which is a finding rather than a pruning problem.

#### If it fails {#prune-recovery}

The collection reads and nothing else, so a run that fails partway is repeated
rather than repaired. A deletion is the other way around: nothing restores a
plan or its chunks, and an export taken beforehand is the whole of the recovery
path. Take one for anything you are not certain about, and check its digest
before the delete rather than after.

### Export a plan before deleting it

`kubectl ptah plan` reads the current plan and `--applied` the last confirmed
one. Neither is the plan you are about to prune -- those two are pinned -- so
a historical plan is exported from its own objects.

What makes an exported plan interpretable later travels with it. The plan
document
carries every binding it was computed under: the fingerprint an approval
names, the artifact and target identity digests, the policy, the execution
binding, and the manager, runner and executor identities that would have run
it. The chunks carry the SQL. The approval, where one exists, carries who
decided and when.

```sh
NS=<namespace>
PLAN=<plan-name>
kubectl -n "$NS" get ptahschemaplan "$PLAN" -o json > "$PLAN.plan.json"
jq -r '.spec.chunks | sort_by(.index)[] | "\(.name) \(.key)"' "$PLAN.plan.json" |
  while read -r chunk key; do
    kubectl -n "$NS" get configmap "$chunk" -o json |
      jq -r --arg key "$key" '.binaryData[$key]' | base64 -d
  done > "$PLAN.sql"
kubectl -n "$NS" get ptahschemaapprovals -o json |
  jq --arg plan "$PLAN" '[.items[] | select(.spec.planRef.name == $plan)]' \
  > "$PLAN.approvals.json"
```

Check what you exported against what the plan says it is. The chunks are
ordered and digested individually, and the plan records the digest of the
bytes they reconstruct:

```sh
printf 'exported sha256:%s\n' "$(shasum -a 256 "$PLAN.sql" | cut -d' ' -f1)"
jq -r '"recorded  " + .spec.contentDigest' "$PLAN.plan.json"
```

The two must match. A mismatch means a chunk is already missing or was
tampered with, and the export is not the plan -- find out why before deleting
anything.

A `PtahMigrationPlan` needs no chunk step: it stores no SQL, only the ordered
versions and their checksums, and the statements stay in the artifact its
`spec.artifactDigest` pins. Export the plan document and keep the artifact.

## Kubernetes versions

The supported minor window and update procedure are defined in
[Kubernetes support](../../support/kubernetes/). A support-window change adds the
new minor and removes the oldest minor atomically, after the entire real-cluster
matrix succeeds.
