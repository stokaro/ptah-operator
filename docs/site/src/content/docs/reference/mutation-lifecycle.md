---
title: Mutation lifecycle
description: The obligations a mutating operation carries, and where each family enforces them.
---

Both resource families run SQL the same way: authorize the exact work, persist
the claim, cross a dispatch boundary that is durable before anything external
happens, account for the outcome, and hand the database back. They differ on
when that last step happens. A schema hands the database back once the account
is settled; a migration hands it back once nothing its claim dispatched can
still write, and settles the account afterwards without the Lease.
[Release coordination](#release-coordination) says why each order is safe.

They enforce all of it in two separate implementations, which is why this page
exists. An obligation stated once and enforced twice drifts, and the way to see
the drift is to put both enforcement points beside each other.

Symbols named without a package live in `internal/controller`:
`schema_controller.go` for `PtahSchema`, `migration_controller.go` and
`migration_apply.go` for `PtahMigration`. A dotted name such as
`targetlock.Acquire` names a package under `internal/`.

## The obligations

A mutating operation owes six things, and every step below is one of them:

| Obligation | What it forbids |
| --- | --- |
| Authorize the exact work and target | Running anything the evidence chain did not compute |
| Persist the claim | A dispatch no durable record names |
| Record the dispatch boundary | Recreating work that may already be running |
| Account for the outcome | Discarding what a run may have done |
| Resolve uncertainty through evidence | Clearing a record on anything but a reading |
| Release coordination only when owed | Handing the database back under a live executor |

The order matters as much as the list. Each step's durable write is what makes
the next step's failure survivable, so a step that writes after acting instead
of before has no failure window at all -- it has a gap.

## Authorize

A claim is permitted only when every binding the plan was computed under still
holds. The check is a re-read, not a cached comparison: the plan object, the
verification-policy ConfigMap and the approval are all read uncached, and any
one of them having moved discards the plan rather than narrowing the claim.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `currentPlan`, `ensureCurrentExecutionBinding`, `verifiedSourcePolicyError`, `ensureCurrentApproval` |
| `PtahMigration` | `currentMigrationPlan`, `verificationPolicyStillBinds`, `findMigrationApproval` |

Nothing durable is written and nothing external happens, so there is no failure
window. A refusal leaves the resource exactly as the pass found it.

## Claim

The claim is written before the Job exists, and it carries the Job's
deterministic name. That is what lets a controller that restarted mid-dispatch
tell a Job it created from one it has not: the name folds the resource UID, the
operation type, the operation ID, the execution-binding epoch, the input
fingerprint and the attempt, so no other claim can produce it.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `claimAt`, name from `workload.NameFor` |
| `PtahMigration` | `claimMigrationApply`, name from `workload.NameForMigration` |

Two writes, in order: the finalizer through a metadata patch, then the claim
through a status patch. A crash between them leaves a finalizer with no claim,
which the next pass removes before doing anything else.

A claim is not evidence that anything ran. The next pass finding a claim with
no dispatch marker and no Job under the reserved name proceeds, because nothing
external has happened yet.

## Bind the realm

The Lease is per coordination digest, not per resource, so two resources
addressing one physical database serialize even when their keys differ in
spelling. The claim persists the epoch it expects before any acquisition, and
an epoch that changed under a dispatched claim is not a retry -- it is the end
of the claim's readability.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `acquireApplyLock`, `acquireActiveLock`, `targetlock.Acquire` |
| `PtahMigration` | `acquireMigrationApplyLock`, `targetlock.Acquire` |

Foreign-lease expiry is measured from this process's own first observation of
an unchanged record, never from another node's clock (`targetlock.Acquire`).

The failure window is between the Lease write and the epoch status write: the
Lease is held under an epoch the status does not name. The next pass acquires
again with the stale expectation. Before dispatch that is adopted silently,
because nothing ran; after dispatch it latches continuity loss, and the run
becomes unreadable rather than retryable.

## Resolve the admission envelope

The Pod admission snapshot is durable before any Job carries its digest, and
resolving it is its own boundary: the pass that writes it returns without
creating anything. A later pass rebuilds the Job and refuses a template whose
digest disagrees with the snapshot.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `podintent.Resolve`, digest compared in `reconcileActive` |
| `PtahMigration` | `podintent.Resolve`, digest compared in `dispatchMigrationJob` |

A crash after the snapshot write cannot leave a Job behind, because the write
returned before the create.

## Cross the dispatch boundary

`dispatchStarted` is the field that converts "the Job is missing" from "create
it" into "the outcome is unknown". It is written before the create and never
cleared for a mutating claim.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `reconcileActive` writes it; a missing Job reaches `finishUncertainApply` |
| `PtahMigration` | `dispatchMigrationJob` writes it; a missing Job reaches `finishUncertainMigrationApply` |

This is the central failure window. A process that dies between the write and
the create leaves a claim that says a Job may exist. The next pass either finds
it and adopts its UID, or finds nothing and declares the outcome unknown. It
never creates the Job a second time.

The approval is consumed in the same stretch, before the create, and the
families put the two writes in opposite orders. A migration consumes the
approval and then writes `dispatchStarted`. A schema writes `dispatchStarted`
and then consumes the approval, so a crash between them leaves a dispatch
marker beside an approval not yet spent; the next pass finds no Job, declares
the outcome unknown, and spends the recorded approval before it writes the
pending observation.

Consumption is evidence that a decision was spent, not permission: a consumed
approval is skipped when a claim looks for one, so it cannot authorize a second
dispatch.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `markApprovalConsumed` after the marker, and `consumeRecordedApprovalAtDispatch` on the uncertain path; skipped by `findApproval` |
| `PtahMigration` | `consumeMigrationApproval` before the marker, skipped by `findMigrationApproval` |

## Create, confirm, record

One create per claim. Any error from it, `AlreadyExists` included, is an
uncertain outcome rather than a retry: a retry would rename the claim and
dispatch beside a Job that may already be running SQL.

The created Job is read back and its whole intent compared against what the
builder produced -- owner reference, labels, annotations and a semantic
comparison of the spec -- before its UID is persisted.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `validateJobIntent`, then the UID written by `reconcileActive` |
| `PtahMigration` | `validateMigrationJobIntent`, then the UID written by `reconcileActiveMigration` |

A crash between the create and the UID write is covered by the dispatch marker:
the next pass adopts the Job found under the reserved name, having checked that
it is owned by exactly this resource.

## Supervise

Every pass rebinds by UID, renews the Lease, and re-validates that the Job is
still the one the claim named and still owned by exactly this resource. Drift
in any of those is an uncertain outcome for a mutating claim, never a discard.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `reconcileActive`, `validateJobIntent` on every pass |
| `PtahMigration` | `reconcileActiveMigration`, UID and owner compared each pass |

Suspension cannot discard a dispatched mutating claim in either family. A
resource suspended mid-Apply keeps its claim and keeps renewing the Lease.

For a migration, suspending is also a spec edit, and the generation it bumps is
one of the inputs `migrationInputFingerprint` digests. So the pass that finds
the Job terminal also finds the inputs changed, and records the run `Unknown`
and unresolved without reading its result, whatever the run did. A suspended
migration takes no reading, so the record stands until the resource is resumed
and a History reading of the same database finds nothing pending.

## Account for the outcome

A terminal Job is not an outcome. The account comes from a result frame parsed
out of the Pod's own log, bound to the operation ID, read from exactly one Pod
that the persisted admission envelope accepts, and only from a container that
actually terminated.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `collectTerminalPodEvidence`, `runner.ParseResultFor`, `awaitFrameArrival` |
| `PtahMigration` | `collectTerminalPodEvidence`, `runner.ParseResultFor`, `awaitFrameArrival` |

The frame's own report of which database it opened is compared against the
claim before the account is accepted. A run that reached a database other than
the one it was planned for is uncertain, not failed.

## Retain uncertainty

An unresolved mutating attempt is durable and independent of `phase` and of
every condition reason, because a later refusal rewrites a reason and a record
has to outlive that.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `status.pendingObservation`, written by `pendingObservationFor` |
| `PtahMigration` | `status.unresolvedRun`, written by `recordUnresolvedMigrationRun` |

Each is written by exactly one function and cleared at exactly one place. The
claim and the record are swapped in a single status patch, so a crash on either
side leaves either a live claim or a retained record, never both and never
neither.

## Prove or refuse

Only a fresh read-only reading settles an uncertain attempt. A Job exit code
never does, and neither does an edit to a condition, an unrelated approval, a
suspension, a spec change or a restart.

| | Enforcement |
| --- | --- |
| `PtahSchema` | a Plan whose target matches the pending snapshot and reports no changes, in `consumeResult` |
| `PtahMigration` | a History reading with nothing pending on the same database, in `migrationUnresolvedRunSettledBy` |

Convergence is not attribution. A schema whose uncertain Apply converges is
recorded as converged with no Apply attributed to it, because the reading
proves the database matches and proves nothing about what changed it.

## Release coordination

Release always follows the durable status write, never leads it. A crash in
between leaves a Lease nobody needs, which expires; the other order hands the
database back under a claim that still reads as live.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `stagePendingLockRelease` in the write that settles the proof, through `stageTargetLockRelease`, retried by `completePendingLockRelease` |
| `PtahMigration` | `stageOwedMigrationRelease` where a result was read, `releaseMigrationApplyLock` where it was not, withheld by `dispatchedApplyMayStillWrite` |

The two families release at different points.

A schema holds the database until the account is settled. Every Apply, read or
uncertain, leaves `status.pendingObservation` carrying the claim's Lease epoch.
The pending observation renews the Lease, waits until no Apply Pod can still
run, and claims the read-only Observe and Plan that settle it. Both run under
that same Lease, and `mutationlifecycle.RealmHeldBy` makes retiring either of
them release nothing while the proof is owed. Only the status write that
settles the proof stages the release. No other claimant can change the database
between the Apply and the reading that accounts for it, so an intervening
mutation cannot be mistaken for this plan's convergence.

A migration holds the database until nothing its claim dispatched can still
write. A run whose result was read stages the release in the write that retires
the claim. A run retired as uncertain (`finishUncertainMigrationApply`) releases
after that write, and only when `dispatchedApplyMayStillWrite` says no: a Job
that is not terminal, a Pod it owns that has not stopped, and a read that could
not say all count as "may still be writing", and the Lease is left to expire
instead. The reading that settles the account comes afterwards and takes no
Lease: the one in `VerifyingHistory` after a run that reported what it did, and
the one that clears `status.unresolvedRun` (`migrationUnresolvedRunSettledBy`)
after a run that ended `Partial` or `Unknown`. Another resource that shares the
realm may hold the Lease while that reading runs.

Handing the database back before that reading lets no two writers overlap and
no run repeat:

- The release happens only when nothing of the claim can still write, so the
  next claimant never starts beside this claim's executor.
- A Lease left to expire outlives the executor. `migrationLeaseDuration` is the
  Apply window plus the Job's deadline grace plus a lease grace, a minute each,
  counted from the last renewal. The runner kills the Ptah child at the claim's
  execution deadline, the end of that window, so the executor stops by its own
  clock at least two minutes before the Lease can lapse, on a node the API
  server cannot reach as well as on one it can.
- The reading authorizes no write. Settling the record only lets this resource
  plan again from a fresh reading, and every migration Apply, this resource's
  next one or another resource's, takes the Lease and runs
  `ptah migrations up --expect-sequence`. Ptah compares the approved sequence
  with what it selects under its own migration lock and refuses before it
  changes anything when the history moved after the approval. A reading taken
  beside another writer can be stale; a stale reading cannot become a replay.

Deciding the hand-back once, for both families, is
[#457](https://github.com/stokaro/ptah-operator/issues/457). Until then the
two orders above are the contract.

## Every durable write, and what follows it

The obligations above are stated step by step, and each names the window its
own write opens. Here they are in one place and in order, because the question
an incident asks is not "what does Claim do" but "the process stopped, what
does the next pass see".

Read the third column as the answer to that. A row whose third column is "the
next pass cannot tell" would be a defect; none of them is.

| Durable write | What may happen next | If the process stops in between |
| --- | --- | --- |
| The finalizer, through a metadata patch | The claim's own status patch | A finalizer with no claim, which the next pass removes before anything else |
| `activeOperation`, with the Job's deterministic name | Nothing external; the pass ends | A claim with no dispatch marker and no Job under the reserved name, which proceeds: a claim is not evidence that anything ran |
| `leaseEpoch`, after the Lease was taken | Nothing external | The Lease held under an epoch the status does not name. The next pass acquires with the stale expectation, which is adopted before dispatch and is continuity loss after |
| `admissionSnapshot` | Nothing; the pass returns deliberately | No Job can exist yet. The next pass rebuilds the Job and refuses a template whose digest disagrees |
| A migration's approval: its `Consumed` condition | The `dispatchStarted` write, then the one create | An approval spent with nothing dispatched. Consumption is evidence, not permission, so it authorizes no second attempt |
| `dispatchStarted` | For a migration, the one permitted create. For a schema, its approval's `Consumed` condition, then the create | A claim that says a Job may exist. The next pass adopts the Job it finds, or declares the outcome unknown; it never creates again. A schema's approval may still be unspent here, and the unknown outcome spends it before the pending observation is written |
| A schema's approval: its `Consumed` condition | The one permitted create | A marker and a spent approval with no Job behind them. The next pass declares the outcome unknown, as in the row above |
| `jobUID` | An Event and the telemetry | Covered by the marker above: the next pass finds the Job under the reserved name and adopts its UID |
| The Job's cleanup TTL | The outcome status patch | A Job carrying a TTL under a live claim. The next pass re-reads the same terminal Job and reaches the same verdict |
| The outcome patch: the claim cleared and the record written together | For a migration, the Lease release. For a schema, the proof, under the same Lease | Either a live claim or a retained record, never both and never neither. Which one decides whether the next pass supervises or proves |
| `pendingLockRelease`, staged in the patch that clears the last record holding the realm | The release itself | The realm still claimed, with a record saying so. The next pass releases it before doing anything else |

The one window that is not closed by a record is a migration run retired as
uncertain. `finishUncertainMigrationApply` releases after its outcome patch, and
only when nothing can still write; it records a release that **failed**, so a
process that stops between that patch and the attempt leaves the Lease to
expire. A schema stages the obligation inside the patch that settles its proof,
and a migration whose result was read stages it inside the patch that retires
the claim. The window costs time, not safety: the Lease it leaves outlives the
run's own execution deadline. That difference is the first one in the list
below.

## Durable safety state

Which family keeps which record. The prose around this table describes what
each one is for; the table says who has it, and `hack` reads the API types to
check that it still does.

| Field | `PtahSchema` | `PtahMigration` |
| --- | --- | --- |
| `status.activeOperation` | yes | yes |
| `status.pendingLockRelease` | yes | yes |
| `status.pendingObservation` | yes | no |
| `status.unresolvedRun` | no | yes |

A field one family keeps and the other does not is not automatically a gap. It
is a gap where the obligation is the same and only the machinery differs, which
is what the section below separates out.

## Where the families differ

Every difference below is a place the same obligation is met by different
machinery, which is what [#225](https://github.com/stokaro/ptah-operator/issues/225)
exists to remove.

A failed release used to be recoverable for a schema and not for a migration.
Both families now keep the complete credential-free release request in
`status.pendingLockRelease` until an idempotent release succeeds, and the top
of every pass retries it before anything else. What remains different is one
window. A schema stages the obligation inside the status patch that settles its
proof, and a migration inside the patch that retires a claim whose result it
read, so a crash between the patch and the release is covered by the record. A
migration run retired as uncertain records only a release that failed, and
leaves a crash at that instant to lease expiry.

The database goes back at a different point. A schema holds it through the
reading that settles the account; a migration hands it back once nothing its
claim dispatched can still write, and reads the history without it.
[Release coordination](#release-coordination) says why both are safe, and
[#457](https://github.com/stokaro/ptah-operator/issues/457) is where one rule
decides it for both.

The proof is a prioritized operation for a schema and an ordinary reading for a
migration. `reconcilePendingObservation` runs before suspension, before the
generation gate and before the realm census, so a schema owing proof cannot be
overtaken by anything. A migration's unresolved record is consulted only when a
History reading happens to run, and a suspended migration does not read at all,
so the record cannot clear while suspension stands.

The record survives deletion for a schema and does not for a migration. The
schema finalizer refuses to come off while proof is owed. A migration
deliberately does not hold deletion, and announces the loss through a Warning
Event instead, which lasts as long as the cluster's Event retention.

Pod evidence is durable for a schema and re-derived for a migration.
`status.pendingObservation` keeps the Apply Pod UIDs and their count, so more
than one Pod forces an unknown outcome even after the Job is collected. A
migration re-reads the Job and its Pods on every pass, which answers the same
question while the Job exists and answers nothing after its cleanup TTL.

The block on replay is a guard in both families now. A schema owing proof
reaches only read-only claims. A migration reads its record where the claim is
taken and refuses there, rather than relying on the History reading forcing
`Blocked` and clearing `status.plan` so a claim fails for want of one. Those
two sites still hold, and each of them is about something else -- one publishes
a phase, the other publishes a plan -- so the invariant no longer depends on
neither being given a branch that leaves a plan standing.

Two condition reasons still gate schema behavior:
`predecessorApplyRetirementPending` and `executionBindingCleanupPending` decide
whether a retired binding's Apply Job is looked for and adopted. They gate the
quality of the evidence rather than permission to run -- without an adopted UID
the pass falls back to the immutable `ObserveAfter` horizon -- but a durable
comparison is already there beside them, and the shared module is where they
should read it instead.
