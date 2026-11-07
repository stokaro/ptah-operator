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

Four of those steps are Jobs: `Resolve`, `Verify`, the two readings of the
revision table, and `Apply`. Planning is arithmetic the manager does on the
reading it already has, and approving is a person writing an object -- neither
reaches a database or a registry, so neither needs a Pod.

Each Job holds either the registry credentials or the database URL, and never
both. The migration files reach the container that runs SQL
through an init container that fetched them; the process holding your database
URL never talks to your registry.

[From source to verified change](../../reference/reconciliation/#from-source-to-verified-change)
draws this path beside the schema's, with who does what at each stage, and
[Run one from the command line](#stage-by-stage) walks it.

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

`coordinationKey` names the physical database within the namespace. Two
resources in the namespace that address the same database must use the same
key: it is what makes them take turns rather than run at once. The key reaches
no further than the namespace, so a database that resources in another
namespace manage too is named by a `PtahRealm` an administrator creates, and
each resource names it with `spec.target.realmRef` instead. See
[A database more than one namespace manages](../operations/#a-database-more-than-one-namespace-manages).

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

An artifact the policy refuses stops the migration at `Blocked`, before it reads
the database: `ArtifactVerified` is `False` with the reason `PolicyRefused`, its
message names each requirement the artifact did not meet, and an
`ArtifactVerificationRefused` Event says the same. No plan outlives the refusal.
The operator reads again from the artifact reference at `spec.interval`, so a new
artifact or a policy edit is noticed then.

A policy that also listed `application/vnd.stokaro.ptah.schema.v1` would let a
schema artifact stand in for a migration directory at the same reference, so
give a migration its own policy rather than reusing the schema one.

`examples/migration-verification-policy.yaml` is that policy, and the migration
example is pointed at the name it is created under:

```sh
kubectl -n application create configmap ptah-migration-verification-policy \
  --from-file=policy.yaml=examples/migration-verification-policy.yaml
kubectl -n application patch configmap ptah-migration-verification-policy \
  --type=merge -p '{"immutable":true}'
```

A schema and a migration against the same database each keep their own, beside
the `sharedRealm` both of them set.

A new version of the sequence is a new artifact. `ptah migrations push
--format reference` prints its reference pinned by digest, and `kubectl ptah
set-source` points the resource at it, changing `spec.artifact.ociRef` and
nothing else:

```sh
kubectl ptah set-source migration/orders -n application --oci "$REF" --for approval
```

The operator reads the history again and plans the versions the new artifact
has that the database does not. [Moving to a new
artifact](../operations/#set-source) says what set-source refuses, an Apply in
flight among them, and how to make the same change with `kubectl edit`.

## Run one from the command line {#stage-by-stage}

`kubectl` and `kubectl ptah` are all this takes.
[Ptah Console](https://github.com/stokaro/ptah-console) is optional, and no step
needs it.

The sequence below comes from a ConfigMap, so it needs no registry and no
verification policy. `examples/configmap-kustomize` holds two migrations and a
`PtahMigration` that selects them, and these commands use them directly, without
Kustomize. Run them from a checkout of the
[version you installed](../../start/install/#choose-a-version-once), against a
database of its own. Keep its URL in a private file with no trailing newline and
set `DATABASE_URL_FILE` to its path, as
[First schema](../../start/first-schema/#create-what-the-schema-reads) does. That
page also creates the namespace.

```sh
kubectl -n application create secret generic orders-database \
  --from-file=url="$DATABASE_URL_FILE"
kubectl -n application create configmap orders-migrations \
  --from-file=examples/configmap-kustomize/db/migrations/
kubectl -n application apply -f examples/configmap-kustomize/ptahmigration.yaml
kubectl ptah wait migration/orders --for approval -n application
kubectl ptah migration orders -n application
kubectl ptah approve migration/orders -n application --wait
kubectl ptah migration orders -n application
```

`kubectl ptah migration orders -n application` shows the phase and every
condition below, at any stage.

| Stage | Run | Expect | Then |
| --- | --- | --- | --- |
| 1 Source | `kubectl create secret`, `kubectl create configmap` and `kubectl apply` | `Source: ConfigMap orders-migrations`, and its `Snapshot` | Wait for the plan |
| 2 Plan | `kubectl ptah wait migration/orders --for approval -n application`, then `kubectl ptah migration orders -n application` | Phase `AwaitingApproval`, `ApprovalRequired=True`, reason `AwaitingApproval`, `Pending: 2`, and each version with its description. No SQL: the statements are in the files | Decide |
| 3 Decision | `kubectl ptah approve migration/orders -n application --wait` | Each version with its checksum, then `Approved:`, with the identity admission stamped | The command stays for stages 4 and 5 |
| 4 Execution | Nothing more. To watch from elsewhere, `kubectl -n application get ptahmigration orders -w` | Phase `Applying`, then `VerifyingHistory` and `Reading` | The command ends with `reached applied` |
| 5 Verification | `kubectl ptah migration orders -n application` | Phase `InSync`, `Ready=True`, reason `HistoryMatched`, `Pending: 0`, `Last run: Applied` | Read the history |
| 6 History | The commands below | The journal holds `PlanPublished`, `ApprovalAccepted` and `RunFinished` with outcome `Succeeded`, and `status.history.revisions` a row for each migration | Add a migration: a file in the ConfigMap and an item in the resource |

You can read the history at any stage, and reading it changes nothing:

```sh
kubectl -n application get ptahmigration orders -o jsonpath='{.status.journal}' | jq
kubectl -n application get ptahmigration orders -o jsonpath='{.status.history.revisions}' | jq
kubectl events -n application --for ptahmigration/orders
```

The journal dates the run as a whole. How long each migration took comes from
the revision table, where the table records it.
[The lifecycle journal](../../reference/reconciliation/#the-lifecycle-journal)
says how far back the journal, the Events and the finished Jobs reach.

A stage can go another way:

- At stage 2, phase `InSync` means nothing is pending, and `Blocked` means
  [the history refused](#what-the-history-says). The wait times out with exit
  status 7 in both cases.
- At stage 3, an `approve` that approves nothing found the plan replaced, or being
  replaced. Run it again, and it shows the current one.
- At stage 4, exit status 7 means the run has not ended yet and 8 that it will not
  end applied. Both say what holds the migration, and
  [What the run reports](#what-the-run-reports) says what follows each outcome.

An artifact source, as in [the minimal resource](#the-minimal-resource), adds a
Resolve and a Verify Job at stage 1 and changes nothing after it. A Git source
adds a Resolve Job, and
[Deliver from a ConfigMap or Git](../sources/) has the fields.

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

`status.history.revisions` lists the newest rows of that table, newest first:
the version, the migration's description, the checksum the row recorded, its
state, when it was written and how long the migration ran in milliseconds. At
most 64 rows are kept, so a longer history leaves its oldest rows out. A
modified row carries the checksum the database recorded, which is the one to
compare with the file you ship. No SQL and no error text is read into them.

```yaml
status:
  history:
    currentVersion: 14
    appliedCount: 14
    revisions:
      - version: 14
        description: add ledger index
        checksum: h1:2iJ5i0iUq9Jc6I3cS0Y8pX3x9mW6E3o7b8kq1V2xU0A=
        state: applied
        appliedAt: "2026-10-24T07:42:30Z"
        executionMillis: 1840
```

**Dirty.** A failed or interrupted run left a revision row behind. Nothing
applies while one exists, and the operator never removes it: what a
half-applied migration did to your data is a question for a person with the
database in front of them.
[A migration that failed at a statement](../recovery/#failed-statement) says how that
ends, and what `ptah migrations repair --force` records instead.

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

Ptah empties the shadow database before it replays anything. On MySQL it refuses
to do that unless the shadow user holds the global `SELECT`, `DROP`, `ALTER`,
`ALTER ROUTINE`, `EVENT`, `LOCK TABLES`, `PROCESS`, `SHOW_ROUTINE` and `TRIGGER`
privileges (MariaDB asks for `SHOW VIEW` in place of the last two): a grant on
one schema cannot prove the user sees every object it is about to drop. Give
the shadow database its own user, on a server that holds nothing else of value,
and keep the target's user on its per-schema grants.

## Adopting an existing database

### Record the history a database already earned {#adopt}

The handoff from a database somebody migrated by hand to one this operator
manages. It writes nothing to the application schema: what it produces is the
revision history the database should already have had.

#### Before you start {#adopt-before}

You need write access to the target database's revision table, a shadow
database of your own on a server holding nothing else of value, and write
access to the `PtahMigration` in its namespace if one exists. Nothing
cluster-scoped is touched.

Confirm the database is the case this is for: the schema exists and the
revision table is empty or absent. A database with a revision history is not
adopted, it is migrated.

Have the exact artifact the operator will use, pinned by digest rather than by
tag, and the migration files that artifact carries. Baseline compares what
those files produce against what the database has, so files that are not the
artifact's prove nothing about the artifact.

**Nothing may be applying while this runs.** If the resource already exists,
suspend it before you start: a plan approved earlier can otherwise dispatch an
Apply into the middle of adoption, and that Apply would run the whole sequence
against a database that already carries the schema.
`kubectl ptah suspend migration/orders -n application --wait` returns once
the operator reports the migration `Suspended`, with no Apply running and none
able to start. A read the suspension dropped can keep its Job running to that
Job's own deadline; it changes nothing, and `kubectl get jobs -n application`
shows when it is gone. If it does not exist yet, create it after baseline rather than before.

#### Run it {#adopt-run}

1. Read the history the database has, and confirm it is empty.
2. Create the shadow database and its user, with the privileges above.
3. Run baseline against the target, with the artifact's own migration files.

```sh
ptah migrations baseline \
  --db-url "$DATABASE_URL" \
  --migrations-dir ./migrations \
  --shadow-db "$SHADOW_DATABASE_URL"
```

4. Create the `PtahMigration`, or resume the suspended one with
   `kubectl ptah resume migration/orders -n application`.

#### What proves it worked {#adopt-evidence}

The revision table now records the versions the files carry, and the
application schema and its rows are unchanged -- baseline replays into the
shadow, never into the target.

The resource then reports it without having run anything:

```sh
kubectl get ptahmigration "$NAME" -o jsonpath=\
'{.status.history.currentVersion}{"\t"}{.status.phase}{"\n"}'
```

`currentVersion` is the last version the files carry, the phase is `InSync`,
and `status.lastRun` is absent, because no Apply happened. A resource that
reports `InSync` while carrying a `lastRun` from this procedure did not adopt
anything; it executed.

Publishing the next migration afterwards is ordinary work: the operator plans
the one pending version, asks for the approval the policy requires, and applies
it.

#### Where to stop {#adopt-stop}

A mismatch refusal is the answer, not an obstacle. It says the files do not
produce the schema the database has, and recording them anyway would mark
migrations as applied that never ran against this database -- which is the
state every later refusal is designed to catch and the one thing adoption must
not manufacture. Nothing is recorded when baseline refuses.

Do not point `--shadow-db` at anything you would miss. Ptah empties it first.

Do not adopt a database whose revision table is not empty, and do not empty it
to make this procedure apply.

#### If it fails {#adopt-recovery}

A mismatch is repaired by finding which of the two is wrong, not by forcing the
record. Compare the schema the shadow ended with against the target's: a
missing object means the files are behind the database, an extra one means the
database is behind the files, and either way the repair is to the files or to
the database before baseline is run again.

Nothing needs undoing after a refusal. The target was never written to, and the
shadow is disposable.

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
that conflicts with it. `kubectl ptah approve` shows the versions and checksums
the plan selected, asks for a decision, and creates the approval as you:

```sh
kubectl ptah approve migration/orders -n application
```

It shows no SQL: a migration plan records versions and checksums, and the
statements live in the source the plan binds, whose digest the review prints.
It reads the migration again before it sends anything, and approves nothing if
the plan was replaced, the spec changed other than by a suspend or a resume,
or the operator would not admit an approval now. An answer given while the
operator reads the history again at its interval is sent when that read ends,
if the same plan is waiting again: approve waits up to five minutes for that
before it sends, and `--timeout` bounds each request it makes rather than that
wait. With `--wait` it then stays until the run of that plan under that approval
has ended and the operator has read the history back. Another run does not satisfy
it. A later plan that replaces this one, another plan's run, or the same plan run
under another approval ends the wait with exit status 8 and says which.
`--dry-run client` prints the approval as YAML with the identifiers filled in,
after the same decision, to keep in a repository:

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigrationApproval
metadata:
  generateName: orders-
  namespace: application
spec:
  migrationRef:
    name: orders
    uid: <metadata.uid of the PtahMigration>
  planRef:
    name: <status.plan.name>
    uid: <status.plan.uid>
  planFingerprint: <the plan's spec.fingerprint>
```

Create it with `kubectl create -f`, as the person who decided: admission
records whoever creates it as the approver, and a controller that applies it
from the repository is recorded in that person's place. The API server names
it, as it names what the command sends, so one file can be created again after
an uncertain run consumed the first approval. `kubectl apply` refuses a
generated name: a workflow that applies its files passes `--name`.

The webhook refuses an approval the evidence no longer supports: a replaced
plan, a history somebody else moved in the meantime, a re-resolved artifact, a
changed verification policy, a rolled-out executor, or a migration that is not
waiting for a decision. An approval authorizes one execution. Deleting or
replacing it before the run's Job exists withdraws it: the claim is retired, no
Job runs, and the resource plans again. Once the Job exists, deleting the approval
changes nothing about the run.
`kubectl ptah migration orders -n application` lists the approvals that name
the migration, whether each names the current plan, and the `Consumed`
condition the controller writes once the approved plan is dispatched.

The chart's optional approver ClusterRole grants what an approver needs for
either family: read on the migration, its plans and approvals, and create on
`PtahMigrationApproval`. The chart binds it to nobody;
[Exact-plan approvals](../approvals/) says what it leaves out and why.

The execution runs the approved sequence and nothing else. The Job hands Ptah
the approved list, and `ptah migrations up --expect-sequence` compares it with
what Ptah selects under the migration lock, after every check the operator can
make and just before anything runs. If the database was restored to an earlier
version after the approval, Ptah selects more than was approved and runs none
of it: `status.lastRun.outcome` reads `Failed`, `status.lastRun.message` names
the selected and the approved versions, and the resource reads the history
again as it does after any failed run.

## What the run reports

The verdict is the database's, read from the revision table rather than from
the Job's exit status — a run that stopped is exactly the run whose outcome
matters most:

| `status.lastRun.outcome` | What it means |
| --- | --- |
| `UpToDate` | The database already had every planned migration |
| `Applied` | Every selected migration is recorded applied |
| `Failed` | A migration failed and committed nothing, or the run stopped before it selected one, as on a database it could not open. The first leaves a [dirty revision row](#what-the-history-says), which a person has to [clear](../recovery/#retry-failed-migration) before anything else runs. After the second, fix the cause and the resource plans the run again |
| `Partial` | A migration committed some of its statements and not the rest |
| `Unknown` | The evidence could not be read |

`Partial` and `Unknown` stop the resource and are never retried. Re-running a
file that committed half its statements would run them twice, and no controller
can know which half. Both leave the database to a person, and the resource
records the run in `status.unresolvedRun` until a reading or a person settles
it. `kubectl ptah migration` prints the last run with the Job it ran as, and
the unresolved run as a block of its own, with what
[A migration run nobody accounted for](../operations/#a-migration-run-nobody-accounted-for)
says to do about it.

A run that stopped before it selected any migration ran nothing and read no
history, so there is nothing to read back at once: the resource records it
`Failed`, waits `spec.execution.failureRetryInterval`, and starts again from
resolution, which reads the history before anything is planned or applied.
Under `apply: Always` the new plan runs; under `OnApproval` it waits for an
approval, because the run consumed the one it carried.
[A database the operation could not open](../../troubleshoot/condition-reasons/#a-database-the-operation-could-not-open)
says what its message names.

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

## Who the database's log names

Ptah keeps its own record of each run in the target database,
`schema_migrations_log`, and the operator tells it who to name. After an approved
run, every row holds the approver's username in `actor`:

```text
 version |  state  |      actor       | actor_source
---------+---------+------------------+--------------
       1 | started | kubernetes-admin | provided
       1 | applied | kubernetes-admin | provided
```

The record survives losing the cluster, and deleting the approval changes nothing
about it. The name comes from the approval the run consumes, and the claim keeps
its own copy from the moment it is written, so what the run records is settled
before its Job exists. Under `apply: Always` nobody approved, and the rows read
`system:ptah-operator:apply-always:<namespace>/<name>`.

A name outside printable ASCII is percent-encoded, so every character set can
store it, and so are `%` and `$`. An ordinary username or email reads unchanged,
and `oidc:José García` reads `oidc:Jos%C3%A9 Garc%C3%ADa`. Read it back with a
percent decoder that leaves `+` alone, such as Python's `urllib.parse.unquote`.

The value is personal data, and `provided` means Ptah took the name on trust.
[The approver's name in the target
database](../security/#approver-in-the-database) says what that means and when a
name is shortened or a row is lost, and [What survives a lost
cluster](../recovery/#what-survives-a-lost-cluster) says what else is left.

## What never appears

No row of any table reaches the status, an Event, or `kubectl ptah migration`.
No SQL does either: a published plan records versions, descriptions and
checksums, and the statements stay in the artifact you built.
