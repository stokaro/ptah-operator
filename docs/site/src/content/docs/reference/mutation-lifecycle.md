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
`schema_controller.go` and `schema_binding_retirement.go` for `PtahSchema`,
`migration_controller.go` and `migration_apply.go` for `PtahMigration`. A dotted name such as
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

## Project the plan

A schema Apply Pod reads its plan through ConfigMaps: it holds no Kubernetes
credential, and the kubelet mounts no custom resource, so it cannot read the
plan's chunks. The controller copies the chunks it has just verified into
immutable ConfigMaps of the same names, owned by the plan, on every pass that
can still create the Job, and before `dispatchStarted`. Once that marker is
durable a missing Job is an unknown outcome, so nothing that can fail may sit
between the marker and the create.

A projection an earlier pass wrote is read back and has to match, so the step
repeats safely, and a write the API refused leaves the claim for the next pass
to try again. A ConfigMap under a projection name that is not this plan's
projection retires the plan instead: its name is derived from the plan, so no
retry can clear it.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `planstore.Project` in `reconcileActive`; the controller-write webhook admits a projection only for an Apply that has not crossed the boundary |
| `PtahMigration` | Nothing to project: a migration plan carries no SQL, and the Apply reads the files from its verified artifact |

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
| `PtahMigration` | `collectTerminalPodEvidence`, `runner.ParseResultFor`, `awaitFrameArrival`, `terminationSummaryStandIn` |

The frame's own report of which database it opened is compared against the
claim before the account is accepted. A run that reached a database other than
the one it was planned for is uncertain, not failed.

A migration Apply whose log holds no frame, or is gone, is read from the
summary its runner wrote into the Pod's termination message, when that summary names this attempt
and agrees with any frame header the log does hold
([The termination summary](../execution/#the-termination-summary)). The same
code decides it as decides a frame, so it can say less than the frame would
have and never more.

## Retain uncertainty

An unresolved mutating attempt is durable and independent of `phase` and of
every condition reason, because a later refusal rewrites a reason and a record
has to outlive that.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `status.pendingObservation`, written by `pendingObservationFor` |
| `PtahMigration` | `status.unresolvedRun`, written by `recordUnresolvedMigrationRun` |

Each is written by exactly one function. The claim and the record are swapped
in a single status patch, so a crash on either side leaves either a live claim
or a retained record, never both and never neither.

A migration's record also lives outside status, because a restore that drops
status would otherwise drop it. The manager copies it into the resource's
`operator.ptah.run/unresolved-run` annotation before the status patch that
stores it, removes the copy after the status patch that settles it, and
`reconcileUnresolvedRunCopy` puts the record back from the copy when status
comes back without it. `status.resolvedRun` names the attempt that was settled,
so a copy left by an interrupted removal is told apart from one a restore
brought back.

## Prove or refuse

Only a fresh read-only reading settles an uncertain attempt on its own. A Job
exit code never does, and neither does an edit to a condition, an unrelated
approval, a suspension, a spec change or a restart. A migration has one more
way out, which a person takes: a `PtahMigrationRunAcknowledgment` that names the
attempt, settled in `settleUnresolvedRunByAcknowledgment` in the name admission
stamped on it and followed by a fresh reading before anything is planned. A
write to status is not one: the chart refuses it to anyone but the manager.

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

## Retire an execution binding

A change to what the execution binding names -- the executor image, the Ptah
version, the runner protocol or the controller-state version -- installs a new
epoch, and work authorized under the old one cannot finish under the new one.
Both families retire it; they differ in what is left over.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `rotateExecutionBinding` writes `status.pendingBindingRetirement` with the epoch, `reconcileBindingRetirement` works it off, `settleRetirement` clears it |
| `PtahMigration` | `reconcileMigrationExecutionBinding` drops an undispatched claim in the same write, `applyUncertainUnderBindingChange` records a dispatched one first |

A migration leaves nothing behind. An undispatched claim goes in the write that
installs the epoch, and a dispatched Apply becomes an unresolved run first, so
the epoch moves on the next pass once no claim is in flight.

A schema leaves up to two things, and names them in the write that installs
the epoch: the retired plan, whose approvals are still to be marked stale, and
the Job the retired claim dispatched, by name and, once known, by UID. The plan
leaves `status.plan` in that write, so the plan status names always belongs to
the current epoch. A read-only claim stays in `status.activeOperation` until a
Job of this resource under the recorded name has stopped, whether or not the
controller can prove that Job's envelope, so a Plan's Lease is not handed back
while its Pod can still reach the database. The claim is then retired, and the
Job gets its cleanup TTL only when its envelope carries the retired epoch and
the claim's admission snapshot. A dispatched Apply, or an outcome-unknown Apply
of the retired epoch that no pass harvested, stays in
`status.pendingObservation`, and no proof is claimed until its Job is accounted
for: its UID adopted from a late create, or given up on at the Apply's
`ObserveAfter` horizon, and its cleanup TTL set once it stops. The
controller-write webhook admits either TTL only for the Job the record names,
and judges the Job's metadata by the rule the controller uses,
`workload.ValidateClaimedMetadata`, so declared Pod metadata neither hides a
Job from the controller nor passes the webhook unchecked.

Each obligation is removed as it is met, and the record with its last one. No
condition reason and no phase takes part in any of these decisions, so a later
refusal that rewrites a reason changes nothing. No other rotation starts while
the record is present, so it always describes exactly one retired epoch. A
rotation that arrives meanwhile waits for the Job the record names: at most
that Job's own deadline for a read-only claim, and for an Apply no longer than
the proof, which waits for the same Job, would have waited anyway.

Proof that completes after its plan's epoch was retired sweeps that plan's
approvals once more in the same pass, for an approval admitted before the Apply
claim that committed after the rotation's sweep. It needs no record: the
approval boundary closed at the rotation, and a pass that stops before the
status write reads the proof Job again.

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
| A schema plan's projection ConfigMaps | The `dispatchStarted` write | Projections with no Job to mount them. The next pass reads them back, finds them matching and goes on; they are owned by the plan and go with it |
| A migration's approval: its `Consumed` condition | The `dispatchStarted` write, then the one create | An approval spent with nothing dispatched. Consumption is evidence, not permission, so it authorizes no second attempt |
| `dispatchStarted` | For a migration, the one permitted create. For a schema, its approval's `Consumed` condition, then the create | A claim that says a Job may exist. The next pass adopts the Job it finds, or declares the outcome unknown; it never creates again. A schema's approval may still be unspent here, and the unknown outcome spends it before the pending observation is written |
| A schema's approval: its `Consumed` condition | The one permitted create | A marker and a spent approval with no Job behind them. The next pass declares the outcome unknown, as in the row above |
| `jobUID` | An Event and the telemetry | Covered by the marker above: the next pass finds the Job under the reserved name and adopts its UID |
| The Job's cleanup TTL | The outcome status patch | A Job carrying a TTL under a live claim. The next pass re-reads the same terminal Job and reaches the same verdict |
| The outcome patch: the claim cleared and the record written together | For a migration, the Lease release. For a schema, the proof, under the same Lease | Either a live claim or a retained record, never both and never neither. Which one decides whether the next pass supervises or proves |
| `pendingLockRelease`, staged in the patch that clears the last record holding the realm | The release itself | The realm still claimed, with a record saying so. The next pass releases it before doing anything else |
| A schema's `pendingBindingRetirement`, written in the patch that installs a new execution-binding epoch | The retired plan's approvals marked stale; the retired Job's UID adopted and its cleanup TTL set | The new epoch with the record beside it. The next pass works the record off before anything else, a Job's TTL already set counts as met, and no other rotation starts until the record is gone |

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
| `status.pendingBindingRetirement` | yes | no |
| `status.pendingObservation` | yes | no |
| `status.unresolvedRun` | no | yes |
| `status.resolvedRun` | no | yes |

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

An execution-binding rotation leaves a record for a schema and none for a
migration. A migration retires everything in the write that moves the epoch. A
schema keeps a read-only claim until its Job stops, and keeps the retired plan
and the retired Apply's Job named until they are tidied, so it carries
`status.pendingBindingRetirement` until they are.
[Retire an execution binding](#retire-an-execution-binding) says what each
family retires and when.
