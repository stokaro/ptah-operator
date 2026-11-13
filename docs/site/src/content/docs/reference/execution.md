---
title: Execution and coordination
description: One operation from claim to evidence, and how resources addressing one database take turns.
---

An operation is a durable claim, a Job, a persisted result and an outcome
somebody can account for. This page follows one from end to end, then says
how two resources addressing the same database avoid each other. The
lifecycles that decide which operation runs next are
[Reconciliation](../reconciliation/).

## One operation, end to end

```mermaid
sequenceDiagram
  participant C as manager
  participant S as PtahSchema status
  participant K as Kubernetes API
  participant J as operation Job
  participant P as Ptah in the Pod
  participant R as result receiver

  C->>S: write the operation claim
  Note over S: named before the Job exists
  C->>K: read ServiceAccount, LimitRange,<br/>RuntimeClass, PriorityClass
  C->>S: persist the admission snapshot digest
  C->>K: mark an Apply's approval consumed
  C->>S: persist dispatchStarted
  Note over S: the one permitted create
  C->>K: create the Job
  K-->>J: schedule the Pod
  Note over J: webhook compares the post-mutation<br/>Pod against the snapshot
  J->>P: runner starts Ptah
  P-->>J: machine-readable output
  J->>R: operation-bound result over TLS with Pod token
  R->>K: persist intent, chunks, completion
  R->>K: validate persisted result
  R-->>J: durable receipt
  C->>K: background consumer reads result
  C->>S: record the outcome
  C->>K: schedule an Apply's cleanup TTL
```

The claim is written before the Job exists. That is what lets the controller
tell a Job it created from one it has not created yet, and what lets admission
refuse a Job no claim asked for.

Between the claim and the create sit two more durable boundaries. The
**admission snapshot** binds the exact ServiceAccount, LimitRange defaults,
RuntimeClass scheduling and overhead, PriorityClass values and configured
built-in admission behavior, by UID and resourceVersion; its digest travels in
the Job and Pod annotations, and a fail-closed webhook compares the final
post-mutation Pod against it before scheduling. The **dispatch boundary** is
persisted immediately before the one permitted create attempt: after it, an
Apply Job that is missing or replaced is outcome-unknown and is never
recreated.

Three durable claims live in status, and they are independent on purpose:

- `status.activeOperation` — the operation in flight, and the serialization
  point for one resource.
- `status.pendingObservation` — the post-Apply verification owed after an Apply
  may have mutated the database. It outranks phase changes, ordinary retries and
  newer desired generations, and it snapshots what that verification needs: the
  applied plan,
  the key-free target selector, the coordination digest, the observation policy
  including the protected-table refusal, the Apply holder, the Lease epoch and
  duration, and the
  admission snapshot. A namespace-wide exact-owner Pod scan binds attempts by
  Job name and UID rather than by mutable labels; at most eight Pod UIDs and
  the Pod count are retained as bounded evidence, and a late or duplicate Apply
  Pod invalidates a verification already in flight.
- `status.pendingLockRelease` — the exact Lease owner and epoch, kept until an
  idempotent release succeeds. It closes the window between a terminal status
  transition and clearing an owner-neutral Lease.

After an execution-binding rotation a `PtahSchema` also carries
`status.pendingBindingRetirement`, which names what the retired epoch left
behind: the plan whose approvals are still to be marked stale, and the Job its
claim dispatched, until that Job has stopped and its cleanup is scheduled. It
authorizes nothing, and no further rotation starts while it is present
([Retire an execution binding](../mutation-lifecycle/#retire-an-execution-binding)).

A read-only Job is created with its cleanup TTL, 300 seconds, which Kubernetes
counts from when the Job finishes. It removes nothing still running, and it
removes a finished read whether or not the controller has read its result: a
read whose Job is gone before then runs again under a new Job. An Apply's Job is
created with none. It stays the active operation until the controller has both
read its result and scheduled its bounded cleanup TTL, so a transient API or
RBAC failure retries the transition instead of orphaning the Job. That TTL is
the one field the manager may add to a Job it already created, and no result is
read before the Job carries its terminal condition. It is 300 seconds, or 86400
for an Apply that failed or whose outcome is unknown, when a person approved it or its
migration run stopped the resource, whose Pod log
[holds what the database answered](../../use/operations/#failed-apply-log).
A read-only Job a release before 0.2.11 created carries no TTL, and gets the
300 seconds the same way.

### Durable result protocol

Each Job fixes the result endpoint, operation generation, and credential
projection before admission. The runner uploads one bounded canonical result;
the receiver authenticates its operation, Job and Pod rather than trusting
identity fields in the body. It validates length, digest, protocol and every
chunk, then commits and reads back the complete publication before acknowledging.
An interrupted publication is incomplete until that boundary. Identical retries
return the same receipt; conflicting bytes are refused. A lost acknowledgment
causes delivery retry, never another Ptah execution.

Both controllers use bounded background readers for these records. Payload
upload and result loading do not occupy their reconcile workers. The accepted
record survives removal of the producing Pod and logs, manager replacement,
and process-key loss. There is no fallback to logs or a termination summary
when a durable receipt is missing or invalid.

SQL execution and result persistence are separate transactions. If a runner
dies after mutation and before publication, the outcome can still be unknown.
The existing observation/history recovery and fresh-approval rules apply.

### Legacy result reads

The following log-read behavior applies only when durable delivery is disabled
for a Job. It is outside the default 0.2.0 qualification profile.

Reading that result is the one blocking call a reconciliation makes against
something other than the API server's own store: a pod/log request the API
server proxies to a kubelet, streaming a body whose size the executor decides.
Each family runs one reconcile worker and leader election admits one manager,
so a response that stops arriving would hold every other resource of that
family behind it, including the passes that renew the Lease of an Apply that is
still executing SQL. The read carries a deadline of its own: sixty seconds, two
thirds of the shortest Apply Lease the API can produce, or the Lease the
operation itself holds where that is shorter. The worker blocked on a read is
the worker that owes every other resource of the family its Lease renewals, so
a read may hold it for no longer, and has to give it back with time to spend --
a bound equal to the whole Lease would let a read beginning just after a
renewal run until the moment that Lease expired. The Lease is counted whole
rather than less its grace, because the grace is what makes a Lease outlive its
Job and not time withheld from reading the result afterwards.

Sixty seconds still clears what a legitimate result needs, which is about fifty
for the largest frame the protocol admits at a floor of a mebibyte a second.
The two constraints meet close together, and where they conflict the Lease
wins: a maximum-size frame on a slower link times out and is retried, while a
renewal that arrives too late lets another family take a realm whose SQL may
still be running.

Bounding the read shortens the window in which one resource's reconcile holds
up another's and does not close it. Closing it means taking the read off the
reconcile path or giving the family more than one worker. The second bound is the one that matters
at the low end -- the API accepts a deadline of thirty seconds, and spending
two minutes reading the result of thirty seconds of work holds the worker for
longer than the operation it is reporting on.

This bound is not what keeps the realm safe. A read is read-only, and a read
that outlives its Lease authorizes nothing: the epoch is checked before the
result is used, and a Lease that changed hands retires the operation and
discards the result. The deadline makes the read proportionate; the epoch makes
it safe.

That duration is taken from the claim, never from the spec. A Lease is renewed
at the duration the claim recorded -- for the verification after an Apply, the
one `status.pendingObservation` copied from it -- and `activeDeadlineSeconds` can
be raised afterwards without lengthening a Lease already held.

A timed-out stream waits sixty seconds after the timeout before another read
can start. The resource still reconciles every five seconds and renews its
Lease during that pause. Job events cannot bypass the pause: it belongs to the
Pod UID, so an independent resource or a replacement Pod can still read its
own result. This gives independent Jobs time to finish and be harvested between
blocking attempts. Several stalled Pods can still consume the family's worker;
the pause bounds repeated attempts by one Pod, not the number of faulty nodes.

A read that fails decides nothing by itself: the claim, the Lease and any
record of an unresolved run are left exactly as they were, because a log this
manager could not read says nothing about what the database now holds. A read
that ran out of time, or failed in any other way that may pass, leaves its resource
requeued at a fixed short interval rather than raising a reconcile error, because the
queue's own backoff climbs past the headroom the deadline was chosen to leave,
and a terminal Job produces no further event to bring the resource back with.
Each failed attempt is reported as `ResultReadTimedOut` or `ResultReadFailed`.
Polls that wait for the next attempt do not emit another failure Event.

What ends the wait is the log being gone. Container garbage collection, a
deleted node and a node that stays away all take the log while the Pod object,
and the termination summary in its status, remain. The API server says so
plainly for a deleted node (a NotFound for the Node) and for a container the
kubelet can no longer serve (a BadRequest), and such a log is given up at once.
An executor the kubelet ended with reason `ContainerStatusUnknown` has no
container left to serve, so its log is given up without a read.
A failure that may pass -- a kubelet it cannot reach, a read that ran out of
time, a kubelet that does not know the Pod yet -- is given up once reads of that
log have failed for two whole read budgets, two minutes, as this process
watched them; a manager restart starts that measure again, which can only make
it wait longer. Once the kubelet has begun answering, a garbage-collected
container or a removed log file ends the stream with the reason as its body,
which reads as a log that holds no frame. Either way a lost log is judged as a
log with no frame: a read-only operation is retried, a schema Apply is unknown,
and a migration Apply is settled from its termination summary when one stands
in. A Pod that is itself gone takes its status with it, and is unknown as
before.

One case adds the field to a Job that is still running, and both admission
layers name it: losing database lock continuity during an Apply retires the
operation at once, and the Job left behind would otherwise hold its whole
deadline with nothing left to schedule its collection. The timing changes
nothing -- Kubernetes starts that timer when a Job finishes either way -- so
the exception is written for a schema Apply and for no other shape.

## Which runner a Job accepts

The runner is installed from `execution.runnerImage`, and nothing but the
installation ties that image to the manager. Every runner container of a Job --
the one that starts the executor and the guard that authorizes OCI access --
is told the protocol its manager speaks in `PTAH_RUNNER_PROTOCOL_VERSION`. A
runner of another protocol refuses the Job before the executor starts.
With durable delivery, it exits with code 2 and writes the diagnostic
`runner_protocol_mismatch` before reading delivery credentials or contacting
the receiver. It cannot publish a result under the Job's foreign protocol.
Without a receipt, the manager treats an Apply outcome as unknown and uses
read-only database recovery; it never uses diagnostic logs to authorize replay.
A guard that refuses stops the Pod before the fetch that uses the registry
credentials.

In legacy log delivery, the refusal frame carries only the operation, the
error code and the runner's own protocol version. The manager recognizes that
bounded cross-version document as `RunnerProtocolMismatch`.

## Meshes and policy engines

Every operation Pod is held to its Job template: the Pod-intent webhook admits
a Pod whose labels equal the template's, whose annotations equal the
template's apart from the one LimitRanger writes, and whose spec is inside the
admission snapshot. That is the invariant that keeps anything unplanned from
running beside the database credential, and it is also what a service mesh, a
policy engine or a managed platform runs into: a mutating webhook that injects
a sidecar, adds a label or rewrites a resource request produces a Pod the
webhook refuses, and a validating policy that requires a label or an
annotation the template does not carry refuses the Pod before it is scheduled.
Either way the Job controller cannot create the Pod, and until #447 the
resource said only that an operation was in progress.

The contract is `spec.execution.podMetadata`: the labels and annotations the
operation Pods carry beside the operator's own. The builder writes them on the
Job and on its Pod template, the admission snapshot's template digest binds
them, and the webhook admits exactly the Pod they describe. So the cluster's
policy and the operator's meet in the declaration:

- A mesh with namespace injection is opted out of, per Pod, by the annotation
  it reads -- `sidecar.istio.io/inject: "false"` for Istio,
  `linkerd.io/inject: disabled` for Linkerd. A sidecar cannot run beside the
  credential; the opt-out is what the mesh offers for exactly that.
- A policy engine that requires a label or an annotation on every Pod is
  satisfied by declaring it. One that mutates every Pod to add one is
  satisfied by declaring the same key with the same value, so the mutation
  changes nothing and the Pod still equals its template.
- A platform that mutates the Pod spec -- a sidecar, a changed resource
  request, an injected volume -- is refused, because what runs beside the
  credential is not negotiable. Opt the operation Pods out of that mutation
  with the metadata the platform reads, or exclude them by namespace.

The declaration is bounded. Each map takes at most 16 entries; a key is a
Kubernetes qualified name, a label value is what Kubernetes accepts for one,
and an annotation value is at most 1024 bytes. Keys under `ptah.run`,
`kubernetes.io` and `k8s.io`, and any subdomain of them, are refused: they
hold the operator's own labels and annotations, the Job controller's tracking
labels, `app.kubernetes.io`, which every object the chart owns selects on,
and the annotations the API server translates into a Pod's security context.
The bare `controller-uid` and `job-name` keys are refused for the same
reason. The CRD refuses these before the resource is stored, the builder
refuses them again, and the Job write guard refuses a Job that carries one
beyond the operator's envelope whoever built it. Unset, nothing changes: a
resource that declares no metadata dispatches the Pods it always did.

Declaring metadata is a spec change like any other. A read-only operation in
flight is discarded and claimed again with the new template; an Apply that
has dispatched runs to its result under the template it was dispatched with,
because the claim, not the spec, is what a running Job is held to. A plan
does not retire over it: the metadata says nothing about what the plan does
to the database, and the plan fingerprint does not read it, so an approval
stands. Editing the declaration does change what the Job carries, so the Job
intent the controller-write guard rebuilds and compares carries it too.

When a Pod is refused anyway, the resource says so. The Job controller
records a `FailedCreate` Event against the Job with the API server's refusal,
and the operator reads it for a Job that stands with no Pod and reports it as
the condition `Ready=False` with reason `PodAdmissionRefused` on a
`PtahSchema`, `Progressing=False` with the same reason on a `PtahMigration`,
with the refusal in the message and a `PodAdmissionRefused` Event beside it.
The claim stands and the Job keeps its deadline: the Job controller keeps
trying, so a policy that stops refusing lets the Pod through and the
condition goes back to an operation in progress, and a declaration added to
the spec takes effect on the next claim. The message is the API server's own
text, bounded and stripped of control characters; it names the policy or the
webhook and what it wanted, and nothing the operation holds.

A Job still refused at its deadline fails with no Pod, and the run ends with no
result. The failure it records keeps the refusal after it as what was last
reported: `ptah runner result frame not found; last reported before that: Job
<the Job> cannot create its Pod; the API server refused it:` and the API server's
text. With no Pod there is no log, so neither the record nor the Event points at
one. A refusal the Job got past is not carried: once the Job counts a Pod, in any
state, the failure is about that Pod. An Apply that ends another way first keeps
it the same way, on the endings
[Before the Secret exists](../credentials-and-admission/#external-secret-stores-not-synced)
lists for the kubelet's words.

A Pod that exists and cannot start because the kubelet cannot configure its
container reads the same way, with reason `PodConfigurationError` and the
kubelet's words in the message: a [Secret or a key that does not exist
yet](../credentials-and-admission/#external-secret-stores-not-synced), usually.

## When a Pod is stopped

A node drain, a preemption, an eviction or a Pod deadline stops an operation
Pod the way Kubernetes stops any Pod: SIGTERM to the container, then SIGKILL
once `terminationGracePeriodSeconds` has passed. The runner is the container's
first process, so the signal reaches the runner rather than Ptah. It passes
SIGTERM on, gives Ptah two thirds of the grace to stop, kills it if it has not,
and keeps the last third to deliver the result. Every operation Pod gets
thirty seconds. A schema Apply records its grace on the claim, and every
mutating Pod is told its grace in `PTAH_TERMINATION_GRACE_PERIOD_SECONDS`, so
the runner never sizes the wait against a grace its Pod does not have; a
mutating Pod that is not told is refused before Ptah starts.

Ptah answers SIGTERM by canceling the statement it is running, which rolls
back a migration it runs in a transaction, and then reads the history and
writes its account of the run. A run stopped between two files, or inside one
that rolled back, reports a failed run with the versions it applied, which the
history read that follows confirms, instead of a run nobody can account for.
One stopped inside a statement that commits on its own reports an unknown run:
a statement outside a transaction (`-- +ptah no_transaction`, or
`spec.transactionMode: none`), or a DDL statement on MySQL. Ptah marks such a
statement in flight before it runs, and once it stops waiting it cannot tell
whether the server applied it. The account names the versions before it and the
statement it stopped in, and the resource blocks until a person checks the
database and [acknowledges the run](../../use/recovery/#failed-pending-row). A child that
does not stop in time writes nothing, and the run is unknown. A schema Apply
that was stopped is unknown either way, because its account is only ever the
whole frame of a run that completed.

Mutating Pods carry `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`,
so the cluster autoscaler does not remove their node while they run. Nothing
else reads that annotation: a drain, a preemption and node-pressure eviction
still stop the Pod. Whatever stops it with less than its own grace can kill the
runner before its result is persisted, and that run is unknown as it always was:
hard node-pressure eviction gives the Pod the kubelet's minimum of two
seconds, soft eviction caps the grace at the kubelet's
`evictionMaxPodGracePeriod`, and a deletion can ask for a shorter one.

### The termination summary

Durable Jobs retain the bounded termination summary for diagnosis. It cannot
replace a missing durable receipt. The fallback below applies only to legacy
Jobs that deliver results through logs.

The legacy frame lives only in the container log on the node, and that log can be gone
before the manager reads it. So the runner also writes a summary of the frame,
at most 2 KiB, to `/dev/termination-log`, which the kubelet copies into the
Pod's status: the operation id, whether a mutation started and whether its
outcome is uncertain, the error code, the realm and target identity digests,
a migration run's outcome with the count and the first and last applied
versions, and the SHA-256 of the frame it summarizes. It carries no plan, no
error text and nothing Ptah printed.

The migration controller reads it for an Apply, and only when the log holds no
frame, ends inside the frame the summary names, or is gone, once the window in
which a frame may still be arriving has passed. A frame that is
there and was refused is never replaced. A summary that names another attempt
or another frame is set aside with a `TerminationSummaryRefused` Event, and the
run is unknown as before. What a summary says is decided by the same code that
decides a frame, including the check that the run reached the planned
database, so it can report less than the frame would have and never more.

Neither family narrows an unknown outcome on a Pod's report that no mutation
started, not even from a frame: a Job may run more than one Pod, and one Pod's
account of itself says nothing about another. So what a summary adds is a
migration run's outcome and nothing else. The schema family and read-only
operations do not read it, because neither can be decided from less than the
whole frame.

## Concurrency and coordination

One operation claim per resource prevents two Jobs for one resource. Across
resources, coordination is by **database realm**: a Lease keyed by a SHA-256
digest of a versioned tuple. A resource names its realm one of two ways. A
`spec.target.coordinationKey` is hashed with the normalized engine and the
resource's namespace, so it names a realm inside that namespace and nowhere
else. A `spec.target.realmRef` names a cluster-scoped `PtahRealm`, hashed with
the normalized engine and the realm's name, which is the same from every
namespace. Either way the name is non-secret and stable, and every resource
that can mutate one database must use the same one, including resources that
reach it through different DNS aliases, proxies or credentials. The plaintext
key stays in spec; status, plans, approvals, Jobs and Leases carry only the
digest.

A `PtahRealm` is an administrator's grant: it lists the namespaces that may
claim it and says whether more than one claimant may share it. A resource the
realm does not admit is refused with reason `RealmNotAuthorized` before any Job
and is left out of every other claimant's census, so a claim nobody granted
cannot contest a realm. A key needs no grant, because it cannot reach past its
namespace.

Two resources claiming one realm is refused rather than queued unless every
claimant declares `spec.target.sharedRealm`, and, for a `PtahRealm`, unless the
realm's `sharing` is `Shared`. The census counts `PtahSchema` and
`PtahMigration` together — a suspended or dormant resource claims nothing — and
the refusal names the conflict instead of letting two owners discover each
other through a lock.

The `targetIdentityDigest` is a separate redirect guard inside one plan
lifecycle. It binds the effective route, database name, username, role, SQL
namespace, semantic session options and non-secret transport and
authentication policy immediately before Apply. Password bytes and timing
controls are deliberately excluded, so credential rotation and liveness tuning
stay possible; certificate or CA bytes may rotate behind an unchanged bound
path without authorizing a transport downgrade. Default ports and equivalent
spellings of one address are normalized; DNS absolute-name markers, address
families and MySQL database-name casing are preserved; ambiguous repeated scope
parameters and multi-endpoint targets are rejected.

Both families are held to it in the same place, and by the same code: the
runner refuses to start a mutating child unless the Pod's own database URL
still hashes to the realm and the route identity its approved plan named, and
unless the claim's absolute dispatch and execution deadlines are both present,
ordered and unexpired. The deadline check is repeated immediately before the
child is executed, because a Pod scheduled late or resumed after an
interruption can cross its window while the plan is being prepared, and the
execution deadline is imposed on the child's own context. At that deadline the
child is asked to stop exactly as it is when its Pod is stopped
([When a Pod is stopped](#when-a-pod-is-stopped)), and killed if it is still
running two thirds of the Pod's grace later. A run outlives its deadline by at
most that, which is inside the whole grace the controller waits out before it
reads the database after an Apply it cannot account for. A migration Apply
carries two more bindings, since its child selects its own work: the approved
ordered sequence with its digest, and the
fingerprint of the history that sequence was computed against. The runner
cannot re-derive either — it holds no artifact and reads no revision table — so
it enforces that a migration child no plan authorized never starts, and hands
the sequence, digested again, to `ptah migrations up --expect-sequence`. Ptah
compares it with what it selects under its own migration lock and refuses
before it changes anything when the two differ, so the executed sequence is
the approved one or nothing: a history that moved after the approval, forwards
or backwards, reads as a failed run with nothing applied.

The reads that prove a schema Apply are held to the same route identity. The
Observe and the Plan that a `PtahSchema` runs after an Apply carry the digest
its plan named, and the runner refuses either one before it starts the child
when the Pod's URL hashes to another, with the digest the URL gave in its
result. A proof whose Secret moved therefore never logs in to the new target.
No other read carries the binding: an ordinary read is how the operator learns
where a URL now points.

All resources use one configurable coordination namespace, so resources in
different namespaces still contend for the same database. The manager's
leader-election Lease lives there too. Replicas of the one supported Helm
release form a single ownership domain with exactly one active reconciler,
while webhook servers stay available on every ready replica. Managers watch
cluster-wide, and the fixed admission configurations form a singleton
availability domain: exactly one release per cluster is supported, and high
availability comes from replicas within it.

The Lease complements the database's own advisory lock. Its immutable duration
covers the maximum Job deadline plus grace. A schema renews the same holder
through its post-Apply verification, including retry delays, and where Job
creation or identity is uncertain that verification waits until the Apply's
execution deadline and the Pod's termination grace have passed, so a possibly
unobserved mutating Pod cannot overlap it. A migration hands the Lease back
once nothing its claim dispatched can still write, and reads its history
afterwards without one; why that is safe is
[Mutation lifecycle](../mutation-lifecycle/#release-coordination).

The executor's advisory lock, its authoritative inspection and the target DDL
share one physical database session. Losing that session aborts the operation
rather than letting pooled DDL continue after the database has released the
lock. The fault-injection suite proves it for the schema Apply on both engines:
on PostgreSQL by joining the advisory lock and the blocked relation lock to one
backend in one database, and on MySQL by binding the named-lock owner to the
metadata-lock waiter.

Raw drift is advisory on purpose: its selector language is not reused as a
planning scope, and its detail never authorizes an Apply. The authoritative
Plan uses the exact `spec.policy.exclude` scope twice, saves each read with
`schema plan --output`, requires the two files to be byte-identical, and passes
those bytes through `schema apply --dry-run`, whose report must name their
digest and list their statements in order. Each step is read from the JSON
document Ptah prints under `--json`, never from the text it writes for a
person. Apply reads its own report on every exit status, and only `applied`
naming the approved plan's digest is a success. A `stale-plan` refusal is
reported as such, and every Apply failure after dispatch, that refusal
included, stays outcome-unknown until an observation settles it.

## What a read does to the database

A `PtahSchema` Plan, and the Observe that proves an Apply, hold the database
realm Lease while they run, and neither writes to the database. Nothing
enforces that at the database: the session is not a read-only transaction, and
the login is the one the resource names, not a read-only role. What keeps a
read from writing is the command the runner runs, one fixed command line for
each operation, and the audit below measures what those commands send. The
runner opens no database connection of its own; it starts Ptah with the target
URL in its environment. The behavior below was read from the source of the Ptah
build [`support/ptah.json`](../../support/ptah/) pins, v0.23.0 at commit
`f47b2bfa`.

| Operation | Runner command | What reaches the database |
| --- | --- | --- |
| Observe | `ptah schema drift --format json` | The comparison of the live schema with the desired one: catalog queries, the managed-data comparison's reads, and the probes below. |
| Plan | `ptah schema plan --output <file> --json`, twice, and the two files must be byte-identical | The same comparison, once for each file. A dev URL names the schema an unqualified file loads into, and the saved plan provisions nothing on it. |
| Plan, then | `ptah schema apply --plan <first file> --auto-approve --json --dry-run`, with no schema file, dev URL or protected-table list in its environment | The apply's preview lock, a check that the plan still fits the target, and a return before any statement of the plan runs. A plan with no changes stops before this step. |
| History, for a `PtahMigration` | `ptah migrations status --migrations-dir <dir> --json` | Catalog reads that find the revision table and its engine, on MySQL the table's definition and one session variable, and a `SELECT` of the revision rows. |

Resolve and Verify, in either family, run `ptah oci resolve` and `ptah oci
verify`, and open no database session. A `PtahMigration` runs no Plan Job.

History creates nothing and takes no lock. `ptah migrations status` puts the
connection's schema writer in dry-run mode before it reads
(`dbcli.SuppressSchemaWrites`), so the step that would create a missing
revision table only looks (`initializeDryRun`), and a database without the
table reads as one with no migration applied. The command has no lock option
and takes neither the migration lock nor the schema one, so the
`PTAH_MIGRATION_LOCK_TIMEOUT` its Job carries goes unused, and it opens no
transaction. When the command fails, the runner runs it again against
`sqlite:///:memory:` to name an unreadable migration file; that run reaches no
database of yours. A migration read holds no Lease either.

The comparison asks the server to spell some things itself. On PostgreSQL it
creates a throwaway object in a transaction it rolls back, so a CHECK or a view
body compares in the server's own spelling. A column's type and default are
read back from a temporary table, which takes `TEMPORARY` on the database, as
[the databases page](../../support/databases/#postgresql-authority) lists. On MySQL it creates a `TEMPORARY`
table to learn which non-ASCII index names collide, and drops it again; a
temporary table belongs to its session and goes with it. Neither persists
anything, and the PostgreSQL probe is why the session cannot be read-only:
PostgreSQL refuses `CREATE` in a read-only transaction, temporary objects
included.

### The SQL audit and its limits

The acceptance suites record every statement a read's session sends to an
isolated database, on PostgreSQL and on MySQL, and hold each one byte for byte
to an exact list. For a `PtahSchema` the list is
`test/e2e/schema-sql-contract.json`, matched by `test/e2e/schema_sql_audit.go`:
the statements Observe, Plan and a stale Apply send for the acceptance schemas,
captured from Ptah `f47b2bfa`, each with the operations that may send it. For
History the lists are in `test/e2e/migration_sql_audit.go` and
`test/e2e/migration_mysql_audit.go`, and what History sent at `f47b2bfa` is
recorded in `testdata/e2e/readings/postgresql-migration-history-audit.md` and
`mysql-migration-history-audit.md`: reads only, no table created. A statement
from a read's Pod that its list does not hold fails the suite.

The audit reaches as far as the fixtures do. It holds the statements the
acceptance schemas and migrations produce and says nothing about any other: a
declaration they do not contain can send a statement no audit has seen. The
MySQL probe for non-ASCII index names is one, and so are the CHECK, view and
trigger probes on PostgreSQL for objects the fixtures leave out. Both History
lists also admit the `CREATE TABLE IF NOT EXISTS` of the revision table
(`postgresHistoryCreate`, `mysqlHistoryCreate`), because each list governs
History and the migration Apply that sends it, so that History does not send it
rests on the recorded readings and on Ptah's source, not on the acceptance run.
A revision table under another name or schema is outside both.

### What a read can still hold

A read cannot change the database, and nothing uses what it reads: the
[result receiver](#durable-result-protocol) authorizes only the Pod of the Job
the live claim names, and the operator reads a result only for that Job. What a
read can still do is hold things on the server for as long as its session
lives. That matters for a read whose Pod outlives its claim, on a node the
operator stopped waiting for ([Suspension and
deletion](../../use/operations/#suspension-and-deletion)). The operator's wait
ends at the Job's stop bound; the Pod and its session end when they end.

The lock is the large cost. The dry run of a Plan with changes holds
`ptah_schema_apply`, the lock every `PtahSchema` Apply takes, from its start
to its end. On PostgreSQL it is a session advisory lock on the key
`1237737229`, which Ptah asks for with `pg_try_advisory_lock` every 25
milliseconds until `spec.policy.lockTimeout` (30 seconds unless set) runs out.
On MySQL it is `GET_LOCK('ptah_schema_apply', <seconds>)`, the same timeout
rounded up to whole seconds. A PostgreSQL
advisory lock belongs to one database. A MySQL lock name belongs to the whole
server, so on MySQL every database on that server shares the lock. Observe,
the proof included, History, and a Plan with no changes take no lock, and a
migration Apply takes its own, `ptah_migrate`, so none of them waits on this
one.

How long the server keeps the lock depends on what the node lost. A node cut
off from the API server alone still reaches the database: Ptah finishes and
lets go, or the kubelet stops the Pod at its `activeDeadlineSeconds`, and the
session ends with the process. The kubelet enforces that deadline on its own
and counts it from the Pod's start, so the session ends no later than the
Pod's start plus its deadline and termination grace, as `podStopBound`
computes it. The operator's wait counts from the Job's start or from its
`FailureTarget` condition, so the session can outlast that wait by up to the
time the Pod took to start.

A node cut off from the database, or a node that is gone, leaves the session to
the server, which keeps it and its lock until it notices the peer has gone.
Both servers turn on TCP keepalive for every client connection, and the
database host's operating system decides when it fires: PostgreSQL leaves the
timing to it while `tcp_keepalives_idle`, `tcp_keepalives_interval` and
`tcp_keepalives_count` are at their default of 0, and MySQL sets
`SO_KEEPALIVE` and no timing (`vio_keepalive`, called from
`sql/sql_connect.cc`). With Linux defaults an idle connection is dropped after
about two hours and eleven minutes. If the server was sending when the peer
went and the data was never acknowledged, Linux gives up sooner, after
`net.ipv4.tcp_retries2` retransmissions, about 15 minutes by default. A server
setting can end an idle session sooner: on PostgreSQL
`idle_in_transaction_session_timeout` for a session inside a transaction and
`idle_session_timeout` for one outside, both off by default, and on MySQL
`wait_timeout`, eight hours by default, which comes first only where keepalive
is off or slower. A managed database may set any of these differently.

While a cut-off session holds the lock, the realm Lease no longer covers it:
the operator let the claim go at the bound, and the database lock is what the
next claimant meets.

- An approved Apply of any `PtahSchema` on that database, and on MySQL on any
  database of that server, takes the Lease once it is free, spends its
  approval when it dispatches, waits `spec.policy.lockTimeout` for the lock and
  fails before it runs a statement. Its outcome is unknown, so it takes a proof
  Observe and, unless `spec.policy.apply` is `Always`, a new approval before
  the plan runs again.
- A Plan with changes there fails when its dry run's wait for the lock runs
  out, and is tried again after `spec.execution.failureRetryInterval`. That
  includes the stale read's own resource, whose read runs again under a new
  Job.

A Pod cut off inside the PostgreSQL probe transaction leaves that transaction
open as well, `idle in transaction`, until the server drops the session. The
window is short, but what it holds is real. Once the transaction has created
its first probe object it has a transaction ID, and while it stays open it
holds back the horizon `VACUUM` cleans up to, so dead rows newer than it stay.
A Pod cut off in the middle of one probe also leaves that probe's locks. Each
probe rolls back to a savepoint once it has read its answer, which releases
what it locked, but until then it holds an `ACCESS SHARE` lock on each table
its declaration names, such as the tables a view body reads. A statement that
needs `ACCESS EXCLUSIVE` on one of them, as `DROP TABLE` and most forms of
`ALTER TABLE` do, waits for it, and the queries queued behind that statement
wait too. The MySQL index-name probe's temporary table stays with its session
and holds nothing another session needs.

To end the hold, end the session. The `ReadJobOverdue` Warning names the Pod
and its node; the server sees the Pod's IP as the client address unless
something between them translates it, and Ptah sets no `application_name`. On
PostgreSQL, the lock's holder:

```sql
SELECT a.pid, a.client_addr, a.state, a.backend_start, a.xact_start
FROM pg_locks AS l
JOIN pg_stat_activity AS a ON a.pid = l.pid
WHERE l.locktype = 'advisory' AND l.classid = 0 AND l.objid = 1237737229 AND l.objsubid = 1
  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database());
```

A probe transaction shows in `pg_stat_activity` as `idle in transaction` from
the Pod's address. `SELECT pg_terminate_backend(<pid>);` ends either: the
server rolls the transaction back and releases the lock. On MySQL,
`IS_USED_LOCK` names the connection that holds the lock, and
`performance_schema.metadata_locks` lists it as a `USER LEVEL LOCK`:

```sql
SELECT ID, USER, HOST, TIME, STATE
FROM performance_schema.processlist
WHERE ID = IS_USED_LOCK('ptah_schema_apply');
```

`KILL <ID>;` ends that connection and releases the lock.
`SELECT IS_USED_LOCK('ptah_schema_apply');` alone returns that ID. The process
list adds who holds the lock and for how long, and is read from the Performance
Schema; where that is off, the ID alone is enough to `KILL`.

The account Ptah logs in as can see and end its own sessions. Another account
needs privileges for each step. On PostgreSQL every role sees the lock in
`pg_locks`, but only superuser, `pg_read_all_stats` or a role with the
privileges of Ptah's login sees the session's address and state in
`pg_stat_activity`, and only superuser, `pg_signal_backend` or such a role can
end it; `pg_signal_backend` cannot end a superuser's session. On MySQL another
account needs `PROCESS` to see the connection in the process list and
`CONNECTION_ADMIN` to `KILL` it.
