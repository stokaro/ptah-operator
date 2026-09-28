---
title: Release lifecycle
description: What install, upgrade, rollback and uninstall do, and what each refuses.
---

A release is installed, upgraded, rolled back and removed by Helm, with one
hook of its own: the CRD manager's reconcile Job. This page is what that hook
and the runtime verifier check, and why a refusal reads the way it does.
Performing any of it is
[Operations](../../use/operations/#installation-and-upgrades), and nothing
here has to be read to follow a runbook there. What a release promises about
objects you have stored is
[API compatibility](../../support/api-compatibility/).

The release namespace is trusted infrastructure: whoever can create workloads
there is a Ptah administrator, as
[the release namespace contract](../../use/security/#release-namespace) says.
Nothing below defends the release against a writer inside that namespace. What
it checks is a stale or buggy previous release: schema identity, the downgrade
preflight and the runtime verifier.

## The hook and the verifier

`ptah-crd-manager` runs in three modes:

| Mode | Where it runs | What it decides |
| --- | --- | --- |
| `reconcile` | The Helm hook, on install, upgrade and rollback | The stored state, the CRDs, and whether the running release has to stop first |
| `runtime-verify` | The init container of the manager and the certificate rotator | That the CRDs and the admission singleton are the ones this release installs, and for the manager, that the stored state is one it reads |
| `verify` | By hand | That the live CRDs match the ones this binary embeds and are established |

A refusal is written to stderr and to the container's termination message.
Helm deletes a failed hook Job, but it prints the Job's log first, because the
Job carries `helm.sh/hook-output-log-policy: hook-failed`: the refusal is in
the output of the command that failed, rather than only the fact that a hook
Job did.

`ptah-cert-rotator` owns the webhook serving certificates. One reconciliation
at a time, serialized by a Lease of its own: it issues or renews the CA
through a staged transition, issues the serving certificate, repairs the trust
bundles on both webhook configurations, and probes every endpoint directly to
confirm the replacement is being served before it adopts it. A planned CA
renewal publishes the old and the new CA side by side, switches the serving
certificate no earlier than a configured delay later, and withdraws the old CA
once every endpoint serves the new certificate; each step is recorded before
the next. A missing Secret is recreated without the delay.
The manager never receives permission to read the Secret this writes.

## What the CRD hook does

CRDs are stored under the chart's `crds/` directory and are installed before
templated resources. The candidate operator image embeds the exact same
generated CRDs. One reconcile hook runs for `pre-install`, `pre-upgrade` and
`pre-rollback`, which makes a fresh installation over CRDs retained by an
earlier uninstall safe and usable, and holds a rollback to the checks an
upgrade passes. The hook runs the image of the release Helm is moving to: on a
rollback, the image of the revision being restored.

In order, it:

1. refuses a chart of another release than its image, before it reads the
   cluster: the chart hands it its controller-state version, and the image
   compiles the same one;
2. scans every durable controller-state version, as described below;
3. checks the schema identity of every CRD against the one it embeds, and
   dry-runs every required schema change;
4. scans the stored state again, and stops the running release when its
   manager image differs from the hook's;
5. scans the stored state a third time, now that no old manager can write it;
6. updates only the `spec` and the two owned schema-identity annotations of
   each existing CRD, and waits for both `Established=True` and
   `NamesAccepted=True`.

The first step is what refuses a `--reuse-values` upgrade that keeps the
previous `image.digest` under a new chart, and a new image under an old chart.
Without it, the first pairing would leave the running release to the new
chart's Deployments, whose verifier refuses to start the old image, and the
second would stop the runtime and update the CRDs before anything noticed.

A refusal before the stop leaves the running release exactly as it was,
because nothing before it changes anything. A refusal after it leaves the
runtime stopped until an upgrade or a rollback to a release that passes these
checks brings it back. Helm does not apply the new Deployments until the hook
succeeds.

The hook never creates, deletes, or force-applies a CRD. A missing CRD, an API
identity conflict, a stored version absent from the candidate, a rejected
dry-run, or an incompatible schema identity introduced concurrently makes the
Helm operation fail. The hook's own ServiceAccount can `get` and `update` only
the nine exact Ptah CRD names. Separate read-only `list` grants for every kind
that stores a controller-state version exist solely for the downgrade
preflight. In the release namespace it can `get` and `update` the two runtime
Deployments by name and `list` Pods, which is what stopping the runtime takes.
Helm deletes the hook's RBAC after either success or failure. Replacing only
`spec` and those two owned annotations preserves CRD UIDs, all other metadata,
status, and all custom resources.

### Stopping the running release

The approval webhooks answer with a patch computed from the object the manager
decoded, so a manager older than the schema would drop every field it does not
know from the objects it mutates. The hook therefore stops the running release
before the first CRD update whenever its manager image differs from the running
one, in either Deployment's template or in any Pod carrying the release's
runtime labels: it scales the certificate rotator and then the manager to zero.
It waits until each Deployment reports that it observed the scale-down and runs
no replica, and until no Pod carrying the release's instance and component
labels is left, terminating or not. A release already running the hook's image
is left alone, because the image fixes the CRDs it serves.

A Deployment that Helm did not install for this release is refused rather than
scaled. One that does not exist has nothing to scale, but its Pods are looked
for all the same: a Deployment deleted with orphaned dependents leaves Pods
that still serve, and the hook refuses at its deadline rather than update a CRD
under them. Stopping twice changes nothing: a stopped Deployment stays at zero,
and the wait finds no Pod. That is what makes rerunning the same upgrade the
recovery for one that failed after the hook.

The scale-down leaves `.spec.replicas` owned by the hook's field manager, and
Helm 4 applies the release server-side, so the apply that raises it again is a
conflict unless it carries `--force-conflicts`. The hook writes no other field.

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
narrowing a newer schema even if a developer forgot to bump the version, and it
is what refuses a rollback to a release whose schemas are older than the ones
installed.

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

The hook, and the manager's init verifier before a manager starts, scan every
kind that stores a controller-state version across the cluster: `PtahSchema`,
`PtahSchemaPlan`, `PtahMigration` and `PtahMigrationPlan`. They check
controller-state versions in `PtahSchema.status.executionBinding`,
`status.plan`, `status.applied`, and `status.pendingObservation.plan`, in
`PtahMigration.status.executionBinding`, and in the immutable
`spec.controllerStateVersion` carried by every plan. An approval stores none:
it names a plan by UID and fingerprint, and the plan carries the version the
approved decision was made under.
Every kind is read through exhaustive pagination anchored to its own single
collection `resourceVersion`. A nonzero version newer than the binary's
supported controller-state version blocks the release, even if another stored
location records none yet. During a Helm downgrade or rollback, the scans
before the stop refuse with the newer manager still running, and the older
candidate never gets an opportunity to reinterpret or rewrite future state.
State the newer manager writes between those scans and the stop is found by
the scan after it, which refuses with the runtime already stopped: the older
candidate still never runs, and the runtime stays down until an upgrade or a
rollback to a release that reads the state.
A location that records no version, such as a resource the manager has not
reconciled yet, blocks nothing. A malformed or negative stored version also
blocks. The hook repeats the scan after all server-side dry-runs, and again
after the old runtime Pods have stopped, immediately before the first real CRD
update. The controller init verifier repeats both CRD and state checks after
the admission singleton becomes ready. A missing client for any of the durable
resource collections fails closed.

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
controller writes -- with fail-closed policies, nonempty CA bundles, the exact
Service, paths, port, rules, selectors, match conditions, review version,
side-effect and match policies, reinvocation policy, and their bounded
timeouts. Nothing else in either configuration is accepted.
The verifier reads this complete contract again after its wait and rechecks the
CRDs immediately before allowing the process to start. A losing release or a
Pod launched while Helm is repairing a drifted singleton can neither reconcile
schemas nor patch the winning release's CA bundle.

## Rollback

`helm rollback` runs the reconcile hook of the revision it restores, with that
revision's image. The hook refuses stored state or CRD schemas newer than that
release reads. The checks before the stop refuse a rollback the cluster has
outgrown with the running release in place; state written in the moment
between them and the stop is found by the scan after it, and then the runtime
stays stopped. A rollback the hook allows stops the running release, because
the images differ, and Helm brings the restored revision up.

Helm records the rollback revision before it runs the hook. A refused rollback
therefore leaves that revision `pending-rollback`, and a later `helm upgrade`
refuses to start while it is. Another `helm rollback`, to a revision the
stored state allows, clears it.

`helm upgrade --rollback-on-failure` rolls back to the last deployed revision
when the upgrade fails. After an upgrade that failed once its hook had updated
the CRDs, that rollback runs the previous release's hook, which refuses the
newer schemas, and the release is left `pending-rollback`. `helm rollback
<release>` with no revision rolls back to the revision before the pending one,
which is the upgrade that failed: its hook reads the schemas it wrote, and Helm
brings that release up.

## Uninstall

An uninstall runs no hook. Helm deletes every object the release installed,
the admission policies and their bindings among them. The CRDs and their
objects stay: Helm deletes neither what it installed from `crds/` nor a CRD a
later release added through its bootstrap hook. A webhook entry an API server
still caches after the uninstall is an availability residue, not authority a
removed release keeps.
