---
title: Reconciliation
description: What each resource family does from one reconciliation to the next, and what its phases and conditions mean.
---

Both families converge a database towards what a resource declares, and they
do it differently enough that the two lifecycles are described side by side
rather than merged. What the operator is put together from is
[Architecture](../architecture/); what enforces each promise, per family, is
[Execution guarantees](../guarantees/).

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
  bounded category-level aggregates.
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

## The lifecycle journal

Every `PtahSchema` and `PtahMigration` keeps a bounded journal of its
significant events in `status.journal`, so a reader can see when a change was
planned, approved and applied, how long it took and how it ended, and how
often something failed, after the Events Kubernetes keeps for about an hour
and the finished Jobs it keeps for five minutes are gone.

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
