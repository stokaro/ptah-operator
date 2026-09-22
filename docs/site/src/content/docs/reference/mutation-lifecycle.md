---
title: Mutation lifecycle
description: The obligations a mutating operation carries, and where each family enforces them.
---

Both resource families run SQL the same way: authorize the exact work, persist
the claim, cross a dispatch boundary that is durable before anything external
happens, account for the outcome, and hand the database back only once the
account is settled. They enforce that in two separate implementations, which is
why this page exists. An obligation stated once and enforced twice drifts, and
the way to see the drift is to put both enforcement points beside each other.

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

The approval is consumed in the same stretch, immediately before the create.
Consumption is evidence that a decision was spent, not permission: a consumed
approval is skipped when a claim looks for one, so it cannot authorize a second
dispatch.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `markApprovalConsumed`, skipped by `findApproval` |
| `PtahMigration` | `consumeMigrationApproval`, skipped by `findMigrationApproval` |

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
| `PtahSchema` | `stageTargetLockRelease`, retried by `completePendingLockRelease` |
| `PtahMigration` | `releaseMigrationApplyLock`, withheld by `dispatchedApplyMayStillWrite` |

Release is withheld while the executor may still be running. A Job that is not
terminal, a Pod that has not stopped, and a read that could not say are all
treated as "may still be writing", and the Lease is left to expire instead.

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
of every pass retries it before anything else. What remains different is the
window: a schema stages the obligation inside the status patch that clears the
claim, so a crash between the two is covered by the record, while a migration
records a release that failed and leaves a crash at that instant to lease
expiry.

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
