---
title: Condition reasons
description: What each condition reason means and what to do about it.
---

Kubernetes Conditions are the operator's stable machine interface. Consumers
should evaluate the complete `type`, `status`, and `reason` tuple and use
`observedGeneration` to decide whether it describes the current spec. Condition
messages are bounded diagnostic text and may become more detailed without an
API version change.

The Go API exports every value below as a typed `ConditionReason` constant.
Existing values are never repurposed. Adding a reason is backward compatible;
renaming, removing, or changing the meaning of one requires an API compatibility
decision.

| Reason | Stable meaning |
| --- | --- |
| `Active` | Reconciliation is not suspended. |
| `ApplyDisabled` | Policy records plans but forbids Apply. |
| `ApplyOutcomeUnknown` | Apply failed without proof of whether mutation occurred, including an Apply whose runner reported it could not open the target: one Pod's report does not speak for its Job. |
| `ApplyPending` | A plan that destroys nothing and changes no privilege is eligible for automatic Apply. |
| `ApprovalRevoked` | A reserved approval became invalid before dispatch. |
| `ApprovedPlan` | The exact current approved plan is applying. |
| `ArtifactUnverified` | No plan is usable because artifact verification did not succeed. |
| `AuthenticationFailed` | `DatabaseReachable` is false because the target database refused the credentials in the URL. |
| `AwaitingApproval` | The current immutable plan needs a new exact approval. |
| `CommitResolved` | A Git source's ref was read at a commit, and the content digest of the files it selects there was recorded. |
| `ConfigurationError` | Controller-side configuration or declared input is invalid. |
| `ConvergedAfterUnknownOutcome` | Read-only proof found convergence after an unattributable Apply outcome. |
| `CurrentPlan` | An approval exactly matches the current immutable plan. |
| `DatabaseNotFound` | `DatabaseReachable` is false because the server has no database by the name the URL gives. |
| `DatabaseUnreachable` | `DatabaseReachable` is false because the host did not resolve, refused the connection, or did not answer in time. |
| `DesiredStateChanged` | New desired inputs arrived after older work completed. |
| `DestructiveChangesDisabled` | A destructive plan is blocked by policy. |
| `DifferenceNotPlannable` | The Plan found a difference Ptah cannot plan and saved only notes about it, such as a generated column's new expression on PostgreSQL before 17. `InSync` and `Ready` are false, nothing is published or applied, and `Ready` carries the notes. A read after an Apply that finds one ends the proof and records no Apply as converged. See [A difference Ptah cannot plan](#a-difference-ptah-cannot-plan). |
| `DigestPinned` | A requested OCI reference resolved to immutable content. |
| `DispatchCommitted` | One exact approval was consumed by an Apply dispatch boundary. |
| `DownloadVerified` | An HTTPS source's download gave the digest the spec pins, and the content digest of the files it selects was recorded. The message names both digests. |
| `DriftCheckUnsupported` | A migration Apply ran nothing because `spec.policy.driftCheck` could not decide: the target's schema cannot be read, the applied history cannot be replayed, the applied migrations fail on the shadow, Ptah refused a selector in its `exclude`, or the comparison could not describe an object. See [A migration whose drift check refused the run](#a-migration-whose-drift-check-refused-the-run). |
| `DriftUndecided` | The Plan changed nothing because Ptah could not decide whether some declared objects exist, such as a PostgreSQL publication under a URL that reads some of the database's schemas. `DriftDetected` is unknown, `InSync` and `Ready` are false, nothing is published or applied, and `Ready` names each object and Ptah's reason. See [A declared object Ptah could not decide](#a-declared-object-ptah-could-not-decide). |
| `ExecutionBindingChanged` | Evidence or approval belongs to a retired execution binding: the executor image, Ptah version, runner protocol or controller-state version changed. A manager-only release does not produce it. |
| `HistoryAhead` | The database records a migration the artifact does not carry. |
| `HistoryDirty` | A failed or interrupted migration run left a revision row behind. |
| `HistoryMatched` | The revision table and the migration artifact agree, and nothing is pending. |
| `HistoryModified` | An applied migration no longer matches the file that recorded it. |
| `HistoryOutOfOrder` | The artifact carries a migration below a version the database has applied. |
| `InputsChanged` | An operation result was discarded because its desired inputs changed. |
| `InSync` | Independent read-only planning proved convergence. |
| `JobCompleted` | Apply exited and independent convergence proof remains pending. |
| `LeaseContinuityLost` | Database lock ownership was not continuous, so evidence was discarded. |
| `LogEntriesLost` | `RunLogged` is false because the database refused entries the last run wrote to `schema_migrations_log`, so its own record of that run is incomplete. The run's outcome is unchanged. See [A run whose log entries the database refused](#a-run-whose-log-entries-the-database-refused). |
| `LogWritten` | `RunLogged` is true because the database accepted every entry the last run wrote to `schema_migrations_log`. |
| `MigrationDirectoryUnreadable` | A history read could not read the migration directory: a file Ptah cannot load, a migration without its up or down file, a SQL file the directory's format does not read, or two files for one direction of a version. `Blocked` is true, nothing is published, and the directory is read again at `spec.interval`, or at once when a ConfigMap source changes. See [A desired file Ptah could not read](#a-desired-file-ptah-could-not-read). |
| `MigrationsPending` | The migration artifact carries versions the database has not applied. |
| `NoChanges` | Scoped planning produced no executable statements. On `ApprovalRequired` it says the database converged while a plan was outstanding, so nothing is left to approve. |
| `NotRequired` | Policy allows this plan without separate approval: it destroys nothing and changes no privilege. |
| `Observed` | `DatabaseReachable` is true because an operation opened the target database: an Observe, a Plan or an Apply on a schema, a history read or an Apply that ran on a migration. |
| `OperationFailed` | A read-only or mutating operation failed. When it could not open the target database, the message starts with the kind of failure; see [A database the operation could not open](#a-database-the-operation-could-not-open). |
| `OperationInProgress` | A durable operation claim is active. |
| `OutcomeUnknown` | Apply attribution is uncertain and only read-only proof may proceed. |
| `Pending` | Resolved artifact content has not yet been verified. |
| `PermissionDenied` | `DatabaseReachable` is false because the server accepted the credentials and refused the user access to the database. |
| `PlanNoLongerCurrent` | An approval's plan binding is no longer current. On `ApprovalRequired` it says a plan was waiting for an approval and the Plan after it ended with `ProtectedTable` or `DifferenceNotPlannable`, or a read of the schema ended with `SchemaSourceUnreadable`, `SchemaInvalid` or `PlanRefused`, so nothing is left to approve. |
| `PlanPublicationRefused` | The API server refused to store the plan a Plan operation computed: an admission policy or webhook, a quota, RBAC, or the object's size. The message carries the refusal. The Plan runs again after `spec.execution.failureRetryInterval`, so a policy or quota that stops refusing lets the next plan through. |
| `PlanReady` | An exact immutable plan is ready for approval. |
| `PlanRefused` | A Plan stopped on a refusal Ptah names by a code, from the plan or from the dry run of the saved plan: a view whose security option or definer the schema does not state, an owner the server would refuse, a backslash its `sql_mode` reads another way, a declaration the target cannot host, or a code this release does not know. It stops the schema as `SchemaInvalid` does, and `DatabaseReachable` is true. See [A Plan that stops without saying why](#a-plan-that-stops-without-saying-why). |
| `PodAdmissionRefused` | The operation's Job cannot create its Pod because the API server refused it; the message carries the refusal. The operation stays claimed, the Job keeps its deadline, and the Job controller keeps trying, so a policy that stops refusing lets it through. Past the deadline, the failure the run records keeps the refusal as what was last reported, unless the Job counted a Pod by then. Declare what the Pod has to carry in `spec.execution.podMetadata`, or change the policy. |
| `PodConfigurationError` | The operation's Pod exists and the kubelet cannot set up its credential volumes or start its container; the message carries the kubelet's words, such as `MountVolume.SetUp failed for volume "database-url" : secret "application-database" not found`. Those words hold names and no value. The operation stays claimed, the Job keeps its deadline, and the kubelet keeps trying, so a Secret that arrives lets the operation go on with no action. |
| `PolicyBlocked` | Destructive-change policy blocks the current plan. |
| `PolicyChanged` | Verification policy identity or bytes changed. |
| `PolicyRefused` | Artifact verification policy explicitly refused the artifact. |
| `PolicySatisfied` | Artifact type and verification policy checks succeeded. |
| `PrivilegeChanges` | Apply policy `Always` waits for an approval because the plan changes privileges; the message names the kinds. |
| `ProofInputsChanged` | Post-Apply proof will restart from its durable immutable binding. |
| `ProofSettled` | A person gave up the post-Apply proof of an Apply whose target moved, so nothing established what that Apply did and none is recorded. The resource reads the database again under the URL the Secret holds. Under `apply: Always` the next plan applies with no approval unless it is destructive or changes privileges. |
| `ProtectedTable` | The artifact asks for a change to a table `spec.policy.protectedTables` fences off. A read after an Apply that is refused this way ends the proof and records no Apply as converged. |
| `Published` | Exact plan bytes were committed to immutable storage. |
| `RealmConflict` | More than one resource claims this database and at least one has not set `spec.target.sharedRealm`, or the `PtahRealm` they name admits one claimant at a time. |
| `RealmNotAuthorized` | The `PtahRealm` in `spec.target.realmRef` does not admit this resource: it does not exist, does not list the resource's namespace, or names another engine. Nothing runs, and the resource is not counted against the ones the realm admits. |
| `RefreshFailed` | A previously resolved source could not be refreshed. |
| `Refreshing` | The requested source is being resolved again. |
| `RefreshSuspended` | Source refresh stopped before dispatch because reconciliation was suspended. |
| `RepeatableMigrationUnfinished` | The last attempt of a repeatable migration failed or recorded no outcome. Ptah runs nothing while such a record exists, so the resource is `Blocked`, publishes no plan, and names each one, what is known of its attempt, and the repair. See [Repeatable migrations](#repeatable-migrations). |
| `Requested` | The spec requests suspension. |
| `ResolveFailed` | No immutable source resolution is available. |
| `RunAcknowledged` | A person's `PtahMigrationRunAcknowledgment` settled the run a migration recorded as unresolved. On the acknowledgment it is `Consumed=True`; on the migration it is `Blocked=False` until the database has been read again, and `status.resolvedRun` names who acknowledged. |
| `RunnerProtocolMismatch` | The runner refused the Job before starting the executor, because `execution.runnerImage` is a runner that speaks another protocol than this manager. The operation is tried again after the failure interval; set `execution.runnerImage` to the runner of the manager's own release. |
| `RunNotUnresolved` | A `PtahMigrationRunAcknowledgment` named a run its migration was not waiting on, so it settled nothing. Read the migration's `status.unresolvedRun` for the run it does record, or `status.resolvedRun` for what settled the one you named. |
| `Satisfied` | Current plan approval requirements passed final validation. |
| `SchemaDrifted` | A migration Apply ran nothing because `spec.policy.driftCheck` found the target's schema differing from the one its applied migrations produce. See [A migration whose drift check refused the run](#a-migration-whose-drift-check-refused-the-run). |
| `SchemaInvalid` | An Observe or a Plan stopped on a fault of the desired schema as a whole, which Ptah reads and refuses: a foreign key into a table the schema does not declare, an index on a column its table does not declare, or a domain whose own default is a relative time string. It stops the schema as `SchemaSourceUnreadable` does, and `DatabaseReachable` is true. See [A desired schema Ptah refuses](#a-desired-schema-ptah-refuses). |
| `SchemaSourceUnreadable` | An Observe or a Plan could not read the desired schema: a file Ptah cannot parse, a relative time string as a date or time column's default, a MySQL table partitioned with `PARTITION BY`, or a row file outside Ptah's working directory. `Ready` is false with Ptah's reason, phase `Blocked`, nothing is published, and the schema is read again at `spec.interval`, at once when a ConfigMap source or the spec changes, or on a read request. After an Apply it ends the proof. See [A desired file Ptah could not read](#a-desired-file-ptah-could-not-read). |
| `ScopedChanges` | The authoritative managed scope differs from desired state. |
| `ScopedConverged` | The authoritative managed scope has no changes. |
| `ScopedPlanPending` | Raw observation completed and scoped planning remains pending. |
| `ShadowDatabaseUnavailable` | The shadow database `spec.policy.driftCheck` names could not be used. A history read that cannot name it publishes no plan and is read again; an Apply that cannot use it ran nothing: unreachable, not empty, or the target under another name. See [A migration whose drift check refused the run](#a-migration-whose-drift-check-refused-the-run). |
| `SnapshotTaken` | A ConfigMap source was read and the files it selects were copied into the snapshot every later operation reads. |
| `SourceFreshnessUnknown` | Currentness cannot be proven because source refresh did not complete. |
| `SourceRefreshPending` | Dependent currentness is unknown during source refresh. |
| `SourceResolutionUnknown` | Convergence cannot be evaluated without a resolved source. |
| `SourceUnresolved` | A plan cannot be produced without a resolved source. |
| `Stale` | The current plan became stale before Apply. |
| `StaleObservation` | Database state must be observed again before planning. |
| `StalePlan` | Ready is false because the plan became stale before Apply. |
| `Succeeded` | The latest operation cleared the `ReconciliationFailed` Condition. |
| `SupersededApproval` | A duplicate approval lost to another approval for the same plan. |
| `SupportedEngine` | The selected database engine has an implemented operator lifecycle. |
| `Suspended` | Reconciliation is suspended and no new operation may start. |
| `TargetIdentityChanged` | The Secret the post-Apply proof reads holds another login, host, port, database or URL parameter than the plan the Apply ran from, so the proof cannot be read. The resource is `Blocked` and keeps its proof and its database realm. It tries the read again every `failureRetryInterval`, and the runner refuses each attempt before it connects to the new target, with `target_binding_mismatch` in the Pod's log. Put the first URL back, or [settle it](../../use/recovery/#a-schema-whose-target-moved), after which `apply: Always` applies the next plan with no approval unless it is destructive or changes privileges. |
| `TLSFailed` | `DatabaseReachable` is false because the TLS handshake with the target database did not complete. |
| `UnsupportedEngine` | The selected engine has no implemented operator lifecycle. |
| `VerifyingConvergence` | Apply completed but independent read-only proof has not. |
| `Waiting` | The controller is waiting for an approval bound to the current plan. |
| `WaitingForDatabaseLock` | A claim that holds the realm's Lease is waiting for it, because another operation on this database holds it, and the message names the Lease. That is a migration's Apply, on `Progressing=True`, or a schema's Apply, Plan or the Observe that proves an Apply, on `Ready=False`. A schema's read tried again after a failure says so in place of the failure, which `ReconciliationFailed` keeps, and its `Ready` reads the failure again once it holds the Lease. The claim asks for the Lease every five seconds, so it goes on within seconds of the Lease coming free. Usually no Job exists yet. A message that names a Job means the claim started it and then lost the Lease, as when the manager was down for longer than the Lease lasts, or, for a read that proves an Apply and has started nothing yet, that the Job is the Apply's, whose Pod can still start: an Apply's SQL may still be running, so read the Job before you act on the database. A Job the claim read finished is not named. A wait that does not end is what `PtahOperatorOperationStalled` covers. |

## A migration history that stops everything

`HistoryDirty` and `HistoryModified` are the two readings of a revision table
that a `PtahMigration` will not act on. Both set `Blocked=True`, leave `Ready`
false, publish no plan, and dispatch nothing.

The operator never writes to the revision table to clear either one. A dirty
row says a run stopped, how far Ptah recorded it getting, and what failed, and a
modified version says an applied migration's file no longer matches what the
database recorded for it; in both cases the answer is in the database, not in the
resource. The operator keeps reading the history at `spec.interval`, so a database
somebody fixed converges on its own, and a spec edit alone does not end either.
After the fix, `kubectl ptah refresh migration/<name> --wait` reads the history
at once and confirms it; a plain `kubectl ptah wait` exits 8 on the `Blocked`
that stood before the fix.
[A migration that failed at a statement](../../use/recovery/#failed-statement) says
how a dirty row ends.

[Run versioned migrations](../../use/migrations/#what-the-history-says) carries
what each state means and which Ptah commands end it.

## Repeatable migrations

Ptah reads an `R__<identity>.sql` file in a migration directory as a repeatable
migration. `ptah migrations up` runs it after the versioned migrations of the
same run, the first time it sees the file and again whenever its content
changes, and keeps one row per identity in `schema_migrations_repeatable`.
[Run versioned migrations](../../use/migrations/#repeatable-migrations) says how
the operator plans and runs them.

A repeatable migration whose last attempt is not settled stops the resource
the way a dirty revision row does. Both set `Blocked=True`, leave `Ready` false,
publish no plan, and read the history again at `spec.interval`. The operator
never writes to the record, so a database somebody repaired converges on its
own.

`RepeatableMigrationUnfinished` is a record whose last attempt failed or
recorded no outcome. Ptah refuses every run while one exists, versioned
migrations included. The message says what is known of each attempt:

```
Repeatable migration: item_names failed inside a transaction, which took no effect (repair: `ptah migrations repair --repeatable item_names --force`). Ptah runs nothing while one is unfinished. Establish what the attempt did and correct the file, then delete the record with the repair command; the next plan runs the file again
```

On PostgreSQL a failed attempt ran in a transaction and left nothing. On MySQL,
and under `transactionMode: none`, the message says the attempt failed outside a
transaction and may have left part of its work behind. A record that says the
attempt started and nothing more is a run that was stopped or lost inside it:
whether its statements took effect is not known. In each case read the database,
correct the file in the artifact, and delete the record:
`ptah migrations repair --repeatable <identity>` deletes the row and runs no
migration SQL, with `--force` while the file is still in the migration
directory the command reads. The next history read finds the file pending, and
the next plan runs it again, under approval as any other plan. The operator
reads the history again at `spec.interval`, so confirm the repair with
`kubectl ptah refresh migration/orders --wait`, which waits for a read that
begins after it and exits 8 with `the read ended Blocked:
RepeatableMigrationUnfinished` while the record is still there. A plain
`kubectl ptah wait` started after the repair stops with exit status 8 on the
`Blocked` that stood before it. A record whose
identity no file name can carry, written to the table by something other than
Ptah, is named quoted and with no command, since Ptah's repair refuses that
identity; only deleting its row by hand removes it.

An Apply that meets such a record, left by a run outside the operator after
the plan was published, selects nothing. Ptah reports that run `unknown` over a
running record, or over a failed one of a migration that ran without a
transaction, and `failed` otherwise. The operator records it `Failed` either
way, with a message that says Ptah ran nothing and names the record. It sets no
`status.unresolvedRun`, since nothing ran that a person would have to account
for, and the history read after it stops with `RepeatableMigrationUnfinished`.

A record of an applied repeatable migration whose file is gone from the
artifact, because it was deleted or renamed, is not a condition reason. Ptah
leaves the record and what the migration created alone and reports a run with
nothing else to do as up to date, and the operator does the same: it plans
nothing for the record, and the resource converges as it would without it.
`status.history.orphanedRepeatables` names the record, and the first history
read that lists it sends one `Warning` Event with reason
`RepeatableMigrationOrphaned`, which
[can be lost](../events/#an-event-can-be-lost), so the status list is the
record:

```
The database records repeatable migration item_names as applied, and no file of the artifact carries it. Ptah runs nothing for it and leaves what it created in place. To remove the record, run `ptah migrations repair --repeatable item_names`
```

Nothing has to be done. Run the command only if you want the record gone: it
deletes the row, needs no `--force` once the file is gone, and leaves what the
migration created in the database, so drop that first if nothing needs it. A
renamed file is a new identity, pending like any new file, and the next plan
runs it. [Repeatable migrations](../../use/migrations/#repeatable-migrations)
says what happens when a file carries the old identity again. While the database is ahead of its artifact, `HistoryAhead`, as after a tag
moved back to an older build, the history lists no record as orphaned and no
Event names one: an older artifact lacks a newer file for the same reason it
lacks a newer version. The Event names a record whose identity no file name
can carry the way the refusal above does: quoted, and with no command.

Operator releases before 0.2.16 ran a versioned sequence only. They stopped a
history that listed a repeatable migration with
`RepeatableMigrationsUnsupported`, which no longer exists.

## A migration that failed and committed nothing

`OperationFailed` with a run that recorded nothing is the shape of a refusal
rather than of a broken statement:

```
lastRun: outcome Failed
"A migration failed and committed nothing; 0 migrations before it are recorded applied"
```

The message names the migration the run stopped in, by its version and the
description Ptah reads from its file name, when the run's report says which one
it was: the revision row the run left dirty, or the file after the ones it
applied. A refusal can leave neither. Ptah asks every file the run selected what
its transaction or its own text refuses before the first of them runs, such as a
`CREATE INDEX CONCURRENTLY` inside the transaction the file runs in, and a run
it refuses there applies none of them and writes no revision row. Such a run of
one file names that file, as
`Migration 1 (Create Widgets) failed and committed nothing; 0 migrations before it are recorded applied`;
a run of several names none, as above.

The message does not carry what Ptah or the database said, because that sentence
can quote a row, and status and Events do not carry one. The sentence is in the
Pod log, and it names the file Ptah refused. The message says how to read it, as
[A statement the database refused](#a-statement-the-database-refused) explains.

On MySQL or MariaDB the usual cause is the transaction mode. Those engines have
no transactional DDL, so Ptah will not run a file it cannot witness as one unit
and says so before any statement executes.

Name the mode and the run proceeds:

```yaml
spec:
  policy:
    transactionMode: none
```

[Choosing a transaction mode](../../use/migrations/#choosing-a-transaction-mode)
says what that costs — with no transaction to unwind, a file that fails halfway
leaves what it already committed.

A run that failed on a statement says the same, with the migration it failed
in and the count of those before it that it applied. The statement the
database refused is in that file, and
[its Pod log](#a-statement-the-database-refused) holds what the database said.

## A migration whose drift check refused the run

A `PtahMigration` whose `spec.policy.driftCheck` names a shadow database has
Ptah compare the target with the schema its applied migrations produce before
any pending migration runs. A check that does not come back clean stops the
run, and the reason says which answer it was:

```
lastRun: outcome Failed
Ready=False SchemaDrifted
"The drift check found 1 difference between the database's schema and the one its applied migrations produce, so the run applied nothing: something changed the schema outside the migrations. What Ptah reported is in the Pod log for 24 hours after the Job finishes: kubectl logs -n application job/ptah-apply-orders-5d06b0eaaa30addb -c ptah"
```

- `SchemaDrifted`: the target holds an object the applied migrations do not
  create, lacks one they do, or holds one differently. Find what changed the
  schema outside the migrations. Bring the database back to its history, or
  record the change in a migration, and the next Apply runs.
- `DriftCheckUnsupported`: the check could not decide. At stage `history` the
  applied history cannot be replayed, as when an applied revision's file is
  gone or a lower version was applied after a higher one. At stage `replay`
  an applied migration failed on the empty shadow, as one that depends on
  something no migration creates does; the Pod log says which migration. At
  stage `scope` Ptah refused a selector in `spec.policy.driftCheck.exclude`,
  such as one with more parts than the URL's scope can address; the Pod log
  quotes it. At stage `schema-match` the read of the target could not describe
  an object the applied migrations create. At stage `target-introspect` Ptah
  could not read the target's schema: the target's user cannot see a kind of
  object, or the target holds one Ptah cannot describe. At stage
  `configuration` the executor has no check for the target's engine. Another
  shadow would fail the same way: fix the selector, the history or the target
  user's privileges, or remove `spec.policy.driftCheck` to run without the
  check.
- `ShadowDatabaseUnavailable`: the shadow database could not be used. It did
  not answer, it already holds something (stage `claim`, reason `not_clean`),
  it is the target under another name (stage `realm-check`), or its user lacks
  what Ptah needs to empty it or to read the replayed schema back (stage
  `re-introspect`). Check the URL the shadow Secret holds, the shadow user's
  privileges, and that the database is empty and belongs to this resource
  alone.

In every case the run applied nothing and the revision table is as the plan
read it. `Ready` and `Progressing` carry the reason, `status.lastRun` records
the run as `Failed` with the same message, and a `MigrationRunFinished`
Warning Event carries it too. The resource waits
`spec.execution.failureRetryInterval` and starts again from resolution, so the
history is read and planned again before anything else runs. Under
`apply: OnApproval` the next run needs an approval of its own, because this
one consumed its approval.

A shadow the runner can tell is unusable never reaches an Apply: the history
read that publishes the plan refuses it before anyone can approve a run
against it, and says `ShadowDatabaseUnavailable` on `Progressing` with a
reason code: `missing_shadow` when the Secret key holds no
URL, `invalid_shadow` when the URL is not a PostgreSQL or MySQL URL with a host
and a database, and `shadow_is_target` when it names the target's server and
database, under any user or `search_path`. No plan is published, the read is
tried again after `spec.execution.failureRetryInterval`, and the message names
the Secret and key to fix.

The message names counts, stages and reason codes, never Ptah's own sentences:
those can quote what the shadow database's driver said, or a default
expression from the schema. The Pod log holds Ptah's sentence, as it does for
[a statement the database refused](#a-statement-the-database-refused), and the
message ends with the command to read it. For a drifted schema that sentence
names the first difference:

```text
ptah-runner: ptah reported: the database schema differs from the schema its 2 applied migrations produce, in 1 place, so no pending migration was run; first: extra column orders.nickname
```

To see every difference, run the same check from a workstation against the
same two databases. `--dry-run` changes nothing on the target, and Ptah empties
the shadow again when the check ends:

```sh
ptah migrations up --dry-run --check-drift \
  --db-url "$DATABASE_URL" \
  --shadow-db "$SHADOW_DATABASE_URL" \
  --migrations-dir ./migrations
```

[Check the schema against its history](../../use/migrations/#drift-check)
says what the shadow database needs.

## A run whose log entries the database refused

`RunLogged` False with `LogEntriesLost` is a run that applied what its outcome
says and left the database's own record of it incomplete:

```
lastRun: outcome Applied
migrationLog: written 0, lost 6, losses: version 1 Started, SQLSTATE 22001, error 1406, ...
"The database refused 6 of the 6 entries the run of operation sha256:... wrote to schema_migrations_log with SQLSTATE 22001 (error 1406), so its own record of the run is incomplete. ..."
```

Ptah writes an entry to `schema_migrations_log` before each migration and one
after it. A write the database refuses does not stop the migration, and nothing
writes the entry later, so the table is missing the actor, the times and the
outcome of those attempts. `status.lastRun.migrationLog` and the run's
`RunFinished` journal entry name the first sixteen entries by version and state,
with the SQLSTATE and the MySQL error number. Neither carries Ptah's message,
which can quote the value the database refused; the Pod log holds it while the
Job lasts.

The codes say what refused the write. MySQL's 1406 (`22001`) is a value longer
than its column, as when the table was created before Ptah created it, with a
narrower `actor` column than Ptah's 256 characters. A trigger, a privilege that
makes the table read-only, or a full disk refuse it too. Fix what refused it,
and the next run writes its entries in full and sets `RunLogged` True. The
journal entry stays the record of the run whose entries were lost.

## A statement the database refused

When a statement in an Apply fails, the operator's handling is the same as for
any other Apply failure: it does not retry the statement, it reads the database
again, and a schema asks for a new approval. Status says the outcome, and the Pod
log says why. The `Applying` condition of a schema, `status.lastRun.message` of a
migration and the Warning Event end with the command to read it, when the run wrote
something to read:

```text
Applying  False  OutcomeUnknown  Apply outcome is uncertain; only read-only observation is permitted. What Ptah reported is in the Pod log for 24 hours after the Job finishes: kubectl logs -n application job/ptah-apply-orders-5d06b0eaaa30addb -c ptah
```

That log holds one line from the failed run, with the database's words:

```text
ptah-runner: ptah reported: apply schema changes: failed to execute SQL statement: SQL execution failed: ERROR: could not create unique index "[in the statement]" (SQLSTATE 23505)
```

`[in the statement]` stands for quoted text the statement also holds, a name or a
literal, which the runner keeps out of the log. The statement is in the plan, or in the
migration file. Reading the log takes `pods/log` in the namespace, which the diagnostic
reader role grants. The line can carry row values, and
[What a failed Apply leaves in its Pod log](../../use/operations/#failed-apply-log)
says who else that reaches.

The Job keeps the log for 24 hours after it finishes when a person approved the Apply,
or when a migration run under `apply: Always` stopped the resource, which a statement
that failed and left a dirty revision row does. A schema Apply under `Always` keeps
five minutes, because its proof can lead to the next Apply.
The message says which. Where admission refused the Job's cleanup TTL, nothing
collects the Job, and the message says the log is there until the Job is deleted.
For a schema Apply whose outcome is unknown, a later change of execution component
asks for the TTL again. Where admission admits it then, a `RetiredApplyLogKept`
Warning Event says how long the log stays, and the message recorded before is not
rewritten.
Past that, or on a node that was replaced,
the log is gone unless something shipped it, and running the statement against a copy
of the database shows the same error. A Job harvested by the release before this one
keeps the five minutes it was given.

A migration's status also records `status.lastRun.failure`: the statement's
position and the SQLSTATE or error number, which hold no row. A schema Apply's
report carries neither yet.

## A definer the login may not name

A MySQL or MariaDB statement that names another account as the definer of a
view, a routine, a trigger or an event fails with error 1227 unless the login
holds the privilege the server checks for it. On MySQL that is
`SET_ANY_DEFINER`, with `ALLOW_NONEXISTENT_DEFINER` as well for an account the
server does not have; `SUPER`, the older `SET_USER_ID` and `ALL PRIVILEGES`
pass Ptah's check too. On MariaDB it is `SET USER`. Those engines run
DDL outside a transaction, so such a failure halfway through a run would leave
the statements before it applied. Since v0.29.4 Ptah reads the login's grants
before the first statement of a schema Apply or a migration run, and refuses
the run when they do not let it name a definer a statement names
([stokaro/ptah#6124](https://github.com/stokaro/ptah/pull/6124)).

A migration file that writes `DEFINER=` for another account is checked with
every other file the run selected, before the first of them runs. Since v0.29.5
that includes a definer inside the executable comments mysqldump writes, as in
``/*!50013 DEFINER=`owner`@`%` SQL SECURITY DEFINER */``, which Ptah reads as the
server would ([stokaro/ptah#6126](https://github.com/stokaro/ptah/pull/6126)).
A refused run applies none of the files and reads as
[a migration that failed and committed nothing](#a-migration-that-failed-and-committed-nothing).
The Pod log names the file, the definer, the login and the privilege:

```text
ptah-runner: ptah reported: refusing before any statement runs: view orders_view in 0000000001_view.up.sql names the definer owner@%, and the connected account deploy@% lacks SET_ANY_DEFINER, which naming another account needs. Grant it with GRANT SET_ANY_DEFINER ON *.* TO 'deploy'@'%', or apply as owner@%
```

Grant the privilege it names to the migration's login, or write the file so
that it names the login, or `CURRENT_USER`, which needs no privilege. Where the
grants do not decide it, Ptah refuses nothing, and the server answers as it did
before v0.29.4: when `SHOW GRANTS` fails, when a MariaDB login holds only
`SUPER`, which some MariaDB releases accept and others refuse, and for
`DEFINER=CURRENT_ROLE` and a host-less definer on MariaDB. When a file of the
run creates or renames an account or a role, Ptah does not ask whether a
definer exists, since it reads the server before that file runs. A login that
holds `SET_ANY_DEFINER` without `ALLOW_NONEXISTENT_DEFINER` is then not refused
a definer an earlier file of the run creates, and the server decides.

A schema plan names a definer only for a view it replaces that runs as its
definer, and only when the plan's read connected as that definer:
``CREATE OR REPLACE DEFINER=`owner`@`%` VIEW``. A plan read as another login
is refused as `view-definer` below. The plan binds the login its URL names, and
the runner refuses an Apply whose Secret names another one with
`target_binding_mismatch` before Ptah runs, so an Apply connects as the login
the plan was read as. It runs as the same account unless that login matches
more than one MySQL account, as `'ptah'@'%'` and `'ptah'@'10.244.1.%'` do. An
Apply whose Pod connects from an address another of them matches runs as that
account, and Ptah refuses the definer the plan names before the first
statement. Status names the code,
`apply_refused: ptah refused the approved plan (definer-privilege)`, and the
Pod log carries Ptah's sentence, which names the privilege. Drop the account
the login should not match, or grant the privilege.

## A database the operation could not open

When an operation cannot open its connection to the target database, what the
operator says about it starts with the kind of failure Ptah reported, then
says what that kind means and names the Secret key the operation read the URL
from:

```
database_unreachable: the target database did not answer: its host did not resolve, refused the connection, or did not respond in time. Check the host and port in the URL that Secret "orders-database" holds under key "url", and that the database is running and accepts connections from this namespace
```

The Secret and the key are the ones `spec.target.urlFrom` named when the
operation started. On every stage the sentence is the message of
`DatabaseReachable=False`, whose reason names the kind. Where else it appears
depends on the stage that failed:

| Stage | Where the sentence appears, and what follows |
| --- | --- |
| `PtahSchema` Observe or Plan | `ReconciliationFailed=True` with reason `OperationFailed`, and an `OperationFailed` Warning Event. The operation runs again after `spec.execution.failureRetryInterval`. |
| `PtahSchema` Apply | `ReconciliationFailed=True` with reason `ApplyOutcomeUnknown`, and an `ApplyOutcomeUnknown` Warning Event that carries the sentence. Every Apply error is followed by a read-only observation before anything else runs; while the database still cannot be opened, that observation fails as above, with reason `OperationFailed` and the same sentence. The outcome stays unknown until the proof completes, so `ptah_operator_unresolved_attempts{family="schema"}` counts it and `PtahOperatorUnresolvedApply` fires for as long as the database is away. |
| `PtahMigration` history read | `Progressing=True` with reason `OperationFailed`, and an `OperationRetried` Warning Event. The read runs again after `spec.execution.failureRetryInterval`. |
| `PtahMigration` Apply | `status.lastRun` is `Failed` with the sentence as its message, `Ready=False` and `Progressing=False` carry it with reason `OperationFailed`, and a `MigrationRunFinished` Warning Event carries it. The run ran nothing, so nothing is read back at once: the resource waits `spec.execution.failureRetryInterval` and then starts again from resolution, which reads the history before anything is planned or applied. |

`kubectl ptah schema` and `kubectl ptah migration` print each condition with
its message, and the resource's status keeps the sentence whole. An Event
keeps 512 bytes of it, which holds the whole sentence while the Secret's name
and key together stay under 230 bytes.

| Kind | `DatabaseReachable` reason | What Ptah found | Where to look |
| --- | --- | --- | --- |
| `database_unreachable` | `DatabaseUnreachable` | The host did not resolve, refused the connection, or did not answer in time. | The host and port in the URL, the database's Service, and any NetworkPolicy between this namespace and the database. |
| `authentication_failed` | `AuthenticationFailed` | The server did not accept the credentials: a wrong password, an unknown or locked user, an expired password, or no host rule that admits the connection. | The user and password in the URL. |
| `database_not_found` | `DatabaseNotFound` | The server has no database by the name the URL gives. | The database name in the URL, or create the database. |
| `permission_denied` | `PermissionDenied` | The server accepted the credentials and refused the user the database: no `CONNECT` privilege on PostgreSQL, no privilege on the database on MySQL. MySQL answers this way whether or not the database exists. | The user in the URL, and what the database grants it. |
| `tls_failed` | `TLSFailed` | The TLS handshake did not complete: the server's certificate did not verify, or the server and the URL disagree about TLS. | The TLS parameters in the URL, and the certificate the server presents. |

Since Ptah v0.23.0 a MySQL read of the target also reports `permission_denied`
when the user reached the database and cannot see every object the comparison
reads, because `information_schema` lists only what a user holds a privilege on.
Ptah names the kinds of object, and the operator says which, from a fixed list of
tables, view definitions, constraints, triggers and routines, with no database,
object or privilege name:

```
permission_denied: the target database server accepted the credentials, and the user cannot see every trigger and routine in the database. Grant the user in the URL that Secret "orders-database" holds under key "url" the privileges that show them, which the MySQL authority section of the operator's supported databases page lists
```

[The MySQL authority](../../support/databases/#mysql-authority) lists what to
grant. The Pod log keeps Ptah's own sentence, which names the database and the
privileges, for whoever holds `pods/log`.

An alert keys on the reason of `DatabaseReachable`, because a message may change
without an API version. `ReconciliationFailed`, `Ready` and `Progressing` keep
saying whether the operation failed, with the reasons in the table above.

`DatabaseReachable=True` with reason `Observed` means the target answered the
last time an operation opened it, not that it answers now. A failure that names
no kind leaves the condition as it was, so a schema can read `True` beside a
`ReconciliationFailed=True` whose message is `child_exit: ptah exited with code
2`. The first operation that opens the target afterwards sets `True`, including
a Plan retried after the database came back. That holds for a Plan that ended
after a read came back from the target, whether it is tried again or stops the
schema: `protected_table`, `plan_refused`, `desired_schema_invalid`,
`unstable_plan`, `credential_leak`, `plan_seal_failed`, and a plan the API server
refused to store. `invalid_plan_output` leaves the condition, because the runner
also writes it for a child that never reached the target.

`False` is not live either. It says what the last operation that reported on the
target found. Neither family's suspend path writes this condition, so a resource
suspended for maintenance keeps `False`, and an alert on it keeps firing.

`lastTransitionTime` moves only when the status changes. A move from one kind to
another changes the reason and keeps the time. The time dates when an operation
first reported the failure, not when the target stopped answering: with
`spec.interval: 1h`, the outage may be an hour older than the condition says.

The message carries nothing Ptah or the database wrote. Ptah's own sentence
names the host, the user and the database it tried, and it does not leave the
operation Pod; running the same Ptah command against the database yourself
prints it.

Ptah classifies only the connection to the target. Any other failure keeps the
code it had, unless [the desired file does not load](#a-desired-file-ptah-could-not-read)
or [Ptah refuses the schema it states](#a-desired-schema-ptah-refuses):
`child_exit: ptah exited with code 2` from an observation or a
migration history read, `invalid_plan_output` from a Plan, `apply_failed` from
an Apply. A development database, a registry or a Git remote that cannot be
reached is one of those, and so is a PostgreSQL URL with `sslmode=require`
against a server that offers no TLS, which the driver reports in a form Ptah
cannot classify. A migration Apply that Ptah stopped before it selected any
migration for another reason -- a lock or a timeout the server cannot carry, a
lint policy that does not load -- is recorded the same way, `Failed` with the
message "The run stopped before it selected any migration, and ran none", and
waits the same interval. When the history read after that failure failed too,
Ptah reports the run `unknown`, and the message is "Ptah stopped before it
selected any migration and could not read the history after it, so it ran
nothing"; nothing ran, so no person has to account for it.

## A desired file Ptah could not read

When Ptah cannot load a desired schema or a migration directory, what the
operator says starts with the code `desired_source_unreadable` and carries Ptah's
reason. For a schema it names the file: the runner puts the file's name in front
where Ptah's words leave it out, as a YAML parser's do. For a migration directory
it names the file Ptah stopped at, or the version when a file is missing:

```
desired_source_unreadable: error parsing schema file: read SQL schema file /source/schema/schema.sql:2:29: unsupported table option: INHERITS
desired_source_unreadable: schema.yaml: error parsing schema file: parse YAML schema: yaml: line 1: did not find expected '-' indicator
desired_source_unreadable: error parsing schema file: read SQL schema file /source/schema/schema.sql:3:43: column created_at: DEFAULT 'now' on a timestamptz column is read once, when the table is created, so every row gets that moment and every plan changes the column again; write a function the server calls for each row, such as now() or CURRENT_DATE
desired_source_unreadable: failed to load up migration 0000000001_init.up.sql: invalid migration directives in 0000000001_init.up.sql: invalid +ptah no_transaction value "sometimes": expected true or false
desired_source_unreadable: incomplete migrations found (missing up or down files): [1]
```

Since Ptah v0.33.1 a migration directory is read whole
([stokaro/ptah#6313](https://github.com/stokaro/ptah/pull/6313)). A SQL file at
the top of the directory that its format does not read refuses the directory,
where v0.33.0 left the file out and ran the migrations beside it. So does an
`R__<name>.sql` file in a directory of Atlas files, whose version Ptah's revision
table cannot record:

```
desired_source_unreadable: migration file the Ptah format does not read: seed.sql. Ptah reads a migration directory whole, so it refuses the directory rather than leave this file unapplied; name it NNNNNNNNNN_<name>.up.sql, NNNNNNNNNN_<name>.down.sql or R__<name>.sql, with .up, .down and .sql in lower case, or move it out of the directory
desired_source_unreadable: migration file format detection does not read: v2_b.sql. The directory reads as Atlas migrations, and Atlas runs every SQL file at the top level of the directory, so Ptah refuses the directory rather than leave this file unapplied; read the directory with --dir-format atlas, or move the file out of the directory
desired_source_unreadable: Atlas migration R__seed.sql has version "R", which a revision table that records numbers cannot keep; use the Atlas revision table (--revision-format atlas), or Ptah's file names, under which R__<name>.sql is a repeatable migration; a directory of repeatable migrations alone is read in Ptah's format with --dir-format ptah
```

A file that is not SQL, such as a `README.md`, and a file in a subdirectory are
still left out.

Since Ptah v0.34.0 a directory of Ptah's files that holds two files for one
direction of a version is refused too
([stokaro/ptah#6388](https://github.com/stokaro/ptah/pull/6388)), where v0.33.1
ran the file that loaded last and never the other. So is a MySQL schema file
that partitions a table
([stokaro/ptah#6375](https://github.com/stokaro/ptah/pull/6375)), since Ptah
does not model MySQL or MariaDB partitioning, and a PostgreSQL relative time
default on a quoted type or an array
([stokaro/ptah#6386](https://github.com/stokaro/ptah/pull/6386)):

```
desired_source_unreadable: duplicate up migration for version 2: 0000000002_add_b.up.sql and 0000000002_add_c.up.sql
desired_source_unreadable: error parsing schema file: read SQL schema file /source/schema/schema.sql:5:3: unsupported feature: PARTITION BY on table events: Ptah does not model MySQL or MariaDB partitioning (stokaro/ptah#6357); leave it out of the desired schema and manage the partitions outside Ptah
```

The path is where the file sits in the operation Pod: `/source/schema/` and the
path a ConfigMap item names or the last part of a Git path. SQL names the line
and the column after the path, as `schema.sql:2:29` above, and YAML, HCL and
DBML name a line.

A `data` block whose row file Ptah refuses reads the same way, with the schema's
file in front of Ptah's refusal. Ptah reads a row file only inside its working
directory, `/work`, and names where the declared path led, or says the path
starts at `/`:

```
desired_source_unreadable: schema.hcl: managed data file "regions.yaml" for table public.regions: "/source/schema/regions.yaml" is outside allowed root "/work"
desired_source_unreadable: schema.hcl: managed data file "../../proc/self/environ" for table public.regions: "/proc/14/environ" is outside allowed root "/work"
desired_source_unreadable: schema.hcl: managed data file "/proc/self/environ" for table public.regions: a path starting at a root is outside allowed root "/work"
```

A schema from a ConfigMap, Git or HTTPS source is one file under `/source`, outside
that root, and no row file arrives beside it, so its row files are refused
wherever the declaration puts them, as the first line shows. Publish a schema
with rows with `ptah schema push`: its artifact carries the rows in the
declaration and reads no row file, as
[Manage reference data](../../use/reference-data/) describes.

| Stage | Where the reason appears, and what follows |
| --- | --- |
| `PtahSchema` Observe or Plan | `Ready=False` with reason `SchemaSourceUnreadable`, phase `Blocked`, and a `SchemaSourceUnreadable` Warning Event. `PlanReady` is false, `DriftDetected` and `InSync` are unknown, and `ReconciliationFailed` is false. |
| `PtahMigration` history read | `Blocked=True` with reason `MigrationDirectoryUnreadable`, phase `Blocked`, and a `MigrationDirectoryUnreadable` Warning Event. |

Neither tries the same files again: another read would give the same answer.
Each publishes no plan, marks any approval stale, and reads the source again at
`spec.interval`. Repairing the source needs no edit to the resource. An edit to a
ConfigMap source is read at once, and a new commit or a moved tag at the
interval, or sooner if you ask:

```bash
kubectl ptah refresh schema/storefront -n application --wait
kubectl ptah refresh migration/orders -n application --wait
```

With `--wait` each command waits for the read it asked for. A read that stops
on the same file again ends the command with exit status 8, the reason, Ptah's
sentence and what ends it, rather than at the timeout; the refusal from before
the request does not end it.
[Waiting for the operator](../../use/operations/#wait) has the rules.

The read after a schema's Apply reads the files the Apply ran from, which no
edit changes, so a file Ptah cannot read ends that proof instead of being tried
again. Nothing records the Apply as converged, and the database-realm Lease is
released. It usually happens after a Ptah upgrade between the Apply and that
read, when the new release refuses what the release before it read. The
upgrade [retires the execution binding](../../reference/mutation-lifecycle/#retire-an-execution-binding), so the
proof ends under the new one: the journal records it `Verified` with outcome
`Superseded`, the resource reads `Pending` with `ExecutionBindingChanged`, and
the `SchemaSourceUnreadable` Warning Event is what names the file. The next
pass reads the source at once and, while the file stays as it is, stops
`Blocked` with `SchemaSourceUnreadable`. `kubectl ptah wait --for applied` and
`kubectl ptah settle --wait` stop and say the proof settled under a newer
generation, policy or execution binding, as they do when a newer generation or
policy ends the proof.

Where the executor that planned the files refuses them on the read after, the
proof ends under the binding the Apply ran under: the resource is `Blocked`
with `SchemaSourceUnreadable` until the interval, the journal records the proof
`Verified` with outcome `Unknown` and reason `SchemaSourceUnreadable`, and both
commands stop and say Ptah could not read the desired schema.

The reason comes from a second run of Ptah that holds no database URL and no
credential, so it cannot quote the database. It can quote a fragment of the file,
as [Output handling](../../use/security/#output-handling) describes. A row file's
refusal is the exception: the second run reads no row file, so the reason is the
sentence of the command that failed, taken only in the form Ptah's guard writes
it, from the declaration and the paths in the Pod.

Not everything that stops a desired file reaches this code. Ptah reads no SQL
statement of a migration file until it runs it, so a statement that does not
parse passes the history read and fails the Apply on the database. A schema
file that loads and that Ptah refuses as a whole, such as a domain's own
relative time default since Ptah v0.34.0, reads as
[A desired schema Ptah refuses](#a-desired-schema-ptah-refuses) describes.

A schema published to an OCI registry reads the same way. The `fetch-schema`
init container checks the manifest, the artifact type, and each layer's media
type, size, digest and name, and writes the schema without parsing it
(`ptah schema pull --skip-parse`). The operation container parses it, and the
second run names the file:

```
desired_source_unreadable: error parsing schema file: parse HCL schema: /source/schema.hcl:3,31-32: Invalid single-argument block definition; A single-line block definition must end with a closing brace immediately after its single argument definition.
```

The fetched file sits at `/source/schema.hcl` in the Pod, not under
`/source/schema/`.

`ptah schema push` refuses a file it cannot parse, so such an artifact comes from
a newer Ptah than the executor, or from something other than Ptah.

What the fetch itself refuses still gets no reason: an artifact of another type,
or a layer whose media type, size or digest does not match. The message names
only the step: `the fetch-schema step exited 2, so the run never started`. That
container holds the registry credentials, so what it wrote does not reach status
or an Event.

An author reads the reason without the cluster, by pulling the artifact with the
Ptah version [the operator pins](../../support/ptah/):

```bash
ptah schema pull oci://registry.example/schemas/storefront:stable --out /tmp/schema.hcl
```

The container's log holds the same sentence until the Job is collected, for
whoever holds `pods/log`. The diagnostic reader role grants that, and the
desired-state author role does not:

```bash
kubectl logs -n application job/<job> -c fetch-schema
```

A migration directory published to an OCI registry reads the same way since
0.2.16. The `fetch-migrations` init container checks the artifact as
`fetch-schema` does, and each file's name and path, and writes the directory
without reading it as migrations (`ptah migrations pull --skip-validation`,
since Ptah v0.34.0,
[stokaro/ptah#6406](https://github.com/stokaro/ptah/pull/6406)). The history
read, in a container that holds no registry credential, reads the directory and
names a file it refuses, such as the `seed.sql` above, and the `PtahMigration`
stops `Blocked` with `MigrationDirectoryUnreadable`. In 0.2.15, and in a Job
built for a Ptah before v0.34.0, the fetch reads the directory itself, so such
an artifact fails at that step and is tried again at each failure retry.

An artifact that holds no SQL file at all is still refused at the fetch, since
an empty directory would read as a history with nothing to apply. The message
names only the step, `the fetch-migrations step exited 2, so the run never
started`, the operation is tried again at each failure retry, and the reason,
`migration artifact contains no migration files`, is only in the step's log:

```bash
kubectl logs -n application job/<job> -c fetch-migrations
```

`ptah migrations push` refuses both directories since Ptah v0.33.0, so such an
artifact comes from an earlier Ptah or from another tool.

## A desired schema Ptah refuses

A desired schema that loads and that Ptah refuses as a whole, such as a foreign
key to a table it does not declare, stops a `PtahSchema` the way
[a file Ptah could not read](#a-desired-file-ptah-could-not-read) does, under
the code `desired_schema_invalid` and the reason `SchemaInvalid`. So does an
index on a table no declaration defines, or on a column its table does not
declare: since Ptah v0.26.0 drift and plan check every index before they compare
anything, and stop on the first one they refuse
([stokaro/ptah#5845](https://github.com/stokaro/ptah/pull/5845)). Since Ptah
v0.34.0 so does a domain whose own default is a relative time string, as
`CREATE DOMAIN moment AS timestamp DEFAULT 'now'`
([stokaro/ptah#6386](https://github.com/stokaro/ptah/pull/6386)). So does a
relative time default on a column typed by a domain over a date or time type,
which Ptah refuses since v0.33.1
([stokaro/ptah#6345](https://github.com/stokaro/ptah/pull/6345)):

```
desired_schema_invalid: schema.sql: invalid foreign key: field "id" references unknown table "nope"
desired_schema_invalid: schema.sql: postgres requires referenced columns code on table "parent" to be declared unique: a primary key, UNIQUE constraint or unique index must key exactly those columns
desired_schema_invalid: schema.sql: index "i": names table "nowhere", which no declaration defines
desired_schema_invalid: schema.sql: domain "moment": DEFAULT 'now' on a timestamp domain is read once, when the domain is created, so every row gets that moment and every plan changes the domain again; write a function the server calls for each row, such as now() or CURRENT_DATE
desired_schema_invalid: schema.sql: column "created_at" of table "events": DEFAULT 'now' on a moment column is read once, when the table is created, so every row gets that moment and every plan changes the column again; write a function the server calls for each row, such as now() or CURRENT_DATE
```

| Stage | Where the reason appears, and what follows |
| --- | --- |
| `PtahSchema` Observe or Plan | `Ready=False` with reason `SchemaInvalid`, phase `Blocked`, and a `SchemaInvalid` Warning Event. `PlanReady` is false, `DriftDetected` and `InSync` are unknown, `ReconciliationFailed` is false, and `DatabaseReachable` is true: Ptah opened the database before it read the file. |

The schema is not read again at the failure retry: another read of the same
file against the same server gives the same answer. `schema validate` words the
fault with the engine's defaults, but drift and plan decide it against the
server, its version and its `sql_mode`, so a server change can move the
verdict. No plan is published, any approval is marked stale, and the source is
read again at `spec.interval`, at once when a ConfigMap source or the spec
changes, or on `kubectl ptah refresh`, which is the way to read it again after
the server changed. A proof after an Apply that meets such a schema ends as one
that meets a file Ptah cannot read does. Under the binding the Apply ran under,
the read after it meets a fault its Plan did not only where the server changed
in between, such as an upgrade that moved what it supports or its `sql_mode`.
The journal then records the proof `Verified` with outcome `Unknown` and reason
`SchemaInvalid`, and `kubectl ptah wait --for applied` and
`kubectl ptah settle --wait` stop and say Ptah refused the desired schema.
Through 0.2.15 such a read was a failed operation instead,
`ReconciliationFailed=True` with reason `OperationFailed` and an
`OperationFailed` Warning Event, tried again after
`spec.execution.failureRetryInterval`.

The fault comes from the second run that names a file Ptah could not read, and
the operator reports it only when it is the one the failed command stopped on:
the words have to end that command's own sentence, or be the whole of it for an
index, and nothing of that sentence is copied. On a PostgreSQL URL that pins no
`search_path`, drift and plan name what the file declares without a schema in
the schema the database resolves it to: a table as `table "public.parent"` and
a domain as `domain "public.moment"`, where validate prints `table "parent"` and
`domain "moment"`, and the type of a column a domain types as `public.moment`,
where validate prints `moment`. The operator allows for those names, and the
reason reads as validate prints it. A foreign key into a schema the database
holds and the file declares nothing of does not reach this code. The comparison
with that database refuses it, and the second run does not have that database.
An Observe keeps `child_exit`, tried again at the failure retry, and a Plan
reads `plan_refused`, which stops the schema with
[`PlanRefused`](#a-plan-that-stops-without-saying-why). A fault the second run
finds elsewhere in the same file is not why the command stopped.

Listing the table of a faulty index in `spec.policy.exclude` does not hide the
fault. An Observe passes no selector to `schema drift`, and the Plan passes the
entry to `schema plan`, which checks every index of the file before it selects
anything. Since Ptah v0.32.0 the plan names the table with its schema, as
`table "public.t"`, where validate prints `table "t"`
([stokaro/ptah#6219](https://github.com/stokaro/ptah/pull/6219)); the reason
still reads as validate prints it.

`schema plan` refuses such a file with the refusal code `declaration-refused`,
and the operator asks the second run about that refusal as it asks about a
failure. A declaration refusal the second run does not explain, such as the
foreign key into a schema only the database holds, reads `plan_refused: ptah
refused to plan the change (declaration-refused)` and stops the schema with
`PlanRefused`.

`ptah schema validate --dialect=<engine> --schema-file <file>` prints what the
file alone can be refused for, indexes included, with no database.

## A source access refused before the fetch

Before an Observe, a Plan, or a migration's History or Apply fetches a source
through a credential, an init container checks the access the credential's
owner granted: `validate-source-authority` for an OCI source, and the
`fetch-git-source` or `fetch-https-source` step itself for Git and HTTPS. A
Resolve checks the same grants first, so the step refuses what changed after
it: a Secret whose grant now names another host, a `caSHA256` pin added to the
Secret, or a check an upgrade added after the source resolved, such as the
refusal of a `caSHA256` pin without `transport.caFrom`. The message names the step, then says what a Resolve says
of the same refusal, code and sentence:

```
the validate-source-authority step refused the source's access, so the run never started: invalid_oci_access: registry credential is not granted for the OCI source authority
```

A schema carries it in `ReconciliationFailed`, a migration in `Progressing`,
both with the reason `OperationFailed`, and the Warning Event carries the same
sentence. An Apply stopped there ends uncertain, as an Apply whose result was
not read does, and the read that settles it meets the same step and says the
same. The sentence names a grant, a key, a field or a host and never a
credential's value, and the runner redacts it with the container's credentials,
as it does a Resolve's error. Change what it names in the Secret or in the
resource; the operation tries again at its failure retry interval.

Any other failure of these steps names only the step and its exit status, as
for the `fetch-schema` step above, and so does a step whose runner writes no
refusal record, such as one from an earlier release.

## A Plan that stops without saying why

Ptah refuses some changes rather than plan them. A refusal Ptah names by a code
reaches status as `plan_refused: ptah refused to plan the change (<code>)`, and
Ptah's sentence, which names the objects and says what to change, is a line of
the Plan Pod's log that starts with `ptah-runner: ptah reported:`. Status never
carries it. The Plan's Job, and the log with it, is collected five minutes
after it finishes; `kubectl ptah refresh` runs a new Plan, which writes a fresh
log. A plan Ptah fails to make for any other reason than opening the target or
reading the file reads `invalid_plan_output: plan command did not complete
successfully`, and its sentence reaches no log. Either way, running `ptah
schema plan` yourself against the database, at the digest the operator pins,
prints the sentence.

| Stage | Where the reason appears, and what follows |
| --- | --- |
| `PtahSchema` Plan | `Ready=False` with reason `PlanRefused` and the message, phase `Blocked`, and a `PlanRefused` Warning Event. `PlanReady` is false, `DriftDetected` and `InSync` are unknown, `ReconciliationFailed` is false, and `DatabaseReachable` is true: Ptah refuses only once it has compared the schema with the database, and the dry run of the saved plan runs only after the plan that compared them. |

A `plan_refused` Plan is not tried again at the failure retry, whatever the
code, one this release does not know included: another Plan of the same schema
against the same server, as the same account, gets the same answer. A MySQL
login that matches more than one account can connect as another one from a Pod
on another node, and a `view-definer` refusal can then lift on the next Plan;
[A definer the login may not name](#a-definer-the-login-may-not-name) and
[MySQL authority](../../support/databases/#mysql-authority) say how such a
login behaves. No plan is published, any approval is marked stale, and the
source is read again at `spec.interval`, at once when a ConfigMap source or the
spec changes, or on `kubectl ptah refresh`. A refusal the server decides, such
as a role it does not hold, a privilege the login lacks or its `sql_mode`,
lifts only on the server, and nothing the operator watches says when: fix it
there, then run `kubectl ptah refresh`. Through 0.2.15 such a Plan was a
failed operation, `ReconciliationFailed=True` with reason `OperationFailed`
and an `OperationFailed` Warning Event, tried again after
`spec.execution.failureRetryInterval`.

A proof after an Apply that meets such a refusal ends as one that meets a
schema Ptah refuses does. The journal records it `Verified` with outcome
`Unknown` and reason `PlanRefused`. After an Apply that succeeded,
`kubectl ptah wait --for applied` and `kubectl ptah settle --wait` then stop
and say Ptah refused to plan the desired schema. The likelier way to such a
proof is an Apply Ptah refused, `apply_refused`, whose proof meets the same
code in its dry run. The wait verbs stop before it, on the Apply's
`ApplyFinished` entry with outcome `Unknown`, as for any Apply that failed.

A dry run that finds the database moved since the plan was read, or that waits
too long for another session's lock, refuses nothing about the plan. It reads
`invalid_plan_output: plan validation reported refused (stale-plan) rather than
a dry run`, or `(lock-timeout)`, and the Plan is tried again at the failure
retry. A plan refused under either code reads `invalid_plan_output: plan
command reported refused (stale-plan) rather than a plan` and is tried again
too, though Ptah v0.34.0's `schema plan` takes no lock and reads no plan file,
so it raises neither. Every other code stops the schema, from the plan or from
the dry run, one this release does not know included. A code Ptah adds later
for a database that moved or was busy stops it as well, and the schema then
waits for `spec.interval` or `kubectl ptah refresh` rather than planning again
at every failure retry.

Where Ptah's report lists the views and options it refused, as it does for
`view-security-option` since v0.29.3, the message names them after the code,
each as the view and the option. Since v0.30.0 a PostgreSQL view is named with
its schema; a MySQL or MariaDB view is spelled as the schema spells it:

```
plan_refused: ptah refused to plan the change (view-security-option: orders_view sql_security, notes_view sql_security)
plan_refused: ptah refused to plan the change (view-security-option: public.notes_view security_invoker)
```

Since v0.29.4 a MySQL or MariaDB refusal with the code `view-definer` lists
each view with the account it runs as and the account the plan's read ran as,
and the message names both after the view:

```
plan_refused: ptah refused to plan the change (view-definer: orders_view definer owner@% account ptah@%)
```

An account the read did not record is left out, and Ptah refuses then too.

Since v0.30.1 Ptah's `schema apply` refuses a PostgreSQL plan that would give
a view or a materialized view to an owner the server would refuse, before any
statement runs, under one of three codes
([stokaro/ptah#6196](https://github.com/stokaro/ptah/pull/6196)). `schema plan`
does not check owners, since the login that plans is often not the one that
applies. A Plan meets the refusal in the `schema apply` dry run it runs on the
saved plan, and the operator reads the code from that dry run; an approved
plan's Apply meets it in the run itself. The message names the statement, the
role, and what is missing:

```
plan_refused: ptah refused to plan the change (missing-owner-role: ALTER VIEW "public"."my_docs" OWNER TO "reporting" role reporting does not exist)
plan_refused: ptah refused to plan the change (owner-transfer: ALTER VIEW "public"."my_docs" OWNER TO "reporting" role reporting connected ptah lacks SET)
plan_refused: ptah refused to plan the change (owner-schema-privilege: ALTER VIEW "public"."my_docs" OWNER TO "reporting" role reporting lacks CREATE on schema public)
```

`SET` is the privilege PostgreSQL 16 and later ask for, and `MEMBER` the one
earlier servers ask for. Where the role already owns the view, as when a plan
rebuilds the view and gives it back to its owner, the message adds `, and the
role owns it now`. Ptah's sentence in the Pod log says what to grant.

It names the first eight that fit in the 512 bytes an Event keeps, then counts
the rest, as in `, and 3 more`. A view name too long to fit is cut and ends in
`...`, and a control character in a view or an account reads as a space. A
report that lists no view, or lists them in a form the operator does not read,
gives the code alone.

Ptah's `schema apply` refuses a MySQL or MariaDB plan with a backslash in a
string literal on a session whose `sql_mode` names `NO_BACKSLASH_ESCAPES`,
before any statement, and since v0.34.0 it names the refusal `backslash-mode`
([stokaro/ptah#6400](https://github.com/stokaro/ptah/pull/6400)). Ptah writes a
saved plan for a session in the default mode, where a backslash starts an
escape, and a server in the other mode would store other characters than the
plan spells. Ptah reads the session's mode and does not set it, and the operator
refuses a `sql_mode` parameter in the URL, so the mode is the server's global
one. A Plan meets the refusal in its dry run, and an approved plan's Apply in
the run itself:

```
plan_refused: ptah refused to plan the change (backslash-mode)
apply_refused: ptah refused the approved plan (backslash-mode)
```

The second follows a Plan made before the server took the mode. Before 0.2.16
the Plan read `invalid_plan_output: plan validation reported failed rather
than a dry run`, and nothing said why.

| What Ptah refused | How it shows | What to do |
| --- | --- | --- |
| A PostgreSQL view holds `security_invoker=true` or `security_barrier=true` and the desired schema does not state it. Removing the option would widen what the view's readers see. | An Observe reports `views_modified`, and the Plan stops with `PlanRefused` and the code `view-security-option`, naming each view and option. | State the option in the schema and push it with a Ptah v0.22.0 or later CLI: `true` to keep it, `false` to remove it. Or list the view in `spec.policy.exclude`. |
| A MySQL or MariaDB view holds `SQL SECURITY INVOKER` and its declaration names no mode, since Ptah v0.29.3. A replacement without the clause would run the view with its definer's privileges, not its reader's. | An Observe reports `views_modified`, and the Plan stops with `PlanRefused` and the code `view-security-option`, naming each view and `sql_security`. | Declare the mode and push the schema with a Ptah v0.26.0 or later CLI: `CREATE SQL SECURITY INVOKER VIEW` in a SQL file, or `security = INVOKER` in HCL, to keep it, `DEFINER` to change it. Or list the view in `spec.policy.exclude`. |
| A MySQL or MariaDB view runs as its definer, under `SQL SECURITY DEFINER` before and after the change, and its definer is another account than the schema's login, since Ptah v0.29.4. A replacement without a `DEFINER` clause would make the login its definer, so the view would run with the login's privileges ([stokaro/ptah#6109](https://github.com/stokaro/ptah/pull/6109)). | An Observe reports `views_modified`, and the Plan stops with `PlanRefused` and the code `view-definer`, naming each view, its definer and the login. | Point the schema's Secret at the definer, so the plan names that definer and the view keeps it. Or declare the view `SQL SECURITY INVOKER` if it should run with its reader's privileges, and the login becomes its definer. Or list the view in `spec.policy.exclude`. |
| The plan gives a PostgreSQL view or materialized view to a role that does not exist, since Ptah v0.30.1. | The Plan stops with `PlanRefused`, or an approved plan's Apply fails, with the code `missing-owner-role`, naming the statement and the role. | Create the role and run `kubectl ptah refresh`, or declare the owner the view should have. |
| The plan gives a view to a role the login may not give an object to: the login lacks `SET` on the role (PostgreSQL 16 and later) or membership in it (earlier), since Ptah v0.30.1. | The Plan stops with `PlanRefused`, or the Apply fails, with the code `owner-transfer`, naming the statement, the role, the login and the privilege. | `GRANT <role> TO <login> WITH SET TRUE` on PostgreSQL 16 and later, `GRANT <role> TO <login>` before, and run `kubectl ptah refresh`; or declare an owner the login may give the view to. |
| The plan gives a view to a role without `CREATE` on the view's schema, since Ptah v0.30.1. | The Plan stops with `PlanRefused`, or the Apply fails, with the code `owner-schema-privilege`, naming the statement, the role and the schema. | `GRANT CREATE ON SCHEMA <schema> TO <role>` and run `kubectl ptah refresh`, or declare another owner. |
| A MySQL or MariaDB plan holds a backslash in a string literal, and the session's `sql_mode` names `NO_BACKSLASH_ESCAPES`, since Ptah v0.34.0. | The Plan stops with `PlanRefused` and the code `backslash-mode`, or an approved plan's Apply fails with it before any statement. Ptah's sentence in the Pod log names the mode. | Take `NO_BACKSLASH_ESCAPES` out of the server's global `sql_mode` and run `kubectl ptah refresh`, or declare the value without a backslash. |
| The plan drops an object something else depends on. | The Plan stops with `PlanRefused` and the code `dependent-object`. | Declare the dependent too, or drop it by hand, or list the dropped object in `spec.policy.exclude`. |
| The target cannot host or plan what the schema states. | The Plan stops with `PlanRefused` and the code `declaration-refused`, unless the file alone explains it, which stops the schema with [`SchemaInvalid`](#a-desired-schema-ptah-refuses) instead. | Read the sentence and change the declaration. |

## A change a policy fences off

`ProtectedTable` sets `Blocked=True` and leaves `Ready` false, publishes no
plan, and dispatches nothing. `DriftDetected` is true with the same reason,
since Ptah refuses only a plan that would change the table. Unlike the
destructive-change policy, it has no approval that overrides it: while the
table is listed, no plan may change it, and the operator keeps reading at
`spec.interval` without making progress. A plan that was waiting for an
approval when the fence refused the next Plan goes with it: `ApprovalRequired`
turns false with `PlanNoLongerCurrent`.

The read after an Apply meets the fence when the fenced table changed in the
database after the Apply's plan was made. The proof ends there: the Apply is
not recorded as converged, the database realm is released, and the journal
records `Verified` `DriftRemains`, which stops `kubectl ptah wait --for
applied`. Before 0.2.15 the proof stayed owed, held the realm, and claimed the
same read again at the next pass.

Two things end it, both of them spec edits by a person: take the table out of
`spec.policy.protectedTables`, or express the change as a versioned migration
instead. [Protecting a table](../../use/reference-data/#protecting-a-table)
says which to choose.

## A difference Ptah cannot plan

Since v0.30.0 Ptah writes a note, and no statement, for a difference it finds
and cannot plan, such as a generated column whose expression changed on
PostgreSQL before 17, which has no `ALTER COLUMN ... SET EXPRESSION`
([stokaro/ptah#6165](https://github.com/stokaro/ptah/pull/6165)). Ptah v0.29.2
planned nothing for it either, and the resource read as in sync.

A Plan whose plan holds only notes sets `DifferenceNotPlannable` on
`DriftDetected`, which is true, and on `PlanReady`, `InSync` and `Ready`, which
are false. The resource is `Blocked`, nothing is published, approved or
applied, and no failure is reported. A plan that was waiting for an approval
goes, and `ApprovalRequired` turns false with `PlanNoLongerCurrent`. `Ready`
and a Warning Event carry the notes:

```
ptah found a difference it cannot plan: WARNING: Generated column public.tasks.code changed, but ALTER COLUMN SET EXPRESSION requires target capability alter_generated_column_expression, unavailable on this target (PostgreSQL added it in 17); manual migration required. Nothing is planned: make the change by hand or with a migration, or change the artifact
```

The operator reads again at `spec.interval`, so a database changed by hand, or
an artifact that matches the database, converges with no other step. A plan
with statements and notes is published and approved as any other plan, and
`kubectl ptah plan` prints each note where Ptah placed it. A read after an
Apply that finds only notes ends the proof as a fenced table does: no Apply is
recorded as converged, and the journal records `Verified` `DriftRemains`.

## A declared object Ptah could not decide

Ptah decides whether a declared object exists only where its read of the
database describes that kind of object. It reads PostgreSQL publications, for
one, only when the read covers every schema the database holds, since a
publication belongs to the whole database. Under a URL that pins
`search_path` in a database with another schema, it can neither create a
declared publication nor show that one exists: the plan leaves it out, and
the drift report counts it as `undecided` and fails without finding drift
([stokaro/ptah#3850](https://github.com/stokaro/ptah/pull/3850)). Before
0.2.15 the operator refused that report, and every Observe failed with
`invalid_observed_state`.

A Plan that changes nothing and lists undecided objects sets `DriftUndecided`
on `DriftDetected`, which is unknown, and on `PlanReady`, `InSync` and
`Ready`, which are false. The resource is `Blocked` with no plan to approve,
and no failure is reported. `Ready` and a Warning Event name each object and
Ptah's reason:

```
ptah could not decide 1 declared object: PostgreSQL publication notes_pub (whether the publication exists was not established: ...). Nothing is planned for them: use a URL that reads the whole database, with a login that may read what each reason names
```

Point the URL at the whole database, with no `search_path` and a login that
may read each schema, and the next read decides the object. The operator reads
again at `spec.interval`. A plan with statements beside undecided objects is
published and approved as any other plan, and it leaves the undecided objects
out, so the read after its Apply sets `DriftUndecided` and records no Apply as
converged.

## Unsupported engines

`spec.target.engine` accepts a bounded engine identifier so GitOps tools can
store desired state before that engine is implemented. An unknown value does
not cause database or registry access. After all previously dispatched Apply
work and its mandatory read-only proof reach a safe boundary, the controller
sets all of the following for the current generation:

- `EngineSupported=False` with reason `UnsupportedEngine`;
- `Ready=False` with reason `UnsupportedEngine`;
- `phase=Blocked` and `status.observedGeneration=metadata.generation`.

The same boundary marks `DatabaseReachable`, `DriftDetected`, and `InSync`
unknown, makes `PlanReady`, `ApprovalRequired`, and `Applying` false, and
prevents new operation Jobs. If an older plan exists, the controller first
persists this non-authorizing status, then marks approvals for the schema stale
in bounded passes and clears the current plan pointer. An approval CREATE that
crossed the status fence is retired on its watch-triggered reconciliation even
after that pointer is gone. Changing the spec back to `PostgreSQL` or `MySQL`
sets `EngineSupported=True` with reason `SupportedEngine` and restarts the
complete workflow at Resolve.
