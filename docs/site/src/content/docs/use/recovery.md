---
title: Recover operator state
description: What to preserve after losing a cluster or a namespace, in what order to restore it, what the operator refuses to assume, and how to retry a migration whose statement failed.
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

[A migration that failed at a statement](#failed-statement) is the other case at
this point: the cluster is intact, and the resource waits on a row in the database.

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

The migration-faults acceptance suites rehearse this mode on PostgreSQL and
MySQL. Each backs up a `PtahMigration` and its approval while the approved
Apply is claimed and held, lets the run finish so the database moves past the
backup, deletes the resource, its plans and its approval, and moves the
artifact tag on to one more migration. It then reapplies both specs. The
rebuilt resource asks for a decision on the new migration, dispatches no Apply
and records no run, and the database holds what it held before the loss.

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
- Every `PtahResultRecord`, preserving its UID, owner references, metadata, and
  immutable payload. These internal records can contain SQL or delivery key
  material; keep them in the restricted backup. Partial publications and records
  with changed identities cannot establish an acknowledged result.
- With durable result delivery enabled, the receiver trust Secret, its private
  rotation journal Secret, and the public enrollment-policy ConfigMap. Preserve
  their UIDs together: the journal binds the projection and policy identities.
  Keep operation credential records in the same restricted backup. Current Jobs
  use public Pod-binding records and projected Pod tokens, with no per-operation
  Secret. Preserve legacy credential Secret projections when present.
  Reinstalling the chart does not restore these identities.
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
   the certificate Secret. For a consistent durable-result restore, restore the
   receiver trust, rotation journal and enrollment policy together before either
   the certificate rotator or manager starts.
3. Restore the Secrets and the verification-policy ConfigMaps.
4. Restore the `PtahRealm` objects, then the seven namespaced kinds with their
   status, then the plan chunks and result records.
5. Start the manager. It re-reads the database before it plans.

Do not start the manager between steps. Its first reconciliation will act on
whatever is present, and a partial restore is a different resource.

## What survives a lost cluster {#what-survives-a-lost-cluster}

With no backup, what is left is what Ptah wrote into the database. The database
answers for a migration and says almost nothing about a schema.

| Resource | What the database still holds | What is gone unless you exported it |
| --- | --- | --- |
| `PtahMigration` | Every version applied, in the revision table, and in `schema_migrations_log` every attempt whose log write succeeded: the run, the version, the outcome, the checksum, the time and the actor | The resource and its status, `status.lastRun` and `status.unresolvedRun` among them |
| `PtahMigrationPlan` | Nothing of the plan. The versions that ran and their checksums are in the two tables above | The plan and its fingerprint |
| `PtahMigrationApproval` | The approver's username, on the log rows the run it authorized wrote, percent-encoded where it leaves printable ASCII | The object: the approver's UID and groups, when they approved, which plan fingerprint they approved, and any approval no run consumed |
| `PtahMigrationRunAcknowledgment` | Nothing | Who accounted for a run, and what they said |
| `PtahSchema` | The schema itself and nothing about how it got there. Ptah keeps no table for a declared schema | The resource and its status |
| `PtahSchemaPlan` and `PtahSchemaPlanChunk` | Nothing | The plan and its SQL |
| `PtahSchemaApproval` | Nothing | Who approved, when, and what |
| `PtahRealm` | Nothing | The grant |
| `PtahResultRecord` | Nothing | The result it delivered |

A schema's plans and approvals are lost with the cluster unless you export them.
`ptah schema apply` writes no log and takes no actor, so afterwards the database
cannot say who approved a schema change, or that anyone did. Recovery does not
need that: a rebuilt `PtahSchema` observes the live schema and plans the
difference. An audit may. The operator exports nothing, so keep what an audit
needs outside the cluster: a [backup](#what-to-preserve) that includes the
approvals and plans, or the API server's audit log, if its audit policy records
writes to these kinds, which is where each approval's creation and who made it
would be.

For a migration, read the record with SQL or with Ptah:

```sql
SELECT logged_at, version, state, actor, actor_source
FROM schema_migrations_log
ORDER BY logged_at, seq;
```

```sh
PTAH_DB_URL="$DATABASE_URL" ptah migrations log
```

On MySQL `ptah migrations log` needs `parseTime=true` in the URL and fails on the
`logged_at` column without it; the SQL above works either way. A run that
stopped mid-flight has a `started` row and no outcome. A run Ptah stops before it
selects a migration writes no row, and neither does one with nothing to do, so
the absence of a row does not show that no run was attempted.

An `actor` outside printable ASCII is percent-encoded, so every character set can
store it, and so are `%` and `$`: `oidc:Jos%C3%A9 Garc%C3%ADa` is
`oidc:José García`. Read it back with a percent decoder that leaves `+` alone:

```sh
python3 -c 'import sys, urllib.parse; print(urllib.parse.unquote(sys.argv[1]))' 'oidc:Jos%C3%A9 Garc%C3%ADa'
```

Rows written before
operator 0.2.10 hold no actor and the source `unknown`, and nothing fills them in
afterwards. [The approver's name in the target
database](../security/#approver-in-the-database) says what the value is worth,
and when a row is lost.

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

## A schema whose target moved under an Apply {#a-schema-whose-target-moved}

A `PtahSchema` proves an Apply by reading the target that Apply was planned for,
through the Secret the Apply read, which `status.pendingObservation.target` names.
Editing `spec.target.urlFrom` does not move the proof. What does is that Secret
holding a URL of another target, after a plan was approved or while its Apply ran.
The target's identity covers the login, the host, the port, the database and every
URL parameter except the ones that only carry a credential or time a connection:
`connect_timeout`, `password` and `sslpassword` on PostgreSQL, and
`checkConnLiveness`, `maxAllowedPacket`, `readTimeout`, `timeout` and `writeTimeout`
on MySQL. So a new password alone is the same target, and a new `sslmode` is not.

No read of another target can say what the Apply did, and the resource stops:

- `status.phase` is `Blocked`, and `ReconciliationFailed=True` has the reason
  `TargetIdentityChanged` and a message that ends with the command below.
- `status.pendingObservation` stays, and the resource keeps its database realm, so
  another claimant of that database waits.
- It reads again every `spec.execution.failureRetryInterval`, so it finds the first
  target if that comes back. Each read is bound to the plan's target, as the Apply
  was, and the runner refuses it before it connects, with `target_binding_mismatch`
  in the Pod's log, so nothing logs in to the new target or reads it. No source is resolved, no plan
  is published, no Apply runs and a read request waits.

**Put the first URL back.** The next read matches, and the proof runs as it would
have. An Apply that succeeded is recorded in `status.applied`. This loses nothing,
so prefer it while the first login still works, and move to the new one after the
resource settles.

**Settle it.** When the first login is gone, or the move was the point, run the
command the message ends with:

```sh
kubectl get ptahschemas storefront -n application \
  -o jsonpath='{.status.conditions[?(@.type=="ReconciliationFailed")].message}'
```

It writes the `operator.ptah.run/settled-apply` annotation with the Apply's operation
ID and the digest of the target the read named. The value is a statement about this
Apply and this target, not a secret: the digest is a plain SHA-256 of the target's
identity, so anyone who knows the URL can work it out, and the status of anything
planned against that URL shows it. The Apply's ID on its own settles nothing.

The statement holds until the proof ends. The operator takes it at a read that names
its target, once the resource has stopped there and printed that value, so you see
that stop even if you wrote the value first. If the store moves on, the resource
prints another value and the statement waits. If the store comes back, the resource
stops there again and the next read settles, with no new write. A store that flips
from the first target to this one and back, A, B, A, B, settles on B, which is what
the statement said. To take it back, remove the annotation:

```sh
kubectl annotate ptahschemas storefront -n application operator.ptah.run/settled-apply-
```

Any other value does nothing, and the next stop says the annotation holds another
value.

That statement says you account for what the Apply did to the first target. A read
under the new target could say nothing about it, and the operator makes none before
you settle. `status.journal` says whether the Apply reported `Succeeded` or
`Unknown`, so check the first target first if it matters. A resource that has stopped takes the statement at its next read, up to
`failureRetryInterval` later. A suspended resource starts no reads, so it takes it
after it resumes, though a read already under way is still taken. By then every Pod
the Apply could still be running has stopped.
It gives up the proof, the plan and the realm in one status write, records
`Verified`, `Unknown` in the journal with a `ProofSettled` Event, and starts again
from `Resolve` under the URL the Secret holds. Nothing is written to `status.applied`.

The plan that follows binds the new target, and the approval of the old plan does not
carry over. Under `apply: OnApproval` it waits for an approval of its own. Under
`apply: Always` it applies with nobody looking, unless it is destructive or changes
privileges, so a Secret pointed at the wrong database gets the schema applied there.
Check what the Secret holds before you settle, and set `apply: OnApproval` first if
you are not sure.

Writing the annotation takes `patch` on `ptahschemas` and records no identity, unlike
a `PtahMigrationRunAcknowledgment`. The API server's audit log says who wrote it, if
its audit policy records writes to `PtahSchema`. It is something `patch` could not do
otherwise: deleting the resource takes `delete`, and patching its target does not end
a proof. So `patch` alone can end a proof hold and release the database realm.

A read names a target only if its Pod starts, its source fetch succeeds, its Job
completes and the URL parses. A read that ends any other way is an ordinary failed
read. The annotation does nothing at it, whatever it holds, `ReconciliationFailed`
reports `OperationFailed` with no command, and a later read of the target stops the
resource again before it settles.

If you point `spec.target.urlFrom` at a new Secret and delete the old one, which is the
natural way to move, the proof still reads the old one. Its Pod cannot start, so `Ready`
reports `PodConfigurationError`, and the resource holds its realm. If it had already
stopped, `ReconciliationFailed` goes on printing that stop's command until the Job
reaches its deadline, and running it settles nothing until a read names that target. A
URL that does not parse fails the read with `invalid_target`. Either way, write the
Secret `status.pendingObservation.target` names, with any valid URL. The first URL
finishes the proof, and any other one leads to `Blocked` and the command above.

A source fetch that fails, or a Job killed at its deadline, names no target either. If
every attempt ends that way, as with an artifact the registry lost, the proof cannot be
settled and a new URL does not help. Restore what the read needs, or delete the
resource.

Deleting the resource also ends the proof, once the Apply's Pods have stopped, and a
resource created again reads the database afresh. It takes the plans, the approvals
and `status.journal` with it, so settling is the better record.

A `PtahMigration` is different. Its history read is not bound to the plan the run
came from, and a run nobody accounted for is settled by a
[`PtahMigrationRunAcknowledgment`](../operations/#a-migration-run-nobody-accounted-for).

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

## A migration that failed at a statement {#failed-statement}

Nothing in the cluster is lost here. A migration failed at a statement, Ptah left
a row in its revision table, and the resource stopped on that row:
`status.lastRun.outcome` reads `Failed`, and the history read that follows sets
`Blocked=True` with reason `HistoryDirty` and publishes no plan. A corrected
artifact does not lift it, and neither does another read. The operator never
writes to the revision table, so nothing runs until a person changes the row.

Ask Ptah what the row records, at the version in
`status.executionBinding.ptahVersion`. `./migrations` here and below is a local
copy of the files the resource reads:

```sh
ptah migrations status --db-url "$DATABASE_URL" --migrations-dir ./migrations
```

```text
Dirty Migration: version=2 state=failed direction=up applied=0/2
Error Statement: INSERT INTO no_such_table (id) VALUES (1)
```

The row's state and count say which path applies:

| The row | What the database holds | `status.lastRun.outcome` | What ends it |
| --- | --- | --- | --- |
| `state=failed applied=0/2` | Nothing of this migration: its transaction rolled back | `Failed` | [Retry the migration](#retry-failed-migration) |
| `state=pending applied=0/2` | Nothing, once you have [checked](#failed-pending-row) | `Unknown`, where a MySQL DDL statement returned an error, where a stopped Pod cut short a statement that commits on its own, and where the session died | [Retry the migration](#retry-failed-migration) and acknowledge the run |
| `state=failed applied=1/2` | The statement before the failure. MySQL commits each DDL statement on its own, and `transactionMode: none` commits every statement | `Partial` | [Finish what it started](#failed-partial) |

A run that ended `Unknown` or `Partial` also leaves
[`status.unresolvedRun`](../operations/#a-migration-run-nobody-accounted-for), and the
resource reads `Blocked` with reason `ApplyOutcomeUnknown` until a later read rewrites
the reason.

Everything below was measured with Ptah v0.22.0, directly and under this operator's
edge build on Kubernetes 1.37.0, against PostgreSQL 17.11 and MySQL 8.4.11, except
where it names Ptah v0.23.0.

### Retry the migration {#retry-failed-migration}

The retry goes through the operator, so it gets a new plan, the approval the
policy asks for, and a Job the resource records.

#### Before you start {#retry-failed-before}

You need write access to the target database, a local copy of the files, and `ptah`
at the version above.

The count has to read zero, and a [`state=pending` row](#failed-pending-row) has to be
checked first. Then fix what made the statement fail: correct the file
and publish it, or correct the database. `status.lastRun.message` names the
statement and its SQLSTATE, and [the Pod log](../operations/#failed-apply-log) holds
what the database answered. Retrying an unchanged file against an unchanged database
fails the same way and leaves the same row.

#### Run it {#retry-failed-run}

If the file changed, point the resource at the corrected artifact. It stays
`Blocked`.

Then remove the row. `ptah migrations set` moves the revision boundary to the version
before the one that failed and runs no SQL. Where the first migration is the one that
failed, that is `--version 0`.

```sh
ptah migrations set --version 1 --db-url "$DATABASE_URL" --migrations-dir ./migrations
```

```text
Current version is 1 (1 removed):

  - 2 (Add Extras)
```

Ask for a read, then approve the plan it publishes. Under `apply: Always` the plan
runs without an approval.

```sh
kubectl ptah refresh migration/orders -n application --wait
kubectl ptah approve migration/orders -n application --wait
```

A run that ended `Unknown` needs one more step, because a read that still finds a
migration pending leaves `status.unresolvedRun` in place: the resource stays `Blocked`
and says `1 migration is pending and the last run's outcome was Unknown, so it may not
run again`. Account for the run and
[acknowledge it](../operations/#clear-unresolved-run) instead of asking for a read. The
plan follows the acknowledgment.

#### What proves it worked {#retry-failed-evidence}

`status.lastRun.outcome` reads `Applied`, `status.lastRun.appliedVersions` names the
version, and the resource reads `Ready=True` with reason `HistoryMatched`.

Look in the database as well for what the migration creates. `Ready` says the
revision table and the artifact agree, and [a forced repair](#what-repair-records)
earns it too.

#### Where to stop {#retry-failed-stop}

Stop if the count is above zero: a second run repeats what committed. Stop if a
version below the one that failed does not read `applied` in the status output,
because `ptah migrations set` records every version up to `--version` as applied,
whether it ran or not.

#### If it fails {#retry-failed-recovery}

A second failure leaves the state this section started from: `Failed`, `Blocked`,
`HistoryDirty`, and the row with the new error. Nothing committed, so read the row
again and go back to the cause.

### Retrying outside the operator

`ptah migrations up --allow-dirty` is the retry Ptah's own messages name. It reuses
the row, and it runs the SQL under your login with no plan, no approval and no Job.
The resource reads the result at its next read and reaches `Ready=True`, and
`status.lastRun` goes on saying `Failed` until a later run replaces it. With nothing
left pending, that read also settles a `status.unresolvedRun`, with no acknowledgment.

```sh
ptah migrations up --allow-dirty --to-version 2 --db-url "$DATABASE_URL" --migrations-dir ./migrations
```

Keep `--to-version`. Without it the command also applies every later migration that
is pending.

### A row in `state=pending` {#failed-pending-row}

A MySQL DDL statement that returns an error leaves this row, with the error
`statement execution outcome is unknown after process interruption`, although the
process was alive and heard the answer and the statement did not commit. Ptah
keeps its in-flight mark on a DDL statement the server refused, and from v0.23.0
([stokaro/ptah#5569](https://github.com/stokaro/ptah/pull/5569)) it reports the
run `unknown` over that mark rather than `failed`
([stokaro/ptah#5661](https://github.com/stokaro/ptah/issues/5661)), so the run
leaves `status.unresolvedRun` like any other unknown run and needs an
acknowledgment. A later Ptah release is expected to read it as `failed`. `status.lastRun.message`
names the statement and the database's answer, measured with the runner harness
for a `CREATE UNIQUE INDEX` that met a duplicate key:
`Migration 1 (Change) stopped at statement 1 of 1 with SQLSTATE 23000 (error 1062), and whether it committed is not recorded`.
On PostgreSQL the same statement leaves `state=failed` and the run `Failed`. A
MySQL statement that changes rows, or one the server refuses to parse, still
leaves the run `Failed`.

A run whose Pod was stopped inside a statement that commits on its own leaves the
same row: a statement outside a transaction (`-- +ptah no_transaction`, or
`spec.transactionMode: none`) on either engine, or a DDL statement on MySQL. Ptah
stopped waiting for the statement when the runner passed SIGTERM on, and a client
that stopped waiting cannot know whether the server applied it, so the mark stays.
Ptah v0.22.0 left the same row and reported the run `failed`, or `partial` above a
count of zero; v0.23.0 reports it `unknown`
([stokaro/ptah#5569](https://github.com/stokaro/ptah/pull/5569)). Where earlier
statements of the file committed, the count says how many, `applied=1/2` for one,
and the retry above stops on it. Measured on kind with a sleep outside a
transaction that Ptah stopped waiting for on each engine:
`Migration 2 (Wait To Be Stopped) stopped at statement 1 of 1, and whether it committed is not recorded`.
A PostgreSQL migration that runs in a transaction rolls back when it is stopped
and still leaves `state=failed` and the run `Failed`.

`ptah migrations status` calls that run interrupted, and tells you to apply what is
missing by hand and then run `ptah migrations repair --version 2`. Do that only if you
do apply it by hand: [that repair](#what-repair-records) records the migration applied
and runs nothing. `ptah migrations up --allow-dirty` refuses the row. Look in the
database for what the statement would have changed, and where it changed nothing,
[remove the row](#retry-failed-run).

A session killed while its transaction was open leaves `state=pending` with no error.
That transaction rolled back, and the retry above takes the row.

### Finish what it started {#failed-partial}

A count above zero means the first statements committed. The retry above runs what
is left. It skips the committed statements, once it has checked that they are
unchanged, and the statements after them may be corrected.
`ptah migrations repair --version 2 --resume-from 2` does the same. When the committed
statements did change, Ptah refuses:

```text
migration 2 cannot resume automatically: the already committed statement prefix changed after 1 of 2 statements committed; inspect the database before choosing a repair point
```

Do not remove the row here unless you have also undone what committed. Without that,
the resource holds `status.unresolvedRun` until you acknowledge the run, and the next
run starts from the first statement. On MySQL it stopped at
`Table 'extras' already exists`, committed nothing, and left the resource `Blocked`
again.

### What repair records {#what-repair-records}

:::caution
Do not use `ptah migrations repair --force` for a statement that did not run. It
records the migration as applied, and nothing runs it afterwards.
:::

`ptah migrations repair --version 2 --force` runs no SQL. It writes the row as
`state=applied`, `applied=2/2`, prints `Repaired migration 2` and exits 0. On
PostgreSQL and on MySQL, with the table that migration creates absent:

- `ptah migrations status` says the database is up to date.
- `ptah migrations up` says it is already up to date, and runs nothing.
- The resource reaches `InSync`, with `Ready=True` and reason `HistoryMatched`.
- A `status.unresolvedRun` settles at the next read, because nothing is left pending,
  so nobody is asked to account for the run.
- A corrected file later reads as modified, and the resource `Blocked` with
  `HistoryModified`, because the row now holds the checksum of the file that failed.

The flag is right when a person ran every statement by hand and wants that recorded.
To take the record back, remove the row as in [Run it](#retry-failed-run), and the
migration is pending again.

Without `--force`, `repair` follows the row, and the refusal covers one shape. At
`state=failed applied=0/2` it refuses and names the retry. At `state=failed applied=1/2`,
and at the [MySQL row above](#failed-pending-row), it prints `Repaired migration 2` and
writes `applied=2/2` without running what is left, because it takes you to have
finished the migration by hand. Use `--resume-from` if you want Ptah to run the rest.

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
