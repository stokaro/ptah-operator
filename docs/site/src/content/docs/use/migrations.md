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
[The lifecycle explorer's composed workflow](../../reference/reconciliation/?lx-view=workflow#lifecycle-explorer)
follows both through one change -- expand the schema, backfill with a
migration, contract -- with what sharing the database takes, what each
approval covers, and the gates no resource holds.

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
[The lifecycle explorer](../../reference/reconciliation/?lx-family=migration#lifecycle-explorer)
steps through one run with the objects each stage touches, and follows it
where it turns: a history with nothing pending, or a run that ends `Partial`
or `Unknown` and how it is settled.

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
`ArtifactVerificationRefused` Event says the same. No plan outlives the refusal,
and no approval does: each one still outstanding goes `Stale`, so a plan that comes
back needs a new one. The operator reads again from the artifact reference at
`spec.interval`, so a new artifact or a policy edit is noticed then, and an edit to
the spec is read at once. Once the artifact passes, or the reference resolves to
another digest, or the source is no longer a verified one, no condition names the
refusal, whatever the history read goes on to say. [A wait](../operations/#wait)
for `in-sync`, `approval` or `applied` stops on the refusal with exit status 8. One
for a run that applied before the refusal says the run applied and that no reading
of the history has followed it, with `CompletionNotProved`.

Under `require_signature` a registry fault can read as a refusal. A registry that
answers 404 from its referrers API looks like one without that API, and with no
fallback index the artifact has no signature to find. A person sees what a real
refusal shows: `Blocked` with `PolicyRefused`, `require_signature` named on
`ArtifactVerified` and in the Event, and each outstanding approval `Stale`. Nothing
runs. When the registry answers again the next read passes, the same plan is
published, and it waits for a new approval.

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
| 6 History | The commands below | The journal holds `PlanPublished`, `ApprovalAccepted`, `RunFinished` with outcome `Succeeded`, and `Verified` with outcome `InSync`, the first history read after the run; `status.history.revisions` holds a row for each migration | Add a migration: a file in the ConfigMap and an item in the resource |

You can read the history at any stage, and reading it changes nothing:

```sh
kubectl ptah history migration/orders -n application
kubectl ptah migration orders -n application --details
kubectl events -n application --for ptahmigration/orders
```

`history` prints the operator's journal, which dates the run as a whole, with
the approval and any acknowledgment that name the migration. `--details`
prints the newest rows of the revision table under `History read`; how long
each migration took comes from there, where the table records it.
[The lifecycle journal](../../reference/reconciliation/#the-lifecycle-journal)
says how far back the journal, the Events and the finished Jobs reach.

A stage can go another way:

- At stage 2, phase `InSync` means nothing is pending, and the wait times out
  with exit status 7. `Blocked` means [the history refused](#what-the-history-says),
  and the wait stops with exit status 8 and the reason.
- At stage 3, an `approve` that approves nothing found the plan replaced, or being
  replaced. Run it again, and it shows the current one.
- At stage 4, exit status 7 means the run has not ended yet and 8 that it will not
  end applied. Both say what holds the migration, and
  [What the run reports](#what-the-run-reports) says what follows each outcome.

An artifact source, as in [the minimal resource](#the-minimal-resource), adds a
Resolve and a Verify Job at stage 1 and changes nothing after it. A Git or
HTTPS source adds a Resolve Job, and
[Deliver from a ConfigMap, Git or HTTPS](../sources/) has the fields.

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

**Unfinished repeatable migration.** The record of
[repeatable migrations](#repeatable-migrations) holds one whose last attempt
failed or recorded no outcome. It is that record's dirty row: Ptah runs nothing
while it exists, versioned migrations included, so the resource is `Blocked`
with `RepeatableMigrationUnfinished`, publishes no plan, and names each one,
what is known of its attempt, and Ptah's repair command for it.
[Repeatable migrations](../../troubleshoot/condition-reasons/#repeatable-migrations)
says how to clear it. An applied record no file carries stops nothing, as
[Repeatable migrations](#repeatable-migrations) says.

**Unreadable directory.** Ptah could not read the artifact's migration
directory: a file it cannot load, a migration whose up or down file is missing,
since Ptah v0.33.1 a SQL file at the top of the directory that its format does
not read, such as a `seed.sql` beside Ptah's numbered files, and since v0.34.0
a second file for one direction of a version, as `0000000002_b.up.sql` beside
`0000000002_c.up.sql`, of which earlier releases ran one
([stokaro/ptah#6388](https://github.com/stokaro/ptah/pull/6388)). Ptah refuses
the whole directory rather than run the files beside such a file, and the
resource is `Blocked` with `MigrationDirectoryUnreadable`, names the file, and
publishes no plan. Since 0.2.16 that holds for an OCI artifact too, which the
fetch writes without reading it as migrations. An OCI artifact with no SQL file
at all still fails at the `fetch-migrations` step, and only that step's log
names why. Rename the file or move it out of the directory the artifact is
built from.
[A desired file Ptah could not read](../../troubleshoot/condition-reasons/#a-desired-file-ptah-could-not-read)
lists what Ptah says.

None of these resolves by waiting. The operator keeps reading the history at
`spec.interval`, so fixing the database or the artifact is enough to unblock it;
nothing else is required. Each of them stops [a wait](../operations/#wait) with
exit status 8 and the reason `Blocked`, after two polls, since no read gives
another answer until somebody fixes it. A fix in the database or the registry
does not end the `Blocked` that stood before it, so a plain `kubectl ptah wait`
started after the fix stops on that one. Confirm the fix with
`kubectl ptah refresh migration/<name> --wait` instead, which waits for a read
that begins after it. For a database ahead of the artifact the fix usually means
restoring the artifact the database was migrated with, rather than touching the
database at all.

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

## Repeatable migrations

Ptah reads an `R__<identity>.sql` file of the migration directory as a
repeatable migration: a file it runs again whenever its content changes, the
usual home of a view, a function or reference rows that are replaced whole.
The identity is the part between `R__` and `.sql`, letters, digits, `_` and `-`
up to 255 characters. Ptah keeps one row per identity in
`schema_migrations_repeatable`, with the content digest it last applied, and
creates that table on the first run that applies one.

Order is Ptah's. A run applies the pending versioned migrations first and then
each repeatable migration that is new or whose content changed, in the order
the history lists them. The operator plans the same run and changes nothing in
it:

- `status.history.pendingRepeatableCount` counts the repeatable migrations the
  next run would execute, and `kubectl get ptahmigrations -o wide` prints it
  as `PENDING REPEATABLE`, `<none>` when nothing is pending;
- the plan lists them in `spec.repeatables` after `spec.migrations`, each with
  the content digest Ptah reported and, for a changed one, the digest it last
  applied. A plan may hold repeatable migrations and no version;
- the plan's fingerprint covers that list, so a file that changes again before
  the approval publishes another plan, and the waiting approval no longer
  names the current one.

A repeatable migration is work like a version. Under `OnApproval` it waits for
an approval, under `Always` it runs as soon as the plan is published, and it
gets no SQL review of its own: like a version, the plan records its digest and
the statements stay in the source the plan binds. `kubectl ptah approve`
prints each one under the versions:

```text
Then these repeatable migrations, in this order:
  repeatable       item_names
                   checksum h1:AAkShW1bQ18rh5+kF6nNdfoHF+XZ/7T1YU+poQYCPN4=, changed from h1:k/Z3dP4G09SEnkXd53jmB5H4czHxWBq6c5E7elEhkoA=
```

The Apply hands Ptah the approved repeatable migrations with their digests in
`--expect-sequence`, beside the versions. Ptah compares them with what it
selects under the migration lock, and a different selection, a different
digest or a different order runs nothing: `status.lastRun.outcome` reads
`Failed` and the message says the history moved after the approval. The
digest a changed migration last applied, the "changed from" above, is part of
the plan's fingerprint and of what the approver reads, but nothing checks it at
run time: Ptah matches the identity and the content it runs, so a record
another tool rewrote between the plan and the Apply does not stop the run.
`status.lastRun.appliedRepeatables` names those the run applied, in the order
it ran them.

On PostgreSQL a repeatable migration runs in a transaction, so one that fails
takes no effect. The run is `Failed` when it applied nothing before it, and
`Partial` when it applied a version or another repeatable migration first. On
MySQL, and in a file Ptah runs without a transaction, a failure can leave part
of its work behind, and the run is `Unknown`. Either way the record now holds
the attempt, and the next history read stops at it with
`RepeatableMigrationUnfinished`, as [what the history says](#what-the-history-says)
describes. Nothing retries it: correct the file, delete the record with
`ptah migrations repair --repeatable <identity> --force`, and the next plan
runs the file again. `kubectl ptah refresh migration/<name> --wait` reads the
history after the repair and confirms it. A `Partial` or `Unknown` run is also recorded in
`status.unresolvedRun`, which stays until a person acknowledges it, as
[A migration run nobody accounted for](../operations/#a-migration-run-nobody-accounted-for)
says, since the file is pending again once the record is gone. A run that
finds a record another run left unfinished selects nothing and runs nothing,
which the operator records as `Failed` with no unresolved run, whatever outcome
Ptah reported for it.

A file deleted from the artifact, or renamed, leaves an applied record no file
carries. Ptah leaves the record and what the migration created alone, and so
does the operator: it plans nothing for the record, and the resource converges
as it would without it. `status.history.orphanedRepeatables` names the record,
`kubectl ptah migration` prints it, and the first history read that lists it
sends one `Warning` Event, reason `RepeatableMigrationOrphaned`, that names
the command which deletes the record, `ptah migrations repair --repeatable
<identity>`, with no `--force` once the file is gone. The Event
[can be lost](../../troubleshoot/events/#an-event-can-be-lost); the status list
is the record. Run the command only if you want the record gone: it deletes the
row and nothing the migration created. A renamed
file is a new identity, so the next plan runs it as new. A file that carries the
old identity again is held to the content the record holds: the same content
runs nothing, and changed content is planned. A database ahead of its artifact
lists no record as orphaned, since the artifact is older, not the file gone.

A [drift check](#drift-check) replays the repeatable migrations the record
holds as applied after the versions, a record no file carries included, from
the content the record keeps, so the schema it compares is the one both
produce.

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
the plan selected, and the [repeatable migrations](#repeatable-migrations) after
them with their content digests, asks for a decision, and creates the approval
as you:

```sh
kubectl ptah approve migration/orders -n application
```

It shows no SQL: a migration plan records versions, repeatable migrations and
their checksums, and the statements live in the source the plan binds, whose
digest the review prints.
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

An approval also ends with a read that ends anywhere but its plan: another plan
published, nothing left to apply, a history nothing may run against, or an artifact
the verification policy refused. It ends, too, when a run spends another approval
of the same plan, so a run that fails is not run again under the second approval
with nobody having seen the failure. The controller marks it `Stale` with the
reason, and one a run already consumed stays as it is. A plan with the same fingerprint is
the object already stored, under the same name and UID, so without this an old
approval would run it the moment it came back; instead it waits for a person. The
controller acts on an approval at once unless the interval's read is already due,
so on a source that keeps moving this costs a person only an approval that lost
that race. Approving again takes a new name: `kubectl ptah approve` generates one,
and a workflow that applies a file under a fixed name has to change it, because
applying the same file again changes nothing.

`kubectl ptah migration orders -n application` lists the approvals that name
the migration, whether each names the current plan, and the `Consumed` or `Stale`
condition the controller writes on it.

The chart's optional approver ClusterRole grants what an approver needs for
either family: read on the migration, its plans and approvals, and create on
`PtahMigrationApproval`. The chart binds it only to the subjects
`approverClusterRole.subjects` names, in every namespace;
[Exact-plan approvals](../approvals/) says what it leaves out and why.

The execution runs the approved sequence and nothing else. The Job hands Ptah
the approved list, the versions and the repeatable migrations with their
content digests, and `ptah migrations up --expect-sequence` compares it with
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
| `Failed` | A migration failed and committed nothing, or the run stopped before it ran one, as on a database it could not open, under a [drift check](#drift-check) that refused it, or on a selected file Ptah refuses before the run applies any, such as a `CREATE INDEX CONCURRENTLY` the file's transaction cannot hold. The first leaves a [dirty revision row](#what-the-history-says), which a person has to [clear](../recovery/#retry-failed-migration) before anything else runs. After the second, fix the cause and the resource plans the run again |
| `Partial` | A migration committed some of its statements and not the rest, or a [repeatable migration](#repeatable-migrations) failed after the run applied other work |
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
Ptah reports such a run `unknown` when the history read after the failure
failed too, as on a lost connection; it ran nothing all the same, and the
resource records it the same way.
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
`RepeatableMigrationUnfinished`, `HistoryModified`, `HistoryOutOfOrder`, `HistoryAhead`, `ApplyOutcomeUnknown`,
`HistoryMatched`.

## Check the schema against its history {#drift-check}

The revision table proves which files ran, and checksums prove they are the
files the database ran. Neither notices a column somebody added by hand or an
index dropped during an incident. `spec.policy.driftCheck` has Ptah look:

```yaml
spec:
  policy:
    driftCheck:
      shadow:
        urlFrom:
          name: orders-shadow-database
          key: url
```

Every Apply then runs `ptah migrations up --check-drift`. Under the migration
lock, before any pending migration, Ptah replays the migrations the revision
table records as applied into the shadow database, then the repeatable
migrations its record holds as applied, compares the schema they
produce with the target's, and empties the shadow again. What a pending
migration is about to create is not drift, since the baseline is the applied
history. A clean target lets the run go on. A target that differs stops it with
reason `SchemaDrifted`, a check that cannot decide stops it with
`DriftCheckUnsupported`, and a shadow it cannot use with
`ShadowDatabaseUnavailable`; nothing pending runs,
and [A migration whose drift check refused the run](../../troubleshoot/condition-reasons/#a-migration-whose-drift-check-refused-the-run)
says what each one means and how to list the differences. A check that was
asked for never turns into a run without one: an Apply whose check did not
happen applies nothing.

The shadow is a database of its own, of the target's engine, empty before each
check. Ptah refuses a shadow that holds anything and one that is the target
under another URL, and the API refuses a shadow named by the target's own
Secret key. Give every resource a shadow of its own: two checks that use one
at the same time find each other's tables in it, and refuse.

Where the shadow can live depends on the engine, because every History and
Apply Job of the resource mounts the shadow's login beside the target's. On
MySQL and MariaDB that login holds the global privileges
[the baseline's shadow](#what-the-history-says) needs, `DROP` and `ALTER` among
them, since Ptah empties the shadow the same way, and a global privilege
reaches every database on its server. Put the shadow on a server of its own,
never the target's, where its login could drop the target. Give its database a
name the target's does not have: Ptah tells two MySQL databases apart by name
alone, and refuses a shadow named like the target as the target, at stage
`realm-check`.

On MySQL and MariaDB the verified Ptah build, v0.35.1, compares a view, a
stored function and a MariaDB sequence under the target's database, as it does
tables and triggers
([stokaro/ptah#6416](https://github.com/stokaro/ptah/pull/6416)). Ptah v0.34.0
and earlier compared them by their database-qualified name, and the shadow's
name always differs from the target's, so each of them an applied migration
created read as drift: `missing_view <shadow>.<view>` beside
`extra_view <target>.<view>` for a view, and every Apply after the migration
that created it stopped with `SchemaDrifted`. A view listed under `exclude` to
get past that can come off the list, and the check then compares it again.

The check replays each applied migration under the directive its file states,
not under `spec.transactionMode`, which Ptah applies to the pending migrations
alone. On MySQL and MariaDB a file whose statements need to run outside a
transaction, as a `CREATE VIEW` does, therefore needs
`-- +ptah no_transaction` on its first line even where `spec.transactionMode`
is `none`. Without it the next drift-checked Apply stops with
`DriftCheckUnsupported` at stage `replay` and applies nothing
([stokaro/ptah#6504](https://github.com/stokaro/ptah/issues/6504) asks the
replay to take the run's mode). [Ptah compatibility](../../support/ptah/)
carries both and what was measured.

A PostgreSQL shadow can share the target's server. Everything Ptah empties is
in the shadow's database, so its login needs privileges there alone, and a
grant in one database reaches no other. Give it a login that owns the shadow
database and holds nothing in the target's, is not a superuser and cannot
create roles. Roles belong to the whole server, and on a server Ptah did not
start it drops none a replay created, so a history whose migrations create or
alter roles would leave them, or the change, on the target's server. Such a
history needs the shadow on a server of its own.

The URL is read like the target's: the History and the Apply Job mount the
Secret key, the runner reads it, and the manager never does. A History reading
records the shadow's identity in `status.history.shadowIdentityDigest`, a
digest of where its URL points with no credential in it, and the plan
binds that identity beside the target's. A URL that names the target's server
and database is the target under any user or `search_path`, and the reading
refuses it with `ShadowDatabaseUnavailable` and publishes no plan, so nobody
approves a run that cannot start. It does the same when the Secret key holds
no URL. Pointing the Secret at another database, or the policy at another key,
publishes a new plan, and an approval of the old one does not carry over. The
Apply refuses a shadow its plan did not bind before Ptah starts; as with a
target that moved, the run is recorded `Unknown` and the next History reading
settles it.

Ptah compares the whole schema the target's URL reaches, less its own
bookkeeping: the revision table and its log, `ptah_migration_tags`,
`schema_seeds` and the `ptah_schema_apply_log` a `PtahSchema` Apply records in,
which its reads leave out by name. A database that also holds objects no
migration declares, such as the tables a `PtahSchema` manages in the same
database or a queue table another tool creates, reads as drifted until
`exclude` names them:

```yaml
spec:
  policy:
    driftCheck:
      shadow:
        urlFrom:
          name: orders-shadow-database
          key: url
      exclude:
        - reporting.*
        - public.job_queue
```

Each entry is a selector as `ptah schema diff --exclude` reads it, and the check
leaves what it matches out of the target and out of the replayed history alike.
On a PostgreSQL URL that sets no `search_path`, Ptah reads every schema and a
selector starts with the schema's name, as above. On one that sets it, and on a
MySQL URL that names its database, a selector starts with the table's name. A
PostgreSQL row-level security policy is named through its table even on a URL
that sets no `search_path`: `orders.orders_owner[type=policy]` leaves out the
policy `orders_owner` on the table `orders`, and
`public.orders_owner[type=policy]` names nothing since Ptah v0.29.2. A
selector that matches nothing leaves nothing out and does not stop the run: the
check still compares everything else, so read each selector against that scope.
Since v0.29.2 Ptah warns about such a selector on standard error, which the
runner does not read, so the warning reaches neither the resource nor the Pod
log.
Ptah judges the grammar when the check runs, and a selector
it refuses stops the run with `DriftCheckUnsupported` at stage `scope`. The API
takes what a `PtahSchema`'s `spec.policy.exclude` takes: at most 128 entries,
none of them empty, none with whitespace at either end or a control character.
The list is part of the policy the plan binds, so changing it publishes a new
plan and an approval of the old one does not carry over; reordering it changes
nothing.

The lock holds off any cooperating runner between the check and the Apply; DDL
from a session that takes no lock can still land after the check. The check
repairs nothing.

Leaving the field out checks nothing, which is how every resource without it
runs.

## Who the database's log names

Ptah keeps its own record of each run in the target database,
`schema_migrations_log`, and the operator tells it who to name, where the name
came from and which run it was. After an approved run, every row holds the
approver's username in `actor`, `admission` in `actor_origin`, and the run's
`operationID` from the journal in `run_reference`:

```text
 version |  state  |      actor       | actor_source | actor_origin |  run_reference
---------+---------+------------------+--------------+--------------+-----------------
       1 | started | kubernetes-admin | provided     | admission    | sha256:9798b1a5...
       1 | applied | kubernetes-admin | provided     | admission    | sha256:9798b1a5...
```

The record survives losing the cluster, and deleting the approval changes nothing
about it. The name comes from the approval the run consumes, and the claim keeps
its own copy from the moment it is written, so what the run records is settled
before its Job exists. Under `apply: Always` nobody approved, and the rows read
`system:ptah-operator:apply-always:<namespace>/<name>` with `policy` in
`actor_origin`. The runner hands Ptah the reference and refuses a result whose
rows Ptah reports under another, so a journal entry's `operationID` finds the
rows of its run with `WHERE run_reference = '<operationID>'`.

A name outside printable ASCII is percent-encoded, so every character set can
store it, and so are `%` and `$`. An ordinary username or email reads unchanged,
and `oidc:José García` reads `oidc:Jos%C3%A9 Garc%C3%ADa`. Read it back with a
percent decoder that leaves `+` alone, such as Python's `urllib.parse.unquote`.

A row the database refuses does not stop the run, and nothing writes it later.
The run's `status.lastRun.migrationLog` and its `RunFinished` journal entry say
how many rows it wrote and lost, and `RunLogged` turns False with
`LogEntriesLost` until a later run writes all of its rows:

```text
  Migration log:      6 of 6 entries refused, so the database's record of the run is incomplete: version 1 Started (SQLSTATE 22001, error 1406), ...
```

The outcome stays what the revision table says.
[A run whose log entries the database refused](../../troubleshoot/condition-reasons/#a-run-whose-log-entries-the-database-refused)
says what to check.

Ptah v0.28.0 added the `run_reference` and `actor_origin` columns, which the
operator fills from 0.2.13; rows an earlier release wrote hold neither. Ptah
v0.30.0 added `repeatable`, which names the repeatable migration a row is
about; a row about one has no version. Operator releases before 0.2.16 ran
none, so every row they wrote leaves it empty. Before
a run writes its first row, Ptah adds whichever of the three a log an earlier
build created lacks,
as nullable columns, and the rows already there keep their values. On
PostgreSQL Ptah writes the log only as the table's owner, who may alter it. On
MySQL the `ALTER` this takes is part of
[the documented grant](../../support/databases/#mysql-authority), and a login
without it on the table loses every row of every run, reported as above, until
the columns are there. A login that may alter the table can add them first:

```sql
ALTER TABLE schema_migrations_log ADD COLUMN run_reference VARCHAR(255) NULL, ADD COLUMN actor_origin VARCHAR(64) NULL, ADD COLUMN repeatable VARCHAR(255) NULL;
```

The value is personal data, and `provided` means Ptah took the name on trust.
[The approver's name in the target
database](../security/#approver-in-the-database) says what that means and when a
name is shortened or a row is lost, and [What survives a lost
cluster](../recovery/#what-survives-a-lost-cluster) says what else is left.

## What never appears

No row of any table reaches the status, an Event, or `kubectl ptah migration`.
No SQL does either: a published plan records versions, descriptions and
checksums, and the statements stay in the artifact you built. A drift check
reports counts and codes there, never what Ptah or a driver said; the first
line of Ptah's report stays in the Apply's Pod log. The shadow database's URL
appears nowhere but in the Pod that reads it.
