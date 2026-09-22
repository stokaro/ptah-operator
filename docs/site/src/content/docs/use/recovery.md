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

Some of what a recovery needs lives outside those objects:

- A `PtahSchemaPlan` stores its SQL in ConfigMaps and records each one by name
  **and UID** in `status.publishedChunks`. A `PtahMigrationPlan` stores no SQL:
  it names versions and checksums, and the statements stay in the artifact.
- The database lock is a Lease in the coordination namespace, one per
  coordination digest -- the engine and the coordination key hashed together.
  It is how two resources addressing one database take turns.
- The release identity is a ConfigMap named `ptah-operator-release-activation`
  in the operator namespace, beside the admission singleton
  `ptah-operator-admission` and the webhook certificate Secret.

## Three recovery modes

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

This mode is safe by construction, and the reason is worth stating: nothing
that authorized work survives it. See [what does not
survive](#what-does-not-survive).

## What to preserve

A backup that omits any of these turns a consistent restore into a rebuild.

- All six kinds, **including status**. A backup that keeps only `spec` keeps
  none of the record.
- The plan ConfigMaps a `PtahSchemaPlan` names in `status.publishedChunks`,
  with their UIDs.
- The verification-policy ConfigMaps the plans and approvals bind by UID and
  digest.
- The Secrets holding database URLs and registry credentials. These are usually
  managed outside the operator; note where, because a restore that recreates
  them with new content changes the target identity the plans were bound to.
- The release activation ConfigMap, the admission singleton, and the webhook
  certificate Secret — or plan to reinstall the chart, which recreates them.
- The OCI artifacts the resources name, by digest. They are outside the
  cluster; a registry that garbage-collected the digest a plan was built from
  makes that plan unreproducible.

Leases are deliberately not on this list. Restoring one hands the new manager a
holder that no longer exists and makes it wait out an interval for nothing.

## Order

1. **Quiesce first.** Scale the manager and the certificate rotator to zero, or
   confirm the old cluster is unreachable. A restore performed while an old
   manager can still reach the database is the one way this procedure can cause
   the harm it exists to prevent.
2. Restore or reinstall the release: CRDs, the chart, the admission singleton,
   the certificate Secret, the release activation ConfigMap.
3. Restore the Secrets and the verification-policy ConfigMaps.
4. Restore the plan ConfigMaps, then the six kinds with their status.
5. Start the manager. It re-reads the database before it plans.

Do not start the manager between steps. Its first reconciliation will act on
whatever is present, and a partial restore is a different resource.

## What does not survive

A restore that changes UIDs invalidates every binding that names one, and the
operator refuses rather than guessing. This is the safety property, not a
limitation to work around:

- A plan whose chunk ConfigMaps came back with new UIDs cannot be loaded. The
  operator observes the database and publishes a new plan.
- An approval names `spec.planRef` by name **and** UID. A new plan UID leaves
  the approval bound to a plan that no longer exists, so it authorizes nothing.
- An execution binding names the components that ran. A rebuilt resource gets a
  new epoch, and an operation claimed under the old one cannot be resumed.

So a rebuilt resource waits for a fresh decision. An approval written before
the loss does not carry over, and an operator who wants the work to proceed
approves the plan the rebuilt resource publishes — against the database as it
is now, rather than as it was when the old plan was made.

## An Apply that was in flight

What you can establish depends on the family.

A `PtahMigration` keeps the answer in the database. The revision table records
which versions were applied, and a rebuilt resource reads it -- after resolving
and verifying its artifact, and before it plans anything. A version that was mid-flight is either recorded or not, and the
history read says which. What a rebuild loses is
`status.unresolvedRun` — the record that stops a blind replay — so if the old
resource carried one, establish what that run did **before** you let the
rebuilt resource proceed. A migration that committed part of a file and cannot
say which part is the case that needs a person; the runbook for it is [A
migration run nobody accounted for](../operations/#a-migration-run-nobody-accounted-for).

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
