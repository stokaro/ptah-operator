---
title: Recover operator state
description: What to preserve after losing a cluster or a namespace, in what order to restore it, and what the operator refuses to assume.
---

The operations guide covers failures the operator recovers from on its own. This
page covers the one it cannot: the Kubernetes objects are gone and the database
is still there.

That case is different because the operator's safety does not come from the
YAML a person wrote. It comes from durable identities — object UIDs, execution
epochs, policy and artifact digests, and the record of an Apply whose outcome
nobody established. Reapplying spec-only YAML recreates the desired state and
none of those, so it cannot tell you whether the migration you approved last
week ran.

## What the operator keeps

Every kind carries durable state, and the version of the contract that wrote it
is stored with it. A manager that predates that contract refuses the state
rather than interpreting it, at startup and on every reconciliation.

| Kind | Where its controller-state version is stored |
| --- | --- |
| `PtahSchema` | `status.executionBinding`, `status.plan`, `status.applied`, `status.pendingObservation.plan` |
| `PtahSchemaPlan` | `spec` |
| `PtahSchemaApproval` | `spec` |
| `PtahMigration` | `status.executionBinding` |
| `PtahMigrationPlan` | `spec` |
| `PtahMigrationApproval` | `spec` |

A `PtahRealm` carries no controller state. It is an administrator's grant,
written and read as `spec` alone. Neither does a
`PtahMigrationRunAcknowledgment`: it is a person's statement that one run was
accounted for, and the migration it settled names it by UID in
`status.resolvedRun`.

A `PtahMigration` also keeps one thing outside status: a copy of
`status.unresolvedRun` in its `operator.ptah.run/unresolved-run` annotation,
which the manager writes before the status record and removes after it. It is
what survives a restore that drops status.

Some of what a recovery needs lives outside those objects:

- A `PtahSchemaPlan` stores its SQL in `PtahSchemaPlanChunk` objects and records
  each one by name **and UID** in `status.publishedChunks`. A `PtahMigrationPlan` stores no SQL:
  it names versions and checksums, and the statements stay in the artifact.
- The database lock is a Lease in the coordination namespace, one per
  coordination digest -- the engine hashed together with either the namespace
  and its coordination key, or the name of the `PtahRealm` a resource names.
  It is how two resources addressing one database take turns.
- The release identity is recorded on the admission singleton
  `ptah-operator-admission`, and the webhook certificate is a Secret in the
  operator namespace.

## Recovery modes

Decide which one you are in before touching anything. They differ in what you
are allowed to assume, not in how much work they are.

### Restart in an intact cluster

The objects are there and the managers are not. Nothing is lost: the operator
resumes its persisted operation, rejects a Job that is not the one its claim
names, and waits out the mutation deadline where a dispatched Apply cannot be
accounted for. This is the ordinary path and needs no procedure.

### Restore a consistent control plane

You have a backup of the Kubernetes objects taken while nothing was running,
and you are restoring all of it together. Object UIDs are preserved by the
backup tool, so plans still load and approvals still bind.

This is the only mode in which a recorded approval survives.

### Rebuild against a surviving database

The backup is older than the database, or there is no backup and you are
reapplying the specs. Assume nothing about what ran. The database is the only
witness, and the operator will read it before it plans anything.

Old approvals cannot authorize a rebuilt resource because their UID bindings
no longer match. That does not disable automatic application: a resource with
`spec.policy.apply: Always` may execute a new plan without a new approval.
For a controlled rebuild, create the resources with `spec.suspend: true` and
`spec.policy.apply: OnApproval`, account for any old execution, and then resume
them for read-only observation and planning. Keep the original backup unchanged
and record these edits in the recovery log. See [what does not
survive](#what-does-not-survive).

The migrations acceptance suite rehearses this mode on PostgreSQL and MySQL. It
backs up a `PtahMigration` and its approval while the approved Apply is claimed
and held, lets the run finish so the database moves past the backup, deletes
the resource, its plans and its approval, and moves the artifact tag on to one
more migration. It then reapplies both specs. The rebuilt resource asks for a
decision on the new migration, dispatches no Apply and records no run, and the
database holds what it held before the loss.

## What to preserve

A backup that omits any of these turns a consistent restore into a rebuild.

- The seven namespaced kinds that carry status, **including status**, and their
  annotations. A backup that keeps only `spec` keeps none of the record. One
  that keeps metadata and drops status -- Velero's default -- keeps the record
  of a run nobody accounted for, through its copy in the annotation, and
  nothing else of it: status is rebuilt by reading the database again.
- Every `PtahSchemaPlanChunk`, with its UID. A chunk has no status, but it
  holds the plan's bytes, and a `PtahSchemaPlan` names each of its chunks by
  UID in `status.publishedChunks`.
- Every `PtahRealm`. A realm has no status, but a restore without it refuses
  every resource that names it.
- The verification-policy ConfigMaps the plans and approvals bind by UID and
  digest.
- The Secrets holding database URLs and registry credentials. These are usually
  managed outside the operator; note where, because a restore that recreates
  them with new content changes the target identity the plans were bound to.
- The admission singleton and the webhook certificate Secret — or plan to
  reinstall the chart, which recreates them.

The ConfigMaps an Apply mounts its plan through need no backup. The operator
writes them from the chunks before it creates an Apply Job, and writes them
again for the next one.
- The OCI artifacts the resources name, by digest. They are outside the
  cluster; a registry that garbage-collected the digest a plan was built from
  makes that plan unreproducible.

Leases are deliberately not on this list. Restoring one hands the new manager a
holder that no longer exists and makes it wait out an interval for nothing.

## Order

1. **Quiesce first.** Stop the old managers and prevent their replacement.
   Account for every executor that may still reach the database. Scaling a
   manager to zero does not stop an existing Apply Job; an unreachable
   Kubernetes API does not prove that its Pods or database sessions stopped.
   Before enabling replacement execution, confirm the old executors and their
   database sessions have ended, or fence their database access and verify that
   the fence also stops existing sessions. Rotating credentials alone does not
   establish that. Preserve the old operation identities and uncertain results
   before removing anything, and keep the fence in place through recovery.
2. Restore or reinstall the release: CRDs, the chart, the admission singleton,
   the certificate Secret.
3. Restore the Secrets and the verification-policy ConfigMaps.
4. Restore the `PtahRealm` objects, then the seven namespaced kinds with their
   status, then the plan chunks.
5. Start the manager. It re-reads the database before it plans.

Do not start the manager between steps. Its first reconciliation will act on
whatever is present, and a partial restore is a different resource.

## What does not survive

A restore that changes UIDs invalidates every binding that names one, and the
operator refuses rather than guessing. This is the safety property, not a
limitation to work around:

- A plan whose chunks came back with new UIDs cannot be loaded. The operator
  observes the database and publishes a new plan.
- An approval names `spec.planRef` by name **and** UID. A new plan UID leaves
  the approval bound to a plan that no longer exists, so it authorizes nothing.
- An execution binding names the components that ran. A rebuilt resource gets a
  new epoch, and an operation claimed under the old one cannot be resumed.

With `spec.policy.apply: OnApproval`, a rebuilt resource waits for a fresh
decision. An approval written before the loss does not carry over, and an
operator who wants the work to proceed
approves the plan the rebuilt resource publishes — against the database as it
is now, rather than as it was when the old plan was made.

## An Apply that was in flight

What you can establish depends on the family.

A `PtahMigration` keeps the answer in the database. The revision table records
which versions were applied, and a rebuilt resource reads it -- after resolving
and verifying its artifact, and before it plans anything. A version that was mid-flight is either recorded or not, and the
history read says which.

What stops a blind replay is `status.unresolvedRun`, and whether a rebuild
keeps it depends on what the rebuild was made from. A backup that kept the
resource's metadata keeps the record's copy in the
`operator.ptah.run/unresolved-run` annotation, and the manager puts the record
back from it before the rebuilt resource plans anything. Specs reapplied from
source control carry no copy, and neither does a backup taken while the Apply
was still running, before its outcome was read: then the record is lost, so if
the old resource carried one, or may have been about to, establish what that
run did **before** you let the rebuilt resource proceed. A migration that
committed part of a file and cannot say which part is the case that needs a
person; the runbook for it is [A migration run nobody accounted
for](../operations/#a-migration-run-nobody-accounted-for).

A `PtahSchema` is declarative, and the database is the whole answer. A rebuilt
resource observes the live schema and plans the difference. An Apply that was
in flight either changed the schema, in which case the observation sees it, or
did not, in which case the plan includes it again.

Neither family is rolled back. The operator repairs forward from what the
database holds.

## Verify the recovery

Read these before letting anything run.

```sh
# Nothing is claimed, and nothing is blocked.
kubectl get ptahschemas,ptahmigrations -A -o custom-columns=\
KIND:.kind,NS:.metadata.namespace,NAME:.metadata.name,\
PHASE:.status.phase,OP:.status.activeOperation.type

# No migration carries a run nobody accounted for.
kubectl get ptahmigrations -A \
  -o jsonpath='{range .items[?(@.status.unresolvedRun)]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'

# Every plan that claims to be stored can still name its chunks.
kubectl get ptahschemaplans -A \
  -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,\
CHUNKS:.status.publishedChunks[*].name

# No database lock is held by a manager that is gone.
kubectl get leases -n "$COORDINATION_NAMESPACE"
```

Then start the manager and confirm each resource reaches a phase you expect. A
resource that goes to `Blocked` is telling you which assumption did not hold;
[Condition reasons](../../troubleshoot/condition-reasons/) says what each refusal
means.

## What this page does not promise

The operator does not roll a database back. It has no snapshot of what the
schema was, and repairing forward from the live database is the only operation
it performs.

Backup frequency, retention, the recovery point you can reach, and how long a
restore takes are the deployment owner's, because they follow from the backup
system and the database, not from this operator. Record them for your
deployment before you need them, and record the two separately: the database
and the Kubernetes objects are backed up by different systems, and the gap
between their recovery points is exactly the window in which a rebuild has to
assume nothing.
