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

## What the history says

```sh
kubectl ptah migration orders -n application
```

`status.history` is the operator's record of one reading of the revision table:
the current version, how many migrations are applied, how many are pending, and
two states that stop everything.

**Dirty.** A failed or interrupted run left a revision row behind. Nothing
applies while one exists, and the operator never removes it: what a
half-applied migration did to your data is a question for a person with the
database in front of them. Ptah's own `migrations repair` and
`migrations baseline` are how that ends.

**Modified.** An applied migration's file no longer hashes to what the database
recorded for it. This is the refusal the whole versioned workflow exists to
make — the file you are shipping is not the file that ran — and the versions
are published so you can find them.

Neither resolves by waiting. The operator keeps reading the history at
`spec.interval`, so fixing the database is enough to unblock it; nothing else
is required.

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

## What never appears

No row of any table reaches the status, an Event, or `kubectl ptah migration`.
No SQL does either: a published plan records versions, descriptions and
checksums, and the statements stay in the artifact you built.
