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
| An upgrade to run | [Upgrade to a new release](#upgrade). A candidate refused before any CRD is touched leaves the running release in place. |
| An upgrade that stopped partway | [Re-run an upgrade that stopped partway](#retry-upgrade) |
| A return to an earlier revision | [Roll back to an earlier revision](#rollback) |
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
bindings are all cluster-scoped.

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

Install into a dedicated namespace and keep it, for the life of the release,
to principals you trust to administer Ptah. The release namespace, and
`coordination.namespace` when it names another one, is a privileged
administrative boundary: Kubernetes lets whoever can create a Pod in a
namespace run it as any ServiceAccount there, so anyone who can create or
modify workloads, exec into Pods or request ServiceAccount tokens in it can act
as the operator, and is a Ptah administrator. Deploy no application workloads
there, and grant no namespace-admin, `edit` or workload-creation access to
anyone else. [Security model](../security/#release-namespace) is the contract,
and it holds for installs, upgrades and uninstalls alike. Cluster
administrators and principals that can change admission policy are inside the
same boundary. When Helm can read the cluster, the chart refuses `default`, a
`kube-*` namespace, or a namespace that runs workloads without this release's
`app.kubernetes.io/instance` label, and its notes warn about RoleBindings that
make somebody else an administrator there.
[What the chart checks](../security/#release-namespace-check) is the whole
list, and what a render without the cluster skips.

The controller runs as one ServiceAccount in every release. With
`serviceAccount.create=false`, create the ServiceAccount `serviceAccount.name`
names in the release namespace before the first install; every later release
runs as the same one. The chart binds its roles to it and never deletes it.

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

Helm reporting success means its hook completed, which is not the same as the
manager serving.
[Confirm it installed](../../start/install/#confirm-it-installed) carries the
two readings that settle it: ten CRDs at `Established=True`, and the manager
and certificate-rotator Deployments available.

#### Where to stop {#install-stop}

A second release in the same cluster is not a supported configuration, and
neither is a values file reaching for a mutable tag. Both are refused rather
than warned about, and there is no value that accepts them. An installation
whose CRDs carry no schema identity is a third: the operator intentionally
provides no value that labels an unknown schema as trusted.

A release namespace the chart would share is refused as well, and that one
has a value: `releaseNamespace.allowSharedNamespace: true`. Setting it says
that everyone who can create workloads in the namespace administers Ptah. If
that is not true, move the release, or the other workloads, instead.

#### If it fails {#install-recovery}

A refused CRD hook leaves the runtime unchanged, and the refusal it reports is
the whole problem. Correct that and rerun the same command. Helm deletes the
hook Job when it fails as well as when it succeeds, and prints its log to
stderr before it does, so the refusal is in the output of the command that
failed.

### Upgrade to a new release {#upgrade}

Upgrades are supported from the first published release onward.

#### Before you start {#upgrade-before}

You need `cluster-admin`.

The [release namespace check](../security/#release-namespace-check) runs on
every upgrade, so a workload somebody deployed into the release namespace
since the last one refuses it until the workload leaves.

Every release since the first stamps the CRDs with the schema version, schema
digest and controller-state version, and the admission singletons with their
release identity. An installation that carries none of that is not an upgrade
source: the chart refuses the upgrade before any change, and
[Offline singleton migration](#offline-singleton-migration) is the way forward.

Changing an established coordination namespace or leader-election mode is not
an upgrade either. Both are pinned by the admission singletons, and connected
Helm rendering fails when the requested values disagree with what they record.
That is the same offline migration.

Free the quota the candidate Pods need. An upgrade that changes the manager
image stops the running release in its hook, before Helm applies the
candidate, so a namespace short of quota leaves no manager running until the
candidate Pods fit. Nothing checks the quota for you.

#### Run it {#upgrade-run}

An upgrade that changes the manager image needs `--force-conflicts`:

```sh
helm upgrade <release> <chart> --values <values> --force-conflicts
```

Such an upgrade stops the runtime in its pre-upgrade hook, which leaves
`.spec.replicas` at zero under the hook's own field manager, and the apply that
follows has to raise it again. Server-side apply reports that as a conflict.
The hook writes no other field, so the force is confined to the replica count
the stop moved. An upgrade that keeps the manager image does not stop the
runtime and does not need the flag:

```sh
helm upgrade <release> <chart> --values <values>
```

#### What proves it worked {#upgrade-evidence}

The readings are the ones an install ends with, against the new digests:
`Established=True` on ten CRDs, both Deployments available, and manager Pods
whose image is the candidate's.

An upgrade that keeps the manager image, the shape a GitOps re-sync or a
values-only change produces, leaves the runtime running. The reconcile hook
checks the stored state and the CRDs as on any upgrade, finds both runtime
Deployments already on its image, and stops nothing. Helm then applies the
manifests, and a changed value rolls the Deployments the ordinary way.

#### Where to stop {#upgrade-stop}

Do not edit the remaining CRDs to imitate the candidate, do not force
server-side apply conflicts on them, and do not change singleton annotations to
make an upgrade pass. Select a manager version compatible with the schemas
already stored rather than working around the refusal.

A Helm `--wait` timeout while the namespace is short of quota is a capacity
incident. The Deployment controller retries candidate Pod creation once the
quota is free, and the manager serves again when it does.

#### If it fails {#upgrade-recovery}

A refusal in the CRD hook before it stops the runtime leaves the runtime
unchanged. Correct the reported conflict or API reachability problem and rerun
the identical candidate chart, image and values.

A hook that reports the chart and the manager image come from different
releases refused before it read the cluster. `--reuse-values` keeps the
previous `image.digest` under a new chart; set `image.digest` to the one
published with the chart and rerun.

A CRD that lacks the schema version or the schema digest is refused before any
CRD mutation, even when its live normalized `spec` matches the candidate
exactly. A malformed annotation, an incomplete identity plus any schema
difference, and a same-version digest collision are refused the same way. For
such an installation, keep the managers offline and restore the identity the
release that created the CRD stamped on it, or reinstall from the first
published release after backing up every CRD and custom resource.

An interruption after the hook stopped the runtime is a different situation,
and [Re-run an upgrade that stopped partway](#retry-upgrade) is the runbook for
it.

### Re-run an upgrade that stopped partway {#retry-upgrade}

CRD updates are necessarily separate Kubernetes API transactions. The complete
dry-run prevents predictable partial upgrades, but an API failure or a
concurrent administrator change can still interrupt the real update sequence.
An upgrade can also fail after its hook finished: the hook stopped the runtime,
and then Helm's apply of the candidate was refused or timed out.

#### Before you start {#retry-before}

You need `cluster-admin`, as for the upgrade this is resuming.

Resolve the API or policy failure that interrupted the upgrade. No step of this
runbook makes progress while it stands.

Find out how far it got. Until the reconcile hook stops the runtime, the
predecessor keeps running. After the stop, both Deployments sit at zero
replicas on the predecessor's Pod template until Helm applies the candidate's.
Do not bring the predecessor back by scaling its Deployments up: its CRDs may
already be the candidate's, and its own init verifier then holds it.

Have the identical candidate to hand.

#### Run it {#retry-run}

Rerun the identical candidate chart, image and values:

```sh
helm upgrade <release> <chart> --values <values> --force-conflicts
```

The hook checks the stored state and the CRDs again, finds the runtime already
stopped, which changes nothing, and finishes any CRD update the interruption
left behind. Helm then applies the candidate over the stopped Deployments.

#### What proves it worked {#retry-evidence}

The same readings an upgrade ends with: `Established=True` on ten CRDs, both
Deployments available, and manager Pods carrying the candidate image.

#### Where to stop {#retry-stop}

Do not scale the stopped Deployments up by hand to get the predecessor back.
[Roll back to an earlier revision](#rollback) is the supported way to the
release before, and it runs the checks an upgrade runs.

#### If it fails {#retry-recovery}

The refusal names what refused, and
[Release lifecycle](../../reference/release-lifecycle/) says what the hook
proves and therefore what has to change. Correct that and rerun the same
candidate again; each attempt revalidates live state, so repeating it costs
nothing and changes nothing on its own.

### Roll back to an earlier revision {#rollback}

`helm rollback` restores an earlier revision's manifests and runs that
revision's CRD hook first, with that revision's image. The hook holds the
rollback to the checks an upgrade passes: it refuses stored state and CRD
schemas newer than the restored release reads. Every check but one runs
before it stops anything; the last state scan runs after the stop, to find
state the running manager wrote in the moment before it stopped.

#### Before you start {#rollback-before}

You need `cluster-admin`.

Rolling back below state already written is refused, whatever revision the
history offers. [The controller-state contract](../../support/releases/#controller-state-contract)
says which release reads which state.

#### Run it {#rollback-run}

```sh
helm rollback <release> <revision> --force-conflicts --wait --timeout 5m
```

The flag is needed for the reason an upgrade that changes the manager image
needs it: the hook stops the runtime by scaling both Deployments to zero, and
Helm raises the replica count again.

#### What proves it worked {#rollback-evidence}

`helm status` reports the new revision `deployed`, and both Deployments are
available with manager Pods carrying the restored revision's image.

#### Where to stop {#rollback-stop}

Do not edit the stored state or the CRD annotations to let a refused rollback
through. The refusal says the cluster holds state the older release cannot
read, and the way forward is a release that reads it.

#### If it fails {#rollback-recovery}

Helm records the rollback revision before it runs the hook, so a refused
rollback leaves that revision `pending-rollback`, and a later `helm upgrade`
refuses to start while it is. A refusal before the stop leaves the running
release as it was; one from the scan after the stop leaves the runtime stopped
until the next rollback or upgrade brings a release up. Another `helm
rollback`, to a revision the stored state allows, clears the pending revision.

`helm upgrade --rollback-on-failure` rolls back to the last deployed revision
when the upgrade fails. After an upgrade that failed once its hook had updated
the CRDs, that rollback runs the previous release's hook, which refuses the
newer schemas, and the release is left `pending-rollback`. `helm rollback
<release>` with no revision rolls back to the revision before the pending one,
which is the upgrade that failed: its hook reads the schemas it wrote, and Helm
brings that release up.

### Repair a release that lost its runtime {#repair-runtime}

A GitOps prune or a namespace-wide delete that spared the release's other
objects can leave a release with neither runtime Deployment.

#### Before you start {#repair-before}

You need `cluster-admin`, as for any upgrade.

Find the chart version that is installed, and use that one, so the repair
changes nothing but the missing Deployments. An upgrade, if you want one, is
the second step.

#### Run it {#repair-run}

```sh
helm upgrade <release> <chart-at-the-installed-version> --values <values>
```

#### What proves it worked {#repair-evidence}

Helm recreates both Deployments. The reconcile hook finds neither to stop and
checks the stored state and the CRDs as on any upgrade. After that, the
readings an install ends with.

#### Where to stop {#repair-stop}

Do not repair with a newer chart, and do not recreate the Deployments by hand.
Restore the release at its installed version first and upgrade afterwards.

#### If it fails {#repair-recovery}

A refusal here is the hook's, and
[Release lifecycle](../../reference/release-lifecycle/) says what it checks. Where the admission singletons no longer carry this release's identity,
the release is not repairable in place and
[Offline singleton migration](#offline-singleton-migration) is the path.

### Uninstall the release {#uninstall}

An uninstall is Helm deleting what the release installed, the admission
policies and their bindings among them. No hook runs, and the CRDs stay.

#### Before you start {#uninstall-before}

You need `cluster-admin`. The uninstall deletes cluster-scoped admission
policies and their bindings.

Back up the CRDs and their custom resources. Helm retains both, and an
uninstall removes the controller and admission resources rather than the
database changes Ptah previously executed, but a backup is what makes the next
install a decision rather than a hope.

A ServiceAccount you created is never deleted by the chart, and any credential
or grant issued outside Kubernetes RBAC has to be retired by hand.

#### Run it {#uninstall-run}

```sh
helm uninstall <release> --wait --timeout 5m
```

#### What proves it worked {#uninstall-evidence}

Helm reports the release uninstalled. What remains afterwards is deliberate:
the ten CRDs with their custom resources.

#### Where to stop {#uninstall-stop}

Do not delete the CRDs to finish the job. Deleting a CRD deletes every
resource of its kind, plans and approvals included.

#### If it fails {#uninstall-recovery}

Helm names the object it could not delete. Correct that and rerun the same
`helm uninstall`; Helm deletes what is left.

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
`status.unresolvedRun`. The first is what says how far the sequence got, and the
second is the record that stops a replacement Apply from running a migration
that may already have executed. The record is the one that has to survive: the
history is read again, and the record cannot be. The manager keeps a copy of it
in the resource's `operator.ptah.run/unresolved-run` annotation, so a backup
that keeps metadata keeps the record even where it drops status; see
[A restore that drops status](#a-restore-that-drops-status).

#### Run it {#offline-run}

In this order:

1. Suspend every `PtahSchema` and every `PtahMigration`.
2. Wait for every operation Job of both families to finish.
3. Place the databases in a maintenance window.
4. Scale the manager and certificate-rotation Deployments to zero.
5. Back up all Ptah custom resources.
6. Uninstall the release, and verify that the ten CRDs and their objects
   remain.
7. Install exactly one release of the first published version or newer, with
   the new invariant values where those are what is changing.
8. Verify its CRDs, its admission annotations and manager readiness.
9. End maintenance and resume both kinds.

#### What proves it worked {#offline-evidence}

The ten CRDs and their objects present after the uninstall, before anything is
installed over them. Then the new release's admission annotations carrying its
own identity, manager readiness, and both kinds converging again.

#### Where to stop {#offline-stop}

Do not start with a migration still running: a migration left running is a
writer this procedure does not stop. Do not install over the uninstalled
release if the ten CRDs or their objects did not survive it, and do not treat a
resource whose `status.unresolvedRun` and its copy in the
`operator.ptah.run/unresolved-run` annotation both went missing as one that has
nothing outstanding.

#### If it fails {#offline-recovery}

Nothing here is time-bounded, so a failed step is repeated rather than worked
around: the databases are in maintenance and both kinds are suspended, which is
the state the procedure is safe to sit in. Restore the backed-up custom
resources into the nine retained CRDs before resuming either kind.

`coordination.namespace` contains the fixed manager leader-election Lease and
the database target Leases. It defaults to the release namespace and may name
a separately administered namespace for the same release. Every resource that
can mutate one physical database must claim the same realm, even when
connection URLs use different aliases, proxies, or credentials: the same
`spec.target.coordinationKey` when they all live in one namespace, or the same
`PtahRealm` when they do not. See
[A database more than one namespace manages](#a-database-more-than-one-namespace-manages).

### One database, one manager

Serialization is not ownership. Two resources that claim one realm never run
at the same time, and they can still undo each other's work by taking turns: one converges the database to a declared schema, the other applies a
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

### A database more than one namespace manages

A coordination key belongs to its namespace. The realm it names is the engine,
the namespace and the key together, so the same key written in another
namespace is another realm, with a census and a Lease of its own. A key is a
string anybody who can create a resource may write, and if it reached across
namespaces, one resource created elsewhere with the key a tenant uses would put
every resource of that tenant in `RealmConflict`, behind a status that names
counts and cannot say where the claimant is.

A database that resources in more than one namespace manage is named by a
`PtahRealm`, which a cluster administrator creates and which lists the
namespaces it admits:

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahRealm
metadata:
  name: production-orders-primary
spec:
  engine: PostgreSQL
  namespaces:
    - application
    - reporting
  sharing: Shared
```

Each resource names it in place of a key:

```yaml
spec:
  target:
    engine: PostgreSQL
    realmRef:
      name: production-orders-primary
    sharedRealm: true
```

The realm is the authorization. A resource that names it from a namespace it
does not list, with another engine, or when no such realm exists goes
`Blocked` with reason `RealmNotAuthorized` and runs no Job. It is also left out
of the census of the resources the realm does list, so it blocks none of them.
The refusal reads the same in all three cases, and names only the realm the
resource asked for and its own namespace.

Among the listed namespaces the rules above hold unchanged, and the realm adds
one of its own. `sharing: Exclusive` admits one claimant at a time, whatever
each declares; `sharing: Shared` admits several when every one of them sets
`sharedRealm`. The administrator permits the sharing and each claimant states
that it manages only part of the database, and neither statement stands in for
the other.

There is no default realm, and no realm is needed for a key. A key can reach
only its own namespace, so it needs nobody's grant; a `realmRef` can reach any
namespace, so it needs one. A resource that names a realm nobody created is
refused rather than treated as a key, because falling back would make the
realm's absence a way into a database that the realm, once created, would not
admit.

The census reads the realm from the manager's cache before every claim, and a
change to a realm wakes the resources that name it. What the wake-up does
depends on the direction. Removing a namespace refuses an idle resource there
on the pass the change starts, and does not stop an operation already running:
that resource is refused at its next claim, the same way a conflict is. Adding
a namespace lifts a standing refusal at the refusal's own re-check, which is
at most a minute away, the same bound a peer's declaration has.

A namespace name in a realm is a grant to whoever holds a namespace of that
name. Remove a namespace from every realm that lists it before deleting it,
because whoever creates a namespace of that name next inherits the grant. Where
tenants choose their own namespace names -- OpenShift project self-provisioning,
Capsule tenants, HNC subnamespaces -- do not list a name before its namespace
exists either: the first tenant to create it claims the database, and can
contest it for the namespaces the realm meant to admit, which is the refusal
realms exist to withhold.

Only an administrator should be able to write a realm. The chart grants the
manager `get`, `list` and `watch` on `ptahrealms` and nothing more, and
[`examples/realm-administrator-role.yaml`](https://github.com/stokaro/ptah-operator/blob/master/examples/realm-administrator-role.yaml)
is a ClusterRole for the people who decide realm membership. None of the
author, approver or diagnostic examples reaches realms.

A realm lists namespaces, so it is an administrator's object: the operator
writes no status to it, and nothing a tenant reads names another namespace. An
administrator finds the claimants of one realm with a cluster-wide read:

```sh
kubectl get ptahschemas,ptahmigrations -A -o json |
  jq -r '.items[] | select(.spec.target.realmRef.name == "production-orders-primary")
    | "\(.kind) \(.metadata.namespace)/\(.metadata.name) \(.status.phase)"'
```

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
AlwaysPullImages are the explicitly modeled mutations. What a service mesh or
a policy engine asks a Pod to carry is declared in
`spec.execution.podMetadata`, and a Pod the API server refuses anyway is
reported with reason `PodAdmissionRefused`;
[Execution](../../reference/execution/#meshes-and-policy-engines) has that
contract.

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

## Webhook certificate lifecycle {#webhook-certificate-lifecycle}

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
cannot limit which fields an `update` changes, so the rotator's grant on the
two webhook configurations reaches every field of both, and the
[release namespace contract](../security/#release-namespace) is what bounds it,
as it bounds every identity that namespace holds. The rotator itself changes
only the `caBundle` of an entry. It treats its configured production webhook
names as required identity anchors, then rotates every additional entry
targeting the exact production Service. URL and foreign-Service entries remain
untouched. This does not make a predecessor restartable once the upgrade has
stopped the runtime; retry the same candidate to finish the interrupted
transition. By default,
`certificateRotation.recreateMissingSecret=false`: the chart grants no
Secret `create` and renders no Secret-creation admission policy or binding. A
deleted Secret therefore makes the rotator fail clearly and remain unready
until an administrator restores it through a controlled Helm or GitOps
operation.

Set `certificateRotation.recreateMissingSecret=true` only when automatic
deletion recovery is required. Secret `create` cannot be restricted by
`resourceNames`, so this opt-in adds a namespace-wide RBAC verb plus a
fail-closed `ValidatingAdmissionPolicy` and binding that limit the rotator
ServiceAccount to one exact TLS Secret name, namespace, two labels, two release
annotations, and four nonempty data fields. A recovered Secret therefore stays
owned by the same Helm release instead of becoming an unmanaged replacement.
The API server applies the policy to the rotator's request, so the opt-in
grants the rotator no access to the policy or the binding themselves.
The policy is written once, in Go, and the chart template that ships it is
generated from that definition; the certificates acceptance suite proves the
refusal against a live API server by creating an unrelated Secret as the
rotator's ServiceAccount and reading the denial back.

The single-release opt-in has a bootstrap tradeoff: Helm cannot atomically
establish the admission policy and grant RBAC. A namespace-wide `create` grant
can therefore exist briefly before policy enforcement is established. The
rotator creates the Secret only after finding it missing, and an install
renders the Secret, but nothing constrains a compromised ServiceAccount acting
outside the rotator during that interval.
Leaving the default disabled removes both the broad verb and this ordering
window. Webhook endpoint discovery uses a read-only EndpointSlice `list` grant
in the release namespace. Kubernetes assigns slice names dynamically and RBAC
cannot constrain a list by label selector, so the rotator validates the
inventory against the exact release Service identity before opening direct
connections.

Serving certificates rotate before `certificateRotation.renewalThreshold`.
The CA rotates before its threshold, when it cannot safely issue a full-lived
serving certificate, when `ca.key` is missing, malformed, or does not match
`ca.crt`, or when `ca.crt` is missing, malformed, or unrelated to the serving
leaf. A Helm upgrade refuses a generated Secret without `ca.key` rather than
rendering one, and the running rotator writes a new CA with its key. Candidate
CAs from managed webhook entries are filtered certificate-by-certificate. A
candidate is retained only when it signs the exact persisted leaf for every
Service DNS name and every stable endpoint proves it is serving that byte-exact
leaf. Unrelated roots are never copied between entries. An expired certificate
follows the same recovery path; the rotator talks to the Kubernetes API and
probes webhook endpoints directly, so recovery never depends on a successful
call through the expired production admission webhook.

A new CA is published before anything presents a certificate it issued, and
the old one is withdrawn only after nothing does. CA replacement takes three
steps, and the rotator records each in the staging Secret before it takes the
next:

1. **Expand.** The rotator atomically persists one versioned pending record,
   which binds the source Secret UID and a canonical digest of its four managed
   fields to the new CA and key and the serving certificate and key issued
   under it. Missing-Secret recovery records the absence explicitly instead of
   inventing a source identity. The rotator then adds the new CA to every
   managed entry, keeping that entry's own parseable certificates and the CA
   that authenticates the certificate being served, and records when every
   entry held both. A current signer may be shared between entries only after
   its signature and exact live-leaf identity are independently proved;
   unauthenticated entry-local trust is never copied to another entry.
2. **Switch.** No earlier than `certificateRotation.caSwitchDelay` after the
   recorded time, one atomic Secret update replaces the serving certificate,
   serving key, CA, and CA key. A lost response is accepted only after
   byte-exact readback. The delay defaults to the reconciliation interval, six
   hours, and is at least one minute. An API server picks up a webhook
   configuration change within seconds, so by the switch every one trusts the
   new CA; the delay stands in for a proof that it does. The switch never
   waits past the current certificate's life: it comes no later than
   `certificateRotation.probeTimeout` before that certificate, or every CA
   that verifies it, expires, which leaves the probe window for the new
   certificate to reach the manager Pods first.
3. **Retire.** The rotator lists the exact production Service EndpointSlices,
   rejects empty, unready, terminating, malformed, or duplicate endpoint sets,
   and performs a TLS handshake with every ready Pod IP using the Service DNS
   name. Once every endpoint serves the exact new certificate, verified by the
   new CA, and a second endpoint snapshot is identical, every managed entry
   trusts the new CA alone and the staging record is cleared. Removing a root
   needs no wait: an API server still holding the wider bundle trusts the
   served certificate all the same.

The rotator comes back at the switch time rather than a full interval later,
and a restart neither resets nor skips the delay, because the expansion time
lives in the staging Secret. If an entry has lost the new CA when the rotator
reads it again, the rotator adds it back and the delay starts over. If the
recorded time lies ahead of the rotator's clock, the delay starts over from the
clock the rotator has, so a clock that moved back cannot hold the switch off
without limit.

The delay is measured on the rotator's own clock, at both ends. A clock that is
uniformly fast or slow does not change it. A clock that jumps forward during
the delay, or a rotator Pod that moves to a node whose clock is ahead of the
one that recorded the expansion, shortens it by that difference. Kubernetes
certificate validity already assumes node clocks agree to within seconds;
keep them synchronized and the delay holds with the same margin.

The delay protects a serving certificate that API servers can still verify, so
it applies to a planned CA renewal. When the current certificate or every CA
that issued it has expired, or no managed entry holds such a CA, admission
through it has already stopped, and the rotator switches in the same pass as
the expansion. When the generated Secret is missing, or its `tls.crt` and
`tls.key` cannot be read, running manager Pods keep serving the pair they last
loaded, but a Pod that restarts cannot load one at all and the rotator cannot
tell what is served; it switches in the same pass as well, since hours of that
cost more than the seconds an API server may take to pick up the expansion. A
certificate that looks not yet valid only means the rotator's clock runs behind
the one that issued it, and does not shorten the delay. Renewing the serving certificate under an unchanged CA needs
no delay either: the new certificate needs exactly the trust the old one had.

The install's bootstrap material does not wait either. The chart renders a
two-day CA, which is inside the renewal threshold from the start, and the
rotator replaces any CA issued inside the threshold in its first pass, before
it reports ready. The chart refuses a `certificateRotation.renewalThreshold`
under 48 hours for that reason. `helm install --wait` therefore returns with the transition
finished. Otherwise the rotator's last bundle write would land a delay later,
and if a `helm upgrade` were running then, between rendering the webhook
configurations and applying them after its pre-upgrade hooks, the upgrade
would fail with a server-side apply conflict on `caBundle`. The same conflict
remains possible when a planned renewal or a recovery writes the bundles
during an upgrade; rerunning the upgrade renders the new bundles and succeeds.

If the rotator stops between steps, its replacement validates and reloads the
same byte-exact pending material, classifies the primary Secret against the
stored source UID and digest, and continues from the recorded step. A primary
Secret that already holds the pending material means the switch happened, so
the replacement proves the endpoints and retires the old CA. The rotator never
generates a second candidate while a temporally usable pending record exists,
even when the old certificate crosses its own renewal threshold during the
delay. It validates durable cryptographic identity separately from certificate
time, then classifies the exact primary relationship before acting. If related
staged material has expired or is not yet valid, the rotator atomically clears
only that record, confirms the clear by exact readback, and retries from the
authoritative primary state; an unrelated record remains untouched and
fail-closed. Ordinary rotation never configures the API server to trust neither
the old nor the new serving certificate.
controller-runtime watches the projected `tls.crt` and `tls.key` files and
reloads them without restarting the manager Pods.

A generated Secret containing valid material with an empty or `Opaque` type is
normalized to `kubernetes.io/tls`. The no-rotation path updates only the Secret
type and the four managed material fields before repairing trust; rotation
paths normalize the type as part of their existing atomic material update.

With missing-Secret recreation enabled, the rotator first appends a newly
generated CA to each exact entry. It preserves every parseable certificate
candidate from that entry even when neighboring bytes are malformed, without
borrowing trust from another entry. A manager Pod that restarts while the
Secret is gone cannot mount its certificate, so the rotator does not wait out
the switch delay: in the same pass it recreates only the policy-constrained
Secret, accepts an uncertain or racing create only after byte-exact read-back,
proves the new leaf at every stable endpoint, and contracts every entry to the
new CA. An API server that has not yet picked up the expanded bundle refuses
the matching requests until it does, usually for seconds. Admission remains fail-closed while Pods reload the recreated
certificate. Manager readiness is tied to the webhook
server's started checker rather than a process-only ping.

The conservative defaults reconcile immediately at startup and every six
hours, bound each reconciliation to 15 minutes, and retry failures with jittered
exponential backoff from five seconds to five minutes. Liveness reports whether
the supervisor is running; readiness is true only after a successful complete
reconciliation and becomes false during retries. The Deployment exposes those
states on `/healthz` and `/readyz` without serving certificate or key material.

The binary accepts those timings only when the operation deadline strictly
exceeds Lease acquisition plus two endpoint-probe windows, and when the
interval, the operation deadline, and the CA switch delay together stay below
the renewal threshold. That keeps a planned renewal noticed a whole interval
late from ever needing its full delay past the threshold; the switch deadline
above is what keeps it ahead of the certificate's expiry. Invalid or
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
that Deployment. A restart does not bring a pending CA switch forward:

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

`status.target.driftFindings` is a canonical summary of the most recent raw
Observe result, one entry for each category the report found. Entries are
ordered by descending severity and then category and contain no object names
or SQL. The list is never cut short: the category vocabulary fits under its
bound of 64 entries, so `driftFindingCount` is always the sum of the entries'
counts. A subsequent scoped Plan deliberately does not replace this raw
observation summary: the `DriftDetected` condition describes the authoritative
managed scope, while `status.target` remains evidence for the observation
identified by `driftReportDigest` and `lastObservedAt`.

Drift does not imply a finding. The report has no category for some objects it
compares: grants, default privileges, views and triggers among them. A change
to one of those alone is observed as drift with no `driftFindings`, a
`driftFindingCount` of zero and a `highestDriftSeverity` of `safe`, which is
what the report rates a list with nothing in it. `highestDriftSeverity` is set
whenever the report found drift and absent when it found none, so read it, not
the count, to tell the two apart. Planning runs either way, and the plan names
the statements and the privilege kinds they change.

Declared reference rows are counted in the same list. A row the database is
missing, one whose managed columns someone changed, and one the declaration no
longer holds appear as `data_rows_inserted`, `data_rows_updated` and
`data_rows_deleted`, each a count with a severity and nothing else: no key, no
column name and no value. Which rows those are is in the plan, and reading a
data plan is data access. `kubectl ptah schema` restates the three as its
reference-data line. The counts say that rows drifted; the plan decides what to
do about it, so an unconverged row keeps `InSync` off and produces a non-empty
plan even when no DDL changed.

`EngineSupported=False` with reason `UnsupportedEngine` is an explicit
non-authorizing state, not a reconciliation crash. It creates no operation Job,
plan, or approval and does not read the database credential. The controller
finishes any already-dispatched Apply and its mandatory read-only proof before
entering that state, while an undispatched Apply claim loses authorization
without creating a Job. Selecting a supported engine later starts again at
Resolve rather than reusing the old plan or approval.

`Ready=True` and `InSync=True` mean a read-only observation matched the verified
artifact. They are not inferred from an apply exit code. `ReadyToApply` means
the `Always` policy accepted a plan that destroys nothing and changes no
privilege without claiming that an approval is needed; `AwaitingApproval` means
an exact approval is required, and under `Always` reason `PrivilegeChanges` says
the plan's privilege changes are why.
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

The plan contract also binds what decides a plan's meaning when it runs: the
controller state version, the Ptah version, the executor image digest, and the
runner protocol. If any of them changes before a mutating Job is dispatched,
the current plan is no longer executable. A recorded approval becomes stale with reason
`ExecutionBindingChanged`. A claimed but undispatched Apply releases its old
authorization and database lock, the plan is cleared, and the replacement
manager runs Resolve, Verify, Observe, and Plan again. Approve only the
replacement plan; do not recreate an approval against the old UID or
fingerprint. A dispatched Apply is never recreated under the new binding; its
outcome is handled conservatively and requires post-Apply observation.

The same proof is required when an Apply runner refuses a target Secret that
changed after approval. The refusal describes one Pod attempt; it cannot account
for every attempt Kubernetes might have started for that Job. The operator keeps
`status.pendingObservation` bound to the originally approved database. Reading
the substituted database cannot settle it. Restore the original connection in
the Secret and let the read-only proof finish. To move the resource afterward,
change `spec.target.urlFrom` to a Secret for the new database, inspect its new
plan, and approve that plan.

What the old binding left behind is listed in a `PtahSchema`'s
`status.pendingBindingRetirement` until it is tidied: the retired epoch, the
plan whose approvals are being marked stale, and the Job the retired claim
dispatched. A read-only Job is left to finish and an Apply Job to stop before
its cleanup is scheduled, and post-Apply observation starts only after that.
The record then clears. A further change to the binding waits for it, so a
rollout rolled back while an old Apply still runs takes effect once that
Apply has stopped.

The manager's own image digest and source revision, and the runner image
digest, are recorded and not bound. A plan records the manager that published
it and a Job the manager that dispatched it; neither is in a plan fingerprint
or an approval. A Job is collected five minutes after it is harvested, so the
harvest copies its dispatcher into status, beside the publisher the plan
names: `status.pendingObservation.dispatchedBy` and then
`status.applied.dispatchedBy` for a schema, `status.lastRun.dispatchedBy` and
`status.unresolvedRun.dispatchedBy` for a migration. A manager release that changes only these -- a patch or a
security fix -- keeps the execution epoch. A pending approval stays valid and
is applied, and a published plan stays current.

Work the previous manager already dispatched is adopted under one rule: the
replacement has to build the same Job, apart from the recorded manager
identity. A claim's live Job, in either family, is compared with the Job the
replacement builds for it, with the manager identity taken from the live Pod
template and the template held to the admission snapshot the claim persisted
before dispatch. A release that changed nothing else in the Job adopts it. A
release that also changed the Job or its Pod template -- an environment
variable, an annotation, a security setting -- cannot confirm it. A dispatched
`PtahSchema` Apply is settled as outcome unknown and the database is observed
before anything else runs. A dispatched `PtahMigration` Apply is recorded as
an `Unknown` run in `status.unresolvedRun`, and no further Apply is claimed
until a History reading or a `PtahMigrationRunAcknowledgment` accounts for it.
A dispatched read-only Job of either family is run again under a new attempt.

A claim that had not dispatched yet resolves its admission snapshot again from
the template the replacement builds, and the `AdmissionSnapshotRefreshed` Event
says so. That happens once per claim: a template that changes again retires
the claim, because a builder that does not build the same Job twice would
otherwise refresh it forever.

The runner is built from the operator's source and ships in the manager's
release, so its digest changes with every release, and binding it would bind the
manager by another name. What the runner enforces inside the Pod -- the
arguments and environment it accepts, the checks around the executor, and the
result frame it returns -- is versioned by the runner protocol instead. A
release that changes any of it bumps the protocol, and that retires the plans
and approvals made under the old one.

The audit-visible `status.executionBinding` records those four components plus
an opaque `epoch`. The epoch changes whenever one of them changes, including a
rollback to a byte-identical set. A plan carries the epoch as
`spec.executionBindingID` and binds it into its fingerprint, and an approval
names the plan by that fingerprint, so approval is one-shot for that exact
transition: it cannot become valid again after a later rollout or rollback,
even if all four components return to their previous values.

A normal chart upgrade has a hard revision boundary: the `Recreate` strategy
terminates every old manager Pod before any replacement manager Pod starts.
Before termination, the old manager may still dispatch an approval that is
valid for the complete old execution binding. That Job remains internally
consistent and is never changed to the replacement binding. If an upgrade
maintenance window requires that no new Apply be dispatched after the window
begins, first scale the manager Deployment to zero and wait until all manager
Pods have terminated. Then run `helm upgrade --wait`; the chart restores the
configured replica count. When the release changes the execution binding, the
replacement manager invalidates every undispatched old-binding authorization
before it can mutate a database. When it does not, those authorizations carry
over, and the scale-down only holds dispatch for the length of the window.

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

For a `PtahMigration` that classification is `Unknown`, whatever the run did.
Suspending is a spec edit, the generation it bumps is one of the inputs the
Apply claim was decided from, and a claim whose inputs changed while its Job ran
is recorded as [a run nobody accounted for](#a-migration-run-nobody-accounted-for)
without its result being read. The Lease goes back once nothing the run
dispatched can still write. The record stays while the resource is suspended,
because a suspended migration takes no reading, and it clears on the first
reading after resume that finds nothing of the artifact pending on the same
database. To have the run's own result recorded, let it finish before
suspending.

If a `PtahSchema` Apply outcome still needs convergence proof, suspension
retains and renews that operation's database-realm Lease. This deliberately
blocks later mutations in the same realm until the resource is resumed and the
read-only proof completes, or until the resource is deleted. Releasing the
Lease while retaining unresolved proof would let an intervening Apply
contaminate the audit result.

Deleting a `PtahSchema` never runs SQL. The transient finalizer exists only to
observe an already active operation and release coordination safely. It waits
on a running Job only while the operation holds the database lock: an Apply, a
Plan, or the Observe that proves an Apply. A Resolve, a Verify or any other
Observe holds nothing, so deletion drops its claim without waiting, including
one whose Pod a policy refuses. Once no operation is active, deletion removes
Kubernetes-owned plans and Jobs through normal garbage collection; database
objects remain untouched.

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
- A migration Apply whose Pod was stopped -- a drain, a preemption, an
  eviction, its deadline -- is recorded with the account Ptah wrote as it
  stopped, when it managed to write one inside the Pod's grace: a run stopped
  between two files is a failed run with the versions it applied, and the
  history read that follows confirms it. Where the log holding that account is
  gone, the summary in the Pod's termination message stands in for it
  ([The termination summary](../../reference/execution/#the-termination-summary)).
- A stale-plan refusal is read from the `refusal` Ptah's apply report carries,
  whether the schema or the declared rows moved, and reported as `stale_plan`.
  It is still `OutcomeUnknown`, because another Pod of the same Job may have
  run: the plan is cleared, its recorded approval becomes stale, and
  reconciliation observes the database again.
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

The record names the attempt (`operationID`), the plan it was
carrying out, and the credential-free identity of a database. It names the Job
by name and UID wherever the manager established that one existed, and the Job
and its logs may be gone by then, which is why the record carries their
identity rather than pointing at them.

Which database `targetIdentityDigest` names depends on what the run managed to
say. A readable result frame reports the database the executor opened, and that
is the one the run reached; so does the summary the runner wrote into its Pod's
termination message, where it was read in place of a log that was gone. Without
either -- an Apply whose create was never confirmed, a Pod that wrote nothing a
reader could use -- the record falls back
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

While the record stands this resource publishes no plan and dispatches no
Apply, including the migration that run was applying. It goes on reading unless
something else has stopped it first: resolve, verify and the history read
continue at the resource's interval, and that history read is how the record
clears. Four gates are answered before any operation is claimed, and a resource
held at one of them never reaches the reading that would clear its record:
suspension, an engine this operator does not support, a database realm another
resource claims, and stored state written by a newer manager than the one
running. The first three do not stop a person's acknowledgment, which reads no
database and is taken ahead of them.

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
record always names one: the plan that run was carrying out
was computed from a reading, and a reading that named no database is refused
before it is stored. Neither does an artifact that ends before the database
does: an artifact pointed at a shorter sequence says nothing about a run that
went past it.

The other way it clears is a person's acknowledgment, below: the case where the
database is accounted for and still has work pending, which no reading can
settle. Either way the resource records how in `status.resolvedRun`, and who,
when a person did.

Nobody clears the record by writing status. The chart refuses a write to the
`status` subresource of every operator kind from anyone but the manager's
ServiceAccount, a cluster administrator included, because status is what the
controller and its admission webhooks decide from; a write that removed the
record would also have recorded nobody.

### Settling it by hand {#clear-unresolved-run}

#### Before you start {#clear-before}

You need `create` on `ptahmigrationrunacknowledgments` in the resource's
namespace. The approver ClusterRole the chart ships grants it, since the
decision is of the same weight as an approval: it lets the next plan be made
against that database. Nothing cluster-scoped is touched.

Establish what the run did. The record is the operator saying it cannot tell,
so acknowledging it without answering that question hands the next Apply a
database in a state nobody checked.

#### Run it {#clear-run}

Copy the resource's UID and the run's operation ID the operator published:

```sh
kubectl -n application get ptahmigration orders \
  -o jsonpath='{.metadata.uid}{"\n"}{.status.unresolvedRun.operationID}{"\n"}'
```

and name them in a `PtahMigrationRunAcknowledgment`:

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigrationRunAcknowledgment
metadata:
  name: orders-run-accounted-for
  namespace: application
spec:
  migrationRef:
    name: orders
    uid: <metadata.uid of the PtahMigration>
  operationID: <status.unresolvedRun.operationID>
```

The admission webhook stamps your authenticated identity on it and refuses one
written for anybody else, or one that names a run the resource does not record
right now. The controller takes it on its next pass, whether or not the
resource is suspended, and reads the database again before it plans anything.

#### What proves it worked {#clear-evidence}

`status.unresolvedRun` and the `operator.ptah.run/unresolved-run` annotation
are gone, and `status.resolvedRun` names the run, the acknowledgment and you:

```sh
kubectl -n application get ptahmigration orders -o jsonpath='{.status.resolvedRun}' | jq
kubectl -n application get ptahmigrationrunacknowledgment orders-run-accounted-for
```

The acknowledgment reads `Consumed=True`, and a Normal Event,
`UnresolvedRunAcknowledged`, names who acknowledged which run. The resource then
plans whatever the database still lacks: under `apply: OnApproval` that plan
waits for an approval like any other, and under `Always` it runs.

#### Where to stop {#clear-stop}

Do not acknowledge a run to make a resource move again. An acknowledgment the
controller answers `Stale=True` settled nothing: it named a run the resource is
not waiting on, usually because a reading settled it first, and a second
acknowledgment of one run is answered the same way once the first has settled
it. Do not reach for `kubectl delete` either;
[Deleting the resource discards it](#deleting-the-resource-discards-it) says
what that costs.

#### If it fails {#clear-recovery}

An acknowledgment admission refused says why: the operation ID the resource
does record, a resource recreated under the same name, or a resource that
records nothing to acknowledge. A record is written only where an Apply ends
`Partial` or `Unknown`, so one that is back names a later run. Compare its
`operationID` and `jobUID` with the run you acknowledged, and account for that
run the same way.

### A restore that drops status

The manager keeps a copy of the record in the resource's own
`operator.ptah.run/unresolved-run` annotation, as JSON. The copy is written
before `status.unresolvedRun` and removed after it, so a stored record always
has its copy. A backup tool that recreates the resource with its metadata and
without its status -- Velero does by default -- brings the copy back, and the
manager puts `status.unresolvedRun` back from it before it reads anything else,
with a Warning Event, `UnresolvedRunRestored`. The restored record names the
same run and clears the same two ways. The restored resource has a new UID, so
an acknowledgment written before the loss does not carry over to it.

Once the resource exists, only the manager's ServiceAccount may add, change or
remove the annotation; the chart refuses anyone else, as it refuses a status
write. Creating a resource that carries it is admitted, because a restore is
exactly that. A copy the manager cannot read holds the resource `Blocked` with
`ApplyOutcomeUnknown` and runs nothing; delete and recreate the resource once
the database is accounted for.

Two restores keep nothing. Specs reapplied from Git carry no annotation, and a
backup taken while an Apply was still in flight predates the record: the record
is written when the run's outcome is read. What protects those is the history
read every rebuilt resource takes before it plans, and the approval a new plan
waits for; [Recover operator state](../recovery/) says what to establish first.

### Deleting the resource discards it

`kubectl delete ptahmigration` removes the record with the object. The operator
does not refuse the deletion. A resource being deleted takes no further
readings, so nothing but a person could clear its record; a refusal would be
one the operator could never lift, and a resource nobody can remove is worse
than a record that ends in the event stream.

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
  of `Always` waives the requirement and counts nothing, except for a schema
  plan that changes privileges, which still asks and counts as `required`.
- `ptah_operator_applies_total{family,outcome}` counts started, completed,
  uncertain, and stale Apply transitions. A migration run that ended `Partial`
  or `Unknown` is `uncertain`: neither may be retried, and both leave a record
  that only a reading with nothing left to re-run, or a person, clears.
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
the plan Ptah saved. Resolve and Verify emit bounded typed evidence, Observe emits
only summaries, and Apply emits no native output. Native stderr never crosses
those operation boundaries; controller-facing failures are generic and typed.

Kubernetes Events provide the object-scoped audit trail for operation claims,
completion, policy refusal, approval changes, and failures. Alert on a sustained
increase in `ptah_operator_failures_total` and on any increase in
`ptah_operator_applies_total{outcome="uncertain"}`. Use status Conditions and
Events to identify the affected object instead of adding unbounded identity
labels to metrics.

### What is still unaccounted for {#unresolved-gauges}

The counters above are events. They say an Apply ended uncertain once, in a
process that may since have restarted, and nothing about whether one is still
standing now. Four gauges answer that, rebuilt from durable status on every
scrape rather than kept in memory, which is what makes them survive a restart
and survive an unrelated refusal rewriting the resource's conditions.

| Series | What it is |
| --- | --- |
| `ptah_operator_unresolved_attempts{family}` | Resources carrying a record of a mutation nobody accounted for |
| `ptah_operator_unresolved_owed_seconds{family}` | Seconds since the operator could first have settled the oldest such record. Absent where a family has none |
| `ptah_operator_unresolved_view_synced{}` | 1 when the leader read the complete state for this scrape |
| `ptah_operator_unresolved_view_read_failures_total{}` | Scrapes that could not read that state |

The leader reads both resource families and both plan kinds directly from the
API server once per scrape, sharing those results across the unresolved,
resource-state and plan-store gauges. All four lists share a three-second
budget. A failed or timed-out list suppresses every state gauge for that scrape
and increments the read-failure counter; a warm controller cache cannot conceal
the loss of API access. Followers publish no state. Losing leadership cancels
an in-progress read. These are separate lists, not an atomic cross-kind snapshot.
Plan chunks and ConfigMap payloads are not listed.

**Every alert on these has to require the view first.** A manager that has just
started, or one whose read failed, publishes no counts at all and reports
`ptah_operator_unresolved_view_synced 0`. An absent series and a series reading
zero are the same thing to most query languages, and here they mean opposite
things: one is "nothing is unresolved", the other is "nobody has looked yet".

```promql
ptah_operator_unresolved_view_synced == 1
  and ptah_operator_unresolved_attempts > 0
```

The gauges are aggregates, so they say that something is unaccounted for and
not which resource. That is deliberate -- an object name in a label grows the
series with the cluster -- and this is the drill-down:

```sh
kubectl get ptahmigrations -A -o json |
  jq -r '.items[] | select(.status.unresolvedRun != null) |
    "\(.metadata.namespace)/\(.metadata.name)\t\(.status.unresolvedRun.outcome)\t\(.status.unresolvedRun.jobName)"'
kubectl get ptahschemas -A -o json |
  jq -r '.items[] | select(.status.pendingObservation.outcome == "OutcomeUnknown") |
    "\(.metadata.namespace)/\(.metadata.name)\t\(.status.pendingObservation.applyJobName)"'
```

A migration named there is
[a run nobody accounted for](#a-migration-run-nobody-accounted-for) and waits
for a person. A schema named there settles itself once its horizon passes and
a reading of the database confirms convergence.

#### The chart's rules {#unresolved-rules}

With the Prometheus Operator, `monitoring.prometheusRule.enabled` renders a
`PrometheusRule` with three alerts over these gauges:

| Alert | Fires when | Severity |
| --- | --- | --- |
| `PtahOperatorUnresolvedApply` | Any resource of a family carries an unresolved record, from the first evaluation that sees it | critical |
| `PtahOperatorUnresolvedViewNotSynced` | No replica reports a synchronized view, or no replica reports at all, for `viewUnsyncedFor` | warning |
| `PtahOperatorUnresolvedViewReadFailures` | A scrape failed to read the state within the last `viewUnsyncedFor` | warning |

The first has no threshold and no pending period. There is no count or age of
unaccounted work that is fine to ignore, and the records are durable, so the
alert does not flap while one is being closed. The other two exist because
the first cannot fire while the view is unsynchronized or unreadable.

Only the elected leader starts the view; every other replica reports it
unsynchronized and publishes no counts. The rules take the `max` across
replicas, which reads the leader and never counts a record twice.

`viewUnsyncedFor` has no default, and the chart refuses to render the rules
without it. How long a manager takes to synchronize after a restart or a
leader change depends on the cluster it runs in, so measure it there: restart
the manager a few times and move leadership once
(`kubectl -n <release-namespace> rollout restart deployment/<release>`, and
delete the leader Pod), then read how many seconds the view spent
unsynchronized in the hour that covers them:

```promql
sum_over_time((max(ptah_operator_unresolved_view_synced) == bool 0)[1h:15s]) * 15
```

Set `viewUnsyncedFor` comfortably above the longest single stretch you saw.
The same number bounds the read-failure window, so a failure is reported for
that long after it happened.

The rules are tested with `promtool test rules` against the scenarios in
`hack/testdata/prometheusrule/`: one unresolved migration beside a follower,
a healthy fleet at zero, a view that stays unsynchronized past the window, a
leader change shorter than it, a lost scrape target, and a failed read.

#### The state gauges {#resource-state}

The same view publishes the fleet as it stands, under the same guard: nothing
while `ptah_operator_unresolved_view_synced` reads 0.

| Series | What it is |
| --- | --- |
| `ptah_operator_resources{family,phase}` | Resources by the phase their status reports; `Unset` before the first reconciliation |
| `ptah_operator_overdue_resources{family}` | Resources, not suspended, past their own `status.nextReconciliationTime` |
| `ptah_operator_overdue_seconds{family}` | How far past it the latest one is. Absent where none is overdue |
| `ptah_operator_active_operations{family,operation}` | Resources with an operation in flight, by type. A failed attempt waiting for its retry is not in flight |
| `ptah_operator_active_operation_seconds{family,operation}` | How long the oldest of each type has been eligible to run, excluding a declared retry delay |
| `ptah_operator_pending_lock_releases{family}` | Resources still owing the release of a realm Lease |
| `ptah_operator_stored_plans{family}` | Plans retained in the cluster. The operator prunes none; [pruning stored plans](#prune-plans) is the procedure |
| `ptah_operator_stored_plan_bytes{}` | Bytes the retained schema plans hold in their chunks, from each plan's `spec.size`. A migration plan stores no chunk |
| `ptah_operator_webhook_certificate_expiry_timestamp_seconds{}` | When the admission certificate this replica presents expires; every replica publishes it |
| `ptah_operator_webhook_certificate_read_failures_total{}` | Scrapes that could not read or parse that certificate, which publish no expiry |

Being overdue for a moment is normal: `nextReconciliationTime` passes when a
pass starts, and the status moves once its Jobs have run. What is not normal
is a resource that stays late, which is why the alert is on how late rather
than on whether.

Each of these alerts renders only when its threshold is set, and the chart
sets none:

| Value | Alert |
| --- | --- |
| `overdueAfterSeconds` | `PtahOperatorResourceOverdue` |
| `operationStalledAfterSeconds` | `PtahOperatorOperationStalled` |
| `lockReleaseOwedFor` | `PtahOperatorLockReleaseOwed` |
| `certificateExpiresWithinSeconds` | `PtahOperatorWebhookCertificateExpiring` |
| `planStoreBytesAbove` | `PtahOperatorPlanStoreLarge` |
| `failures.window` with `failures.count` | `PtahOperatorOperationsFailing` |
| `admissionFailingFor` | `PtahOperatorAdmissionUnavailable`, which reads the API server's metrics |

Once `PtahOperatorPlanStoreLarge` fires, a missing state reading keeps the
existing incident open. A fresh reading at or below the configured budget
clears it; loss of API access alone cannot establish that plans were pruned.

Read them off the installation they are for, over a period that includes a
rollout and a busy day, and set each comfortably above what normal work
produced:

```promql
max_over_time(max(ptah_operator_overdue_seconds)[7d:1m])
max_over_time(max(ptah_operator_active_operation_seconds)[7d:1m])
max_over_time(sum(increase(ptah_operator_failures_total[30m]))[7d:5m])
```

#### What reaches a receiver {#alert-delivery}

The rules are tested against synthetic series with promtool, and the path a
real alert takes is tested on a cluster. The PostgreSQL migrations suite ends
with a phase that stands up Prometheus, Alertmanager and a webhook receiver as
plain Deployments, loads the rules from the chart's `PrometheusRule` as a rule
file, scrapes every manager Pod behind the metrics Service, and asserts what
the receiver logged:

| Condition the phase creates | Alert at the receiver | Asserted |
| --- | --- | --- |
| An Apply Job removed while its run was going, from the migration rows | `PtahOperatorUnresolvedApply{family="migration"}` | Fires; the count in its summary equals what the drill-down above lists; its runbook link resolves to this page |
| A schema whose Resolve Pod no node will schedule | `PtahOperatorOperationStalled{family="schema",operation="Resolve"}` | Fires no earlier than the threshold and within the detection target; clears within the slack once the Pod runs |
| Every manager Pod deleted with every node cordoned | `PtahOperatorUnresolvedViewNotSynced` | Fires within `viewUnsyncedFor` plus the slack; clears once the managers are back |

With a five-second scrape and evaluation interval and a five-second
Alertmanager group wait, the phase declares a detection target of the rule's
own threshold plus 45 seconds, and a delivery later than that fails it. Those
intervals are the phase's, not a recommendation: a deployment that scrapes
every 30 seconds has a target larger by the same amount.

The phase does not drive a certificate close to expiry, an admission webhook
the API server cannot reach, or a failed upgrade hook. Their rules are tested
with promtool only. `PtahOperatorAdmissionUnavailable` reads the API server's
own `apiserver_admission_webhook_rejection_count`, and a Job that a hook left
failed is Kubernetes object state; both come from the cluster's Kubernetes
monitoring, not from this operator's endpoint.

The supported integration is a Prometheus that scrapes each manager Pod, as the
chart's `ServiceMonitor` configures one to, and loads the chart's rules, either
as a `PrometheusRule` through the Prometheus Operator or as the rule file that
object's `spec` is.

### Queries for a dashboard {#dashboard-queries}

A panel per question, over the series above. The state gauges come from the
leader, so each reads the max across replicas; the counters are per process,
so each sums them. The operation label on a state gauge is the API's type
(`Apply`), and on a counter it is the lower-case stage (`apply`).

| Question | Query |
| --- | --- |
| What state is the fleet in | `max by (family, phase) (ptah_operator_resources)` |
| What needs a person | `max by (family) (ptah_operator_unresolved_attempts)` and `max by (family) (ptah_operator_resources{phase=~"Blocked\|AwaitingApproval"})` |
| What has stopped being looked at | `max by (family) (ptah_operator_overdue_resources)` and `max by (family) (ptah_operator_overdue_seconds)` |
| How long operations take | `histogram_quantile(0.95, sum by (family, operation, le) (rate(ptah_operator_operation_duration_seconds_bucket[1h])))` |
| What is in flight, and for how long | `max by (family, operation) (ptah_operator_active_operation_seconds)` |
| What is failing | `sum by (family, stage, category) (increase(ptah_operator_failures_total[1h]))` |
| Whether a realm is held up | `max by (family) (ptah_operator_active_operation_seconds{operation="Apply"})` and `max by (family) (ptah_operator_pending_lock_releases)` |
| How much history is kept | `max by (family) (ptah_operator_stored_plans)` and `max(ptah_operator_stored_plan_bytes)` |
| What the manager costs | `process_resident_memory_bytes`, `rate(process_cpu_seconds_total[5m])` and `workqueue_depth` for the manager Pods |

A claimed Apply that is waiting for its realm's Lease holds its operation
claim while it waits, so contention for a realm reads as an Apply in flight for
longer than its Job would take, and `PtahOperatorOperationStalled` covers a
wait that does not end. The Lease names its holder:
`kubectl -n <operator namespace> get leases -o wide`. Two resources claiming
one realm without `spec.target.sharedRealm` is a refusal instead, and reads as
`Blocked` with reason `RealmConflict`; a resource a `PtahRealm` does not admit
reads as `Blocked` with reason `RealmNotAuthorized`.

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

Plans are owned by the schema, and a plan's chunks by the plan. Old plan objects may remain useful as
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
stored in immutable 512 KiB `PtahSchemaPlanChunk` objects; that chunk size
leaves headroom below the Kubernetes object-size limit after API JSON base64
encoding. A plan that reaches an Apply is copied into immutable ConfigMaps of
the same size and names, owned by the plan, which is what the Apply Pod
mounts.

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
| `ptahschema.status.pendingBindingRetirement.plan` | an execution-binding rotation retired it, and the approvals that name it are still being marked stale |
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
namespaces you are pruning, and delete access to plan objects in them. Nothing
cluster-scoped is touched.

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
    ($s.pendingBindingRetirement.plan.name // empty),
    ($s.activeOperation.planRef.name // empty),
    ($s.unresolvedRun.planRef.name // empty)' |
  sort -u > pinned.txt
kubectl get ptahschemaapprovals,ptahmigrationapprovals -A \
  -o jsonpath='{range .items[*]}{.spec.planRef.name}{"\n"}{end}' |
  sort -u >> pinned.txt
```

Delete a plan only if its name is absent from `pinned.txt`. Its chunks, and any
ConfigMaps an Apply projected it into, are owned by the plan, so garbage
collection removes them after it. Delete one plan at a time with
`--cascade=foreground --wait=true`, and wait for its payloads to disappear
before deleting the next. This bounds the garbage-collection work competing
with live API reads during maintenance. Each payload carries the label
`operator.ptah.run/plan=<plan-name>`, which is how to confirm they went.

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
jq -r '.spec.chunks | sort_by(.index)[] | .name' "$PLAN.plan.json" |
  while read -r chunk; do
    kubectl -n "$NS" get ptahschemaplanchunk "$chunk" -o json |
      jq -r '.spec.data' | base64 -d
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
