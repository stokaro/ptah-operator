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

## 0.2.11

**Before you upgrade.** The manager image is new, so keep `--force-conflicts`
on the upgrade. Under `apply: Always`, a plan that changes an existing
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
UID while that Job is still running. A migration Job an earlier release left
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
one `stale` outcome, where it took two of each.

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
  Warning that said a running Job's cleanup could not be scheduled is gone. A Job
  whose claim never recorded its UID still stays, and a
  `MigrationJobCleanupDeferred` Warning now names it with `its claim never
  recorded the Job's UID`.
- **A recorded approval wins over a due read.** An approval the operator had
  recorded in `status.plan.approval` when the interval ended was spent by the
  read that followed: the read's observation marked it `Stale` although the
  plan did not change, and the same plan waited for a second approval. The
  Apply it authorizes now runs first, and the read-only proof after the Apply
  reads the database. An automatic Apply under `apply: Always` and an approval
  not yet recorded still wait for the read. A read requested on a schema with
  a recorded approval follows its Apply too, and the `ReadRequested` Event now
  says so.
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
