---
title: Run versioned migrations
description: What a PtahMigration does, what it refuses to do, and how an approval authorizes one run.
---

A `PtahSchema` reconciles a desired structure: you declare what the database
should look like, and the operator works out the statements. A `PtahMigration`
executes a sequence somebody already wrote — numbered SQL files in an OCI
artifact — and matches them against the database's own record of what has run.

Use the second when the migrations are the source of truth: when a file does
data backfill the schema cannot express, when the order matters, or when your
team already reviews migrations in the repository that produces them.

The two resources are independent. One database can have both, and they
serialize against each other through the same lock.

## What the operator does, in order

```
Resolve   the artifact reference to a digest
Verify    that digest against the verification policy
Read      the database's own revision table against the artifact
Plan      the pending sequence, and publish it
Approve   that exact plan, if the policy asks for one
Apply     the approved sequence
Verify    the history again, to confirm what the run did
```

Each step is a Job that holds either the registry credentials or the database
URL, and never both. The migration files reach the container that runs SQL
through an init container that fetched them; the process holding your database
URL never talks to your registry.

## The minimal resource

`examples/ptahmigration.yaml` is the annotated version; this is the shape:

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigration
metadata:
  name: orders
  namespace: application
spec:
  target:
    engine: PostgreSQL
    coordinationKey: prod/application/orders-primary
    urlFrom:
      name: orders-database
      key: url
  artifact:
    ociRef: oci://registry.example/acme/orders-migrations:stable
    verificationPolicyFrom:
      name: orders-verification
      key: policy.yaml
```

`coordinationKey` names the physical database. Two resources that address the
same database must use the same key, in any namespace: it is what makes them
take turns rather than run at once.

Taking turns is not enough to share a database, though, and the operator does
not pretend otherwise. A database claimed by more than one resource is refused
— every claimant `Blocked`, no Job — unless each of them sets
`spec.target.sharedRealm: true`. One that has not blocks all of them. See
[One database, one manager](../operations/#one-database-one-manager).

The verification policy names the artifact type it accepts, and a migration
directory is not a schema:

```yaml
version: 1
artifact_types:
  - application/vnd.stokaro.ptah.migrations.v1
```

A policy that also listed `application/vnd.stokaro.ptah.schema.v1` would let a
schema artifact stand in for a migration directory at the same reference, so
give a migration its own policy rather than reusing the schema one.

## Choosing a transaction mode

By default the operator names no transaction mode and Ptah picks one. That is
the right setting for PostgreSQL, where a migration file runs inside a
transaction and either lands whole or not at all.

MySQL and MariaDB have no transactional DDL. A migration that alters a table
commits as it goes, whatever the surrounding transaction says, so Ptah cannot
witness the file as one unit — and it refuses rather than pretending, which is
why a migration on those engines may stop before a statement runs. Naming the
mode is how you tell it that you know:

```yaml
spec:
  policy:
    transactionMode: none
```

`none` means Ptah wraps nothing, and each statement stands on its own. The
consequence is the one worth planning for: a file that fails halfway leaves
what it already committed, and the history records the run as dirty rather than
as applied. Nothing rolls back for you.

`file` is the other spelling, and it asks for the per-file transaction
explicitly. It is what PostgreSQL gets when the field is unset.

Leaving the field out keeps today's behavior exactly, so an existing resource
does not change when you upgrade.

## What the history says

```sh
kubectl ptah migration orders -n application
```

`status.history` is the operator's record of one reading of the revision table:
the current version, how many migrations are applied, how many are pending, and
three states that stop everything.

**Dirty.** A failed or interrupted run left a revision row behind. Nothing
applies while one exists, and the operator never removes it: what a
half-applied migration did to your data is a question for a person with the
database in front of them. Ptah's own `migrations repair` and
`migrations baseline` are how that ends.

**Modified.** An applied migration's file no longer hashes to what the database
recorded for it. This is the refusal the whole versioned workflow exists to
make — the file you are shipping is not the file that ran — and the versions
are published so you can find them.

**Out of order.** The artifact carries a migration whose version sorts below
one the database has already applied, which happens when a migration written on
one branch lands after a later one has run. Ptah executes in linear order and
refuses the whole run while such a file is pending, so the operator refuses
first: publishing a plan for it would ask you to approve a sequence the
executor could never run. `status.history.outOfOrderVersions` names the files,
because which one arrived late is what the decision depends on. Renumbering it
above the current version, or applying it deliberately with Ptah's own
non-linear execution order, are both decisions a person makes.

**Ahead.** The database records a migration this artifact does not carry:
`status.history.currentVersion` is past everything the artifact accounts for.
A rolled-back deployment, a tag moved to yesterday's build, a branch whose
migrations were never merged, all arrive here. The operator will not roll a
database back to match an older artifact, and which of the two is wrong is not
a question it can answer, so it stops and names both versions.

None of the four resolves by waiting. The operator keeps reading the history at
`spec.interval`, so fixing the database or the artifact is enough to unblock it;
nothing else is required. For the last of them that usually means restoring the
artifact the database was migrated with, rather than touching the database at
all.

**Checkpoints.** A migration file marked as a checkpoint carries the whole
schema up to its version, so a database created after it bootstraps from that
file instead of replaying everything before it.
`status.history.checkpointVersion` names the checkpoint covering the versions
below it, and those versions are reported as applied rather than pending. It
stays after the bootstrap has run, because the coverage is what keeps them
applied: a reading that dropped the checkpoint would leave migrations 1 and 2
covered by nothing, report them pending again, and publish a plan for versions
the database already holds. A database that was already past the checkpoint when
it arrived ignores it: a checkpoint changes where a new database starts, never
what an existing one has run.

**An existing schema with no history.** An empty revision table is not the same
thing as an empty database, and the operator cannot tell the two apart: Ptah
reports every migration pending either way. So it never guesses. It publishes
the whole sequence as a plan, waits for the approval its policy asks for, and
writes nothing to the database in the meantime. On a database that already
carries the schema, approving that plan runs migrations the engine refuses, and
the refusal leaves a dirty revision behind.

Record the history first, with Ptah's own baseline and a disposable shadow
database:

```sh
ptah migrations baseline \
  --db-url "$DATABASE_URL" \
  --migrations-dir ./migrations \
  --shadow-db "$SHADOW_DATABASE_URL"
```

The shadow database is where the migrations are replayed, so the schema they
produce can be compared with the schema the target already has; the revisions
are recorded only when the two agree. The operator reads the recorded history on
its next interval and reports `InSync`, having run nothing.

## Approving a run

`spec.policy.apply` decides what a published plan may do:

| Value | What happens |
| --- | --- |
| `OnApproval` (the default) | The plan waits for a `PtahMigrationApproval` naming it |
| `Always` | The plan runs as soon as it is published |
| `Never` | The plan is published and never runs |

The default is the conservative one because a migration artifact carries
arbitrary SQL, and no analyzer classifies arbitrary SQL as safe.

An approval names the migration, the plan and the plan's fingerprint; the
admission webhook fills in the rest from the plan itself and refuses any value
that conflicts with it. `examples/migration-approval.yaml` carries the commands
that print those three identifiers:

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigrationApproval
metadata:
  name: orders-to-20260830
  namespace: application
spec:
  migrationRef:
    name: orders
    uid: <status.plan is published against this UID>
  planRef:
    name: <status.plan.name>
    uid: <status.plan.uid>
  planFingerprint: <the plan's spec.fingerprint>
```

The webhook refuses an approval the evidence no longer supports: a replaced
plan, a history somebody else moved in the meantime, a re-resolved artifact, a
changed verification policy, a rolled-out executor, or a migration that is not
waiting for a decision. An approval authorizes one execution.

## What the run reports

The verdict is the database's, read from the revision table rather than from
the Job's exit status — a run that stopped is exactly the run whose outcome
matters most:

| `status.lastRun.outcome` | What it means |
| --- | --- |
| `UpToDate` | The database already had every planned migration |
| `Applied` | Every selected migration is recorded applied |
| `Failed` | A migration failed and committed nothing; fix the cause and it runs again |
| `Partial` | A migration committed some of its statements and not the rest |
| `Unknown` | The evidence could not be read |

`Partial` and `Unknown` stop the resource and are never retried. Re-running a
file that committed half its statements would run them twice, and no controller
can know which half. Both leave the database to a person.

Every other outcome is confirmed by reading the history back. What a run claims
and what the revision table holds are two statements, and only the second one
settles the resource.

`Progressing` is the condition a dashboard watches to decide whether the
resource is still moving on its own. It is true only while something is coming:
an operation is running, a plan is being published, an Apply is next under the
`Always` policy, or a finished run is being confirmed against the history. It is
false wherever the resource has stopped or is waiting for a person, and its
reason says which: `AwaitingApproval`, `ApplyDisabled`, `HistoryDirty`,
`HistoryModified`, `HistoryOutOfOrder`, `HistoryAhead`, `ApplyOutcomeUnknown`,
`HistoryMatched`.

## What never appears

No row of any table reaches the status, an Event, or `kubectl ptah migration`.
No SQL does either: a published plan records versions, descriptions and
checksums, and the statements stay in the artifact you built.
