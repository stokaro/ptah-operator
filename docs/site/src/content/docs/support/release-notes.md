---
title: Release notes
description: What a cluster already running the previous version has to change before it moves to the next one.
---

One entry per version, newest first, answering one question: what a cluster
already running the version before it has to change before it moves.

This is not a commit log. What a release publishes and how to verify it before
you install it is [Releases and provenance](../releases/). What the API holds
to between versions, and what it does not, is
[API compatibility](../api-compatibility/).

Before `v1` there are no deprecations: a field that has to go, goes, in the
release that says so, and this is where it is said. A version that asks
nothing of you says so in as many words, rather than leaving you to read it
out of silence.

## 0.2.12

**Before you upgrade.** The manager image is new, so keep `--force-conflicts`
on the upgrade. The CRD schemas gain the record of a migration run's
`schema_migrations_log` writes and the HTTPS source, so the CRD schema version
moves from 18 to 20. Once this release's hook has updated the CRDs, 0.2.11's
CRD hook refuses [a rollback](../../use/operations/#rollback) to 0.2.11: a
release's hook refuses CRD schemas newer than the release reads, and 0.2.11
reads neither field.
An installation that renders
the chart's rules gains `PtahOperatorJournalLinesDropped`. Set
`monitoring.prometheusRule.journalLinesDroppedWithin` to `""` to leave it out.
The executor moves to Ptah v0.26.0, which retires the execution binding:
approve again whatever was waiting, after the resource has read its database
with v0.26.0 and planned. Move `kubectl ptah` with the operator. A plan with a PostgreSQL view statement
now carries the `effects` Ptah records for it, and a plugin from an earlier
release names that field as one it does not read and refuses to approve the
plan or print it with `-o sql` or `-o json`. Ptah Console 0.3.2 still holds
approval for every plan, as the 0.2.10 notes say, so approve with
`kubectl ptah` from this release. On MySQL and MariaDB, declare the security
mode of every view the schema manages before the upgrade, with
`CREATE SQL SECURITY INVOKER VIEW` in a SQL schema file or
`security="INVOKER"` on a view annotation where the view reads with its
readers' rights. Ptah v0.26.0 reads a view's mode, and a view the database
holds as `INVOKER` that the declaration leaves without one is replaced as a
`DEFINER` view, whose definer is the login the Apply runs as. Under
`apply: Always`, a plan that replaces or alters a MySQL or MariaDB view now
waits for an approval. An alert or a script that matches `child_exit` or
`invalid_plan_output` to find an index on a table no declaration defines has to
match `desired_schema_invalid`, and an index on a column its table does not
declare now stops the Observe and the Plan with that reason rather than
reaching the database at Apply. A database audit that lists what the executor
sends has to expect the reads the executor bullet names. A consumer that reads
every condition of a `PtahMigration` has to expect `RunLogged`. A Git source
whose URL host, or whose credential Secret's `host`, holds a character outside
printable ASCII now stops before Ptah runs with `invalid_git_access`, and
`allowedGitHosts` no longer admits such a host: write an internationalized name
in its `xn--` form in all three places. A Git source over `https://` that
selects no `transport.caFrom` under a credential Secret holding `caSHA256` now
stops before Ptah runs with `invalid_git_access`, where it sent the credential
under the system roots: select the CA the Secret pins, or remove `caSHA256`
from the Secret if the system roots are what its owner grants. Nothing else.

- **A migration run says when the database lost its log entries.** Ptah
  writes an entry to `schema_migrations_log` before and after each migration,
  and a write the database refuses does not stop the run. Since v0.23.0 Ptah
  reports each entry it lost
  ([stokaro/ptah#5541](https://github.com/stokaro/ptah/pull/5541)), and the
  operator now records it: `status.lastRun.migrationLog` and the run's
  `RunFinished` journal entry count the entries the database accepted and
  refused, and name the first sixteen refused by version, state, SQLSTATE and
  MySQL error number. The new `RunLogged` condition is `False` with
  `LogEntriesLost` until a later run writes all of its entries, and `True` with
  `LogWritten` then. Neither carries Ptah's message, which can quote the value
  refused. The run's outcome does not change. `kubectl ptah migration` and
  `kubectl ptah history` print the record. The runner carries it as an
  optional field of the run it already reports, so the runner protocol stays 1.
  [A run whose log entries the database refused](../../troubleshoot/condition-reasons/#a-run-whose-log-entries-the-database-refused)
  says what to check.
- **A source can be an HTTPS download pinned by its digest.**
  `spec.desired.https` and `spec.artifact.https` name a URL, the SHA-256 of
  the bytes it serves, a format (`File`, `TarGzip` or `Zip`) and the path the
  resource reads, with an optional Basic or Bearer credential and a private CA.
  Every operation downloads the URL again and refuses other bytes, and nothing
  is stored in the cluster. A plan binds the URL, the path and the selected
  files, not the download digest, so an archive published again with the same
  files keeps its plan once the spec names the new digest.
  `ArtifactResolved` says `DownloadVerified`, and a source policy gains
  `allowedHTTPSHosts`. [From HTTPS](../../use/sources/#from-https) has the
  fields and what each interval costs in downloads.
- **Source hosts compare as ASCII.** The credential grant and a source policy
  compared a Git host with Unicode case folding, so a URL whose host held
  U+0130, which folds to `i`, matched a grant for the ASCII name while the
  transport dialed its IDNA form, another host. The runner and the source
  policy now refuse a host with any byte outside printable ASCII, written
  directly or percent-escaped, and compare the rest without ASCII case.
- **Executor.** The verified executor is now Ptah v0.26.0. Each PostgreSQL
  schema read now also reads the inherit and set options of a role membership,
  whether the role it runs as is a superuser, the foreign tables that keep an
  extension's foreign-data wrapper in use, the aggregates, collations, foreign
  tables, event triggers and publications it does not compare, and each
  trigger's arguments, all from `pg_catalog`. Each MySQL view read selects
  `SECURITY_TYPE`. A MySQL revision table Ptah creates is now
  `ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`; one that exists keeps its own.
  Ptah now rates a partition `DETACH` that leaves its parent destructive
  itself, which the operator already did, so such a plan is held as before.
- **A view's invoker rights are read from what the plan does.** 0.2.11 raised
  `security_invoker` named by `CREATE OR REPLACE VIEW` whatever its value, so a
  plan that changed the body of a view stating the option waited for an
  approval under `apply: Always` even where the option kept its value. Ptah
  v0.26.0 records what each PostgreSQL view statement does to the view's
  security options, before and after, read from the database the plan was
  computed against, and the operator reads `security_invoker` from that record.
  A change on a view that existed is raised in either direction, and an option
  that keeps its value raises nothing, so such a body change now runs
  unattended. A view the plan drops and creates again, as Ptah rebuilds one it
  cannot change in place, is read the same way against the value its drop
  recorded. A new view is raised when it reads with its readers' rights. One
  that states `security_invoker=false` is no longer raised, as one that states
  nothing is not. A view the plan makes after a drop that may have taken it
  without a record of what it held, through a `CASCADE`, `DROP OWNED` or a
  `DROP VIEW` Ptah records nothing for, is raised whatever it states: before,
  one made again with no option took its owner's rights unapproved. A
  statement Ptah records nothing for is otherwise read from its SQL as before.
  [Plans that change privileges](../../reference/plans-and-approvals/#privilege-changes)
  has the rule.
- **Every MySQL view a plan replaces waits under `apply: Always`.** A MySQL or
  MariaDB view runs with its definer's rights unless it says
  `SQL SECURITY INVOKER`, and `CREATE OR REPLACE VIEW` and `ALTER VIEW` set the
  mode afresh. Ptah records no effect for a MySQL view, and the statement does
  not say what the view held, so the operator now records `SecurityDefiner` for
  every such statement. A new view is raised only when it states
  `SQL SECURITY DEFINER`. Before, no MySQL view was raised.
- **An index fault is `desired_schema_invalid`.** Ptah v0.26.0 checks every
  index of the desired schema before drift and plan compare anything, and stops
  on the first one it refuses
  ([stokaro/ptah#5845](https://github.com/stokaro/ptah/pull/5845)). The
  Observe or the Plan fails with `desired_schema_invalid` and validate's line,
  such as
  `schema.sql: index "i": names table "nowhere", which no declaration defines`,
  on the same retry as any other fault of the file.
- **A refused MySQL index build ends `Failed`.** A MySQL migration whose
  `CREATE INDEX` or `CREATE UNIQUE INDEX` the server refuses on an InnoDB table
  now ends `Failed` rather than `Unknown`
  ([stokaro/ptah#5838](https://github.com/stokaro/ptah/pull/5838)): it leaves no
  `status.unresolvedRun` and takes no acknowledgment, and the revision row
  reads `state=failed`. That row's `error` quotes the database, and MySQL's
  answer to a duplicate key names the duplicate value; the runner keeps it out
  of the run's record and message, but a person or a tool that reads the
  revision table reads it. Any other DDL statement the server refuses, and an
  index build whose answer was lost, still end `Unknown`.
  [A row in `state=pending`](../../use/recovery/#failed-pending-row) has both.
- **A plan that would drop an extension under a foreign table is refused.**
  Where the database holds a foreign table on an extension's foreign-data
  wrapper and the declaration drops the extension, the Plan fails with
  `invalid_plan_output` and Ptah's sentence, which names the extension and the
  foreign table and says to drop the foreign table first
  ([stokaro/ptah#5898](https://github.com/stokaro/ptah/pull/5898)). Before, the
  plan held `DROP EXTENSION` and the server refused it at Apply.
- **A PostgreSQL Apply the database rolled back still reads `Unknown`.** Ptah
  v0.26.0 reports a schema Apply as `failed` once the database confirmed it
  rolled back every statement it sent
  ([stokaro/ptah#5737](https://github.com/stokaro/ptah/pull/5737)). The runner
  passes that on as `apply_failed` where it said `apply_outcome_unknown`, and
  the journal, the `ApplyOutcomeUnknown` Event and the observation that follows
  are as before: Kubernetes can start more than one Pod for a Job, and one
  Pod's report does not speak for the others.
- **An alert for dropped journal lines.** `PtahOperatorJournalLinesDropped`
  fires when any manager replica dropped a journal line within
  `monitoring.prometheusRule.journalLinesDroppedWithin`, 10 minutes by default.
  A dropped line reached no collector, so any drop counts, and the alert renders
  with the rules rather than waiting for a threshold.
  [Dropped journal lines](../../use/operations/#journal-lines-dropped) says what
  to check.

## 0.2.11

**Before you upgrade.** The manager image is new, so keep `--force-conflicts`
on the upgrade. The CRD schemas gain the fields a schema's Apply journals, so
the CRD schema version moves and [a rollback](../../use/operations/#rollback)
to 0.2.10 is refused once this release has updated the CRDs. Under `apply: Always`, a plan that changes an existing
PostgreSQL view's `security_invoker`, or changes the definition of a view that
states it, now waits for an approval bound to it. A plan published before the
upgrade that holds such a statement and waits for an approval is planned again
and asks for a new one. A dashboard or an alert that reads
`ptah_operator_plans_total` or
`ptah_operator_approvals_total{outcome="required"}` for `family="schema"`, or a
script that counts `ApprovalRequired` Events, has to expect one count for each
plan rather than one for each read of a plan waiting for approval. A script or
an alert that matches the text of a `MigrationJobCleanupDeferred` Event has to
match `has no cleanup scheduled, so it stays until it is deleted by hand or
with its owner` in place of `keeps its own deadline because its cleanup could
not be scheduled`, and has to expect no such Event for a Job its claim names by
UID while that Job is still running. A claim that never recorded its Job's UID
now records it before the TTL is asked for, so such a Job gets no such Event
either where admission admits the TTL. A dashboard or an alert on
`ptah_operator_applies_total{family="migration",outcome="started"}` has to expect
a count for an Apply whose Job's UID is recorded only as its run goes `Unknown`,
once for each such run. A migration Job an earlier release left
without a TTL stays until you delete it. A script that runs `kubectl ptah
refresh --wait` or `kubectl ptah wait --for read` on a resource whose read ends
in the verification policy's refusal of the artifact now gets exit status 8
with `PolicyRefused`, where it ran to the timeout and exited 7. The resource
may have been refused already, or in sync until a tag moved. A refusal from
before the request does not stop the wait; only the refusal the requested read
ends in does. A script that approves a schema plan again once its approval goes
`Stale`, or an alert on
`ptah_operator_approvals_total{family="schema",outcome="stale"}`, has to expect
an approval recorded before the interval ended to be consumed by an Apply and
then marked `Stale` by the proof after it, as every applied approval is, rather
than marked `Stale` before any Apply. Read `Consumed` to tell the two apart. A
plan approved just before its interval ended now takes one approval and counts
one `stale` outcome, where it took two of each. A Plan retried after a failure
no longer counts a `stale` outcome or records a `LeaseContinuityLost` Warning
Event. A script that acts on the exit
status of a `kubectl ptah` wait on a migration run has to expect the statuses
the bullet on waits under two managers' clocks lists. A namespace with a
ResourceQuota on `count/ptahresultrecords.operator.ptah.run` should leave room
in it for one `retired` record per operation, as [A quota on result
records](../../use/operations/#result-record-quota) says.
The manager Pod now asks for a 60-second termination grace period, so a policy
that refuses or lowers `terminationGracePeriodSeconds` has to allow 60 seconds.
An alert on `MigrationJobCleanupDeferred` can now fire for a read-only
migration Job too, including one a suspension leaves running. A `PtahSchema`
records the same Warning under the new reason `SchemaJobCleanupDeferred`.
Either Warning can now also name a finished Job its claim harvests, a
succeeded, failed or uncertain Apply's included, where admission answers the
Job's TTL with Forbidden or Invalid. A
script that reads the `kubectl logs` command out of the
`MigrationRunUncertain` Event of a `PtahMigration` deleted with the default
propagation finds none there now. A script or an alert that matches the whole
text of a `PtahSchema`'s `ApplyOutcomeUnknown` Event has to expect
`; last reported before that:` and what the schema last reported after it, on
an Apply whose Pod never ran, and on a deleted schema
`the PtahSchema is being deleted, so nothing will observe the database:` and
how the Apply ended in place of `observing database state`. A deleted schema
whose Apply's executor ran and stopped now records that Apply: an alert on
`ptah_operator_applies_total{family="schema",outcome="uncertain"}` or on
`ptah_operator_failures_total{family="schema",stage="apply"}` counts it once,
and `ptah_operator_operation_duration_seconds` records it with
`outcome="uncertain"` where it recorded `canceled`. Such records count in
`ptah_operator_unresolved_attempts` until the deleted schema goes, so
`PtahOperatorUnresolvedApply`, which is critical and has no pending period, can
fire for a schema deleted while its Apply runs: for about 30 seconds past the
Apply's deadline when its Pod never started, and for a pass when its executor
ran and stopped. `kubectl ptah suspend --wait` on a `PtahSchema` no longer
waits for the Job of a read that an execution-binding change retired or whose
database lock epoch moved, unless the read serves a post-apply proof: the
resource reports `Suspended` while that Job may still run, and a
`SchemaJobCleanupDeferred` Warning names the Job. On a schema whose proof read
runs a Job this release cannot confirm, it now waits for that Job to stop, up
to 60 seconds past the Job's deadline, which can outlast the five-minute
default of `--wait-timeout`. 0.2.10 retried such a read at once without waiting
for its Job. A script or a GitOps tool that deletes a `PtahSchema` or a
`PtahMigration` with `--cascade=orphan`, `propagationPolicy: Orphan` or
`orphanDependents: true`, or deletes one that carries the `orphan` finalizer
without naming a propagation, is now refused and has to name `Background`, the
default. A live `PtahSchema` whose Apply's executor the kubelet ended with
reason `ContainerStatusUnknown` now counts in
`ptah_operator_unresolved_attempts`, and can fire `PtahOperatorUnresolvedApply`,
until the Apply's execution deadline and grace period have passed and a reading
follows. With `resultDelivery.enabled=false`, the failure either family records
for such an executor still says `the result log is gone:`, now followed by the
container and the Pod the kubelet lost rather than the refusal of a
log read. A script that reads the `kubectl logs` command out of an
`ApplyOutcomeUnknown`, `MigrationRunUncertain` or `MigrationRunFinished` Event,
`Applying` or `status.lastRun.message` finds none where every executor of the Job
ended that way, or where every Pod of the Job ended with no status for its
executor or with its executor last reported waiting, and an alert on Warning
Events can see the new reasons `PodStuckTerminating`, on a `PtahMigration` and
on a live `PtahSchema` as well, `ReadJobOverdue`, and `ApplyPodGone`, on a
`PtahMigration` and on a deleted `PtahSchema`. Where a Job's cleanup TTL could not
be set, as for a Job the deletion of a `PtahMigration` orphaned, that pointer is
no longer left out: it says the log is there `until the Job is deleted`, with
the same `kubectl logs` command. A deleted `PtahSchema` whose Plan, or whose
read that proves an Apply, runs past its Job's deadline, the grace of its Pods
and 30 seconds no longer waits for that Job: it hands the database Lease back
and goes while the Pod may still read, and records the proof `Unknown`. A
script that waits for a `PtahMigration` to be deleted, for an acknowledgment of
its run to be taken, or for its database to be free, after its Apply Pod was
deleted with `--force` or its Apply Job was deleted, or after PodGC marked its
Apply Pod `Failed` over an executor it had not seen stop, has to allow until
the end of the Apply's term, the time the `ApplyPodGone` Warning names. A script
that reads a `PtahSchema`'s `Ready` while its claim waits for the database lock
sees reason `WaitingForDatabaseLock` where it saw `OperationInProgress`, or,
for a read tried again after a failure, the failure's reason, which
`ReconciliationFailed` still carries.
A script or an alert that reads `child_exit` from an Observe or
`invalid_plan_output` from a Plan of a schema whose row file Ptah refuses as
outside its working directory reads `desired_source_unreadable` now.
A script that reads the text of `kubectl ptah schema` or `migration`, rather
than `-o json`, finds the `kubectl ptah history` command where it found a
`kubectl get` of the namespace, when more than five approvals or acknowledgments
name the resource, and one more line before the closing `kubectl events` line.
Operation Pods now read every credential from a file in a Secret volume, so a
policy that refuses a Secret volume in a Pod, or admits only the volumes it
knows, has to admit `database-url`, `development-database-url`,
`registry-credentials` and `git-credentials`, each of them keys of a Secret the
resource already names. A script or an alert that matches the text of a
`PodConfigurationError` Event or condition has to match
`MountVolume.SetUp failed for volume "database-url" : secret "<name>" not found`
in place of `secret "<name>" not found`, and
`references non-existent secret key: url (volume database-url reads Secret <namespace>/<name>)`
in place of `couldn't find key url in Secret <namespace>/<name>`. A missing
database Secret or key now stops the Pod before its init containers run, where it
stopped only the operation container once the source was fetched. A Job 0.2.10
dispatched is adopted and supervised as before.

Every read-only Job, a `PtahSchema`'s Observe, Plan, Verify and Resolve and a
`PtahMigration`'s Resolve, Verify and History read, is now created with
`ttlSecondsAfterFinished: 300`. An admission policy of your own that refuses a
Job created with a TTL has to admit it on those Jobs. A script that reads a
finished read Job or its Pod log has to expect both gone five minutes after the
Job finished, whether or not the operator had read it. An alert on
`SchemaJobCleanupDeferred` or `MigrationJobCleanupDeferred` no longer fires for
a read-only Job this release creates.

The manager now writes each journal entry it stores as one JSON line on its
standard output; its diagnostic log stays on standard error. A pipeline that
collects the manager's container log receives these lines too, and they name
approvers and acknowledgment authors by username, as the journal does. A parser
that reads every line of that log as a diagnostic record has to skip the lines
whose `logger` is `journal`.

PostgreSQL 16.x joins the supported release lines. Run 43292445627 of the
Added database lines workflow passed the whole lifecycle on it on every
supported Kubernetes minor at the pinned Ptah build. A cluster on 17.x or 18.x
has nothing to change for that.

A MySQL account made from [MySQL authority](../databases/#mysql-authority) as
0.2.10 showed it can hold more than its one database. The page named the
database bare, in `GRANT ... ON app.* TO 'ptah'@'%'`, and while
`partial_revokes` is off, MySQL's default, such a grant reads `_` and `%` in the
name as wildcards: a grant on `my_app` also covers `myXapp` and every other
database the pattern matches on that server. On MySQL 8.4.13 an account granted
that way created a table in `myXapp`. The page now writes the name in backquotes
with both escaped. Where the managed database's name holds `_` or `%`, read what
the account holds:

```sql
SHOW GRANTS FOR 'ptah'@'%';
```

A line that names the database with no backslash before each `_` or `%`, such
as `` ON `my_app`.* ``, is the pattern. Revoke it as it is spelled there, and
grant the same privileges on the escaped name:

```sql
REVOKE SELECT, CREATE, ALTER, DROP, INDEX, REFERENCES, TRIGGER, EXECUTE, SHOW VIEW ON `my_app`.* FROM 'ptah'@'%';
GRANT SELECT, CREATE, ALTER, DROP, INDEX, REFERENCES, TRIGGER, EXECUTE, SHOW VIEW ON `my\_app`.* TO 'ptah'@'%';
```

`SHOW GRANTS` then names `` `my\_app`.* ``, and the account is refused in
`myXapp`.

[PostgreSQL authority](../databases/#postgresql-authority) now lists
`TEMPORARY` on the database beside `CONNECT`. Ptah reads a declared column's
type and default back from a temporary table, and without the privilege a
column can read as changed on every plan. A new database grants `TEMPORARY` to
`PUBLIC`, so a login has it unless that grant was revoked. If it was:

```sql
GRANT TEMPORARY ON DATABASE app TO ptah;
```

- **`kubectl ptah history`.** `kubectl ptah history schema/NAME` or
  `migration/NAME` prints the lifecycle journal the resource's status keeps, in
  the order the operator recorded it and from when it is complete, then every
  approval and run acknowledgment that names the resource by name and UID,
  oldest first. Reading the journal needed `jsonpath` and `jq`, and a resource
  with more than five decisions sent the reader to a list of the whole
  namespace. Each entry shows its plan, decision, Job and outcome, folded
  failures show their count and the last time, a schema's Apply names the
  `Verified` entry that proved it or says the proof is still owed, and each
  plan and decision an entry names is marked as still stored or not. An entry
  an earlier release wrote without the UIDs that would join it says so, and a
  status with no journal reads differently from an empty one. `-o json`
  carries every field of every entry. It needs `get` on the resource; listing
  plans and decisions is optional.
  [What a resource kept of its past](../../use/read-a-plan/#history) has the
  rest.
- **`kubectl ptah schema --details` and `migration --details`.** The views
  printed none of the operation a resource has claimed, a schema's proof still
  owed after an Apply, a Lease release or binding retirement still owed, or a
  migration's revision rows, and `-o json` carried only the view. `--details`
  prints every status field the plugin's API version knows after the view,
  each labeled by its API name, with a line that says what a claim knows of its
  Job (confirmed by UID, a create attempted, not created yet, or not confirmed
  for a claim that marks nothing before its create), that an Apply's own result
  is not the reading that proves it, and that an acknowledgment is a statement
  rather than proof. With `-o json` it adds `details.status`, the status as the
  plugin decodes it. Without the flag neither output changes. [Every status
  field the plugin knows](../../use/read-a-plan/#details) has the rest.
- **`kubectl ptah settle` gives up a proof whose target moved.** A schema whose
  URL Secret names another target than the plan its Apply ran from stops with a
  `kubectl annotate` command in its message, and the value had to be copied out
  of it. `kubectl ptah settle schema/NAME` reads the value from the message and
  writes the same annotation, and nothing else. It prints how the journal
  records the Apply's end and what the operator gives up when it takes the
  statement, and changes nothing, with exit status 4, on a schema that has not
  stopped there or a value that no longer matches it. `--expect` names the
  value you read, so a stop that moved to another target since is refused
  rather than settled. `--wait` waits until the operator has taken the
  statement.
  [Settle from the command line](../../use/recovery/#settle) has the details.
- **A view's invoker rights wait for an approval.** `ALTER VIEW ... SET
  (security_invoker=true)` ran unattended under `apply: Always`, and the view's
  readers then needed rights of their own on the tables it reads, so a read that
  worked could start failing. The operator now records `SecurityDefiner` for
  `security_invoker` set to any value or reset by `ALTER VIEW` or
  `CREATE OR REPLACE VIEW`. It read only `false` and `off` before, so a value
  Ptah wrote as `0` or `no` ran unattended as well. Ptah restates a view's
  options when its definition changes, so such a change waits too, as a
  `CREATE OR REPLACE FUNCTION` does. `CREATE VIEW` is read as before. A
  `CREATE SCHEMA` for a schema the URL leaves out still runs unattended;
  [Databases and privileges](../databases/#postgresql) says what it does under
  the documented login.
- **A waiting plan counted once.** A scheduled read of a schema clears the plan
  it waits on and publishes the same plan again when the database has not
  changed. The plan counter, the required-approval counter and the
  `ApprovalRequired` Event took that plan for a new one, so a plan waiting for a
  day under a short interval was counted, and announced, once for each read.
  They now follow the plan's `PlanPublished` entry in `status.journal`. A plan
  already published still records an `ApprovalRequired` Event, and counts as
  `required`, each time it starts asking: after something withdrew its
  requirement, or after an edit of `spec.policy.apply` such as `Never` to
  `OnApproval`. [Events](../../troubleshoot/events/) lists the cases. Migrations
  keep the plan they wait on in status through a read, so their counters did not
  move.
- **A migration Job left running by an unknown run is collected.** A migration
  Apply whose outcome went `Unknown` while its Job was still running, as when a
  release changed the executor or the Job under it, kept its Job until the
  resource was deleted, and the Job held a place in a `count/jobs.batch` quota.
  So did a Job that had finished when the database lock changed hands under its
  run. Where the claim recorded the Job's UID, the Job now takes its cleanup TTL
  by the rule every migration run keeps its Pod log by, once it stops, and the
  Warning that said a running Job's cleanup could not be scheduled is gone.
- **A migration Job its claim never recorded is collected too.** A migration
  Apply claim that never recorded its Job's UID, as when the manager stopped
  between creating the Job and recording it, left the Job without a cleanup TTL
  once its run went `Unknown`, because admission admits that TTL only on the Job
  a claim names by UID. The claim now records the UID of the Job under the name
  it reserved, where the resource owns that Job and the Job is the one the claim
  fixes, before the TTL is asked for, and the Job takes its TTL by the same rule.
  A run recorded `Unknown` before the claim could read that name names the Job
  when the claim records it. A Job the claim does not fix, such as one under
  another execution epoch, is never recorded, and a `MigrationJobCleanupDeferred`
  Warning names it when the claim goes. A TTL write that meets a newer version of
  the Job, as when the Job controller has just written its status, keeps the
  claim for the next pass, which asks again, where it used to leave the Job with
  no TTL.
- **A recorded approval wins over a due read.** An approval the operator had
  recorded in `status.plan.approval` when the interval ended was spent by the
  read that followed: the read's observation marked it `Stale` although the
  plan did not change, and the same plan waited for a second approval. The
  Apply it authorizes now runs first, and the read-only proof after the Apply
  reads the database. An automatic Apply under `apply: Always` and an approval
  not yet recorded still wait for the read. A read requested on a schema with
  a recorded approval follows its Apply too, and the `ReadRequested` Event now
  says so.
- **Waits under two managers' clocks.** After a leader failover the manager that
  writes a resource's status can run a clock behind or ahead of the one before
  it, and some waits put times the two had written in order. A wait now counts a
  reading by a new stamp and decides a migration run by the reading the operator
  records on it, `status.lastRun.verification`. So
  `kubectl ptah wait migration/... --for applied` now exits 8 with
  `CompletionNotProved` when the reading after the run was of another database,
  where it exited 0, or 7 at its timeout under skew. It also ends at the
  read-back itself, which is in `Planning` when the read-back found more
  migrations pending. Under skew it keeps waiting where it exited 0 on a
  migration suspended before the history was read back, or 8 with
  `NotConverged` on one its realm blocked, and exits 0 where it timed out with 7
  on a read-back dated before the run's end.
  `kubectl ptah approve migration/... --wait` and `wait --for applied --plan`
  keep waiting under skew where they exited 8 with `CompletionNotProved` on a
  migration suspended or blocked by its realm before the read-back, and exit 8 with
  `CompletionNotProved` where they timed out after a reading of another database
  dated before the run's end. A wait for `in-sync`, `approval` or `read`, and the
  `--wait` of `refresh`, `resume` and `set-source`, exits 0 under skew where it
  timed out on a reading dated before the one before it. With one clock as
  well, `wait --for applied` now goes by the run's record when the resource
  moves on before the next poll: it exits 0 where it exited 8 with
  `NotConverged` on a resource blocked after a read-back that found it in sync
  or pending, and 8 with `NotConverged` where it exited 0 after a read-back that
  was blocked. A run that an operator older than 0.2.10 recorded carries no
  verification, so `wait --for applied` still dates its reading by the run's
  end.
- **The receiver says why it could not verify a runner's token.** A result
  receiver that could not complete a TokenReview, or read the Pod a token is
  bound to, answered the runner 503 and logged nothing, so nothing told a
  receiver denied `tokenreviews` from one the API server answered slowly. The
  manager now logs a `result-receiver` line for each with the namespace and Pod
  the request names, and the API server's `code` and `reason`, such as `403`
  and `Forbidden`, or `deadlineExceeded` where no status came back. It never
  logs the token or the text of an answer: a review that returns an error says
  so without its text. An authority check that reached no verdict after the
  runner authenticated, with either Pod tokens or client certificates, answered
  503 silently as well, and now logs `result authorization reached no verdict`
  the same way, or that it is waiting for the Job's adoption or the Pod's pin.
- **Result cleanup under a full quota.** A quota that refused the `retired` record
  stopped cleanup in a live namespace: the collector asked again every 30 seconds,
  nothing started the retention window, and the operation's records stayed. Once the
  operation's Job is gone, the collector now starts the window with a foreground
  deletion of the result and credential records, as it already did in a terminating
  namespace, and the records go an hour after it. A `ResultRetirementQuotaExceeded`
  Warning Event on the `PtahSchema` or `PtahMigration` names the quota. While the
  Job exists, the collector retries with backoff up to every 5 minutes.
- **The kubelet's words stay on an Apply whose Pod never started.** An Apply
  whose Secret or key is missing reports the kubelet's words while it waits. A
  `PtahMigration` Apply past its Job's deadline then recorded only `result frame
  not found`, and an Apply that ended another way before its result was read
  recorded only how it ended: its inputs changed, as when `urlFrom` is pointed at
  the Secret that exists, its Job was deleted, replaced or left without its
  owner, a release changed an execution component under it, or, on a
  `PtahMigration`, its database Lease changed hands or the resource was deleted.
  Those records now carry the words after that sentence, as what was last
  reported: `status.lastRun` and `Blocked` on a `PtahMigration`, and
  `ReconciliationFailed` on a `PtahSchema`. A deleted `PtahMigration` keeps them
  in its `MigrationRunUncertain` Warning Event, and now looks at the Pod while
  it waits for it, so a Pod that starts in that wait takes them off. Nothing is
  added when the claim's own Job reports a Pod that succeeded.
  [Before the Secret exists](../../reference/credentials-and-admission/#external-secret-stores-not-synced)
  has the messages.
- **A deleted schema's Apply is never discarded in silence.** A `PtahSchema`
  deleted while its Apply ran discarded the claim with no record and no Event
  when the Job's one Pod showed its executor terminated. That included an
  executor that never started: at the Job's deadline the kubelet stops a Pod
  whose container still waits on a missing Secret and marks the container
  terminated with reason `ContainerStatusUnknown`, so the case where why the Pod
  never ran matters most left nothing. A `ContainerStatusUnknown` termination now
  proves nothing: the deletion records the outcome as unknown, keeps the last
  report, and holds the database until the Apply's execution deadline and its
  grace period have passed. An executor that ran and stopped is recorded as well,
  since nothing read its result, and the database goes back without that wait.
  Every Apply ending of a deleted schema, including a database Lease that changed
  hands while it waited, leaves an `ApplyOutcomeUnknown` Warning Event that says
  the schema is being deleted and how the Apply ended, as a deleted
  `PtahMigration` leaves `MigrationRunUncertain`. A Pod that started since the
  last look leaves the last report out of the Lease and lost-Job endings too.
- **More endings keep why the Pod never ran.** The same words now follow an Apply
  whose Job stopped being its claim's after the inputs moved, whose claim could not
  build its Job again because its plan was deleted, whose Job a newer release
  builds differently, whose Job left more than one Pod, or whose Pod does not match
  what its claim built. On the last two, a Pod that shows its executor started
  leaves the words out. A Pod the API server refused to create is covered as well:
  an Apply or a read still refused at its Job's deadline recorded only `result
  frame not found`, and now carries the refusal after it and points at no Pod log;
  a refusal the Job got past by creating a Pod is left out. A `PtahSchema` Apply
  keeps the words when its database Lease changed hands, where the write that
  records the lost Lease used to replace them, and when the schema is deleted while
  its Apply runs. Its `ApplyOutcomeUnknown` Event now carries
  them, which is what stays of a deleted schema, and a deleted schema looks at the
  Pod while it waits for it, as a deleted `PtahMigration` does.
  [Before the Secret exists](../../reference/credentials-and-admission/#external-secret-stores-not-synced)
  lists each of these endings, and
  [Execution](../../reference/execution/#meshes-and-policy-engines) says what a
  refused Job records.
- **A slow Job-write webhook no longer spends an approval.** A dispatch gave
  its mark, create, read-back and record 15 seconds, while the chart gives the
  manager's Job-write webhook 30. A webhook slower than 15 seconds made the
  manager give up on its own create, which reads as an outcome nobody
  established, so a single manager retired the Apply as `Unknown` and the
  approval was spent. The dispatch now waits 40 seconds, so the API server
  answers first: the Job is created, or the create is refused and the
  dispatch is withdrawn with the approval kept. The write that withdraws or
  retires the claim no longer runs on the ended context, so it lands at once
  instead of on a later pass. A stopping manager now takes up to 50 seconds to
  finish such a dispatch, and its Pod's termination grace period is 60
  seconds, up from Kubernetes' default 30. `webhook.controllerWriteTimeoutSeconds`
  is unchanged. While the webhook does not answer, each attempt holds the
  family's one reconcile worker for those 30 seconds, and the family's other
  resources wait behind it. A refused create is now withdrawn when something
  wrote the resource while admission ran, such as an annotation, a label or
  `kubectl ptah refresh`: the write made the withdrawal conflict, and the Apply
  ended `Unknown` with its approval spent.
- **A webhook's 500 denial keeps the approval.** The manager's Job-write
  webhook answers a read it could not make with code 500, and the API server
  keeps that code on the webhook's denial. The dispatcher read every 500 other
  than a failed webhook call as an outcome nobody established, so it retired
  the Apply as `Unknown` with its approval spent, although nothing had been
  stored. An answer that reads `admission webhook "..." denied the request` is
  now a refusal whatever its code: the dispatch is withdrawn and the next pass
  makes the create again.
- **A Job another manager is still creating is waited for.** After a node
  pause two managers act at once until the old leader's lease runs out, and one
  of them could read the other's dispatch mark before that manager's Job create
  landed. It found no Job and retired the Apply as `Unknown` at once, with the
  approval spent. A manager that finds a mutating claim marked, with no Job UID
  recorded and no Job, now writes nothing for 55 seconds from the first time it
  saw it so, and calls the Job lost only after that. An Apply whose manager
  died between its mark and its create therefore goes `Unknown` up to a minute
  later than before, and a manager that restarts in that time waits again. A
  claim that recorded its Job's UID, and a resource being deleted, do not
  wait.
- **A refused read-only migration Job is collected.** A read-only migration
  claim that could not confirm the Job under its name, as after a release that
  changed the Job, ran again under a new attempt and left that Job with no TTL,
  so the Job and its Pod stayed until the resource was deleted, and nothing named
  them. Admission admits the TTL only on a finished Job the active claim names by
  UID. Where the Job is the one the claim fixes, the claim now waits for the Job
  to stop, as it waits for a Job it supervises, and gives it the five minutes a
  harvested Job gets before it runs again, recording the Job's UID first if it
  never did. It waits at most the Job's `spec.execution.activeDeadlineSeconds`, 15
  minutes by default, plus the termination grace its Pods are given and 30
  seconds, and `Progressing` names the Job meanwhile. A Job admission would
  refuse, such as one under another execution epoch, is left at once, and so is
  one that has not finished by then.
  A `MigrationJobCleanupDeferred` Warning Event names the Job it leaves.
- **A read-only migration claim over another Job runs again.** A read-only
  migration claim whose Job under its reserved name had been replaced, or was no
  longer owned by the resource, asked for that Job's cleanup TTL before it moved
  to a new attempt. Admission refuses that TTL on a Job the claim does not name
  by UID or does not own, so every pass failed on the refusal, and the read never
  ran again. The claim now moves to a new attempt at once, as a schema claim
  does, and writes nothing to that Job.
- **A deleted migration no longer points at a log it takes with it.** A
  `PtahMigration` deleted while its Apply waited records the run `Unknown`, and
  its `MigrationRunUncertain` Warning Event said the Pod log stayed for 24 hours
  and gave the `kubectl logs` command. The next pass releases the resource, and
  the garbage collector deletes the Job and its Pod with it, so the command found
  nothing. The Event now names the Job and says the Job and its Pod are deleted
  with the resource, and `UnresolvedRunDiscarded` says the same while the Job
  stands. A `FailedApplyLogShortened` Event no longer fires for such a Job. To keep
  the log, follow it with `kubectl logs -f` from before the delete, as
  [Suspension and deletion](../../use/operations/#suspension-and-deletion) says.
- **More read-only Jobs are collected when their claim lets go.** Several
  read-only claims let go of the Job under their name and left it with no TTL:
  a `PtahSchema` claim that could not confirm its Job, as after a release that
  changed the Job, a claim of either family discarded because its resource was
  suspended, and a migration claim retired because an execution component
  changed. Admission admits the TTL only on a finished Job the active claim
  names by UID, so the Job and its Pod stayed until the resource was deleted,
  and nothing named them. A `PtahSchema` claim that could not confirm its Job,
  and a migration claim a rotation retires, now wait for a Job they can confirm
  to stop, by the bound the entry on a refused read-only migration Job gives,
  and give it the five minutes a harvested Job gets. `Ready` names the Job while
  a `PtahSchema` claim waits.
  A claim that never recorded the Job's UID records it first. Suspension
  prevents new operations but does not stop one already running, and a TTL only
  removes a Job that has finished, so a suspended claim does not wait, including
  one a migration rotation was holding: a Job that has finished gets its TTL as
  the claim goes, and a Warning names one still running, which stays until it is
  deleted. A TTL write that fails for any reason but a refusal fails the pass
  and keeps the claim, so the next pass asks again. A new
  `SchemaJobCleanupDeferred` Warning Event names a Job a `PtahSchema` claim
  leaves with no TTL. A `PtahSchema` claim a binding change retires already
  waited for its Job; a TTL write admission refuses there no longer fails the
  pass, and a Warning names the Job. A `PtahSchema` Plan whose database lock
  epoch moved while its Job ran waited for that Job with no bound, renewing the
  Lease, and failed every pass on a refused TTL write. It now waits by the same
  bound, and a Warning names a Job it leaves. A migration being deleted records
  the UID of an Apply Job its claim never recorded before it asks for that
  Job's TTL, and a Job deleted before that TTL was asked for is no longer named
  by a Warning.
- **A read-only wait ends at the Job's stop bound and gives way to suspension.**
  A `PtahSchema` claim a binding change retired waited for its read-only Job
  with no bound, and the resource ran nothing else until the Job stopped, which
  a Job that carries only `FailureTarget` never does. It now waits no longer
  than the Job's deadline, the grace of its Pods and 30 seconds, and a
  `SchemaJobCleanupDeferred` Warning names a Job still running then. That
  claim, and a Plan whose database lock epoch moved while its Job ran, waited
  for the Job under `spec.suspend` too. Neither waits once the resource is
  suspended, and the Warning names a Job they leave running; a deletion still
  waits for the Plan's Job, by the same bound. The retired claim also renews
  the Lease it holds while it waits. It let the Lease run out before, so a Job
  that outlasted the Lease could let another claimant take the realm, and a
  proof whose Lease changed hands is recorded `Unknown`. A suspended
  `PtahSchema` claim that serves a post-apply proof is the exception, here and
  in the entry above: the proof keeps its Lease and runs to its end under
  suspension, so a proof read whose Job this release cannot confirm waits for
  that Job by the same bound and gives it its TTL before it reads again.
  [Suspended, and losing a lock](../../reference/reconciliation/#suspended-and-losing-a-lock)
  says what a suspended resource still waits for.
- **An orphaning deletion is refused.** `kubectl delete --cascade=orphan` on a
  `PtahSchema` or a `PtahMigration` left the resource `Terminating`: the garbage
  collector has to take the resource's owner reference off each dependent before
  the orphan finalizer comes off, and the controller-write webhook refuses that
  change to the `PtahResultRecord` objects the resource owns. A new admission
  policy refuses such a deletion instead, `propagationPolicy: Orphan` and
  `orphanDependents: true` alike, and a deletion that names no propagation of a
  resource that carries the `orphan` finalizer, which the API server would
  honor. Its message names `Background`, the default propagation. It runs in the
  API server, so it refuses no deletion because the manager is down, and the
  namespace controller, which names `Background`, is not refused. A resource an
  orphaning deletion left `Terminating` before the upgrade stays so until it is
  deleted again with `kubectl delete`, or its namespace is deleted, either of
  which takes the `orphan` finalizer off. The dependents that deletion already
  freed stay behind, as
  [Suspension and deletion](../../use/operations/#suspension-and-deletion) says.
- **A lost executor proves no stop on a live schema either.** The kubelet gives
  an executor it has no status for the reason `ContainerStatusUnknown`: one that
  never started, when the Job's deadline stops its Pod, and one the container
  runtime stopped reporting while it ran. A live `PtahSchema` took that for a
  stop: it recorded the outcome as unknown and read the database at once, while
  the container could still be running. It now reads the database only once the
  Apply's execution deadline and its grace period have passed, as a deleted
  schema does. With `resultDelivery.enabled=false`, neither family asks for such
  an executor's log, which the kubelet refuses; the log is taken as gone, so no
  pass waits for a frame to arrive in it. Neither family points at the Pod log of
  a Job whose every executor ended that way, since `kubectl logs` cannot read
  one, even where a delivered result says what Ptah wrote there. The live
  endings for a missing Job and for inputs that changed while the Job ran leave
  out the last report when a Pod of the Job shows its executor started, and a
  deleted schema whose claim never recorded its Job's UID reads that Job's Pods
  when its Lease changes hands. A deleted `PtahSchema` that waits on a Pod stuck
  terminating on a node that is gone said nothing about it. A
  `PodStuckTerminating` Warning Event now names the Pod, its node and the forced
  delete that ends the wait once the Pod's own grace period has run out,
  whether the deletion waits for its claim's Job or for the Pod of an Apply that
  has already ended, and
  [Suspension and deletion](../../use/operations/#suspension-and-deletion) says
  when to run it.
- **A refused cleanup of a harvested Job no longer stops the resource.** A
  claim asks for its finished Job's cleanup TTL as it harvests the Job: before
  it consumes the result or records a migration run, retries, or is discarded
  because its inputs changed. A failed or uncertain Apply asks for the TTL its
  log is kept by, 24 hours or the five-minute fallback, before its run or its
  unknown outcome is recorded. A refusal of that write, Forbidden from the
  controller-write webhook or Invalid from a ValidatingAdmissionPolicy such as
  one of your own or the guard of another manager version during an upgrade,
  failed every pass. The result was never consumed, a migration Apply's run was
  never recorded and its Lease stayed held, and the resource did not move
  again. Such a refusal now lets the pass go on, and a
  `SchemaJobCleanupDeferred` or `MigrationJobCleanupDeferred` Warning names the
  Job, which stays until it is deleted by hand or with its owner. Admission
  reads the claim as stored, so the pass goes on only where the stored claim
  still names the Job. Where another manager has moved the claim, the pass
  fails and the next one reads the claim again. A TTL write that fails for any
  reason but a refusal still fails the pass. Under a policy that refuses the
  manager's TTL patch, every harvested Job now stays instead, about 430 a day
  for each resource at the default 10-minute interval, until a
  `count/jobs.batch` quota refuses the next Job. Admit the patch. A schema pass
  that retries a result it cannot take asks for the TTL once, where it asked a
  second time and named the Job in a second Warning. A run or an unknown
  outcome recorded under such a refusal points at its Pod log until the Job is
  deleted, rather than at no log, since the log stays longest then.
- **A read is not waited for past its Job's stop bound.** A read Job whose Pod
  sits on a node that is gone carries only `FailureTarget` and never finishes. A
  deleted `PtahSchema` waited for such a Plan, or for the read that proves an
  Apply, with no bound and renewed the database Lease all along, so neither the
  resource nor the realm was let go. It now waits no longer than the Job's
  deadline, the grace of its Pods and 30 seconds, and then discards the claim,
  records the proof `Unknown` and goes. A Plan hands the Lease back without
  renewing it past the bound. A proof's Lease is the Apply's: deletion renews
  it once more and hands it back on the same pass. A live read in either kind
  that supervised a running Job, or waited for the Pods of a recorded Job that
  is gone, had no bound either; it now runs again under a new Job at that
  bound, whether or not the Job controller marked the Job `FailureTarget`. A
  Job controller that is down marks nothing, and a Plan's Lease was renewed for
  as long as it stayed down. A Plan whose database lock epoch moved checks the
  bound before it asks for the Lease, and once it waits no longer it is
  discarded on that pass, where it waited first for a Lease another resource
  had taken meanwhile. Each records a `ReadJobOverdue` Warning Event that names
  the Job, the Pod that may still read, its node and the forced delete. Nothing
  uses what such a Pod reads, and
  [What a read does to the database](../../reference/execution/#what-a-read-does-to-the-database)
  says what it can still hold. An Apply is still waited for until its Job
  stops.
- **A retried Plan runs again.** A Plan that failed handed its database Lease
  back and moved to its next attempt, as one past its Job's stop bound now does
  too. When that attempt took the Lease again, the operator read the new epoch
  as lost continuity: it recorded a `LeaseContinuityLost` Warning Event and
  `Ready=False` with that reason, cleared `status.plan` and started over with an
  Observe. The next attempt now takes the epoch as its own and runs the Plan
  again. A proof keeps the Apply's Lease across its attempts and was not
  affected, and a claim whose Lease went while its Job could run still reports
  the loss.
- **Every wait on a stuck Apply Pod names it.** A Pod on a node that is gone
  keeps an Apply's Job from stopping. A deleted `PtahMigration` waiting for its
  Apply, a live `PtahSchema` or `PtahMigration` supervising its own, a live
  `PtahSchema` waiting to prove an Apply that ended, and a live or deleted
  `PtahMigration` that keeps its claim after recording its Apply as an
  `Unknown` run, waited for such a Pod in silence and renewed the database
  Lease all along. All of them now record the
  `PodStuckTerminating` Warning Event a deleted `PtahSchema` records, with the
  same backoff, once the Job is past its deadline, the grace of its Pods and 30
  seconds. A live resource's message starts `The Apply waits for`, and a
  proof's `The Apply's proof waits for`. A deletion, or a migration holding a
  run it recorded `Unknown`, whose Job was deleted by hand or replaced by
  another Job under its name, and a schema waiting for an Apply that ended
  still find the Pods by the Job's UID and name a Pod once it
  is past its own deadline or deletion grace and 30 seconds. The message names
  the Job's UID beside its name, since another Job can hold the name by then,
  and each wait keeps its own backoff, so a deletion names a Pod at once
  although its live Apply named it a moment before. The waits themselves are
  unchanged but one: a deleted `PtahSchema` whose claim never recorded its
  Job's UID, and whose Lease changed hands, now records the UID of the Job
  whose Pods it listed, and waits for those Pods as it waits for a recorded
  Job's.
- **A migration keeps its database while a gone Pod may still write.** A
  `PtahMigration` Apply whose Job had finished and whose Pod object was gone,
  deleted with `--force` or with its Job, handed the database Lease back at
  once, although its container could still be running on a node the API server
  cannot reach. It now keeps the Lease, renewing it, until the end of the term
  the Apply took it for: the claim's `startedAt` plus `leaseDurationSeconds`,
  which is `spec.execution.activeDeadlineSeconds` and 2 minutes. That holds for
  a deleted resource waiting for its Apply, for a live one recording its run
  `Unknown`, and for an Apply whose recorded Job is gone or whose name another
  Job holds. An `ApplyPodGone` Warning Event names the Job, its UID and when
  the wait ends, with the backoff `PodStuckTerminating` has, kept for each
  wait, so a deletion says so at once although its live Apply said so a moment
  before. A Job that finished with no
  Pod counted failed, as one whose Pod the API server refused, had no Pod to
  lose and hands the Lease back at once, as before. A `PtahSchema` already held
  the Lease there, until its pending observation's `observeAfter`.
- **A Pod PodGC marked Failed no longer frees the database.** PodGC marks a Pod
  `Failed` when its Node object is gone, or when it is terminating on a node
  tainted `node.kubernetes.io/out-of-service`, keeping the executor status the
  kubelet last reported, and force-deletes it. Where the delete failed, or
  another finalizer held the Pod, the Pod stood `Failed` with its executor still
  reading running, and a `PtahMigration` Apply read the phase alone and handed
  the database Lease back while the executor could still be running on that
  machine. A `Failed` Pod whose executor reads
  running, or one with PodGC's `DisruptionTarget` condition, reason
  `DeletionByPodGC`, whose executor never read terminated, now counts as gone:
  the Apply keeps the Lease until the end of its term, and the `ApplyPodGone`
  Warning names the Pod. A Pod the kubelet ended, by an eviction, a deadline or
  an `OutOfcpu` refusal, or whose executor reads `ContainerStatusUnknown`, still
  hands the Lease back at once. A `PtahSchema` already held the Lease there.
- **A deleted schema says why it waits after its Apply Pod is gone or
  stopped.** A `PtahSchema` deleted while its Apply ran keeps the database
  Lease until its pending observation's `observeAfter`, the Apply's execution
  deadline and its Pod's termination grace, since a container on a node the
  API server cannot reach can outlive its Pod object. Once no Pod of the Job
  was left, as after a forced delete, or once the Pods left had all stopped, as
  one PodGC marked `Failed` whose delete did not succeed, it stayed
  `Terminating` with no word about it. An `ApplyPodGone` Warning Event now
  names the Job, its UID, when the wait ends and a Pod that still stands, with
  its phase. It repeats with the backoff `PodStuckTerminating` has. The wait
  itself is unchanged.
- **A Pod log is pointed at only where the Pod shows an executor ran.** A Pod
  that ended with no status for its executor, as one the kubelet refused at
  admission, had no log for `kubectl logs` to read, yet the record of its Apply
  said `Anything the run wrote is in the Pod log`. So had one PodGC marked
  `Failed` after its node went, over an executor the kubelet last reported
  waiting. Neither family points at a log there now. A schema Apply whose Pod
  started after the last look and then failed without a result no longer
  repeats the report that the Pod could not start, and neither does a
  `PtahMigration` Apply whose result was not read, nor a schema Apply that a
  changed execution component ended. A read still does.
- **A schema waiting for the database lock says so.** A `PtahSchema` Apply, a
  Plan or the Observe that proves an Apply that found another operation holding
  the realm's Lease asked for it every five seconds, and its `Ready` went on
  reading `OperationInProgress`, as it does over a Job at work, with no Event.
  `Ready=False` now reads reason `WaitingForDatabaseLock` and names the Lease
  while the claim waits, as a `PtahMigration`'s `Progressing` does, and names
  the Job where the claim started one before it lost the Lease, since an
  Apply's SQL may still be running then. A read that proves an Apply names the
  Apply's Job until it starts one of its own. A read tried again after a
  failure says so in place of the failure, and `Ready` reads the failure again
  once the read holds the Lease. A report that the claim's Pod cannot start
  stays on `Ready` instead. The wait itself is unchanged.
- **A stop says when the annotation already settles it.** A statement in the
  `operator.ptah.run/settled-apply` annotation holds until the proof ends, so a
  store that flips from the first target to another and back, A, B, A, B, stops
  on B a second time with the value already written, and the next read gives the
  proof up with no new write. That stop printed the command as if a person still
  had to run it. It now says `the annotation already holds this value, so the
  next read of this target gives the proof up`, and never together with the
  clause for another value.
  [A schema whose target moved](../../use/recovery/#a-schema-whose-target-moved)
  says how to take the statement back.
- **A log on a node that is gone names no command.** A Pod PodGC marked
  `Failed` after its node went, over an executor the kubelet last reported
  running, shows a run, and the record of its Apply still pointed at the log
  with a `kubectl logs` command. The API server answers that command with
  NotFound for the node, since no kubelet is left to serve the log. Where the
  Pod carries PodGC's `DisruptionTarget` condition, reason `DeletionByPodGC`,
  both families now say instead that the log was on a node that is gone, so it
  can be read only from that node or from a log shipper. That holds for the
  `Applying` condition and the `ApplyOutcomeUnknown` and `RetiredApplyLogKept`
  Events of a `PtahSchema`, and for `status.lastRun.message` and the
  `MigrationRunUncertain` Event of a `PtahMigration`, whatever the Job's TTL. A
  script that matches `is in the Pod log` there has to match `was in the Pod
  log of Job` as well.
- **A refused row file is named.** Ptah reads a row file that a `data` block
  names only inside its working directory, `/work`, and a schema from a
  ConfigMap or Git source arrives as one file under `/source`, so its row files
  are refused wherever they sit. The Observe failed with `child_exit: ptah exited
  with code 2` and the Plan with `invalid_plan_output`, and neither said which
  file. Both now fail with `desired_source_unreadable` and Ptah's sentence
  behind the schema's file name, such as `schema.hcl: managed data file
  "regions.yaml" for table public.regions: "/source/schema/regions.yaml" is
  outside allowed root "/work"`. The runner copies that sentence only whole and
  in the form Ptah's guard writes it, which holds nothing the database answered;
  any other failure keeps the code it had
  ([#939](https://github.com/stokaro/ptah-operator/issues/939)). Row files work
  only from an artifact that `ptah schema push` built, which carries its rows
  and reads no row file
  ([#1044](https://github.com/stokaro/ptah-operator/issues/1044)).
- **Credentials reach the Pod as files.** Every operation Pod read its database
  URL, and a registry or Git credential, from a `secretKeyRef` in its container's
  environment, and the container runtime keeps a container's environment on the
  node until the Pod is removed, which for a failed Apply is a day. Each
  credential is now a file in a read-only Secret volume mounted in the one
  container that needs it, mode `0440` for the Pod's `fsGroup`, and the
  container's environment names the file. The runner reads it and gives the value
  to Ptah's environment alone, since Ptah `v0.23.0` takes a database URL and a
  registry or Git credential from nowhere else, and the OCI fetch init container
  now runs Ptah's pull through the runner for the same reason. The kubelet
  unmounts the volumes once the Pod's containers stop. The registry and Git
  grants stay `secretKeyRef` variables, since they hold no credential. The Job
  admission guard refuses a credential variable on a Job the manager creates,
  and still admits the cleanup TTL of a Job 0.2.10 created.
  [Credentials are files](../../reference/credentials-and-admission/#credential-files)
  has the detail.
- **`kubectl ptah install`.** The plugin installs the chart a release published
  to `oci://ghcr.io/stokaro/charts/ptah-operator` with Helm 4 and no image
  value, by default at the plugin's own version, and waits for the release to
  be ready. `--yaml` prints what it would apply instead, and `--out-dir` writes
  it to files. [Install](../../start/install/#choose-a-version-once) now opens
  with it. A release before v0.2.10 has no chart there, and the plugin says so.
- **Approvers at install.** The new chart value `approverClusterRole.subjects`
  binds the approver ClusterRole in every namespace, and `kubectl ptah install
  --approver` fills it. It is empty by default, so an installation that does not
  set it binds nobody, as before. A chart from v0.2.10 or earlier refuses the
  value.
- **Read-only Jobs are created with their cleanup TTL.** A read-only Job got its
  TTL only from a claim that harvested it, and admission admits that TTL only on
  a finished Job the active claim names by UID. A claim that let go of a read
  without harvesting it, under suspension, after a refusal or a retirement, had
  to wait for the Job or leave it behind, and the entries above are the cases
  that left Jobs behind. Every read-only Job is now created with the five-minute
  TTL a harvested one got. Kubernetes counts it from when the Job finishes, so
  it removes nothing still running, and removes a finished read whether or not
  the manager has read it. A read whose Job is gone before the manager read it
  runs again under a new Job. A claim that lets go of such a Job writes nothing
  to it and names it in no Warning: a suspended or refused read-only claim moves
  on at once, a retired read-only claim that holds no database lock is retired
  at once, and a `PtahSchema` Plan that cannot confirm its Job runs again
  without waiting for that Job to stop. A Plan or a proof read that a binding
  change retires, and a Plan whose database lock epoch moved, still wait for
  their Job by its stop bound, since the database Lease they hold depends on the
  Job having stopped. An Apply's Job is still created with no TTL and gets the
  one its outcome decides. The waits, TTL writes and Warnings the entries above
  describe for read-only Jobs serve the read-only Jobs 0.2.10 created, which
  carry no TTL. A `ReadJobOverdue` Warning on a read that runs again now says
  what becomes of its Job: it `is deleted 300 seconds after it finishes`, or,
  for a Job 0.2.10 created, it `stays until it is deleted by hand or with its
  owner`.
- **An approval's audit event names its stamp.** The webhooks that stamp a
  `PtahSchemaApproval`, a `PtahMigrationApproval` and a
  `PtahMigrationRunAcknowledgment` answer with the `mutationRequestUID` and the
  username they stamped as audit annotations. The API server records them on
  the create's audit event at `Metadata` and above, under the webhook's name,
  such as `mapproval.operator.ptah.run/mutationRequestUID`. At `Metadata` an
  approval met its audit event only by namespace, name and time.
  [Joining one change across the record](../../use/audit/#joins) lists the keys.
- **The journal in the manager's log.** After a status write that adds or
  changes a journal entry is stored, the manager writes the entry as one JSON
  line on its standard output, with the resource's kind, namespace, name and
  UID, read from the object the write returned. A folded failure writes a
  line each time its count moves. The lines never hold up a status write: a
  line that finds the queue full, or whose write fails, is dropped and counted
  in `ptah_operator_journal_lines_dropped_total`. A collector that keeps them
  keeps the journal after the resource and the cluster are gone, and whoever
  controls the manager's output can drop or forge them.
  [The journal in the manager's log](../../use/audit/#journal-lines) says how to
  read them and when one can be missing.
- **A schema's Apply names what ran and where it stopped.** A schema's
  `ApplyFinished` entry held the outcome, the plan and when the Apply started,
  so it could not be joined to the approval it consumed, the Job that ran it or
  the reading that proved it, and said nothing of where a refused Apply
  stopped. It now carries the claim's `operationID`, the plan's UID and
  fingerprint, the approval's name and UID and its approver, and the Job's name
  and UID once the API server confirmed the Job, as a migration's `RunFinished`
  does, and the `Verified` entry after it carries the same `operationID`.
  Where the run names the statement it stopped at, `applyFailure` on the entry
  and on `status.pendingObservation` records its position and the SQLSTATE and
  MySQL error number the database answered with, never the database's
  message. Ptah v0.23.0 reports such an Apply as `unknown`, and the entry still
  reads `Unknown`.
  `kubectl ptah history` prints the new fields and joins a proof to its Apply
  by the operation where both name one. Every field is optional, so a stored
  object validates as before, and an entry an earlier release wrote reads as it
  did. [The lifecycle journal](../../reference/reconciliation/#the-lifecycle-journal)
  has the rest.

## 0.2.10

**Before you upgrade.** The manager image is new, so keep `--force-conflicts`
on the upgrade. This operator needs Ptah v0.23.0 or later as its executor: it
fetches an OCI schema with `--skip-parse`, which an older executor does not know,
so every OCI Observe and Plan would fail at `fetch-schema`. A MySQL account that holds only `SELECT`, `CREATE`, `ALTER`,
`DROP`, `INDEX` and `REFERENCES` on its database, the login these pages
documented until now, fails every Observe and Plan once the executor moves to
Ptah v0.23.0, so grant it `TRIGGER`, `EXECUTE` and `SHOW VIEW` on the database
first, and `SHOW_ROUTINE` globally where the database holds a routine another
account defined ([MySQL authority](../databases/#mysql-authority)). An alert or
a script that matches `child_exit: ptah exited with code 2` to find a database
the operator could not open has to match the kind of failure instead, and one
that matches it or `invalid_plan_output` to find a desired file Ptah could not
read has to match
`desired_source_unreadable`, or `desired_schema_invalid` for a schema it read and
refuses. A pipeline that ships Pod logs now receives what the
database answered when an Apply fails, row values included, so decide what it may
store first. A plan waiting for approval has to be approved again
once the executor moves to Ptah v0.23.0, a database audit that lists what the
executor sends to PostgreSQL has to expect reads of `pg_catalog` in place of
`information_schema`, and `kubectl ptah` and Ptah Console have to move with the
operator. Every plan this release publishes carries `scope`, which Ptah Console 0.3.2 does not
read yet, so it holds approval for each of them: approve with `kubectl ptah` from this
release until the console release that moves to it
([ptah-console#167](https://github.com/stokaro/ptah-console/issues/167)). Do not use
Ptah Console 0.3.1 with this release, because it shows such a plan as unsupported and
still offers Approve. A script that matches a sentence about a count of migrations in a
status message has to expect the singular for one: `1 migration is pending`, not
`1 migrations are pending`. A script that passes the output of `kubectl ptah approve --dry-run
client` to `kubectl apply` has to pass `--name`, since the printed approval now
carries a `generateName`. A script that matches the whole text
of `result publication retention has not authorized deletion` has to match its
start, and one that matches `immutable operation binding` to find a refused
deletion has to match that text too: a credential record now says it, and that
sentence is left to an update. A script that matches `invalid_verification_output`
to find an artifact the verification policy refused has to match the reason
`PolicyRefused`, and one that waits on a `PtahMigration` so refused has to expect
`Blocked` at its interval rather than a retry. A `PtahMigrationApproval` now goes
`Stale` when a read ends anywhere but its plan or the verification policy refuses
the artifact, and a plan that comes back needs a new one, so a workflow that applies
an approval under a fixed name has to change the name. `ptah_operator_approvals_total`
with `family="migration"` and `outcome="stale"` now counts those approvals, and no
longer a plan discarded with nobody having approved it. A run that spends one
approval of a plan marks the plan's other approvals `Stale` too, and
`kubectl ptah approve migration/... --wait` reads the approval it stored and ends
with `PlanSuperseded` or `OtherApprovalConsumed` once it is stale, where it used to
run to its timeout. `ApprovalRequired` on a migration goes `False` when a reading
publishes no plan, with the reason that reading wrote on `Ready`. A script that reads
`status.target.inventory` has
to expect a kind that is not a stem. Ptah Console up to 0.3.x misstates such a kind until
[ptah-console#166](https://github.com/stokaro/ptah-console/issues/166) is fixed:
it says a dropped extension's counts and findings disagree, and shows a changed
policy at 100% coverage with its finding under other findings. It also titles a
migration's `Verified` entry with the outcome `Blocked` or `Pending` as "Verified"
until it ships a fix, so read a blocked reading from `status.journal` or
`kubectl ptah migration`. `kubectl ptah wait
--for approval` now ends on a plan already waiting, so a script that used it to
wait for a newer reading has to run `kubectl ptah refresh --wait`. `kubectl ptah
wait --for in-sync`, `approval` or `applied`, `approve --wait`, `set-source --wait`
and `resume --wait` now exit 8 with `PolicyRefused` on an artifact the verification
policy refuses, where they ran to the timeout and exited 7. A wait for a migration
run that applied before the refusal exits 8 with `CompletionNotProved` and names the
run, so a script that reads exit status 8 as a plan that did not run has to read the
reason as well.
A migration Apply
now writes the approver's username to the target database, so check what that
means for the retention of its backups and dumps if the name is an email address.
A script that reads `ApprovalAccepted` entries in `status.journal` has to tell
decisions apart by `approvalUID`, since an approval that reuses a name now writes
an entry of its own, and has to treat an entry with no `approvalUID` as evidence
of a name and not of an object.
A script that counts `RunFinished` entries in `status.journal` as runs now gets
one entry per run, on its plan, where a `Partial` or `Unknown` run used to write
a second `Unknown` entry; entries already stored stay as they are until the
journal's bounds drop them.
`status.lastRun` and `status.journal` of a migration now carry the same username, and
the identity admission stamped, so treat them as you treat a schema's `status.plan.approval`.
`kubectl ptah approve migration/... --wait` now waits for the run of the plan it
approved under the approval it stored, and exits 8 where it used to report success
for another run: a script has to expect `PlanSuperseded`, `OtherPlanApplied` and
`OtherApprovalConsumed`. It also exits 8 with `CompletionNotProved` against an operator
that records no plan on a run, so upgrade the operator first.
`kubectl ptah plan` now prints a plan a newer Ptah added a field to and exits 0,
naming the field, where it used to exit 1. A script that takes that exit status to
mean nobody may approve the plan has to ask `kubectl ptah approve`, which still
refuses it, as `plan -o sql` and `plan -o json` do.
A migration's journal now has `Verified` entries, for the first reading after a run,
and a `RunAcknowledged` entry now carries `outcome`, so a script that filters
`status.journal` on `.outcome` alone has to filter on `.type` as well.
`status.lastRun.planRef` is a new pin on a stored plan, so add it to a pruning script
written from an earlier copy of [the guide](../../use/operations/#prune-run): a plan
such a script deletes leaves the field naming nothing, which the retention audit in
`hack/capacity` refuses.
`status.lastRun.targetIdentityDigest` is the database a run's result named, and it is
absent on a run that left no result.
After a manual `kubectl rollout undo` to an earlier manager, a run keeps the attribution
of the run before it, because [its merge patch leaves the fields it does not
know](../../use/operations/#rollback-stop).
An installation with `resultDelivery.enabled=false` that still holds result records
starts losing them at the upgrade, so copy any you want first. Records whose attempt
was retired while delivery was on go as soon as the collector reaches them, once that
retirement is an hour old. The collector retires any other attempt once no resource
pins it, and its records go about an hour later. Either way they also wait for their
Job to be gone. The manager's ClusterRole there gains `get`, `list`, `create` and
`delete` on `ptahresultrecords`, so check that against whatever audits the role.
The move to Ptah v0.23.0 can also change what a running schema reports and plans,
which the paragraphs after the next one cover. Nothing else.

The manager now retires and collects result records whether delivery is on or off,
because a record outlives the setting that wrote it. With delivery off they used to
stay for good, and a namespace holding them stayed `Terminating` until delivery came
back or the release was uninstalled, and an installation with `webhook.existingSecret`
could turn delivery on only by giving up its own webhook trust. The window stays one
hour. A `credential` record now keeps it too: with delivery off it used to be deleted
at once. Admission lets a delivery-off manager create a retirement and no other record,
and the collector adds a metadata list of records every 5 seconds to that installation,
with or without records to find.
[While result delivery is off](../../use/operations/#delivery-off) has what was measured.
A rollback to 0.2.9 with delivery off can leave `retired` records this release wrote,
and 0.2.9 never deletes them: it runs no collector with delivery off, and its webhook
refuses to delete any record but a `credential` one, so a namespace holding one stays
`Terminating`. The collector writes one for every attempt nothing pins on its first
pass after the upgrade, and each stays at least an hour, so roll back only once
`kubectl get ptahresultrecords -A -l operator.ptah.run/result-record=retired` lists
nothing. Where a namespace is already stuck, upgrade to 0.2.10 again, whose collector
removes those records once they are an hour old, or turn delivery on in 0.2.9 as
[that section](../../use/operations/#delivery-off) says, where the 0.2.9 chart allows it.

PostgreSQL 18.x joins the supported release lines, and a cluster on 17.x has
nothing to change for that. Its official image keeps its data under
`/var/lib/postgresql`, not `/var/lib/postgresql/data`, which matters only to a
manifest of your own.

Ptah v0.22.0 compares more than the executor before it, so a database nobody
touched can leave `InSync` on its first read after the upgrade if it differs from
its declaration, as an adopted one or one with a hand change can. It now finds a
column's fractional-seconds precision on PostgreSQL and MySQL
([stokaro/ptah#5405](https://github.com/stokaro/ptah/pull/5405)), an identity or a
`serial` default the declaration does not match
([#5448](https://github.com/stokaro/ptah/pull/5448),
[#5424](https://github.com/stokaro/ptah/pull/5424),
[#5454](https://github.com/stokaro/ptah/pull/5454)), and a schema the artifact
declares and the database lacks
([#5402](https://github.com/stokaro/ptah/pull/5402)). Ptah rates those statements
safe. The operator reads a retyped column and any `DROP` as destructive itself, so
`ALTER COLUMN ... TYPE timestamp(0)`, MySQL `MODIFY COLUMN`, `DROP IDENTITY` and
`DROP DEFAULT` stop at `allowDestructive` and then wait for an approval under every
policy. Read them before you approve: the first two lose a column's fractional
seconds. What only adds is not stopped: under `apply: Always` an identity or a
sequence default the declaration asks for runs with nobody looking, and so does
`CREATE SCHEMA`, which under the documented login fails with `permission denied for
database` and an unknown outcome. Set `apply: OnApproval` until you have read the
first report after the upgrade. `spec.suspend: true` holds a resource too, but it
starts no read, so run `ptah schema drift` at the new digest yourself to see what
it would report.

Ptah v0.23.0 refuses a MySQL read that cannot see every trigger, routine or view
definition in the database, where the executor before it reported what it could
not see as absent ([stokaro/ptah#5545](https://github.com/stokaro/ptah/pull/5545)).
The Observe or the Plan fails with `permission_denied`, and the message says which
kinds of object the account cannot see, with no database or object name. The grants
above end it. A Git or ConfigMap schema in SQL can now move a PostgreSQL partition:
it plans `DETACH PARTITION` and `ATTACH PARTITION` where the executor before it
could not parse `PARTITION OF` or read the database as converged. Ptah rates those
statements a warning. The operator holds a `DETACH` that leaves the partition out of
its parent, or attaches it to another parent, as destructive, because the parent
stops returning those rows; a bound change and a plain `ATTACH` run under
`apply: Always` with nobody approving them. On MySQL, Ptah replaces the index behind a foreign key
with `DROP INDEX ..., ADD ...` in one statement and rates it safe, and the operator
holds it as destructive for its `DROP`, so a plan that adds a unique key or an index
on such a column now asks for `allowDestructive` and an approval. A MySQL migration
whose DDL statement the server refuses now ends `Unknown` rather than `Failed`.
The statement did not commit, but Ptah keeps its in-flight mark on a DDL statement
the server refused, and v0.23.0 reports the run unknown over that mark
([stokaro/ptah#5661](https://github.com/stokaro/ptah/issues/5661)), so the run
leaves `status.unresolvedRun`, the resource reads `Blocked` with reason
`ApplyOutcomeUnknown`, and the retry needs an acknowledgment
([A row in `state=pending`](../../use/recovery/#failed-pending-row)). A later Ptah
release is expected to read it as `Failed`.
A migration run whose Pod is stopped, by a node drain, an eviction or its deadline,
inside a statement that commits on its own also ends `Unknown` rather than `Failed`
or `Partial`, and needs the same acknowledgment: a statement outside a transaction
(`-- +ptah no_transaction`, or `spec.transactionMode: none`) on either engine, or a
DDL statement on MySQL, so a drain during a long MySQL `ALTER TABLE` is one. Ptah
always left an in-flight mark on such a statement, since it cannot tell whether the
server applied it, and reported the run failed over it; v0.23.0 reports it unknown
([stokaro/ptah#5569](https://github.com/stokaro/ptah/pull/5569)). This one is Ptah's
intent, so no later release is expected to change it. A PostgreSQL migration that
runs in a transaction rolls back and still ends `Failed`.
A PostgreSQL migration whose commit fails, such as one a deferred foreign key stops,
reads `Failed` with a message that names the migration and says nothing committed.
A narrowed column type and a dropped identity now carry `column_types_narrowed` and
`column_identities_removed` in `status.target.driftFindings` beside
`columns_modified`, so a script that matches the categories has to expect them.
A Plan that Ptah refuses as `declaration-refused` for a reason the file alone does
not show, such as a foreign key into a schema only the database holds, reads
`plan_refused` where it read `invalid_plan_output`.

- **One command to install.** A release now publishes its chart to
  `oci://ghcr.io/stokaro/charts/ptah-operator` with its images stamped in, signed
  and attested, so `helm upgrade --install ptah-operator
  oci://ghcr.io/stokaro/charts/ptah-operator --version 0.2.10` needs no values.
  [Install](../../start/install/#choose-a-version-once) has the command beside its
  verification. A chart from the source tree and the `.tgz` asset still refuse to
  install without the four values, and they now say so in the chart's own message
  rather than a schema error.
- **A namespace waiting on result retention.** The refusal now says when: it
  names the record and the time it can be deleted, as `record <name> cannot be
  deleted before <time>`, wherever time is the reason, credential records
  included. A refusal for any other reason names no time. The window itself has
  not changed.
  [Deleting a namespace](../../use/operations/#delete-namespace) says how to read
  it, why the wait exists, and how to tear down a test namespace.
- **A database the operation could not open.** When Ptah cannot open the
  target database, the condition message and the Warning Event start with
  the kind of failure: `database_unreachable`, `authentication_failed`,
  `database_not_found`, `permission_denied` or `tls_failed`. A fixed sentence
  follows that says what the kind means and names the Secret and key in
  `spec.target.urlFrom` to check. Before, the same failure read
  `child_exit: ptah exited with code 2`. Nothing Ptah or the database wrote
  reaches status or Events.
  [A database the operation could not open](../../troubleshoot/condition-reasons/#a-database-the-operation-could-not-open)
  lists the kinds and where each stage shows them.
- **A desired file Ptah could not read.** When a desired schema or a migration
  directory does not load, the condition message and the Warning Event carry
  Ptah's reason under the code `desired_source_unreadable`, naming a schema's file
  and a migration's file or version. Before, the same failure read `child_exit: ptah
  exited with code 2`, or `invalid_plan_output` from a Plan. The reason comes
  from a second run of Ptah with no database URL and no credential, and it can
  quote a fragment of the file, which status readers then see:
  [Output handling](../../use/security/#output-handling) says who that reaches.
  [A desired file Ptah could not read](../../troubleshoot/condition-reasons/#a-desired-file-ptah-could-not-read)
  says how to read the reason and how to make the resource read the repaired
  file. An OCI schema the executor cannot parse reads the same way: the
  `fetch-schema` step checks the artifact and leaves the schema unparsed, and
  the operation container names the fault. What that step refuses itself, such
  as a layer of another media type, names the step and its exit status and gives
  no reason: that page says how to get one.
- **A schema Ptah read and refuses.** When drift or a Plan stops on a fault of the
  schema as a whole, such as a foreign key into a table it does not declare, the
  condition message and the Warning Event carry that fault under the code
  `desired_schema_invalid`. Such a schema used to read as the two codes above, with
  no reason. The words come from the same second run of Ptah. A fault that run finds
  and the failed command did not stop on keeps the code it had, and
  [the same page](../../troubleshoot/condition-reasons/#a-desired-file-ptah-could-not-read)
  lists those.
- **A read that keeps failing reads the repaired source.** A read-only operation
  that keeps failing used to hold its snapshot, commit or digest until a spec edit
  or a suspension ended it, and `kubectl ptah refresh` got no answer meanwhile. It
  now [gives way](../../reference/reconciliation/#a-read-that-keeps-failing) to an
  edited ConfigMap or source policy at once, and to a read request or a passed
  `spec.interval` when its retry delay ends. An Apply and the read that proves it
  keep their binding. A dashboard that reads `status.activeOperation.attempt`
  sees it restart from 1.
- **A schema whose target moved after approval.** A login, host, port, database or
  URL parameter that changes in the URL Secret after a plan was approved, or while
  its Apply ran, used to leave the resource `Failed` for good, retrying `post-apply
  observation target does not match the applied plan`. It now stops `Blocked` with
  reason `TargetIdentityChanged` and says how to end it: put the first URL back, or
  run the command it prints, which writes the `operator.ptah.run/settled-apply`
  annotation. Under `apply: Always` the next plan then applies with no approval
  unless it is destructive or changes privileges, and `patch` on the resource is
  enough to do this.
  [A schema whose target moved](../../use/recovery/#a-schema-whose-target-moved) says
  what settling means. An alert or a script that matches that sentence has to match
  the reason instead. A `Verified` journal entry can now read `Unknown`.
- **`DatabaseReachable` says which kind it was.** `DatabaseReachable` is `False`
  when an operation's result names a kind, with a reason for each:
  `DatabaseUnreachable`, `AuthenticationFailed`, `DatabaseNotFound`,
  `PermissionDenied` or `TLSFailed`. `PtahMigration` now publishes the condition
  too. A successful Plan or Apply sets `True` as an Observe does, so a Plan
  retried after the database came back does not leave it `False` for a whole
  `spec.interval`, and so does a Plan that failed after a read came back from the
  target. It means the target answered the last time an operation opened it, and a
  failure with no kind leaves it as it was. An alert can key on the
  reason instead of the start of the message. `ReconciliationFailed`, `Ready` and
  `Progressing` keep their reasons. After a rollback, an earlier manager leaves a
  migration's `DatabaseReachable` as this one last wrote it, and reads nothing
  from it.
- **An Apply that could not open its database says so in the Event.** The
  `ApplyOutcomeUnknown` Event carries the sentence for the kind when the Apply's
  result named one; any other Apply error keeps the fixed text. The outcome, the
  unresolved count and `PtahOperatorUnresolvedApply` do not change: a database
  that goes away between the Plan and the Apply keeps paging until it answers and
  the read-only proof completes. `DatabaseReachable=False` says the target cannot
  be opened now, not that nothing ran; only the completed proof settles the record.
- **A statement the database refused.** When an Apply fails, the operation Pod's
  log holds one line with what the database said, and the `Applying` condition of
  a schema, `status.lastRun.message` of a migration and the Warning Event say how
  to read it. That line can carry a role, a host, a database name and values read
  from a row, and it stays out of status, Events and the result frame. Text the failed
  statement also holds is replaced with `[in the statement]`.
  [What a failed Apply leaves in its Pod log](../../use/operations/#failed-apply-log)
  says who can read it. The operator's handling of the failure does not change.
- **A failed Apply a person approved keeps its Job 24 hours, and so does a migration
  run that stops its resource.** The Pod log stays for 24 hours after the Job finishes,
  in both families, when a person approved the Apply, and under `apply: Always` when a
  migration run stops the resource: it reported `Partial` or `Unknown`, or nothing, or
  it failed at a statement and left a dirty revision row, because no other Apply runs
  until the run is settled or a person repairs the row. Every other finished Job keeps
  five minutes. That includes a schema Apply under `Always`, whose proof can lead to the
  next Apply, and a migration run under it that reported `Failed` and left no row, as
  one that stopped before it selected a migration does, because that run comes around
  again.
  [Which run keeps what](../../use/operations/#failed-apply-log) says why. A retained
  Job counts toward a `count/jobs.batch` quota, and one Kubernetes marked `Failed`
  keeps a `KubeJobFailed` alert firing until it is gone. A Job the previous release
  harvested keeps the TTL it has. While an upgrade leaves the previous release's
  webhook or guard answering, a run it refuses the 24 hours keeps five minutes, and a
  `FailedApplyLogShortened` Warning Event says so. After a rollback a Job that holds 24
  hours is collected on that time with nothing further from the manager.
- **A migration run that never started.** A migration Apply that Ptah stopped
  before it selected any migration, as on a database it could not open, is
  now `Failed`. After `spec.execution.failureRetryInterval` the resource reads
  its history and plans again. Under `apply: Always` the new plan runs; under
  `OnApproval` it waits for an approval, because the run consumed the one it
  carried. Before, it was `Unknown` and stopped the migration until a person
  accounted for the run.
- **Who approved a migration.** A `PtahMigration` Apply now names its approver
  in the database's own `schema_migrations_log`, so an audit can read who approved
  a run after the approval or the cluster is gone. Before, `actor` was empty and
  `actor_source` read `unknown`. The source now reads `provided`, Ptah's word for
  a name it took on trust. A name outside printable ASCII is percent-encoded so
  that every character set can store it, and so are `%` and `$`: `oidc:José
  García` reads `oidc:Jos%C3%A9 Garc%C3%ADa`, and a percent decoder that leaves
  `+` alone reads it back. Under `apply: Always` the name is
  `system:ptah-operator:apply-always:<namespace>/<name>`, and admission now
  refuses a migration approval from a username under `system:ptah-operator:`. The
  name is personal data; [the security
  model](../../use/security/#approver-in-the-database) says what it is worth and
  when a row is lost. A `PtahSchema` has no such log, and
  [What survives a lost cluster](../../use/recovery/#what-survives-a-lost-cluster)
  lists what each resource leaves. Rows already written stay as they are.
- **A run dispatched across this upgrade.** The executor moves in this release, so
  the execution binding changes under a dispatched Apply. It is recorded `Unknown`
  and blocks the resource until a History reading or an acknowledgment settles it,
  and its rows carry no name.
- **A login that owns its tables, and a row file outside the schema.** Earlier
  executors planned to revoke a login's own privileges on a table it owns when the
  desired schema declared that login, and an approved Apply left it with
  `permission denied` on its own table
  ([stokaro/ptah#5508](https://github.com/stokaro/ptah/issues/5508)). Ptah v0.23.0
  leaves what an owner holds out of the comparison, so the workaround of leaving
  the login role out of the desired schema can go. It gives nothing back: on a
  database where such an Apply ran, the owner's missing privileges read `InSync`
  and the next plan is empty. Run, as the login, a `GRANT` for each `REVOKE` that
  Apply ran, such as `GRANT ALL ON TABLE t TO shop`; the plan after it stays empty.
  `schema drift` also read a row file that a `data` block names outside the
  schema's directory. Ptah v0.23.0 refuses that file in drift and plan alike
  ([stokaro/ptah#5560](https://github.com/stokaro/ptah/pull/5560)), and the Observe
  fails with `child_exit` and the Plan with `invalid_plan_output`, not
  `desired_source_unreadable`, with no reason that names the file;
  [#939](https://github.com/stokaro/ptah-operator/issues/939) tracks naming it.
- **A withdrawn migration approval.** Deleting or replacing a
  `PtahMigrationApproval` after the Apply is claimed and before its Job exists now
  retires the claim, and no Job runs. Before, the run went ahead under the
  withdrawn approval. Once the Job exists, deleting the approval changes nothing
  about the run.
- **Executor.** The verified executor is now Ptah v0.23.0. A new executor
  retires the execution binding, as in 0.2.9: approve again whatever was
  waiting, after the resource has read its database with v0.23.0 and planned.
  Applied state is not touched. Ptah v0.18.0 names its command in every document
  and moves the plan, apply and migration document versions; this operator
  reads them and refuses the old ones, so the operator and the executor move
  together. On PostgreSQL, drift now reads every schema the URL covers, as
  planning already did: with a URL that pins no `search_path`, the tables of a
  schema the desired schema declares nothing in count as drift, the same
  tables a plan already proposed to drop. Plans are written in Ptah's exact
  statement style ([stokaro/ptah#5341](https://github.com/stokaro/ptah/pull/5341)):
  most comment lines in front of a statement are gone, though a `-- POSTGRES: ...`
  warning stays where a statement can fail on the rows it meets, and Ptah v0.23.0
  adds one before a `NOT NULL` `ADD COLUMN` with no default
  ([stokaro/ptah#5526](https://github.com/stokaro/ptah/pull/5526)). A drop reads `DROP TABLE
  "name"` where it read `DROP TABLE IF EXISTS "name" CASCADE`, so `kubectl ptah plan
  -o sql` and a database audit show different text. Ptah now rates a view rebuilt for
  a changed column list `warning`. The operator still reads its `DROP VIEW` as
  destructive, so that plan is held as it was.
- **A drop stops at what depends on it.** A table or column with a dependent the
  plan does not name, such as a view or a foreign key in a schema the URL leaves out
  or an object under `spec.policy.exclude`, used to go, and `CASCADE` took the view
  or the key with it without a word. That Apply now fails with `cannot drop table
  parent because other objects depend on it` (`2BP01`), an unknown outcome, and
  nothing dropped. The next plan holds the same statement, so approving it again
  fails the same way. Change or remove the dependent first, or keep the table or
  column in the declaration.
- **A view with a security option the artifact does not state.** A view holding
  `security_invoker=true` or `security_barrier=true` that the artifact leaves out
  read as converged. Now an Observe reports `views_modified`, and the Plan stops with
  `plan_refused: ptah refused to plan the change (view-security-option)`, because the plan
  that removed the option would widen what readers see
  ([stokaro/ptah#5361](https://github.com/stokaro/ptah/pull/5361)). The resource does
  not converge until you state the option in an artifact pushed with a v0.22.0 CLI,
  or list the view in `spec.policy.exclude`.
  [A Plan that stops without saying why](../../troubleshoot/condition-reasons/#a-plan-that-stops-without-saying-why)
  has what to do.
- **A plan's scope.** Since v0.19.0 Ptah records in each plan what the URL
  covered, and this release reads that field. `kubectl ptah` and any Ptah Console
  built on an earlier operator refuse such a plan as unsupported, so move them
  with the operator.
- **A table the login cannot read.** The executor reads a PostgreSQL schema
  through `pg_catalog` for every role, so
  [the login these pages describe](../databases/#what-the-login-cannot-see) now
  reports a table another role added, and its primary key, as a superuser would.
  A schema that read `InSync` because its login could not see such a table reports
  it after its next read, though the database has not changed, and the plan drops
  it. That plan is destructive, so it stays blocked unless `allowDestructive` is
  set, and until someone acts every later release of that resource plans the same
  `DROP` beside its own change and stops with it. With `allowDestructive` and an
  approval the Apply fails with `must be owner of table`, unless the login owns the
  database or the schema: then the table and its rows are gone, whatever privilege
  the login holds on the table. For a table that should stay, such as a monitoring
  agent's, list it in `spec.policy.exclude`. A table the desired schema declares no
  longer reads as missing when the login holds nothing on it. A MySQL read that
  cannot see every object is refused rather than short, as the paragraphs above
  say.
- **Extensions and policies in the inventory.** `status.target.inventory` now
  counts what a feature owner compares, which Ptah v0.18.0 reports. An entry
  `ptah.run/pgextension/extension-change` with `desired: 1` and `observed: 0`
  says the database holds none of the one extension the schema declares. A kind
  like this joins `feature_changes:<kind>` and `feature_access_*:<kind>`, where
  a stem such as `tables` joins `tables_added` and `tables_removed`.
  [Operations](../../use/operations/) shows one. The kind may now be 128 bytes
  long, so the CRD schema version moves, and
  [a rollback](../../use/operations/#rollback) to an earlier version is refused
  once this one has updated the CRDs.
- **`PlanStale` means a plan was replaced.** The Warning fired on every scheduled
  read of a waiting plan and after every Apply, though the plan had not changed.
  It now fires once, when another plan takes the place of one a person could
  approve or had approved, and its message names both. A plan discarded before it
  was published, because the database target moved during the read, records
  `PlanTargetChanged` instead. A script that counts `PlanStale` to see reads has to
  read `status.target.lastObservedAt`. An Event can be lost, so the `PlanPublished`
  entry in `status.journal` now records `replaced` and `approvalRequired`, which
  moves the CRD schema version. That entry and the newest approval of a plan nothing
  has carried out stay past the journal's bounds, so a script that reads the journal
  can meet an entry older than `coveredSince`. `ApprovalRequired` reads `False` with
  reason `NoChanges` once the database converges on its own, where it stayed `True`.
- **A Secret that does not exist yet.** An operation whose Pod the kubelet cannot
  configure reported only that it was in progress, and past the Job's deadline read
  `ptah runner result frame not found`. Its condition now reads
  `PodConfigurationError` and carries the kubelet's words, such as `secret
  "application-database" not found`, with a Warning Event of the same name, and the
  failure past the deadline carries them as what the resource last reported. A Secret
  that arrives still lets the operation go on by itself. An external secret store that has not synced yet looks
  like this: [Before the Secret exists](../../reference/credentials-and-admission/#external-secret-stores-not-synced).
  A script that reads `Ready` or `Progressing` while a Secret is missing sees the new
  reason, and on a `PtahMigration` `Progressing` is `False` meanwhile.
- **A migration Apply that lost the lock.** It waited for all the time the Lease
  had left, about five minutes, though the holder usually finished in seconds, and
  it said nothing while it did. It now asks every five seconds, as a schema always
  has, and `Progressing` reads reason `WaitingForDatabaseLock` and names the Lease
  while it waits, and the Job when the Apply had started one before it lost the Lease.

## 0.2.9

**Before you upgrade.** The manager image is new, so keep `--force-conflicts`
on the upgrade. A script that reads the text of `kubectl ptah` has to move to
`-o json`. A plan waiting for approval has to be approved again once the
executor moves to Ptah v0.17.0, and a database audit that lists what the
executor sends to PostgreSQL has to expect one more read. Nothing else.

- **Executor.** The verified executor is now Ptah v0.17.0. A new executor
  retires the execution binding, as in 0.2.8: approve again whatever was
  waiting, after the resource has read its database with v0.17.0 and planned.
  Applied state is not touched. v0.17.0 adds no drift category, and the plan,
  apply, drift and migration documents the operator reads keep their shape.
  A plan no longer carries the `-- POSTGRES TABLE`, `-- MYSQL TABLE` and
  `-- ALTER statements: --` comment lines, so `kubectl ptah plan -o sql` shows
  only the comments that describe a change. Every operation that connects to
  PostgreSQL now also reads the session's search path when it connects;
  Resolve, Verify and the artifact fetch connect to no database.
- **Pushing a pinned reference.** Ptah v0.17.0 is also the first Ptah release
  whose `ptah schema push` and `ptah migrations push` take
  `--format reference`, which prints the pinned reference
  `kubectl ptah set-source` takes, and `--format json`. That is the Ptah CLI
  you build artifacts with, not the executor the operator runs, so upgrading
  the operator does not bring it: install Ptah v0.17.0 where you push.
- **`kubectl ptah` output.** The text `kubectl ptah schema`, `migration` and
  `plan` print is for people, and its layout changed: wider label columns, the
  conditions as a table with their messages, approvals and acknowledgments,
  and a severity line above each plan statement. `-o json` kept every field it
  had and only gained new ones, and `kubectl ptah plan -o sql` is unchanged.
  [Read a plan](../../use/read-a-plan/) shows the new output.
- **`kubectl ptah approve`.** `kubectl ptah approve schema/NAME` or
  `migration/NAME` shows the current plan with the identities an approval
  binds, asks for a decision, and creates the approval as you, so approving
  no longer means writing a manifest by hand. It reads the resource again
  before it sends anything and approves nothing if the plan changed, or the
  spec changed other than by a suspend or a resume. An answer given while the
  operator reads the resource again at its interval is sent when that read
  ends, if the same plan is waiting again; approve waits up to five minutes
  for that, and `--timeout` bounds each request rather than the wait.
  `--plan-fingerprint` approves from a script and `--dry-run client` prints the
  manifest instead, which the person who decided creates: a GitOps controller
  that applies it is recorded as the approver. It needs `create` on the
  approval kind besides the plan reads, which the chart's approver
  ClusterRole carries.
  [Exact-plan approvals](../../use/approvals/) shows it.
- **`kubectl ptah suspend`, `resume`, `refresh` and `wait`.** suspend and
  resume set `spec.suspend` and nothing else, and report what they asked for
  beside what the operator had observed. refresh writes the read-request
  annotation and says that the read has not happened yet. wait reads a schema
  or a migration until it is in sync, waits for an approval, has applied, is
  suspended, or has ended the read a refresh asked for, and never counts what
  was already true when it began. A plan the resource's own policy holds, a
  schema plan that is destructive while `allowDestructive` is false or any
  plan under apply `Never`, stops a wait for in-sync, approval or applied once
  the operator has planned it again after the wait began. The three
  that write need `get` and `patch` on the resource, `wait` needs `get`. The
  changes, `approve` included, take `--wait`. [Waiting for the
  operator](../../use/operations/#wait) has the targets.
- **`kubectl ptah set-source`.** `kubectl ptah set-source schema/NAME --oci
  REF` or `migration/NAME` points a resource at another artifact, given as a
  reference pinned by digest, which `ptah schema push --format reference` and
  `ptah migrations push --format reference` print. It changes the reference
  and nothing else, keeps the verification policy, the registry credential, the
  transport and the apply policy, and refuses a tag, a ConfigMap or Git source,
  and a resource with an Apply already running. With `--wait` it stays until a
  plan from the new artifact waits for a decision or the resource is in sync
  from it, and stops when the policy refuses that plan. It needs `get` and
  `patch`, like suspend. [Moving to a new artifact](../../use/operations/#set-source)
  has the rest.
- **Exit statuses.** A write the plugin sends exits 4 when nothing was
  approved or changed, 5 when RBAC or the API server refused it, and 6 when it
  was sent without an answer and may have been stored. A wait exits 7 when it
  timed out, naming what holds the resource, and 8 when the resource can no
  longer reach the target. An unknown flag is named on stderr instead of
  exiting 2 in silence.
- **Four-eyes approvals.** With `approvals.requireDistinctApprover` on,
  suspending or resuming a schema or a migration no longer records whoever did
  it as the last spec writer. Before, a colleague who paused and resumed a
  resource became the recorded writer, and the author of the change could then
  approve it. A spec edit other than `spec.suspend` records its writer as
  before. A record that a toggle moved before the upgrade stays as it is until
  the spec is next changed.

## 0.2.8

**Before you upgrade.** The manager image is new, so keep `--force-conflicts`
on the upgrade. Resources need nothing else.

- **Approvals.** The executor is now Ptah v0.16.0, which retires the
  execution binding as every new executor does: approve again whatever was
  waiting, after the resource has read its database with v0.16.0 and planned.
  Applied state is not touched.

Ptah v0.16.0 compares more kinds of object, and a drift report can now name
grants and grant options, sequences, views, materialized views, triggers,
role memberships, comments, index visibility and renames, and validated
constraints. A grant the database lacks used to be drift in no category; it is
now one `grants_added`. Tooling that reads `status.target.driftFindings`
should expect these categories.

A `PtahSchema` now counts each kind of object its last observation compared,
in `status.target.inventory`: how many the desired schema declares and how
many the database holds, by the stem of the kind's drift categories, such as
`tables` or `rls_policies`. [Operations](../../use/operations/) shows one.

A `PtahMigration`'s `status.history.revisions` now carries each row's
`appliedAt` and `executionMillis`. The fields came in 0.2.7 and stayed absent,
because its executor did not report them; Ptah v0.16.0 does.

## 0.2.7

**Before you upgrade.** The manager image is new, so keep `--force-conflicts`
on the upgrade. Resources with an OCI source need nothing else.

- **Approvals.** The executor is now Ptah v0.15.0. As in 0.2.6, a new
  executor retires the execution binding: a plan waiting for approval is
  planned again and needs a new approval, and each resource reads its database
  again with the new executor before it plans. Applied state is not touched.

A schema or a migration sequence can now come from a ConfigMap in the
resource's namespace or from a Git repository, beside an OCI artifact, and
neither needs a registry. [Deliver from a ConfigMap or Git](../../use/sources/)
shows both, with Kustomize and Helm. A namespace that uses one of them sees
the following.

- A ConfigMap source is copied into an immutable snapshot ConfigMap the
  resource owns, named `ptah-src-<hash>`, which a ResourceQuota on
  `count/configmaps` counts. The manager can now delete ConfigMaps for these,
  and a new admission policy refuses it the deletion of any other ConfigMap.
- A Git source is fetched by the Resolve Job and by an init container in every
  later operation Pod, and only those containers hold the Git credential.
  `examples/networkpolicy-egress.yaml` allows DNS, the registry and the
  database, so a namespace that applies it has to allow the Git host as well.
- A Git source pinned to a commit id is fetched by that id only from a server
  that says it gives reachable commits by id, as GitHub, GitLab and Codeberg
  do. Elsewhere it is refused once a push moves the branch past the commit; pin
  a tag there. [Reconciliation](../../reference/reconciliation/) has the
  details.
- `sourcePolicyFrom` names an immutable ConfigMap that limits the ConfigMaps,
  Git hosts and content size a resource's source may use.
- A plan computed from a ConfigMap or a Git commit is written under plan
  contract version 2, which a 0.2.6 manager refuses. Once a resource uses
  either source, do not roll the manager back to 0.2.6.
- `kubectl ptah schema` and `kubectl ptah migration` from this release show a
  ConfigMap or Git source. The 0.2.6 plugin shows no source for them.

Each `PtahSchema` and `PtahMigration` now keeps a bounded journal of its
lifecycle in `status.journal`: plans published, approvals accepted, Applies
and runs with their start and end, verifications, acknowledgments, and failed
attempts by reason, at most 64 entries and none more than 30 days older than
the newest. `coveredSince` says from when it is complete, which on an upgraded
resource is the manager's first status write after the upgrade.
[Reconciliation](../../reference/reconciliation/#the-lifecycle-journal) lists
each entry. A failed migration run now also names, in `status.lastRun.failure`,
the version it stopped in, the statement's position and the engine's SQLSTATE
or error number, never the engine's message.

After a person changes the database outside the operator, such as clearing a
dirty revision row with `ptah migrations repair`, the annotation
`operator.ptah.run/read-requested-at`, set to an RFC 3339 instant, asks the
resource to read it now instead of at `nextReconciliationTime`. It is answered
once per value, when no operation is running, recorded in
`status.lastReadRequest`, and never starts a mutating operation;
[Reconciliation](../../reference/reconciliation/#reading-again-on-request) says
in which phases and how.

A `PtahMigration` now lists the newest rows of its revision table in
`status.history.revisions`: the version, the description, the checksum the row
recorded, its state, when it was written and how long the migration ran, at
most 64 rows. [Migrations](../../use/migrations/#what-the-history-says) shows
one. The executor this release pins does not report the time and the duration
yet, so the rows carry neither until it does.

A Plan whose publication the API server refuses, by admission, quota, RBAC or
size, now fails with the reason `PlanPublicationRefused` and the refusal in its
message. 0.2.6 retried it until the Plan Job was gone and then reported the Job
missing.

The operator image is built on ca-certificates 20250419+deb13u1, without the
roots Debian distrusted in DSA-6572-1.

## 0.2.6

**Before you upgrade.** The manager image is new, so keep `--force-conflicts`
on the upgrade, and check the following.

- **Operation Pod memory.** The executor is now Ptah v0.14.0, which needs
  about twice the memory v0.13.0 did. The runner and Ptah share the operation
  container, and a 64Mi limit that 0.2.5 fit in gets Observe and Plan Pods
  OOM-killed; the resource then reports that the runner's result frame was not
  found. If `spec.execution.resources`, or the LimitRange default where it is
  unset, gives the container less than 128Mi, raise it to 128Mi, which is what
  the acceptance suite runs with.
- **Drift categories.** `status.target.driftFindings[].category` now holds the
  category exactly as Ptah reports it. Ptah v0.14.0 hands PostgreSQL extensions
  and the YDB objects to feature owners and counts a change to one under
  `feature_changes` followed by the type of object, as in
  `feature_changes:ptah.run/pgextension/extension-change`, and an owner's
  access verdict under `feature_access_widened`, `feature_access_narrowed`,
  `feature_access_unchanged` or `feature_access_unknown`. Thirty-one
  per-object categories, such as `extensions_added`, are no longer written. An
  alert rule, dashboard or script that matches those names stops matching.
  Objects the comparison could not decide are counted under `undecided`; 0.2.5
  refused a report that held it, and the Observe failed.
- **Approvals.** A new executor retires the execution binding, so a plan
  waiting for approval is planned again and needs a new approval. Applied
  state is not touched.

A failed or partial migration run now also says where in the migration it
stopped and what the database answered, without the database's text:

```text
Migration 2 (Read Ledger) failed at statement 2 of 3 with SQLSTATE 42P01 and committed nothing; ...
Migration 2 (Fill Ledger) committed 2 of its 4 statements, then failed with SQLSTATE 23000 (error 1062); ...
```

Each manager process now paces its requests to the Kubernetes API server:
80 a second with a burst of 40, shared by every client in the process, so one
process sends at most 4,840 in any minute. After a controller-manager leader
change, the Job controller can hold back Job completions for minutes and then
report them at once; the work they wake is now spread over the following
minutes instead of sent in one. An installation larger than the 0.2.0 profile
can raise `apiRequests.perSecond` and `apiRequests.burst`.

The binaries are built with Go 1.27.3, which fixes GO-2026-6809 in
`crypto/x509`, a path the manager reaches.

## 0.2.5

**Before you upgrade.** Nothing has to change. The manager image is new, so
keep `--force-conflicts` on the upgrade.

The chart's ServiceMonitor now selects the metrics Service. In 0.2.4 it
selected Services by a label only the manager Pods carry, so
`monitoring.serviceMonitor.enabled` gave Prometheus no target and every alert
rule read an absent series. The metrics Service now carries
`app.kubernetes.io/component: metrics`, and the ServiceMonitor selects that.
A ServiceMonitor you wrote yourself to work around it keeps working; remove it
once the chart's own one is scraped, so the managers are not scraped twice.

A failed or partial migration run now names the migration it stopped in, in
`status.lastRun.message` and in its `MigrationRunFinished` Event: its version,
the description Ptah reads from its file name, and for a partial run how many
of its statements it committed. The message still carries nothing the
database said.

`pkg/planread` is new. It reads a stored schema plan through the same checks
`kubectl ptah plan` makes, for a Go program that shows plans, such as the
optional console in stokaro/ptah-console. Nothing in the operator's own
behavior changes with it.

## 0.2.4

**Before you upgrade.** Nothing has to change. This upgrade swaps the manager
image as well, and `--force-conflicts` stays on the command.

Schemas and migrations that share an interval no longer refresh at the same
moment. Each Resolve set the next refresh exactly one interval after it
started, so resources created together stayed in step for as long as they
lived, and every interval began with the whole fleet's API traffic at once.
Each of them now takes an evenly spaced slot in its interval, in UID order,
and a refresh that started well past its slot sets the next one later onto it.
The next refresh is never earlier than one interval after the last; aligning
can lengthen one or two cycles by at most half an interval, after a resource
joins or leaves the fleet or drifts from its slot. A resource alone in its
interval keeps the plain cadence.

## 0.2.3

**Before you upgrade.** Nothing has to change. The manager image is new again,
so keep `--force-conflicts` on the upgrade.

Replacing every manager at once while an approval arrives no longer blocks the
migration or schema. Deleting all manager Pods removes the endpoints of the
manager's own Job-write webhook before either process is told to stop, so a
leader still dispatching had its Job create refused and left an approved Apply
that only a person could acknowledge. A create the API server refused before
storing it proves no Job exists, so the dispatch is now withdrawn and made
again by the next leader under the same approval. A create whose outcome is
uncertain, such as a timeout, still blocks for a person, as before.

## 0.2.2

**Before you upgrade.** Nothing has to change. As from 0.2.0, the manager image
changes, so the upgrade needs `--force-conflicts`.

A manager that is stopping no longer starts a Job, and one whose dispatch had
already begun finishes creating it. Before, an approval that arrived while the
managers were being replaced could be taken by the stopping manager, whose
interrupted create left an outcome nobody established, and a migration then
blocked until a person acknowledged the run. The manager now finishes stopping
within 20 seconds, inside the Pod's grace period, so it releases its leader
Lease before it is killed and the next manager takes over at once.

## 0.2.1

**Before you upgrade.** Nothing has to change. The upgrade replaces the
manager image, so run it as
[Upgrade to a new release](../../use/operations/#upgrade) says, with
`--force-conflicts`. One behavior changes:
`spec.interval` now runs from the start of one refresh to the start of the
next, as the API describes it. 0.2.0 waited one interval after each refresh
finished, so the refresh's own Jobs lengthened every period.

At the same interval a converged resource now refreshes more often. With the
default 10m and a refresh of about a minute, that is about one Job in ten more.
Raise `spec.interval` if that load matters. A published plan still waits a full
interval for its approval, and a refresh that takes longer than its interval
waits one interval after it ends rather than starting the next at once.

The manager's leader Lease now lasts 60 seconds and is renewed within 45, up
from 15 and 10, and a manager that stops releases it. An API server stall
shorter than 45 seconds no longer restarts the leader, and a restart or an
upgrade hands over without waiting out the Lease. A manager that dies without
stopping is replaced after about a minute instead of 15 seconds.

## 0.2.0

**Before you upgrade.** There is no earlier published release to upgrade from.

0.2.0 is the first published release. Its images, chart and clients were
qualified on their own digests across Kubernetes 1.35, 1.36 and 1.37 on amd64
and arm64, and published as an immutable release; see
[Releases and provenance](../releases/) to verify them.

Installing it is [Install](../../start/install/), and the database privileges
it needs before the first schema converges are
[Databases and privileges](../databases/).
