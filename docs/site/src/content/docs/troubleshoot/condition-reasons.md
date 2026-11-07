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
| `DigestPinned` | A requested OCI reference resolved to immutable content. |
| `DispatchCommitted` | One exact approval was consumed by an Apply dispatch boundary. |
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
| `MigrationsPending` | The migration artifact carries versions the database has not applied. |
| `NoChanges` | Scoped planning produced no executable statements. |
| `NotRequired` | Policy allows this plan without separate approval: it destroys nothing and changes no privilege. |
| `Observed` | `DatabaseReachable` is true because an operation opened the target database: an Observe, a Plan or an Apply on a schema, a history read or an Apply that ran on a migration. |
| `OperationFailed` | A read-only or mutating operation failed. When it could not open the target database, the message starts with the kind of failure; see [A database the operation could not open](#a-database-the-operation-could-not-open). |
| `OperationInProgress` | A durable operation claim is active. |
| `OutcomeUnknown` | Apply attribution is uncertain and only read-only proof may proceed. |
| `Pending` | Resolved artifact content has not yet been verified. |
| `PermissionDenied` | `DatabaseReachable` is false because the server accepted the credentials and refused the user access to the database. |
| `PlanNoLongerCurrent` | An approval's plan binding is no longer current. |
| `PlanPublicationRefused` | The API server refused to store the plan a Plan operation computed: an admission policy or webhook, a quota, RBAC, or the object's size. The message carries the refusal. The Plan runs again after `spec.execution.failureRetryInterval`, so a policy or quota that stops refusing lets the next plan through. |
| `PlanReady` | An exact immutable plan is ready for approval. |
| `PodAdmissionRefused` | The operation's Job cannot create its Pod because the API server refused it; the message carries the refusal. The operation stays claimed, the Job keeps its deadline, and the Job controller keeps trying, so a policy that stops refusing lets it through. Declare what the Pod has to carry in `spec.execution.podMetadata`, or change the policy. |
| `PodConfigurationError` | The operation's Pod exists and the kubelet cannot start its container; the message carries the kubelet's words, such as `secret "application-database" not found`. Those words hold names and no value. The operation stays claimed, the Job keeps its deadline, and the kubelet keeps trying, so a Secret that arrives lets the operation go on with no action. |
| `PolicyBlocked` | Destructive-change policy blocks the current plan. |
| `PolicyChanged` | Verification policy identity or bytes changed. |
| `PolicyRefused` | Artifact verification policy explicitly refused the artifact. |
| `PolicySatisfied` | Artifact type and verification policy checks succeeded. |
| `PrivilegeChanges` | Apply policy `Always` waits for an approval because the plan changes privileges; the message names the kinds. |
| `ProofInputsChanged` | Post-Apply proof will restart from its durable immutable binding. |
| `ProofSettled` | A person gave up the post-Apply proof of an Apply whose target moved, so nothing established what that Apply did and none is recorded. The resource reads the database again under the target the URL Secret names. |
| `ProtectedTable` | The artifact asks for a change to a table `spec.policy.protectedTables` fences off. |
| `Published` | Exact plan bytes were committed to immutable storage. |
| `RealmConflict` | More than one resource claims this database and at least one has not set `spec.target.sharedRealm`, or the `PtahRealm` they name admits one claimant at a time. |
| `RealmNotAuthorized` | The `PtahRealm` in `spec.target.realmRef` does not admit this resource: it does not exist, does not list the resource's namespace, or names another engine. Nothing runs, and the resource is not counted against the ones the realm admits. |
| `RefreshFailed` | A previously resolved source could not be refreshed. |
| `Refreshing` | The requested source is being resolved again. |
| `RefreshSuspended` | Source refresh stopped before dispatch because reconciliation was suspended. |
| `Requested` | The spec requests suspension. |
| `ResolveFailed` | No immutable source resolution is available. |
| `RunAcknowledged` | A person's `PtahMigrationRunAcknowledgment` settled the run a migration recorded as unresolved. On the acknowledgment it is `Consumed=True`; on the migration it is `Blocked=False` until the database has been read again, and `status.resolvedRun` names who acknowledged. |
| `RunnerProtocolMismatch` | The runner refused the Job before starting the executor, because `execution.runnerImage` is a runner that speaks another protocol than this manager. The operation is tried again after the failure interval; set `execution.runnerImage` to the runner of the manager's own release. |
| `RunNotUnresolved` | A `PtahMigrationRunAcknowledgment` named a run its migration was not waiting on, so it settled nothing. Read the migration's `status.unresolvedRun` for the run it does record, or `status.resolvedRun` for what settled the one you named. |
| `Satisfied` | Current plan approval requirements passed final validation. |
| `ScopedChanges` | The authoritative managed scope differs from desired state. |
| `ScopedConverged` | The authoritative managed scope has no changes. |
| `ScopedPlanPending` | Raw observation completed and scoped planning remains pending. |
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
| `TargetIdentityChanged` | The URL Secret names another login or address than the plan an Apply ran from, so its post-Apply proof cannot be read. The resource is `Blocked` and keeps its proof and its database realm. Put the first target back, or [settle it](../../use/recovery/#a-schema-whose-target-moved). |
| `TLSFailed` | `DatabaseReachable` is false because the TLS handshake with the target database did not complete. |
| `UnsupportedEngine` | The selected engine has no implemented operator lifecycle. |
| `VerifyingConvergence` | Apply completed but independent read-only proof has not. |
| `Waiting` | The controller is waiting for an approval bound to the current plan. |
| `WaitingForDatabaseLock` | A migration's Apply is claimed and another operation on this database holds the realm's Lease, which the message names. No Job exists yet, and the claim asks for the Lease every five seconds, so it goes on within seconds of the Lease coming free. A wait that does not end is what `PtahOperatorOperationStalled` covers. |

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
[A migration that failed at a statement](../../use/recovery/#failed-statement) says
how a dirty row ends.

[Run versioned migrations](../../use/migrations/#what-the-history-says) carries
what each state means and which Ptah commands end it.

## A migration that failed and committed nothing

`OperationFailed` with a run that recorded nothing is the shape of a refusal
rather than of a broken statement:

```
lastRun: outcome Failed
"Migration 1 (Create Widgets) failed and committed nothing; 0 migrations before it are recorded applied"
```

The message names the migration the run stopped in, by its version and the
description Ptah reads from its file name. It does not carry what Ptah or the
database said, because that sentence can quote a row, and status and Events do not
carry one. The sentence is in the Pod log, and the message says how to read it, as
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
The message says which. Past that, or on a node that was replaced,
the log is gone unless something shipped it, and running the statement against a copy
of the database shows the same error. A Job harvested by the release before this one
keeps the five minutes it was given.

A migration's status also records `status.lastRun.failure`: the statement's
position and the SQLSTATE or error number, which hold no row. A schema Apply's
report carries neither yet.

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

An alert keys on the reason of `DatabaseReachable`, because a message may change
without an API version. `ReconciliationFailed`, `Ready` and `Progressing` keep
saying whether the operation failed, with the reasons in the table above.

`DatabaseReachable=True` with reason `Observed` means the target answered the
last time an operation opened it, not that it answers now. A failure that names
no kind leaves the condition as it was, so a schema can read `True` beside a
`ReconciliationFailed=True` whose message is `child_exit: ptah exited with code
2`. The first operation that opens the target afterwards sets `True`, including
a Plan retried after the database came back. That holds for a Plan that failed
after a read came back from the target: `protected_table`, `plan_refused`,
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
code it had, unless [the desired file does not load](#a-desired-file-ptah-could-not-read):
`child_exit: ptah exited with code 2` from an observation or a
migration history read, `invalid_plan_output` from a Plan, `apply_failed` from
an Apply. A development database, a registry or a Git remote that cannot be
reached is one of those, and so is a PostgreSQL URL with `sslmode=require`
against a server that offers no TLS, which the driver reports in a form Ptah
cannot classify. A migration Apply that Ptah stopped before it selected any
migration for another reason -- a lock or a timeout the server cannot carry, a
lint policy that does not load -- is recorded the same way, `Failed` with the
message "The run stopped before it selected any migration, and ran none", and
waits the same interval.

## A desired file Ptah could not read

When Ptah cannot load a desired schema or a migration directory, what the
operator says starts with the code `desired_source_unreadable` and carries Ptah's
reason. For a schema it names the file: the runner puts the file's name in front
where Ptah's words leave it out, as a YAML parser's do. For a migration directory
it names the file Ptah stopped at, or the version when a file is missing:

```
desired_source_unreadable: error parsing schema file: read SQL schema file /source/schema/schema.sql: expected '(' after table name: expected Operator, got Identifier at position 180
desired_source_unreadable: schema.yaml: error parsing schema file: parse YAML schema: yaml: line 1: did not find expected '-' indicator
desired_source_unreadable: failed to load up migration 0000000001_init.up.sql: invalid migration directives in 0000000001_init.up.sql: invalid +ptah no_transaction value "sometimes": expected true or false
desired_source_unreadable: incomplete migrations found (missing up or down files): [1]
```

The path is where the file sits in the operation Pod: `/source/schema/` and the
path a ConfigMap item names or the last part of a Git path. In a SQL file,
`position` is the offset in bytes from the start of the file, counted from zero,
and `head -c $((180 + 1)) schema.sql | tail -n 1` prints that line up to the first
byte of the token Ptah stopped at. YAML, HCL and DBML name a line.

| Stage | Where the reason appears, and what follows |
| --- | --- |
| `PtahSchema` Observe or Plan | `ReconciliationFailed=True` with reason `OperationFailed`, and an `OperationFailed` Warning Event. |
| `PtahMigration` history read | `Progressing=True` with reason `OperationFailed`, and an `OperationRetried` Warning Event. |

The operation runs again after `spec.execution.failureRetryInterval`, on the same
files. Repairing the source ends that with no edit to the resource. An edit to a
ConfigMap is read at once. A new commit or a moved tag is read when the retry
delay ends after `spec.interval` has passed since the source was last read, or
sooner if you ask:

```bash
kubectl ptah refresh schema/storefront -n application --wait
```

A `ReadRestarted` Event says the failed operation gave way and why, and
`status.activeOperation` starts again at attempt 1.

The reason comes from a second run of Ptah that holds no database URL and no
credential, so it cannot quote the database. It can quote a fragment of the file,
as [Output handling](../../use/security/#output-handling) describes.

Not everything that stops a desired file reaches this code. Ptah reads no SQL
statement of a migration file until it runs it, so a statement that does not
parse passes the history read and fails the Apply on the database.

A desired schema that loads and that Ptah refuses as a whole, such as a foreign
key to a table it does not declare, reads the same way under the code
`desired_schema_invalid`, in the same places and on the same retry:

```
desired_schema_invalid: schema.sql: invalid foreign key: field "id" references unknown table "nope"
desired_schema_invalid: schema.sql: postgres requires referenced columns code on table "parent" to be declared unique
```

The fault comes from the same second run, and the operator reports it only when
it is the one the failed command stopped on: the words have to end that command's
own sentence, and nothing of that sentence is copied. Some faults of a file do
not reach this code:

- An index on a column no table declares does not stop `schema drift` or
  `schema plan`, so the plan holds an index the server has to refuse.
- An index on a table no declaration defines stops them in other words, so the
  failure keeps `child_exit`, or `invalid_plan_output` from a Plan.
- A foreign key into a schema the database holds and the file declares nothing of
  is refused by the comparison with that database, which the second run does not
  have. It keeps those codes too, and a fault the second run finds elsewhere in the
  same file is not why the command stopped.

`ptah schema validate --dialect=<engine> --schema-file <file>` prints what the
file alone can be refused for, indexes included, with no database.

A schema published to an OCI registry is the exception, and for it the operator
gives no reason. Ptah parses the artifact in the `fetch-schema` init container,
before the operation starts. `ptah schema push` refuses a file it cannot parse, so
an artifact that fails here comes from a newer Ptah than the executor, or from
something other than Ptah. The message names only the step, with no file and no
reason: `the fetch-schema step exited 2, so the run never started`. That container
holds the registry credentials, so what it wrote does not reach status or an Event.

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

## A Plan that stops without saying why

Ptah refuses some changes rather than plan them, and only its failure to open the
target or to read the file reaches status. Any other sentence stays in the Pod, and
what the operator says is `invalid_plan_output: plan command did not complete
successfully`. Running `ptah schema plan` yourself, at the digest the operator pins,
prints the sentence.

| What Ptah refused | How it shows | What to do |
| --- | --- | --- |
| A view holds `security_invoker=true` or `security_barrier=true` and the desired schema does not state it. Removing the option would widen what the view's readers see. Since Ptah v0.22.0. | An Observe reports `views_modified`, and the Plan fails at every retry. | State the option in the schema and push it with a Ptah v0.22.0 CLI, since v0.18.0 cannot parse it: `true` to keep it, `false` to remove it. Or list the view in `spec.policy.exclude`. |

## A change a policy fences off

`ProtectedTable` sets `Blocked=True` and leaves `Ready` false, publishes no
plan, and dispatches nothing. Unlike the destructive-change policy, it has no
approval that overrides it: while the table is listed, no plan may change it,
and the operator keeps reading at `spec.interval` without making progress.

Two things end it, both of them spec edits by a person: take the table out of
`spec.policy.protectedTables`, or express the change as a versioned migration
instead. [Protecting a table](../../use/reference-data/#protecting-a-table)
says which to choose.

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
