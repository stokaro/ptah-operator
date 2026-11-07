---
title: Reconciliation
description: What each resource family does from one reconciliation to the next, and what its phases and conditions mean.
---

Both families converge a database towards what a resource declares, and they
do it differently enough that the two lifecycles are described side by side
rather than merged. What the operator is put together from is
[Architecture](../architecture/); what enforces each promise, per family, is
[Execution guarantees](../guarantees/).

## From source to verified change {#from-source-to-verified-change}

A change reaches a database along one path, whichever resource carries it. You
declare what you want, the operator plans it and holds it to your policy, a
Ptah executor runs it in a Job, and a later read proves what happened. The
[schema](#the-schema-lifecycle) and [migration](#the-migration-lifecycle)
lifecycles below are the precise reference. To walk the path with commands, use
[First schema](../../start/first-schema/#stage-by-stage) or
[Run versioned migrations](../../use/migrations/#stage-by-stage).

`kubectl` and the [`kubectl ptah` plugin](../../use/read-a-plan/) cover every
stage. [Ptah Console](https://github.com/stokaro/ptah-console) is optional: it
is another interface to the same Kubernetes resources, under the same RBAC and
admission rules, and it runs no database change itself. Running the standalone
Ptah CLI directly against a database is a different path. No plan, approval or
journal entry records it, and the operator finds the result at its next read.

```mermaid
---
config:
  look: classic
  themeVariables:
    fontSize: 14px
  flowchart:
    wrappingWidth: 340
    nodeSpacing: 16
    rankSpacing: 26
---
flowchart TD
  accTitle: The path of a database change, in six numbered stages
  accDescr: The table after this diagram gives each stage and who does what in it. Ptah Console is optional, and no stage needs it.
  subgraph you["You act through either, at any stage"]
    direction LR
    cli(["kubectl, kubectl ptah"])
    console(["Optional: Ptah Console"])
  end
  source["<b>1 Source</b><br/>You: apply the resource, or change its source<br/>Kubernetes: PtahSchema or PtahMigration<br/>Operator: pins a snapshot, commit or digest<br/>Executor: fetches a Git or OCI source"]
  plan["<b>2 Plan</b><br/>You: read the plan and its findings<br/>Kubernetes: an immutable plan<br/>Operator: publishes it<br/>Executor: reads the database or its history"]
  decision["<b>3 Decision</b><br/>You: approve that exact plan, if policy asks<br/>Kubernetes: an approval bound to it<br/>Operator: refuses, waits or proceeds, by policy<br/>Executor: runs nothing"]
  execution["<b>4 Execution</b><br/>You: watch status<br/>Kubernetes: one Job, one Lease<br/>Operator: starts the Job once its gates hold<br/>Executor: runs the plan or the sequence"]
  verification["<b>5 Verification</b><br/>You: read the verdict<br/>Kubernetes: status conditions<br/>Operator: believes this read, not the Job's exit<br/>Executor: reads the database or history again"]
  insync(["In sync, until the next read"])
  history["<b>6 History</b>, at any stage<br/>You: read it; reading changes nothing<br/>Kubernetes: status and Events, all bounded<br/>Operator: keeps the record<br/>Executor: runs nothing"]
  you -->|"the same resources, RBAC and admission"| source
  source --> plan --> decision --> execution --> verification --> insync
  insync -.- history
  classDef optional stroke-dasharray: 5 4
  class console,history optional
```

The same path, with where to read about each stage. The executor is Ptah
itself, run by the operator in a Job.

| Stage | You | Kubernetes resources | Operator | Ptah executor |
| --- | --- | --- | --- | --- |
| [1 Source](../../use/sources/) | `kubectl apply`, or `kubectl ptah set-source` for another OCI artifact | `PtahSchema` or `PtahMigration`, and the source it names | Pins the content: a [ConfigMap snapshot](#a-configmap-source), a [Git commit](#a-git-source), or an OCI digest its verification policy accepts | Runs a Resolve Job for Git and OCI, and a Verify Job for OCI. A ConfigMap needs neither |
| [2 Plan](../../use/read-a-plan/) | `kubectl ptah plan`, `schema` or `migration` shows the plan's identity, contents and findings | `PtahSchemaPlan` and its chunks, or `PtahMigrationPlan` | Publishes a plan bound to what was read | Observes the database and plans, or reads the revision table |
| [3 Decision](../../use/approvals/) | `kubectl ptah approve`, when the policy asks | `PtahSchemaApproval` or `PtahMigrationApproval`, stamped by admission | Applies `spec.policy`: refuses, waits for an approval of this exact plan, or proceeds | Runs nothing |
| [4 Execution](../../use/operations/#wait) | `kubectl ptah wait`, or `approve --wait` | An Apply Job and the database-realm Lease | Creates the Job once every gate holds | Runs the plan or the approved sequence |
| [5 Verification](../../use/operations/#normal-status-progression) | `kubectl ptah schema` or `migration` | Status conditions | Takes this read as the proof, not the Job's exit | Reads the database or the revision table again |
| [6 History](#the-lifecycle-journal) | `kubectl get`, `kubectl ptah plan --applied`, `kubectl events` | Status, stored plans and approvals, Events | Writes the record in the status writes it already makes | Runs nothing |

History is not a step after verification, and reading it changes nothing.
`status.journal` records when a plan was published, approved and applied and
how each attempt ended. [The lifecycle journal](#the-lifecycle-journal) says how
far back it reaches, and how much sooner Events and finished Jobs go. A migration
also keeps the newest rows of its revision table in `status.history.revisions`,
and its last run in `status.lastRun`. A schema keeps what its last confirmed
Apply ran in `status.applied`, and `kubectl ptah plan --applied` prints that
plan. None of this is an unlimited audit log, and what a resource holds depends
on the version that wrote it: `coveredSince` says when its journal began, and a
revision table too old to record a duration leaves a row's `executionMillis`
absent.

### Where the path turns {#where-the-path-turns}

```mermaid
---
config:
  look: classic
  themeVariables:
    fontSize: 14px
  flowchart:
    wrappingWidth: 260
    nodeSpacing: 12
    rankSpacing: 10
    padding: 4
    diagramPadding: 6
---
flowchart LR
  accTitle: Where the path of a database change turns
  accDescr: The list after this diagram gives each branch and what follows it.
  before["2 Plan,<br/>3 Decision"]
  execution["4 Execution"]
  verification["5 Verification"]
  reads["1, 2, 5"]
  before --> nochange["<b>No change, nothing pending</b><br/>In sync. No Apply, no approval."]
  before --> refused["<b>Refused</b><br/>Blocked: no Apply. Reads again<br/>at its interval until an input<br/>changes."]
  before --> stale["<b>Plan went stale</b><br/>Back to 2 from a fresh read. An<br/>approval must name the new plan."]
  execution --> unknown["<b>Schema: Apply uncertain</b><br/>Any Apply error counts. On to 5,<br/>a fresh observation. Never<br/>replayed."]
  execution --> failed["<b>Migration: Failed</b><br/>No rollback. A revision row the<br/>run left holds it Blocked until a<br/>person repairs it. A later plan<br/>needs a new approval, if asked."]
  execution --> unresolved["<b>Migration: Partial or Unknown</b><br/>No further Apply until a read with<br/>nothing pending, or a person's<br/>acknowledgment, settles it."]
  verification --> remain["<b>Changes remain</b><br/>Back to 2. A new plan and, if<br/>policy asks, a new approval."]
  reads --> retry["<b>A read fails</b><br/>Changed nothing. Retried after<br/>failureRetryInterval."]
```

The operator rolls nothing back. Each branch has its own recovery, and
[Failure recovery](../../use/operations/#failure-recovery) has the detail.

- **No change, nothing pending.** A no-change plan or a history with nothing
  pending needs no Apply and no approval, and the resource reports `InSync`.
- **Refused.** [`Blocked`](#blocked-is-a-refusal-not-a-fault) does not proceed
  to Apply. It keeps reading at its interval, so a changed input is noticed
  without anybody touching the resource. A migration's history refuses at stage
  2 when it is dirty, modified, out of order or ahead. A migration under
  `apply: Never` rests in `Planning` instead, with `Ready=False`, reason
  `ApplyDisabled`.
- **Plan went stale.** A changed input retires the plan before Apply. The
  resource reads the database or the history again and plans again, and an
  approval of the old plan does not carry over. `kubectl ptah approve` approves
  nothing if the plan it showed was replaced. Read the replacement, then approve
  that.
- **Schema, Apply uncertain.** Every Apply error is reported as
  `ApplyOutcomeUnknown`, and the plan is never replayed. A fresh read-only
  observation comes before anything else. If it finds no change the schema is
  `InSync`, with reason `ConvergedAfterUnknownOutcome`; otherwise it plans
  again.
- **Migration, `Failed`.** [`status.lastRun`](../../use/migrations/#what-the-run-reports)
  says how far the run got. A migration that failed at a statement leaves a
  dirty revision row, and the history read that follows holds the resource
  `Blocked`, reason `HistoryDirty`, until a person
  [repairs it with Ptah](../../use/migrations/#what-the-history-says). A run
  that stopped before it selected a migration leaves no row, and the resource
  starts again after `spec.execution.failureRetryInterval`. Either way a later
  plan needs its own approval under `apply: OnApproval`, because the run spent
  the one it carried.
- **Migration, `Partial` or `Unknown`.** The run stays in
  `status.unresolvedRun`, and the resource [publishes no plan and dispatches no
  Apply](../../use/operations/#a-migration-run-nobody-accounted-for) until it is
  settled. [A history read of that database with nothing pending](../../use/operations/#how-it-clears)
  settles it. So does a [`PtahMigrationRunAcknowledgment` bound to that
  run](../../use/operations/#clear-unresolved-run), which a person creates after
  establishing what the run did.
- **Changes remain.** A proof that still finds changes publishes a new plan.
- **A read fails.** Resolve, Verify, Observe, Plan and a history read change
  nothing, so they retry after `spec.execution.failureRetryInterval`.

### What differs between a schema and a migration {#schema-and-migration}

| | `PtahSchema` | `PtahMigration` |
| --- | --- | --- |
| The plan holds | The generated SQL, which `kubectl ptah plan` prints | The prepared versions and checksums. `kubectl ptah migration` and `approve` print no SQL: it stays in the source |
| The proof is | A fresh observation and a no-change plan: `InSync=True`, reason `ScopedConverged` | A fresh history read: phase `InSync`, `Ready=True`, reason `HistoryMatched` |
| An Apply nobody can account for | Goes straight to a fresh observation, which can settle it | Stops every later Apply until a read or a person settles it |
| Walk it with | [First schema](../../start/first-schema/#stage-by-stage) | [Run versioned migrations](../../use/migrations/#stage-by-stage) |

## The schema lifecycle

```mermaid
stateDiagram-v2
  [*] --> Pending
  Pending --> Resolving
  Resolving --> Verifying: digest pinned
  Resolving --> Observing: ConfigMap snapshot taken
  Resolving --> Observing: Git commit resolved
  Verifying --> Observing: policy satisfied
  Observing --> Planning: drift read
  Planning --> InSync: no change
  Planning --> AwaitingApproval: change, approval required
  Planning --> ReadyToApply: change, policy Always
  AwaitingApproval --> ReadyToApply: exact approval accepted
  ReadyToApply --> Applying
  Applying --> VerifyingConvergence: result read
  VerifyingConvergence --> InSync: fresh no-change plan
  InSync --> Resolving: interval
  Blocked --> Resolving: interval
  Verifying --> Blocked: artifact refused
  Planning --> Blocked: protected table, destructive<br/>disallowed, or apply Never
  Pending --> Blocked: realm conflict or refusal,<br/>unsupported engine
  Applying --> Failed: run failed
  Failed --> Resolving: retry interval
  Pending --> Suspended: suspend
  Suspended --> Pending: resume
```

Five operations run as Jobs: `Resolve`, `Verify`, `Observe`, `Plan`, `Apply`.
Approval is not an operation — it is a phase in which nothing runs.

- **Resolve** records the immutable digest behind the requested OCI reference.
- **Verify** evaluates the selected policy and independently inspects the
  digest-pinned artifact type.
- **Observe** fetches the verified schema by digest and validates a raw,
  read-only drift report. The report's detail stays inside the runner: status
  receives a digest, a dialect, a total count, the highest severity, and
  bounded category-level aggregates. The report covers
  [what the login can see](../../support/databases/#what-the-login-cannot-see).
- **Plan** performs two independent native scoped plans while holding the
  database-realm Lease, accepts only byte-identical results, and then makes the
  native Apply path parse and dry-run those exact bytes. A no-change result is
  the authoritative statement that the database converged; changed bytes are
  published.
- **Apply** reconstructs and hashes the published bytes immediately before
  executing them.

### A ConfigMap source

A schema or a migration whose `configMap` names its files reaches no registry,
and two of the operations above change for it. Resolve is not a Job: the
manager reads the ConfigMap from the API server, takes the selected keys as
files, and copies them into an immutable snapshot ConfigMap named
`ptah-src-<hash>` that the resource owns, with a manifest of every file's path,
size and SHA-256 beside them. The content digest is the SHA-256 of that
manifest, so the same files give the same digest whatever key they came from.
There is no verification policy for such a source, so there is no Verify step
and no `ArtifactVerified` condition; `ArtifactResolved` says
`SnapshotTaken`.

Every later operation that reads the files mounts the snapshot, never the
source ConfigMap. An init container in the Pod checks the snapshot against the
content digest the operation was bound to, refuses one whose manifest or any
file no longer gives it, and only then copies the files where the operation
container reads them. A refusal stops the Pod before the container that holds
the database URL starts, and the init container's status says which file
failed.

An edit to the source ConfigMap is read at once rather than at the next
interval. An edit to a key the source does not select gives the same content
digest, keeps the snapshot and the plan, and plans nothing new; an edit to a
selected key gives a new snapshot and retires the plan. A plan binds the
ConfigMap's UID and the content digest in place of an artifact digest, under
plan contract version 2, so another ConfigMap with the same name retires it
too. The manager deletes a snapshot once nothing names it any more, checking
after each resolution is stored. A resource whose source moves to Git or OCI
loses its last snapshot with the first resolution of the new source.

If a snapshot is deleted or replaced while an operation still needs it, that
operation is refused rather than read from anywhere else. Putting back the
exact bytes, or editing the source so that it is read again once nothing is in
flight, lets it continue.

### A Git source

A schema or a migration whose `git` names a repository, a ref and a path
reaches it only from Jobs, through Ptah's pull. Resolve is a Job that fetches
the commit the ref names, a branch at its tip, a tag at the commit it names,
or a commit by its id, and reports that commit and the content digest of the
selected files, computed as for a ConfigMap source. The status records the
repository, ref and path it answered beside them. As with a ConfigMap source,
observation follows Resolve directly, with no verification policy to check;
`ArtifactResolved` says `CommitResolved` and names the commit.

Every later operation that reads the files fetches that commit again, in an
init container that holds the credential, and refuses content other than the
recorded digest: Ptah's pull is held to it, and the runner reads back what Ptah
wrote and checks it again before it copies the files where the operation
container reads them. The commit is fetched by the ref that named it while the
ref still points there. Afterwards it is fetched by its id where the server
allows that, and otherwise along the ref's history, deeper each time within the
fetch budget. A commit that is no longer reachable, because the branch was
force-pushed, is refused, and the next Resolve reads the ref again.

A resource pinned to a commit names no branch or tag to look along, so Ptah
asks the server for the commit by its id, and only when the server's
advertisement allows it: a branch or a lightweight tag points at the commit,
or the server says it gives any reachable commit by id (Git's
`uploadpack.allowReachableSHA1InWant` or `uploadpack.allowAnySHA1InWant`, both
off by default). Otherwise Resolve is refused before anything is fetched. On a
server that does not say so, an ordinary push on top of the pinned commit
refuses the next Resolve just as a force-push does, and a commit that is not
already at a branch tip is refused at the first one. The resource keeps what it
applied. A tag stays on one commit on every server: to hold a resource at one
commit there, name the tag.

A refused pull says why in `ReconciliationFailed`, with the last line Ptah
wrote: a submodule or an LFS pointer at the selected path, a host key the known
hosts do not pin, a commit no branch or tag names on a server that gives
commits only at a branch or tag.

A branch or a tag is read again at every interval. A commit that leaves the
selected files as they were keeps the plan; other files retire it. A plan
binds the SHA-256 of the repository URL and the path, with the content digest,
in place of an artifact digest, under plan contract version 2, so another
repository or path retires it even when the files match.

### A source policy

A ConfigMap or Git source may name a source policy with `sourcePolicyFrom`.
Resolve checks the source against it: a ConfigMap's name and size before a
snapshot is written, a Git host before the Resolve Job is made, and the size
of a commit's selected files before the commit is recorded. A refused source
fails with the rule named and plans nothing. The policy is recorded beside the
resolution and bound into every plan computed from it, and a policy replaced
in the cluster is read at once: the source is resolved again under it, and
the plan made under the old one retires.

After an Apply, `Observe` and `Plan` run again under the original Apply Lease.
Only a new, coherent no-change plan establishes convergence: a process exit
never establishes that the database changed the way the plan said it would.

### Blocked is a refusal, not a fault

`Blocked` means the answer will not change until somebody changes an input. It
is reached from seven places: a realm conflict, a `PtahRealm` that does not
admit the resource, an unsupported engine, a
verification policy that refused the artifact, a plan that would change a
a protected table, a destructive plan the policy disallows, and `apply: Never`,
which is not a problem at all — it is the policy that records plans and applies
none, and a resource can sit there for as long as somebody wants. `Failed` is
different again: something went wrong and a retry is reasonable.

A blocked resource still refreshes on its interval through the complete
Resolve → Verify → Observe → Plan pipeline, so a policy edit or a new artifact
is noticed without anybody touching the resource.

### Suspended, and losing a lock

`spec.suspend` prevents new Jobs. A Job already applying is still observed to a
terminal result, because abandoning it would leave the database in a state
nothing recorded; suspension while an operation is live stages the Lease
release first.

`leaseContinuityLost` is set before any result can be harvested when the
persisted epoch no longer owns an uninterrupted lock interval. A result
produced across an epoch change is discarded rather than read: the lock it held
was somebody else's by then.

## The migration lifecycle

```mermaid
stateDiagram-v2
  [*] --> Pending
  Pending --> Resolving
  Resolving --> Verifying: digest pinned
  Resolving --> Reading: ConfigMap snapshot taken
  Resolving --> Reading: Git commit resolved
  Verifying --> Reading: policy satisfied
  Reading --> InSync: nothing pending
  Reading --> Planning: pending versions
  Reading --> Blocked: dirty, modified, out of order, ahead
  Planning --> AwaitingApproval: approval required
  Planning --> Applying: policy Always
  AwaitingApproval --> Applying: exact approval accepted
  Applying --> VerifyingHistory: result read
  VerifyingHistory --> InSync: history matches
  Applying --> Blocked: partial or unknown outcome
  Resolving --> Failed: dispatch or configuration
  InSync --> Resolving: interval
  Blocked --> Resolving: interval
  Failed --> Resolving: retry interval
  Pending --> Suspended: suspend
  Suspended --> Pending: resume
```

Four operations run as Jobs: `Resolve`, `Verify`, `History`, `Apply`. Only
`Apply` takes the database-realm Lease; reading history does not, because it
mutates nothing. A migration directory held in a ConfigMap is resolved by the
manager into a snapshot, as [a schema's is](#a-configmap-source), and both
`History` and `Apply` read the checked snapshot. One held in Git is resolved by
a Job and fetched again by both, as [a schema's is](#a-git-source).

Reading the history produces one of six verdicts, and the operator never
guesses at any of them: the revision table is dirty; a recorded migration's
checksum no longer matches the artifact; a pending version sorts below one
already applied; the database is ahead of the artifact; nothing is pending; or
a sequence is ready to plan. Pending selection is Ptah's own list rather than a
subtraction of counts, because a checkpoint bootstrap makes those two answers
differ.

The two ways out of the ordinary path are the same two words the schema path
uses, and they mean the same thing here. `Blocked` is a verdict about the
database that will not change on its own; `Failed` is a configuration or
dispatch failure the controller could not resolve by retrying immediately, and
it retries on its own interval.

An outcome that is `Partial` or `Unknown` refuses every later Apply until the
database itself settles it. The resource goes to `Blocked`, and only a history
reading that shows nothing pending releases it — never a retry, because
replaying a non-idempotent statement is exactly the damage the refusal exists to
prevent. Every other outcome is confirmed by reading the history back in
`VerifyingHistory`.

An `Apply` runs the approved sequence and nothing else. The Job carries the
sequence, and Ptah compares it with its own selection under the migration lock,
the moment every earlier check is behind it. Any difference refuses the run
before it changes anything, which reads as `Failed` with nothing applied: a
history restored backwards after the approval is caught there rather than
executed.

Adoption of an existing schema is deliberately not an operator feature: an
empty revision table reads as everything pending, and the answer is Ptah's own
`migrations baseline`, run by a person against a shadow database. The operator
never holds a shadow credential.

## Reading again on request

A resource reads its database again at `status.nextReconciliationTime`. After
a person changes the database outside the operator, such as clearing a dirty
revision row with `ptah migrations repair`, the annotation
`operator.ptah.run/read-requested-at`, set to an RFC 3339 instant, asks for
that read now:

```bash
kubectl annotate ptahmigration orders \
  operator.ptah.run/read-requested-at="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

`kubectl ptah refresh migration/orders -n application` writes the same
annotation, with a value later than any the resource holds and than the last
one `status.lastReadRequest` answered, so a second request a moment after the
first is not taken for an answered one. With `--wait` it waits for the read
the request starts, under the rules below.

A value `status.lastReadRequest` does not record as answered is answered once
no operation is running. In `InSync`, `Blocked`, `ReadyToApply`,
`AwaitingApproval` and a migration's `Planning`, the answer records the value
in `status.lastReadRequest`, moves `nextReconciliationTime` to now and writes a
`ReadRequested` Event, and the next pass starts the read the interval would
have started: Resolve, then Observe for a schema or the history read for a
migration. A plan that waits for approval is kept unless the new reading
retires it, as at any interval.

A resource `Blocked` because its engine is not supported (`UnsupportedEngine`)
or because its database realm does not admit it (`RealmConflict`,
`RealmNotAuthorized`) does not answer: the controller checks the engine and the
realm before it reaches the request, and the request waits until those checks
pass. `Blocked` for any other reason, such as a policy that refuses the plan,
answers as above.

The request never starts a mutating operation and does not shorten a retry
delay: a `Failed` resource answers it when its retry starts. A read already
under way when the request arrives does not answer it, because it may have read
the database before the change the request is about; the request waits for the
read that starts after it. An annotation does not change the generation, so
nothing else about the resource changes.

A value that is not an RFC 3339 instant starts nothing. It is recorded as
answered, with a `ReadRequestInvalid` Warning Event that names it, so it is
reported once rather than on every pass. A value longer than 64 bytes is
recorded as its SHA-256 digest, written `sha256:<hex>`.

## The lifecycle journal

Every `PtahSchema` and `PtahMigration` keeps a bounded journal of its
significant events in `status.journal`, so a reader can see when a change was
planned, approved and applied, how long it took and how it ended, and how
often something failed, after the Events Kubernetes keeps for about an hour
and the finished Jobs it keeps for five minutes, or 24 hours after
[a failed Apply that keeps its log](../../use/operations/#failed-apply-log), are gone.

| Entry | Written when |
| --- | --- |
| `PlanPublished` | `status.plan` names a plan the journal does not already hold. A plan republished while the resource refreshes is the same plan and writes nothing. |
| `ApprovalAccepted` | An Apply consumes an approval: the one a schema's plan records, with the approver admission stamped, or the one a migration's Apply claim names, without one, since a migration's status does not carry it. |
| `ApplyFinished` | A schema's Apply ends and its post-Apply observation takes its place: `Succeeded`, or `Unknown` when its outcome is unknown. An Apply withdrawn before dispatch changed nothing and writes nothing. |
| `Verified` | That observation settles: `InSync`, `DriftRemains`, or `Superseded` when a newer generation, policy or execution binding took over. |
| `RunFinished` | A migration run ends, with its outcome, how many versions it applied, and where it stopped; or it is recorded as unresolved, as `Unknown`. |
| `RunAcknowledged` | A person acknowledges an unresolved run. |
| `OperationFailed` | An attempt fails. Attempts that fail with the same operation and reason in a row are one entry with a `count` and the time of the last one. |

A cycle that reads the database and finds nothing to do writes no entry. An
entry names objects in the resource's namespace and carries condition reasons,
never messages: a message can carry what status must not.

The journal holds at most 64 entries, none more than 30 days older than the
newest, and drops the oldest first. `coveredSince` is the instant from which it
is complete: the first status write a manager that keeps a journal made,
moved forward past the newest entry a bound dropped. A reader states the
period the journal covers from it rather than assuming a week or a month.

The journal is written in the status write that records the transition, so it
costs no request of its own. Its times come from that status, such as a plan's
creation or a run's end, or from the manager's clock when the transition
carries none, as conditions do.

`status.lastRun.failure` records where a failed or partial run stopped, when a
statement reached the database and failed there: the migration's version, the
statement's position, and the SQLSTATE and MySQL error number the database
answered with. It carries no text, for the reason the run's message carries
none.
