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

The release namespace is trusted infrastructure: whoever can create workloads
there is a Ptah administrator, as
[the release namespace contract](../../use/security/#release-namespace) says.
Several mechanisms below go further and defend the hooks against a hostile
writer inside that namespace. They sit outside the contract, and
[#443](https://github.com/stokaro/ptah-operator/issues/443) removes them. The
checks against a stale or buggy previous release stay: schema identity, the
downgrade preflight and the runtime verifier.

## The hooks and what each decides

`ptah-crd-manager` runs as Helm hooks. Each mode validates its preconditions
before proceeding:

| Mode | What it decides |
| --- | --- |
| `identity-probe` | Whether the live release identity matches the one being installed |
| `preflight` | Whether the install or upgrade may start at all: ownership, RBAC, namespaces, PriorityClass, resource quota |
| `reconcile` | The CRDs themselves, against the stored schema history |
| `verify`, `runtime-verify` | That what was applied is what runs |
| `teardown-quiesce` | Uninstall: stop the runtime, then delete what the release keeps outside Helm's own deletion |

A refusal is written to the container's termination message as well as to
stderr, because Helm reports only that a hook Job failed: without that, an
operator is told an uninstall was refused and never why.

`ptah-cert-rotator` owns the webhook serving certificates. One reconciliation
at a time, serialized by a Lease of its own: it issues or renews the CA
through a staged transition, issues the serving certificate, repairs the trust
bundles on both webhook configurations, and probes every endpoint directly to
confirm the replacement is being served before it adopts it. A CA transition
publishes the old and the new CA side by side, switches the serving certificate
no earlier than a configured delay later, and withdraws the old CA once every
endpoint serves the new certificate; each step is recorded before the next.
The manager never receives permission to read the Secret this writes.

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
dedicated hook ServiceAccount can `get` and `update` only the seven exact Ptah
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
location records none yet. During a Helm downgrade, the pre-upgrade
hook fails before Helm changes the Deployment, so the newer ready manager Pods
remain in place and the older candidate never gets an opportunity to
reinterpret or rewrite future state.
A location that records no version, such as a resource the manager has not
reconciled yet, blocks nothing. A malformed or negative
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
TokenRequests for the candidate and predecessor controller identities. The hook
then stops the runtime and waits until no Pod running as a protected runtime
identity remains in the namespace before any grant moves. A failed response at
any transition is retried only for the same target and full attempt digest;
there is no cancellation or backward state transition. A candidate that fails
after the drain began leaves it in place, and rerunning the same candidate
finishes the cutover. A fresh install can skip the drain only when the
activation state and complete preflight prove that no predecessor, candidate
ServiceAccount, candidate grant, protected Pod, or prior drain exists.

For a predecessor cutover, the hook receives `bind` only on the stable
controller ClusterRole and the exact existing controller Roles in their
coordination, release, and discovery namespaces. A fresh install receives no
`bind` grant. The ServiceAccount-origin guard permits only the current reconcile
Pod to replace the predecessor subject with the candidate subject during that
attempt's exact draining state. Role references, binding identity and metadata,
and certificate subjects must remain unchanged. Other binding writes, including
granting a role to the hook itself, are denied.

## The admission inventory marker

Each release attempt owns an immutable, sequence-keyed admission marker. The
chart inventories at most the active predecessor marker and current candidate
marker, rejects gaps, future or malformed markers, and refuses a same-sequence
marker bound to another full manager-image attempt. Before activation, the
reconcile hook reads every release-scoped admission policy and binding this
release installed, and the hook identity probe ConfigMap, checks each against
the contract it compiles, and seals the marker immutable with the live UID and
a semantic digest of each object. The next release's hook reads that inventory
back after it activates and retires exactly those objects: every binding first,
then every policy, the probe ConfigMap and finally the marker, each deleted with
the UID and resourceVersion of an immediate re-read. It never discovers what to
delete from a label or a name prefix.

The hooks and the runtime init verifier compare the stored contract of every
retained guard exactly and wait for its CEL type checking; they do not probe
whether each API server has loaded it. The optional missing-Secret recovery
policy is installed later, and the rotator verifies the pair's complete stored
structure before using its Secret `create` permission.

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
must be absent. The rotator no longer calls or updates the canary webhooks;
they remain because this verifier still requires them. Nothing else in either configuration is accepted.
The verifier reads this complete contract again after its wait and rechecks the
CRDs immediately before allowing the process to start. A losing release or a
Pod launched while Helm is repairing a drifted singleton can neither reconcile
schemas nor patch the winning release's CA bundle.

## Uninstall

An uninstall is one pre-delete hook Job, `teardown-quiesce`, running as the
release's CRD manager ServiceAccount. It keeps the name and arguments of the
quiesce step it grew out of, because the admission guards that are still in
place while it runs admit exactly that Job.

The Job first reads every object it is about to delete and checks each one
against the contract this release compiles, without changing anything, so an
inventory it would refuse to delete fails before the runtime stops. It then
stops the runtime: it records a drain toward the active sequence in the
release activation, which is what the retained rollout guards require before
they admit the stop, scales both runtime Deployments to zero, and waits until
no Pod in the namespace runs as a runtime identity. Only then does it delete
the admission guards that fence the controller's writes, so no controller runs
once they are gone.

It deletes, by exact name, what the release keeps outside Helm's own deletion:
every admission guard binding, then every guard policy, the certificate
staging Secret when the chart generates certificates, the hook identity probe
and parent-origin readiness ConfigMaps, the release activation guard, the
admission inventory marker, and the release activation parameter last. Each
object is read, checked and deleted with the UID and resourceVersion of that
read; the staging Secret is deleted by name without being read, because it
holds a pending CA private key. An object already gone is skipped, so rerunning
the uninstall after a failure deletes what is left.

A guard the Job has just deleted can still be cached by an API server for a
moment, so a refusal that names a ValidatingAdmissionPolicy, a conflict, and an
API server that does not answer are retried until the Job's own deadline. Any
other refusal, and an object that differs from its contract, stops the Job with
that reason in its termination message.

The release activation goes back to the state a fresh install starts from
before it is deleted. An API server can go on serving a deleted policy
parameter, and a reinstall in the same namespace needs what it serves to be the
bootstrap state rather than the sequence the removed release last activated.

Helm then deletes everything else in the release. The CRDs and their objects
stay, and so does the parameter informer anchor below. A webhook entry an API server still
caches after the uninstall is an availability residue, not authority a
removed release keeps.

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

