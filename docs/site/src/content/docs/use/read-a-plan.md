---
title: Read a plan
description: See the SQL the operator would run, or the SQL the last apply ran, with a read-only command.
---

The SQL the operator applies is not in a status field or the manager's log. It
is a document the operator publishes into immutable `PtahSchemaPlanChunk` objects, bound to a
`PtahSchemaPlan` by index, key, size and digest, and read back only after every
one of those bindings has held. The Plan runner first delivers the result over
TLS to the receiver, which persists it in confidential `PtahResultRecord`
objects before acknowledging it. The controller validates that stored result
before publishing the plan chunks. [Plan results stay out of logs](../security/#pod-logs-carry-plans)
describes this default delivery path and the separate legacy log transport. An
Apply projects the verified chunks into immutable ConfigMaps for its Pod to
mount. Those projections are created only when the Apply is dispatched.

`kubectl ptah` is how you read it. Its read commands read Kubernetes objects
and never create or change a resource, start a Job, or connect to your
database. The commands that write do so as you, and none of them connects to a
database either: `kubectl ptah approve` creates the approval of a plan you
reviewed, which [Exact-plan approvals](../approvals/) describes;
`suspend`, `resume` and `refresh` set one field or annotation of a schema or a
migration, which [Operations](../operations/#suspension-and-deletion)
describes; and `set-source` points one at another artifact pinned by digest,
which [Moving to a new artifact](../operations/#set-source) describes.
`kubectl ptah wait` reads a resource until the operator has done what you are
waiting for; [Waiting for the operator](../operations/#wait) has the targets.

```sh
kubectl ptah plan storefront -n application
```

Three verbs read three different things: `plan` reads the SQL a stored plan
holds, `schema` reads where a `PtahSchema` stands, what holds it and who
approved what, and `migration` reads the same for a `PtahMigration`.

[From source to verified change](../../reference/reconciliation/#from-source-to-verified-change)
shows where each command fits in a change's life: these read stages 2, 5 and 6,
`approve` records stage 3, and `wait` watches stage 4.
[Ptah Console](https://github.com/stokaro/ptah-console) is an optional interface
to the same resources, and nothing on this page needs it.

## Install it {#install}

`kubectl` runs any executable named `kubectl-<verb>` on your `PATH` as
`kubectl <verb>`, so installing the plugin is putting one file there.

Each release publishes a binary for the supported client platforms and a
`SHA256SUMS` beside them:

```sh
version=<release tag>
platform=darwin-arm64   # or linux-amd64, linux-arm64, darwin-amd64
base=https://github.com/stokaro/ptah-operator/releases/download/$version

curl -fsSLO "$base/kubectl-ptah-$platform"
curl -fsSLO "$base/SHA256SUMS"
shasum -a 256 --check --ignore-missing SHA256SUMS

install -m 0755 "kubectl-ptah-$platform" /usr/local/bin/kubectl-ptah
kubectl ptah --version
```

`kubectl plugin list` shows it once it is on `PATH`. It needs nothing else: no
checkout of this repository, no Helm release on your machine, and no files
beside it.

## Which plan

Current and applied are two different plans, and neither stands in for the
other.

| Flag | What it reads |
| --- | --- |
| `--current` (the default) | the plan the operator would run next, which is what `status.plan` names |
| `--applied` | the plan the last confirmed apply ran, which is what `status.applied` records |

A converged schema usually has no current plan, because there is nothing left
to do; a schema that has never applied anything has no applied plan. Either way
the command says so and exits with status 3 rather than printing empty SQL.

`--applied` shows the plan that ran. It is not a log of what each statement did
in the database, and it does not read the database again.

## What comes out

Without `-o` you get what the plan is, and then each statement it holds,
under a line that says where it sits in the plan and how it was rated:

```sh
kubectl ptah plan storefront --applied -n application
```

```text
Schema:         application/storefront
Plan:           applied (ptah-plan-4b44084123f0958600629632)
Fingerprint:    sha256:4b44084123f095860062963255f9f84c2921946524e187186eca71ca88cc366e
Content digest: sha256:99a821c0f52b1eb6d643633d40a7db7467b81e10a3be16982626807ef6c692ad
Dialect:        postgres
Statements:     1
Destructive:    false
Privileges:     none
Stored:         2026-09-13T08:04:05Z
Applied:        2026-09-13T08:04:24Z

Statement 1 of 1: safe (does not remove data or tighten constraints)
CREATE TABLE "customers" (
  "id" bigint PRIMARY KEY NOT NULL,
  "email" text NOT NULL
);
```

The statements come out as the planner wrote them. The line above each one is
not SQL. It gives the statement's severity, which
`Destructive` summarizes for the whole plan, and the reason Ptah gave for it.
Statements that do different things read differently:

```text
Statement 1 of 2: safe (does not remove data or tighten constraints)
ALTER TABLE "e2e_widgets" ADD COLUMN "fault_token" text;

Statement 2 of 2: destructive (DROP TABLE removes the table and all rows)
DROP TABLE "e2e_excluded_policy_keep";
```

The plugin also reads each statement, by the rules the manager reads a plan
with, and rates destructive any that has a `DROP` or a `TRUNCATE` in it, an
`ALTER COLUMN ... TYPE`, a MySQL `MODIFY COLUMN` or `CHANGE COLUMN` whatever it
changes, a `DISABLE ROW LEVEL SECURITY` or a `DELETE FROM pg_enum`, whatever
Ptah said. Where that rating is higher than Ptah's, the line says whose each
one is, because Ptah's reason still explains the lower one. It names this
plugin rather than the operator for the reason the `Privileges` line does: the
plugin and the manager that published the plan are separate builds.

```text
Statement 1 of 1: destructive as this plugin reads it; Ptah rated it safe (does not remove data or tighten constraints)
ALTER TABLE "customers" ALTER COLUMN "email" DROP NOT NULL;
```

`Privileges` names the kinds of authority the plan records, or says `none`. A
plan that records any waited for an approval even under `apply: Always`, and
[Plans that change privileges](../../reference/plans-and-approvals/#privilege-changes)
says what each kind covers. A kind the plugin reads in the SQL that the plan
does not record is listed after them, marked `(read by this plugin, not
recorded)`: the plugin and the manager that published the plan are different
builds, and the manager did not hold the plan for it.

`-o sql` prints the statements alone, in the order the plan holds them, each
terminated once, with no line above them: it is the format a script reads and a
reviewer diffs. Nothing is re-split on a semicolon: a statement may carry one
inside a string, a comment or a function body.

`-o json` prints the stored plan document byte for byte, so what you save still
matches the content digest printed above it. It is the executable plan, not the
Kubernetes object:

```sh
kubectl ptah plan storefront --applied -n application -o json > plan.json
```

Diagnostics go to standard error. Nothing reaches standard output until every
check has held, so a plan whose last chunk is corrupt leaves you with an error
rather than half its SQL.

Exit status: `0` printed, approved, changed or reached, `1` could not read or
verify, `2` the command line, `3` nothing stored to print, approve, change or
wait on. The commands that write add `4` nothing approved or changed (the plan
or the spec changed, the operator would not admit an approval now, the
fingerprint given is not the current plan's, you declined, the resource was
deleted and created again, or set-source met a source that is not an OCI
artifact, an Apply running, or a source another writer changed after it was
read), `5` refused by
RBAC or the API server, and `6` sent with no answer: the approval or the
change may have been stored. A wait adds `7` timed out and `8` the resource
will not reach the target.

## Where a schema stands {#schema}

A plan is what the operator would run. It does not say what the operator saw,
and a converged schema has no plan at all, which is exactly when you want to
know what the last reading found.

```sh
kubectl ptah schema storefront -n application
```

```text
Schema:               application/storefront
Phase:                AwaitingApproval
Ready:                False, AwaitingApproval: Plan is waiting for approval
Policy:               apply OnApproval, allowDestructive true, driftSeverity all
Artifact:             oci://registry.example/acme/storefront@sha256:...
Verified:             true
Observed:             2026-09-16T06:00:00Z
Drift:                6 in 3 categories, highest destructive
Reference data:       2 to insert, 1 to update, 3 to delete
  data_rows_deleted                       3  destructive
  data_rows_inserted                      2  safe
  data_rows_updated                       1  destructive

Plan ptah-plan-4b44084123f0958600629632, 4 statements, stored 2026-09-16T06:01:00Z
  destructive
  not approved
  ApprovalRequired: True, Waiting: Create an approval bound to the current plan fingerprint

Approvals, newest first:
  storefront-v2
    approved by alice@example.com at 2026-09-15T17:02:00Z
    plan ptah-plan-0f6e1d2c3b4a59687706f5e4, not the current plan
    TYPE      STATUS  REASON               SINCE                 MESSAGE
    Accepted  False   PlanNoLongerCurrent  2026-09-16T06:00:00Z  The approved plan no longer matches the current database state
    Stale     True    PlanNoLongerCurrent  2026-09-16T06:00:00Z  The approved plan no longer matches the current database state

Conditions:
  TYPE                  STATUS  REASON            SINCE                 MESSAGE
  EngineSupported       True    SupportedEngine   2026-09-16T06:00:00Z  Database engine "PostgreSQL" is supported
  Ready                 False   AwaitingApproval  2026-09-16T06:01:00Z  Plan is waiting for approval
  ReconciliationFailed  False   Succeeded         2026-09-16T06:00:00Z  The latest operation completed
  ArtifactResolved      True    DigestPinned      2026-09-16T06:00:00Z  Desired OCI reference resolved to immutable content
  ArtifactVerified      True    PolicySatisfied   2026-09-16T06:00:00Z  Artifact type and verification policy were satisfied
  DatabaseReachable     True    Observed          2026-09-16T06:00:00Z  Database schema was observed
  DriftDetected         True    ScopedChanges     2026-09-16T06:00:00Z  Managed scope requires 4 schema statements; highest severity destructive
  InSync                False   ScopedChanges     2026-09-16T06:00:00Z  The authoritative managed scope differs from the verified artifact
  ApprovalRequired      True    Waiting           2026-09-16T06:01:00Z  Create an approval bound to the current plan fingerprint
  PlanReady             True    Published         2026-09-16T06:01:00Z  Exact plan bytes are stored in immutable chunks
  Suspended             False   Active            2026-09-16T06:00:00Z  Reconciliation is active
  Applying              False   JobCompleted      2026-09-16T06:00:00Z  Apply Job completed; convergence observation is pending (written for generation 1)

kubectl ptah plan storefront -n application prints the SQL this plan holds.
kubectl events -n application --for ptahschema/storefront prints its Events.
```

The lines under `Phase` say why the schema is where it is, in the controller's
own words. `Ready` comes first, whatever its status, because its reason is the
one that names what holds the schema; a `Blocked`, `Suspended` or
`ReconciliationFailed` condition that is `True` follows it. `Policy` is what
`spec.policy` asks for now, by the names of the fields you would edit, and
`Protected tables` follows it when the spec fences any. `Suspend: requested`
appears when `spec.suspend` is set, apart from the `Suspended` condition that
says whether the controller has acted on it. `Status for` appears when the
status was written for an older generation of the spec than the one stored.

Under the plan, `ApprovalRequired` is the controller's verdict on that plan, in
either status: `True` waits for an approval and its message says what to
create, and `False` needs none or is refused by the policy. A destructive plan
under `allowDestructive: false` reads:

```text
Phase:                Blocked
Ready:                False, PolicyBlocked: Plan is blocked by destructive-change policy
Policy:               apply OnApproval, allowDestructive false, driftSeverity all
...
Plan ptah-plan-4b44084123f0958600629632, 4 statements, stored 2026-09-16T06:01:00Z
  destructive
  not approved
  ApprovalRequired: False, DestructiveChangesDisabled: Plan contains destructive changes and policy disallows them; read them with kubectl ptah plan, then either narrow the desired schema or set spec.policy.allowDestructive
```

`Approvals` lists the `PtahSchemaApproval` objects that name this schema by
name and UID, newest first. It shows the five newest and, when there are more,
how many, and the `kubectl get` that lists every approval in the namespace.
Each shows the identity admission stamped on it, the plan it names and whether
that is the current plan or the one the last confirmed apply ran, and every
condition the controller wrote on it -- `Accepted`, `Consumed` and `Stale`,
with the reason and the message. An approval of an earlier plan stays
listed, so why it was not used, or what it was used for, is still there to read
after the plan changed. `kubectl describe ptahschemaapproval <name>` shows one
approval whole.

`Conditions` is every condition the status carries, with the time its status
last changed and its message, including the ones that report nothing wrong:
`Suspended False Active` is a schema nobody suspended, and
`Applying False JobCompleted` is an Apply that finished and is waiting for the
observation that proves it. A condition written for an older generation of the
spec says so after its message.

The last lines name the commands for what this one does not print: the plan's
SQL, and the schema's Events, which `kubectl events` reads and follows with
`--watch`.

A schema read from a ConfigMap or a Git repository prints its source in
place of the `Artifact` line:

```text
Source:               Git https://git.example.com/acme/storefront.git
Ref:                  branch main
Path:                 db/schema.sql
Commit:               3f1c2b0a9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a
Content:              sha256:...
Source policy:        sha256:...
```

`Content` is the digest of the selected files, the content a plan computed
from them is bound to. A ConfigMap source prints `Source: ConfigMap` with the
ConfigMap's name, and `Snapshot`, the copy every operation reads.
`Source policy` appears only when the resource names one.

`Observed: not yet` and `Drift: none` are different answers. The first is a
schema nothing has read; the second is a database that matches its declaration
as far as the login can read it, which on MySQL
[leaves some objects out](../../support/databases/#what-the-login-cannot-see).

A change the drift report has no category for prints
`Drift: found, in no category the report counts; the plan says what changes`.
The report saw a difference and counted nothing, so there is no count and no
severity to show; the plan below names the privilege kinds it changes.

### The reference-data line {#reference-data}

`Reference data` is how far the declared rows sit from the database, in counts:
how many rows the declaration has and the database is missing, how many the
database holds with a different value in a managed column, and how many it
holds that the declaration no longer has.

It carries no key, no column name and no value. Which rows those are is in the
plan, and a data plan contains values, so reading one is data access.

The line appears only when the observation carried a reference-data category. A
schema that declares no rows says nothing here, rather than reporting three
zeroes about a comparison that never ran.

`-o json` prints the same view as a document, including `referenceData`,
`policy`, `approvals` and every condition with its `observedGeneration` and
`lastTransitionTime`, for a script that wants them without parsing text.

Reading a schema needs `get` on `ptahschemas` in the namespace and nothing
else: no plan object, no ConfigMap, and no Secret. Listing its approvals needs
`list` on `ptahschemaapprovals` as well, and that is optional. Without it the
command still prints the schema, says in one line that the approvals are not
shown because listing them is forbidden, and exits 0; `-o json` carries the
same answer as `approvalsUnreadable`, in place of `approvals`.

## A migration, not a plan {#migration}

A `PtahMigration` runs SQL that is already written. There is no generated plan
to print: the statements live in the artifact, and what the operator publishes
is which versions it would run, against which reading of the database's own
history.

```sh
kubectl ptah migration orders -n application
```

```text
Migration:            application/orders
Phase:                AwaitingApproval
Ready:                False, AwaitingApproval: The plan is waiting for the approval its policy requires
Policy:               apply OnApproval
Artifact:             oci://registry.example/acme/orders-migrations@sha256:...
History read:         2026-08-30T12:00:00Z
Current version:      2
Applied:              2
Pending:              1

Last run:             Applied, started 2026-08-28T12:00:00Z, finished 2026-08-28T12:01:00Z
  Job:                ptah-m-apply-orders-6f1c2d3e4a5b6c7d, UID 0b7e6c1a-9d2f-4e3b-8a5c-1f2e3d4c5b6a
  Versions:           2

Plan ptah-mplan-0123456789abcdef01234567, 1 migration from version 2:
  3                    add the orders index
  ApprovalRequired: True, AwaitingApproval: 1 planned migrations need an approval naming this plan

Approvals: none

Run acknowledgments: none

Conditions:
  TYPE              STATUS  REASON            SINCE                 MESSAGE
  Progressing       False   AwaitingApproval  2026-08-30T12:00:00Z  Nothing runs until a person writes the approval
  ArtifactVerified  True    PolicySatisfied   2026-08-30T12:00:00Z  The resolved artifact satisfied its verification policy
  Blocked           False   HistoryMatched    2026-08-30T12:00:00Z  The history continues this artifact
  Ready             False   AwaitingApproval  2026-08-30T12:00:00Z  The plan is waiting for the approval its policy requires
  ApprovalRequired  True    AwaitingApproval  2026-08-30T12:00:00Z  1 planned migrations need an approval naming this plan

kubectl events -n application --for ptahmigration/orders prints its Events.
```

The lines under `Phase` and the plan's `ApprovalRequired` line read as they do
for a schema. `Policy` is `spec.policy.apply`.

`Last run` is the most recent execution: its outcome, when it started and
finished, the Job it ran as by name and UID, the versions it applied, where it
stopped when a statement failed there, and its message. The UID is what tells a
later Job under the same name apart from this one.

A run that ended `Partial` or `Unknown` is also recorded as unresolved, and the
view prints that record as a block of its own, because it is a hold and not an
outcome: while it stands no plan is published and no Apply is dispatched,
whatever the last run and the conditions say, and the reads that could clear it
go on. Here the dispatched Job went missing:

```text
Last run:             Unknown, started 2026-09-30T06:30:36Z, finished 2026-09-30T06:30:43Z
  Job:                ptah-m-apply-orders-04d370d4ccc65a4c, UID 2cde010b-1e46-46af-a3fb-35a75edacc7f
  Message:            the dispatched Apply Job is missing and will not be recreated

Unresolved run:       Unknown, recorded 2026-09-30T06:30:43Z
  Operation:          sha256:9d8a95ebef8319572f9c4f38853a5a4815316e07f83f56865f9f7cab742dcded
  Job:                ptah-m-apply-orders-04d370d4ccc65a4c, UID 2cde010b-1e46-46af-a3fb-35a75edacc7f
  Plan:               ptah-mplan-8f517ba4f0200f2a4e095b65, UID 04800e23-c41a-40ff-9745-c77de45237b1
  Database:           sha256:dd3598d94799d326af49fb6f455ea24c3959af067ca4cff5219f1987480e4abd (target identity digest)
  While this record stands no plan is published and no Apply is dispatched;
  resolve, verify and the history read go on.
  The database is the one the run reported reaching or, where it reported
  nothing, the one it was dispatched against: where to start looking, not
  proof of where the run went. A reader who inspects the wrong database finds
  nothing wrong and settles a record that was telling the truth.
  Establish what the run did there first. A history read of that database that
  finds nothing of the artifact left to apply clears the record by itself. If
  the database is accounted for and still has work pending, settle it with a
  PtahMigrationRunAcknowledgment naming migration UID
  5b0e2a8c-1d3f-4e6a-9b7c-2f4d6e8a0c1b and the operation above.
  The operations guide has the procedure, under "A migration run nobody
  accounted for".
```

It names the attempt, the Job, the plan the run was carrying out and the
database it addressed. That database is the one the run reported or, where it
reported nothing, the one it was dispatched against, so it is where to start
looking rather than proof of where the run went. A resource that is suspended,
or refused for its engine or its realm, never reaches the history read that
would clear the record, and the block says so; an acknowledgment still settles
it. The block ends with what the operations guide says to do:
[A migration run nobody accounted for](../operations/#a-migration-run-nobody-accounted-for)
has the procedure, and the
[acknowledgment](../operations/#clear-unresolved-run) it describes takes the
migration UID and the operation printed here. Nothing in the view offers a
retry, because the operator never makes one. Once the record is settled,
`Settled run` says by what and, for an acknowledgment, by whom.

`Approvals` lists the `PtahMigrationApproval` objects that name this migration,
the way the schema view lists its approvals. A migration approval carries only
`Consumed`, written when its plan is dispatched, so one with no condition says
just that; whether the plan it names is the current one is printed beside it,
and whether it matches that plan is the migration's own `ApprovalRequired`.
`Run acknowledgments` lists the `PtahMigrationRunAcknowledgment` objects, each
with the operation it names, marked when that is the unresolved run, and its
`Consumed` or `Stale` condition.

A ConfigMap or Git source prints the same lines as
[`kubectl ptah schema`](#schema) in place of `Artifact`.

`-o json` prints the same view as a document, for a script that wants the
pending count, the last run's outcome and Job, or the unresolved run without
parsing text.

What it never prints is SQL or a table row. A migration plan records versions,
descriptions and checksums; reading the statements means reading the artifact,
which this command does not fetch. The same is true of reference data: a row
this operator writes is never printed back to you here, in the status, or in an
Event.

Three states are worth recognizing in the output:

- **Dirty** -- a failed or interrupted run left a revision row behind. Nothing
  applies while one exists, and the operator never removes it.
- **Modified** -- an applied migration's file no longer accounts for it, and the
  versions are listed. This is the refusal a versioned workflow exists to make.
- **Last run: Partial** -- a migration committed some of its statements and not
  the rest. Retrying the file would run them twice, so the operator stops,
  records the run under **Unresolved run**, and the recovery is yours to
  choose.

Reading a migration needs `get` on `ptahmigrations` and `ptahmigrationplans` in
the namespace, and nothing else -- no chunk, because a migration plan stores no
SQL of its own. Listing its approvals and run acknowledgments needs `list` on
`ptahmigrationapprovals` and `ptahmigrationrunacknowledgments`, optional the
same way as for a schema: a list the reader may not read is one line saying so,
and the command still exits 0.

## What it needs to be allowed to do

Reading a plan is reading three kinds of object in one namespace, all of them
this operator's, through one rule. No Secret, no ConfigMap, no Pod log, no
`pods/exec`, no Job, and nothing cluster-wide:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ptah-plan-reader
  namespace: application
rules:
  - apiGroups: [operator.ptah.run]
    resources: [ptahschemas, ptahschemaplans, ptahschemaplanchunks]
    verbs: [get]
```

The rule names no object, so it reads the next plan as well as this one.
`examples/approver-plan-reader-role.yaml` is the same Role with a binding, and
the chart's approver ClusterRole carries the same read.

`kubectl ptah schema` and `kubectl ptah migration` also list the approvals and
run acknowledgments that name a resource, when the reader may. That is one more
rule, and an optional one; a reader without it gets everything else:

```yaml
  - apiGroups: [operator.ptah.run]
    resources: [ptahschemaapprovals, ptahmigrationapprovals, ptahmigrationrunacknowledgments]
    verbs: [list]
```

The chart's approver ClusterRole and `examples/diagnostic-reader-role.yaml`
carry it.

`kubectl ptah approve` reads the resource and its current plan with `get`,
and for a schema the plan's chunks too, to show the SQL. It lists no
approvals, and gets one only to say what holds a name `--name` asked for that
is already taken. It creates the approval as you, so it needs `create` on
`ptahschemaapprovals` or `ptahmigrationapprovals` as well. The chart's approver
ClusterRole carries both.

:::caution
The command reads the plan chunks as you, so granting it means granting
`get` on `ptahschemaplanchunks`. The chunks are not secret and base64 is not
protection: anyone who can read them can read the SQL, with or without this
plugin. That is why the database URL is in a Secret and the plan is not.
:::

`kubectl ptah suspend`, `resume`, `refresh` and `set-source` read the resource
and patch it as you, so they need `get` and `patch` on `ptahschemas` or
`ptahmigrations`:

```yaml
  - apiGroups: [operator.ptah.run]
    resources: [ptahschemas, ptahmigrations]
    verbs: [get, patch]
```

RBAC grants `patch` on the whole object, not on one field: whoever may suspend
a resource or change its artifact may also edit the rest of its spec, with
`kubectl edit` or any other client. `examples/desired-state-author-role.yaml`
carries these verbs for the people who own desired state. The chart's approver ClusterRole does not, and
the read commands do not need them. `kubectl ptah wait` needs `get` on the
resource and nothing else, and `approve --wait` reads with the same `get`
approve already has.

## How the plan is stored

Worth knowing when you are diagnosing the store rather than reading a plan.

A plan document is split into 512 KiB chunks by bytes, up to 8 MiB in total.
The split is of the serialized document, so a boundary falls wherever 512 KiB
falls -- possibly inside a SQL string, inside a JSON escape, or inside a
multi-byte character. Each chunk is an immutable `PtahSchemaPlanChunk` the plan
owns; the plan's `spec.chunks` binds every one by name, index, size and digest,
`status.publishedChunks` by UID, and `spec.contentDigest` binds the whole
document.

A plan that was applied is also in the ConfigMaps its Apply Pod mounted: the
operator copies the chunks into immutable ConfigMaps of the same names just
before it creates the Apply Job, because the Pod holds no Kubernetes
credential and the kubelet mounts no custom resource. Those copies are
transport, not a second place to read a plan from, and a plan that was never
applied has none.

Reading one chunk on its own is therefore not reading a plan, and decoding
chunks by hand is reproducing bindings that already exist. The command does
what the operator does: read every chunk, check each against its binding,
concatenate in index order, check the whole against the content digest, and
only then parse.
