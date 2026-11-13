---
title: Audit a database change
description: What the operator records about a change to a database, how long each trace lasts, who can change it, and what an installation adds before an audit can rely on it.
---

An auditor asks who asked for a change, who approved it, what ran, how it
ended, and whether the record could have been changed since. The operator
answers most of that inside the cluster, in objects that live as long as the
resource or its namespace. Only a migration leaves an answer in the database it
changed. Whatever has to outlast the cluster is kept by the installation: the
API server's audit log, collected logs and backups.

Every finding below comes from [one measured run](#the-measured-run) and says
whether it was measured or only read from the code.

## The questions {#questions}

SOC 2 CC8.1, ISO/IEC 27001:2022 A.8.32 and PCI DSS v4.0 requirement 10 are
where these questions come from. This page claims no compliance with any of
them.

| Question | Where the answer is | After the cluster is lost |
| --- | --- | --- |
| Who asked for the change? | The API server's audit log, at the write that changed the spec | Only if the log was kept outside the cluster |
| What was approved, by whom, and when? | The approval object, the journal's `ApprovalAccepted` entry, the audit log | A migration's approver, by username, on its rows in `schema_migrations_log`. Nothing for a schema |
| Was it approved before it ran? | The journal's order and the approval's `Consumed` condition | A migration's `actor` names its approver, or the `apply-always` prefix for a run no one approved |
| Did a second person approve it? | [`approvals.requireDistinctApprover`](../approvals/#refusing-a-self-approval), when the installation turned it on | No |
| What ran, exactly? | A schema plan's chunks hold its SQL. A migration plan holds versions and checksums, and the statements are in the artifact | A migration's versions and checksums, in the database |
| How did each attempt end, and when? | The journal, `status.lastRun`, and for a migration the database | For a migration |
| An approval or a policy? | A migration run's `authorization` in the journal. For a schema, whether an `ApprovalAccepted` comes before the `ApplyFinished` | A migration's `actor` |
| Was the database changed outside this process? | Drift readings and the plans they publish | No |
| Could the record have been changed, and would anyone notice? | [Who can change the record](#integrity) | |
| How long is it kept? | [What survives](#survival). PCI DSS asks for a year, with three months at hand | |

## The measured run {#the-measured-run}

Run on 2026-11-12, on a kind cluster with three control-plane nodes on Docker
context `diabolocom`.

| Component | Version |
| --- | --- |
| Operator | Built from `master` at `cd01025a`, chart version 0.2.10 in that tree |
| `kubectl ptah history` | Built from `master` at `f7c4eb7c`, which changes no controller code since `cd01025a` |
| Ptah executor | v0.23.0 |
| Kubernetes | 1.36.4 (`kindest/node:v1.36.4`), kind v0.33.0 |
| PostgreSQL | 17.11 |
| MySQL | 8.4.11 |

The API servers wrote an audit log, under an earlier form of [the policy
below](#audit-policy) with one more rule, which logged one approver at
`Metadata` to show what that level keeps. Requests came from ServiceAccounts of
their own: an author who may write `PtahSchema` and `PtahMigration` specs and
ConfigMaps, two approvers bound to the chart's approver role, and a namespace
user bound to the built-in `edit` role plus every verb on every kind in
`operator.ptah.run`. A cluster administrator impersonated `alice@example.test`
for one approval.

On each engine a `PtahSchema` applied an approved plan, and then a second
approved plan failed: a unique index over rows that were not unique. A
`PtahMigration` applied one approved version, and then a second approved
version failed: a duplicate primary key on PostgreSQL, where nothing committed,
and a `CHECK` on a column that does not exist on MySQL, after two of its three
statements committed. The MySQL run ended `Partial` and was acknowledged. A
third migration ran under `apply: Always` on PostgreSQL. A hand edit made drift
on the PostgreSQL schema and was undone, and the MySQL schema was suspended and
resumed.

Then, in order: an upgrade to a synthetic next release and a rollback, while
every object still existed; deleting the PostgreSQL schema and migration;
deleting the namespace; restoring a backup of the namespace, taken before the
upgrade, into a new namespace through the API; and deleting the cluster. The
upgrade and the rollback come first because after the other steps there is
nothing left for them to keep. After each step the run read every kind, the
Events, Jobs and Pod logs, the manager logs, the audit log on each node and the
tables in each database.

Bootstrap took 25 minutes and the run about 80 more.

## What the operator records {#coverage}

"Measured" means this run read it. "Read" means it comes from the code or this
documentation and the run did not exercise it.

### `PtahSchema` {#coverage-schema}

| Event | Where it is written | Written by | Kept for | Source |
| --- | --- | --- | --- | --- |
| A spec edit, suspend and resume included | The API audit log, with the request body at `RequestResponse`. No journal entry | The API server | As long as the log is kept | Measured |
| Who last changed the spec | `operator.ptah.run/last-spec-writer-username` and `-uid`, only while `requireDistinctApprover` is on | A mutating webhook | Until the next spec edit | Read |
| Source resolved | The `ArtifactResolved` condition. A ConfigMap source is copied into an immutable snapshot ConfigMap the schema owns | The manager | Until the next resolution | Measured |
| Observation and drift | Conditions. Drift that needs a change publishes a plan, and the journal records it with `destructive` where it is | The manager | The journal's [bounds](../../reference/reconciliation/#the-lifecycle-journal): 64 entries, 30 days | Measured |
| A plan | `PtahSchemaPlan` and its chunks, with the exact SQL and the fingerprint; the journal's `PlanPublished` | The manager | Until the schema is deleted | Measured |
| An approval | `PtahSchemaApproval`, with the username, UID and groups admission stamped, `approvedAt` and `mutationRequestUID`; the journal's `ApprovalAccepted`, with the approval's name and UID, when an Apply consumes it | The approver, stamped by admission | The approval until it or its namespace is deleted; the entry for the journal's bounds | Measured |
| Apply dispatched | The Apply Job and Pod, and an `operation claimed` line in the manager's log | The manager | The Job: five minutes, or 24 hours when a person approved an Apply that failed or ended unknown | Measured |
| How each statement ended | The journal's `ApplyFinished`: `Succeeded` or `Unknown`, the plan by name, UID and fingerprint, the approval and its approver, the `operationID`, the Job once the API server confirmed it, and the position of the statement it stopped at with the SQLSTATE, with no message. At 0.2.10, in the measured run, it held only the outcome, the plan and when it started | The manager | The journal's bounds | Read |
| What the database answered | One line in the Apply Pod's log | The runner | As long as the Job | Measured |
| Verification after Apply | The journal's `Verified`: `InSync` or `DriftRemains` | The manager | The journal's bounds | Measured |
| `apply: Always` | No approval, so no `ApprovalAccepted` before the `ApplyFinished` | The manager | The journal's bounds | Read |
| Deletion | The API audit log | The API server | As long as the log is kept | Measured |
| Anything, in the target database | Nothing. Ptah keeps no table for a declared schema | | | Measured |

Both failed Applies ended `Unknown`, and at 0.2.10 the journal said no more
than that. What failed is in the Pod log for 24 hours. On MySQL that line read
`Duplicate entry 'dup@example.test' for key 'customers.customers_email_unique'`:
a value from a table row.

### `PtahMigration` {#coverage-migration}

| Event | Where it is written | Written by | Kept for | Source |
| --- | --- | --- | --- | --- |
| A spec edit | The API audit log, as for a schema | The API server | As long as the log is kept | Measured |
| Source resolved | `status.lastRun.source` when a run ends | The manager | Until the next run | Measured |
| History read | Conditions; the journal's `Verified` for the first reading after a run | The manager | The journal's bounds | Measured |
| A plan | `PtahMigrationPlan`, with the versions, their checksums and the fingerprint; the journal's `PlanPublished` | The manager | Until the migration is deleted | Measured |
| An approval | `PtahMigrationApproval`; the journal's `ApprovalAccepted` with the plan's name, UID and fingerprint | The approver, stamped by admission | As for a schema | Measured |
| Apply dispatched | The Job and Pod, and a `migration operation claimed` line in the manager's log | The manager | As for a schema | Measured |
| How each statement ended | `schema_migrations` and `schema_migrations_log` in the target database: the run, the version, `started`, `applied` or `failed`, the error and the failed statement. The journal's `RunFinished` with the version, statement number and SQLSTATE where it stopped | Ptah in the Apply Pod, and the manager | The database until someone deletes the rows; the journal for its bounds | Measured |
| Who approved, in the database | `actor` on each log row: the approver's username, with `actor_source` `provided` | Ptah, from what the operator passes | Until someone deletes the rows | Measured |
| Verification after the run | The journal's `Verified`: `InSync`, `Pending` or `Blocked`, with the run's `operationID` | The manager | The journal's bounds | Measured |
| A run nobody accounted for, settled | `PtahMigrationRunAcknowledgment`; the journal's `RunAcknowledged` with who acknowledged it | The person, stamped by admission | As for an approval | Measured |
| `apply: Always` | The journal's `RunFinished` with `authorization: Policy` and no approval; `actor` reads `system:ptah-operator:apply-always:<namespace>/<name>` | The manager and Ptah | As above | Measured |
| Deletion | The API audit log | The API server | As long as the log is kept | Measured |

`kubectl ptah history migration/<name>` printed every one of these journal
entries with the plans, approvals and acknowledgment they name.

### What no family records {#not-recorded}

Events said less than the journal and lost part of it. They are a notification.
At a one-minute interval each schema used its 25 Normal Events within about
five minutes, and the recorder then lets one through every five minutes. The
second plan's `ApprovalRequired` and the second approval's `ApprovalAccepted`
reached no Event on either schema, while the journal recorded both. Events
about a deleted resource stay for their hour and go with the namespace.
[An Event can be lost](../../troubleshoot/events/#an-event-can-be-lost) says why.

The manager's log lines carry the namespace, the name, the operation, the
attempt and the phase. They name no plan, approver or outcome, and they go
with the manager's Pod: the upgrade replaced both managers, and the cluster no
longer served what the old Pods had logged.

## What survives {#survival}

Every cell is measured.

| Trace | Resource deleted | Namespace deleted | Upgrade, then rollback | Restored through the API | Cluster lost |
| --- | --- | --- | --- | --- | --- |
| `status`: the journal, `lastRun`, `applied` | Gone | Gone | Kept, entry for entry | Refused, for a cluster administrator too; the journal starts empty | Gone |
| Plans and chunks | Gone, collected with the owner | Gone | Kept | Admitted, then collected within seconds, because their owner references name the old UIDs | Gone |
| Approvals and acknowledgments | Kept: they have no owner | Gone | Kept | Refused: their plans are gone, and an acknowledgment names the old migration's UID | Gone |
| Result records | Kept, and deletion refused for about an hour | Hold the namespace in `Terminating` for up to an hour | Kept | Refused: only the manager creates one | Gone |
| Jobs and Pod logs | Gone, the 24-hour failed-Apply Job with them | Gone | Kept | Not restored | Gone |
| Events | Kept for their hour | Gone | Kept | Not restored | Gone |
| Manager logs | Kept | Kept | Gone with the replaced Pods | Not applicable | Gone |
| The API audit log, on the control-plane nodes | Kept | Kept | Kept | Kept | Gone with the nodes |
| `schema_migrations` and `schema_migrations_log` | Unchanged | Unchanged | Unchanged | Unchanged; the restored migrations read the dirty revision and stopped | Unchanged |
| A schema's record in the database | None | None | None | None | None |

A restore through the API, as a backup tool that writes objects does it, brings
back the specs and none of the record. The restored resources read the
databases again, and the migrations stopped on the revision the failed runs left
dirty, with nothing in the cluster to say who ran it or that a person had
accounted for the MySQL run. The database log still named the approver of each
run. Only a restore of etcd itself keeps UIDs and status. That was not measured;
[Recover operator state](../recovery/#restore-a-consistent-control-plane) says
what it takes.

## Joining one change across the record {#joins}

The journal joins itself and the cluster objects by UID. An `ApprovalAccepted`
entry carries the approval's UID. A migration's `RunFinished` carries the plan's
UID and fingerprint, the approval's UID, the Job's UID and the `operationID`,
and its `Verified` and `RunAcknowledged` entries carry the same `operationID`.
From 0.2.11 a schema's `ApplyFinished` carries the same identifiers, and the
`Verified` entry after it the same `operationID`. That is read from the code;
the measured run predates it.

The database log joins nothing by identifier. Its `run_id` is Ptah's own, such
as `28-dmth8umhdwq7`, and the operator does not pass the operation ID. A row
meets its run by the version, the actor and the time.

`mutationRequestUID` is the UID of the admission review the mutating webhook
answered, and the API server makes a new one for each webhook call. The audit
event has an `auditID` of its own, and the two differed on every approval in the
run. So the webhook names its stamp on the event: it answers with audit
annotations, and the API server records them on the create's event, each key
prefixed with the name of the webhook that answered.

| Kind | Annotations on the create's audit event |
| --- | --- |
| `PtahSchemaApproval` | `mapproval.operator.ptah.run/mutationRequestUID`, `mapproval.operator.ptah.run/username` |
| `PtahMigrationApproval` | `mmigrationapproval.operator.ptah.run/mutationRequestUID`, `mmigrationapproval.operator.ptah.run/username` |
| `PtahMigrationRunAcknowledgment` | `mmigrationrunacknowledgment.operator.ptah.run/mutationRequestUID`, `mmigrationrunacknowledgment.operator.ptah.run/username` |

`mutationRequestUID` holds the value the object stores in
`spec.mutationRequestUID`, and `username` the username it stores in
`spec.approver`, or in `spec.acknowledgedBy` for an acknowledgment. On an
impersonated request that is the impersonated identity, which the event also
holds in `impersonatedUser`, while `user` names who sent the request. The
annotations say what the webhook stamped, and `responseStatus.code` says
whether the object was stored: validation after the webhook can still refuse
it. The prefix is the webhook's entry name, not the name of its
`MutatingWebhookConfiguration`, which
`mutation.webhook.admission.k8s.io/round_0_index_0` names beside it.
[Reading the record](#reading) finds an approval's event by its
`mutationRequestUID`.

This is measured at `Metadata` against the API server `make test-envtest`
runs, in `TestStampingWebhooksNameTheirStampInTheAuditLog` under
`test/envtest/webhook`. The levels above `Metadata` keep the annotations too,
as read in `k8s.io/apiserver` v0.37.2, `pkg/admission/attributes.go`. The run's
build predates them. There, at `Metadata`, the approval's create named the UID
nowhere: it carried only `mutation.webhook.admission.k8s.io/round_0_index_0`
with `"mutated":true`, and `objectRef.uid` is empty on a create, so an approval
met its audit event only by namespace, name and time. At `RequestResponse` the
UID is also in the stored object in `responseObject` and in the webhook's patch
annotation, `patch.webhook.admission.k8s.io/round_0_index_0`. A delete names the
UID it removed in `responseStatus.details.uid`.

[Ptah Console](https://github.com/stokaro/ptah-console) writes the API server's
`Audit-Id` header into each line of its own audit stream, so a console action
does join the audit log. That is read from its code, not measured here.

## Identity {#identity}

| Identity | What it is worth | Source |
| --- | --- | --- |
| The approver admission stamped | The username, UID and groups the API server authenticated. A request that named someone else, with an earlier `approvedAt`, was stored with the requester's identity and the time of the request | Measured |
| An impersonated approval | The approval, the journal and the database name `alice@example.test`. Only the audit log says that `kubernetes-admin` made the request, in `user` beside `impersonatedUser` | Measured |
| The spec writer | The audit log. The annotations exist only while `requireDistinctApprover` is on | Measured with the switch off |
| A GitOps controller as writer | The controller's ServiceAccount; the person is in the repository's history | Read |
| A shared credential | One identity for everyone who holds it, as [Refusing a self-approval](../approvals/#refusing-a-self-approval) says | Read |
| A console user | The console's audit stream maps its account to the Kubernetes identity it acted as | Read |
| `actor` in the database | What the operator passed, under `actor_source` `provided`. The login that runs migrations rewrote it on every row, and `ptah migrations up --actor` writes the same source | Measured; the `--actor` case from [#800](https://github.com/stokaro/ptah-operator/issues/800) |

An audit policy matches a request against the user who authenticated, not the
one impersonated: the API server's audit filter runs before its impersonation
filter (read in `k8s.io/apiserver` v0.37.2, `pkg/server/config.go`). Give the
identities that may impersonate a rule that keeps request bodies.

## Who can change the record {#integrity}

The namespace user from [the run](#the-measured-run) tried each write to the
cluster with `--dry-run=server`, which runs authorization and admission and
stores nothing, and RBAC allowed every one of them. The `apply: Always` row is
the author's own request, and the database row a transaction that was rolled
back.

| Trace | Result |
| --- | --- |
| A resource's status: the journal, `lastRun` | Refused by the status write guard: only the manager writes status |
| `operator.ptah.run/unresolved-run` | Refused by its guard |
| A plan or a chunk | Its spec refused as immutable; deleting it admitted |
| An approval or an acknowledgment | Its spec refused as immutable; adding a label and deleting it admitted |
| An approval naming someone else | Stored with the requester's identity in place of the one it named |
| A result record | Deletion refused for about an hour |
| The spec-writer annotations | Admitted with `requireDistinctApprover` off. With it on, the webhook restores them (read) |
| `spec.policy.apply: Always` | Refused for the author by the apply-mode guard; a member of the group the guard exempts set it |
| The resource itself | Deletion admitted, and it takes the journal and the plans with it |
| Jobs, Pods and Events | Deletion admitted: the built-in `edit` role grants it |
| `schema_migrations_log` | The login in the migration's Secret updated all four rows on each engine, in a transaction that was rolled back. The namespace user could read that Secret |

Deleting the right objects removes most of the in-cluster record without
editing any of it: the resource takes its journal, plans and Jobs, and deleting
the approvals and Events removes the rest. RBAC on `delete` is the control, and
the API audit log is where a deletion shows. Every refused and admitted request
was there, with the binding that allowed it. At `Metadata` a dry-run delete
reads exactly like a real one, with code 200 and no body, so log deletes of
these kinds with their request.

## What the installation adds {#what-the-installation-adds}

### An API audit policy {#audit-policy}

The operator neither needs nor configures one. This policy keeps what the run
showed an audit needs:

```yaml
apiVersion: audit.k8s.io/v1
kind: Policy
omitStages: ["RequestReceived"]
rules:
  # Leases renew every few seconds and say nothing about a change.
  - level: None
    resources: [{group: "coordination.k8s.io", resources: ["leases"]}]
  # The manager's own writes: who wrote which object, without bodies.
  # Name the namespace the chart is installed in.
  - level: Metadata
    userGroups: ["system:serviceaccounts:ptah-system"]
    verbs: ["create", "update", "patch", "delete", "deletecollection"]
  # Chunks hold plan SQL, and a result record can hold SQL and key material.
  - level: Metadata
    resources:
      - {group: "operator.ptah.run", resources: ["ptahschemaplanchunks", "ptahresultrecords"]}
    verbs: ["create", "update", "patch", "delete", "deletecollection"]
  # Every other write to the operator's kinds, with what was sent and stored.
  # The create of an approval or an acknowledgment also names its stamp in the
  # event's annotations, which every level from Metadata up keeps.
  - level: RequestResponse
    resources: [{group: "operator.ptah.run"}]
    verbs: ["create", "update", "patch", "delete", "deletecollection"]
  # Deleting Jobs and Events, with the request that says whether it was a dry run.
  - level: Request
    resources:
      - {group: "batch", resources: ["jobs"]}
      - {group: "", resources: ["events"]}
    verbs: ["delete", "deletecollection"]
  - level: Metadata
    resources: [{group: "", resources: ["namespaces"]}]
    verbs: ["create", "delete"]
  - level: None
```

It was loaded and evaluated with the API server's own policy code
(`k8s.io/apiserver` v0.37.2) for each request named in its comments. The run's
API servers used an earlier form of it: one `Metadata` rule for every write in
the namespace, Jobs and Events included, and no rule of its own for chunks and
result records.

At `RequestResponse` an approval's line holds the stamped identity, `approvedAt`
and `mutationRequestUID` in the stored object. Its `mutationRequestUID` and
username are also in the annotations [Joining one change across the
record](#joins) lists, as they are at `Metadata`. A spec edit's line holds the
patch, such as `{"spec":{"suspend":true}}`. A namespace deletion's line lists every object it
removed, status included: the deleted migration came back with all nine of its
journal entries. A single delete returns the object only when the delete waits
on a finalizer. The schema deleted during an operation came back with its 12
entries, and the migration deleted between operations came back as a `Status`
that names its UID. Export what an audit needs before deleting a resource.

The manager's writes are frequent. Five resources on a one-minute interval made
1,228 writes to `PtahSchema` and `PtahMigration` objects in 25 minutes, three
in four of them to status, which is why its rule keeps no bodies. While a
deleted resource's result records held its namespace, the garbage collector
retried each refused delete: 7,818 refusals in 16 minutes for 471 records.

A managed control plane owns its audit policy. Read what it records for writes
to custom resources before relying on it, and ship the log off the control
plane: in the run, it was gone with the nodes.

### Log collection {#log-collection}

A collector that ships Pod logs out of the cluster keeps the manager's lines
past an upgrade and past the cluster. It also ships every Apply Pod's log, and
the line a failed statement leaves there can hold a table row, as
[Output handling](../security/#output-handling) says. Decide what that pipeline
may keep. An Event exporter keeps the Events the API server received and none
of the ones the recorder dropped. The console writes its audit stream to its
standard error, one JSON line per action.

### Backups and exports {#backups}

A backup tool that writes through the API restores none of the record, as
[What survives](#survival) shows. What it keeps is evidence to read: the JSON of
every object as it was, journal and approvals included. Before deleting a
resource, export it:

```sh
kubectl ptah history schema/<name> -n <namespace> -o json > <name>.history.json
kubectl -n <namespace> get ptahschema <name> -o json > <name>.json
```

[Export a plan before deleting it](../operations/#export-a-plan-before-deleting-it)
covers the SQL of a schema plan.

### In the database {#in-the-database}

`schema_migrations_log` is an ordinary table that the login running migrations
can write. A database's own audit facility, writing somewhere that login cannot
reach, is what shows a row was changed afterwards, and a copy of the rows kept
outside the database is what keeps them. A schema change leaves nothing in the
database to protect, so for a `PtahSchema` the API audit log is the record.

## Privacy {#privacy}

[The approver's name in the target database](../security/#approver-in-the-database)
is personal data whenever it names a person, and every reader, backup and
replica of that table carries it. The operator deletes no row, and editing or
removing one with SQL is safe for the operator.

The log's `error` column holds what the database answered and the statement it
refused, with every literal that statement holds. The PostgreSQL row read
`SQL: INSERT INTO shipments (id, reference) VALUES (1, 'EU-200')`. A failed
statement's Pod log can hold a value from a table row, as the MySQL schema's
did. Under the policy above the API audit log holds approvers' identities and
the specs people wrote, and no plan SQL.

Erasure meets an append-only record unevenly. Deleting an approval removes the
object, and the journal still names the approver until the entry ages out of its
bounds. A log row stays until someone deletes it. An audit log store keeps what
its own retention says.

## Reading the record {#reading}

```sh
kubectl ptah history schema/<name> -n <namespace>
kubectl ptah history migration/<name> -n <namespace>
kubectl -n <namespace> get ptahschemaapprovals,ptahmigrationapprovals -o yaml
```

```sql
SELECT logged_at, run_id, version, state, actor, actor_source, error
FROM schema_migrations_log
ORDER BY logged_at, seq;
```

To find the API write behind an approval in an audit log of JSON lines:

```sh
jq -c 'select(.objectRef.resource == "ptahschemaapprovals"
  and .objectRef.name == "<approval>" and .verb == "create")
  | {auditID, user, impersonatedUser, code: .responseStatus.code,
     approver: .responseObject.spec.approver}' audit.log
```

At any level from `Metadata` up, the approval's `mutationRequestUID` finds the
same write by its annotation, with no body in the log:

```sh
uid=$(kubectl -n <namespace> get ptahschemaapproval <approval> \
  -o jsonpath='{.spec.mutationRequestUID}')
jq -c --arg uid "$uid" '
  select(.annotations["mapproval.operator.ptah.run/mutationRequestUID"] == $uid)
  | {auditID, user, impersonatedUser, code: .responseStatus.code}' audit.log
```

For a migration approval or an acknowledgment, use the key of its webhook from
[the table](#joins).

`ptah migrations log` prints the same table, and on MySQL needs `parseTime=true`
in the URL, as [What survives a lost cluster](../recovery/#what-survives-a-lost-cluster)
says. No one command reads one database's history across the resources that
managed it.

## Other tools {#other-tools}

Read from each project's documentation and not measured, except SchemaHero.

- [Flyway](https://documentation.red-gate.com/fd/flyway-schema-history-table-273973417.html)
  keeps a schema history table in the target database: which migrations ran,
  when and by whom, with checksums and failures. "By whom" is
  [`installedBy`](https://documentation.red-gate.com/fd/reference/configuration/flyway-namespace/flyway-installed-by-setting),
  the current database user unless the caller names someone.
- [Liquibase](https://docs.liquibase.com/concepts/tracking-tables/databasechangelog-table.html)
  writes one `DATABASECHANGELOG` row per changeset, with a deployment ID and an
  `EXECTYPE` that includes `FAILED`. Its `AUTHOR` is the changeset's author
  attribute, not who ran it.
- [Atlas](https://atlasgo.io/versioned/apply) keeps `atlas_schema_revisions` in
  the target database. [Atlas Cloud](https://atlasgo.io/cloud/deployment) keeps
  a trace of each deployment outside it: who ran what and what failed.
- [Argo CD](https://argo-cd.readthedocs.io/en/stable/operator-manual/security/)
  emits Kubernetes Events naming the responsible actor, to be kept with an event
  exporter, and treats Git history as the audit log of configuration.
- SchemaHero, [measured on this site](../../start/compare-schemahero/), keeps
  no record in the database, and its approvals are lost with the cluster.
