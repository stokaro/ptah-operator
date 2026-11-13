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

The order of the writes that take a claim to its Job is decided once, for
both families, and so is what retiring a claim owes the database. Each family
supplies the rest -- how it authorizes a claim, what its Job is, where it keeps
its status -- and this page puts both families' enforcement points beside each
other, because an obligation stated once and enforced twice drifts, and that
is where the drift shows.

Symbols named without a package live in `internal/controller`:
`schema_controller.go`, `schema_binding_retirement.go` and
`schema_read_only_job_release.go` for `PtahSchema`, `migration_controller.go`
and `migration_apply.go` for `PtahMigration`, and `read_only_job_release.go`
for the rule a read-only claim of either family keeps. A dotted name such as
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

## Operation types

Which obligations a claim carries depends on its operation type, and both
families read the answers from one table: `mutationlifecycle.SchemaOperation`
and `mutationlifecycle.MigrationOperation`, which also name the phase a claim
publishes and the operation the runner is told to perform.

| | Mutating | Holds the database realm | Carries out a proof |
| --- | --- | --- | --- |
| `PtahSchema` | Apply | Apply and Plan, and an Observe while it proves an Apply | Observe and Plan |
| `PtahMigration` | Apply | Apply | none |

A type this binary does not define, which a resource written by a newer
operator can carry, answers no to every question, and in particular is not
read-only: a site that acts on "not mutating" would act on it.

## The dispatch sequence

Both families take a claim whose Job does not exist yet to its one permitted
create through one sequence of steps, `mutationlifecycle.Dispatch`, and each
step's durable write is made before the next step starts:

1. Authorize: re-read everything the claim was decided from.
2. Lease: take the database realm under the claim's own epoch.
3. Stage: write what the Job reads besides the Job -- a schema Apply's plan
   projection.
4. Snapshot: make the Pod admission envelope durable.
5. Consume: spend the approval that authorized a mutating claim.
6. Mark: write `dispatchStarted`.
7. Create the Job, once.
8. Confirm: read the Job back and hold it to the claim.
9. Record the Job's UID.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `mutationlifecycle.Dispatch` from `reconcileActive` |
| `PtahMigration` | `mutationlifecycle.Dispatch` from `reconcileActiveMigration` |

Each position has a reason. Authorization comes first, so a claim whose
authorization moved is retired without waiting for a Lease another claimant
holds, and without taking the realm for work that will not run. Everything a
Job depends on -- the realm under the claim's epoch, the plan it mounts, the
envelope it is judged by, the approval it spends -- is durable before the mark
that says a Job may exist, so every state in which a Job may exist carries the
whole account of what authorized it. The mark comes before the one create, so
the create is always covered by it, and the created Job is read back before
its UID is recorded.

The approval is spent before the mark. The mark is what turns a missing Job into an outcome nobody established, so
nothing that can fail may sit between the mark and the create, and spending the
approval is a write that can fail. A process that stops between the two
writes leaves a spent approval and no mark: the same claim dispatches on the
next pass under the approval it already spent, since the claim holds that
approval by reference and only a claim looking for a new approval skips a
spent one. A claim retired there instead leaves the approval spent, which asks
a person to approve again and authorizes nothing. Marking first would leave a
mark beside an approval not yet spent, which the uncertain path would have to
find and spend afterwards.

`internal/controller/dispatch_order_test.go` drives each family through every
step with a fault at that step, reads what the API server holds when the fault
fires, and runs the passes that recover. Each row is also run against a driver
with the two steps its boundary is about exchanged, and has to fail there with
the problem the boundary prevents.

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

A retried Plan expects no epoch at all. The retry hands the failed attempt's
Lease back in the write that moves the claim to its next attempt, and that
attempt has dispatched nothing when it takes the realm again, so the epoch it
gets is adopted the same way. A proof is not: it reads under the epoch of the
Apply it accounts for, so a proof with no epoch to compare has lost
continuity before it dispatches anything.

## Resolve the admission envelope

The Pod admission snapshot is durable before any Job carries its digest, and
resolving it is its own boundary: the pass that writes it returns without
creating anything. A later pass rebuilds the Job and refuses a template whose
digest disagrees with the snapshot.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `podintent.Resolve`, digest compared by `mutationlifecycle.Dispatch` |
| `PtahMigration` | `podintent.Resolve`, digest compared by `mutationlifecycle.Dispatch` |

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
| `PtahSchema` | `planstore.Project`, the stage step of `mutationlifecycle.Dispatch`; the controller-write webhook admits a projection only for an Apply that has not crossed the boundary |
| `PtahMigration` | Nothing to project: a migration plan carries no SQL, and the Apply reads the files from its verified artifact |

## Cross the dispatch boundary

`dispatchStarted` is the field that converts "the Job is missing" from "create
it" into "the outcome is unknown". It is written before the create and never
cleared for a mutating claim.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `mutationlifecycle.Dispatch` writes it; a missing Job reaches `finishUncertainApply` |
| `PtahMigration` | `mutationlifecycle.Dispatch` writes it; a missing Job reaches `finishUncertainMigrationApply` |

This is the central failure window. A process that dies between the write and
the create leaves a claim that says a Job may exist. The next pass either finds
it and adopts its UID, or finds nothing and declares the outcome unknown. It
never creates the Job a second time.

Finding nothing under a fresh marker is not yet a lost Job. After a node pause
two managers act at once until the old leader's lease runs out, and one of them
can read the other's marker before that manager's create lands. So a pass over
an active claim that finds it marked, with no recorded Job UID and no Job,
waits `mutationlifecycle.MissingJobGrace`, 55 seconds, before it declares the
outcome unknown.

The dispatching manager runs its mark-to-record steps for at most 40 seconds,
`mutationlifecycle.DispatchCriticalSection`. That is longer than the 30 seconds
the chart gives the Job-write webhook, and longer than the 34 seconds the API
server allows a create, so the API server answers the create first: it stores
the Job, or it refuses the create, and a refusal takes the marker back with the
approval kept. A shorter bound made the client give up on a create the server
was still admitting, and the claim was retired with its approval spent. A
client that gives up at the bound anyway cancels the request on the API server.
The write that withdraws the marker or retires the claim after the create runs
on a context of its own, for up to 10 more seconds, so a create that ended with
the bound does not make it fail, and the grace waits for that write too. The
last 5 seconds of the grace cover a cancellation arriving and a storage write
already in flight. The Job-write webhook also reads the claim uncached, so a
create whose admission runs after the claim was retired is refused. A
dispatcher that cannot cancel, because it is frozen or partitioned, leaves the
API server to finish the request on its own bounds. That case fails closed: a
Job stored under a retired claim runs no Pod, because Pod admission refuses a
Pod whose claim is no longer active.

The wait is measured on the reading manager's own clock, from the first time
it saw that claim so, because the marker carries no time and another manager's
clock is not this one's. It is kept in memory, so a restarted manager waits
again, which only delays a verdict that fails closed. A claim that recorded its
Job's UID saw the create return, and its missing Job is unknown at once. A
resource being deleted does not wait either: the Job-write webhook refuses a
create for an owner being deleted.

A dispatch holds its family's reconcile worker while it runs. Each controller
runs one worker, the default of the pinned controller-runtime, so the family's
other resources wait behind a dispatch for up to 50 seconds. A Job-write
webhook that does not answer holds the worker for its whole 30 seconds on every
attempt, and the refused create is tried again at the per-item backoff, which
starts at 5 milliseconds and doubles: about a dozen attempts pass before the
backoff reaches the webhook's timeout, and each of them holds the worker for 30
seconds. That is what waiting for the answer costs instead of an approval.

The approval is spent before the marker, in both families, so every marked
claim's approval is already spent ([The dispatch sequence](#the-dispatch-sequence)
says why that order). Consumption is evidence that a decision was spent, not
permission: a consumed approval is skipped when a claim looks for one, so it
cannot authorize a second dispatch.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `markApprovalConsumed` through `ensureCurrentApproval`, before the marker; skipped by `findApproval` |
| `PtahMigration` | `consumeMigrationApproval` before the marker, skipped by `findMigrationApproval`; an approval deleted or replaced by then retires the claim |

## Create, confirm, record

One create per claim. An error from it, `AlreadyExists` included, is an
uncertain outcome rather than a retry: a retry would rename the claim and
dispatch beside a Job that may already be running SQL.

The one exception is a create the API server refused before storing it:
authentication, authorization, validation or admission turned it away, or
priority and fairness shed it. A webhook that fails closed and cannot be
called is such a refusal, and so is a webhook's own denial whatever code it
carries: the Job-write webhook answers a read it could not make with 500, and
the API server keeps that code on `admission webhook "..." denied the
request`. Nothing stands under the reserved name, so the claim withdraws its
marker and is back where a process that stopped between spending its approval
and marking leaves it; the next pass makes the create under the approval
already spent. A timeout or any other server error may follow the write and
stays uncertain. `mutationlifecycle.ClassifyCreate` decides which answer a
failed create gave. Replacing every manager at once is when this happens: the
replaced managers' webhook endpoints go first, and a leader not yet told to
stop can still be dispatching.

The withdrawal is a status write from the version the marker was written at,
and admission can take the webhook's whole timeout, in which a person or a
GitOps tool may write the resource. `mutationlifecycle.WithdrawMark` reads the
resource again after such a conflict and withdraws again, as long as it holds
the same claim, still marked, with no Job recorded. A claim that moved is left
for the next pass to judge.

The created Job is read back and its whole intent compared against what the
builder produced -- owner reference, labels, annotations and a semantic
comparison of the spec -- before its UID is persisted. The same comparison
holds the Job to the epoch its claim was made under, that epoch to the binding
in force, and its Pod template to the one the admission snapshot recorded.

Whether a Job is the one its claim built is decided in one place,
`jobclaim.Match`, and every reader asks it: the read-back after the create,
the adoption of a Job a stopped pass created, the cleanup of a Job a rotation
retired, and the controller-write webhook when it admits the create, the
cleanup TTL or a plan published from a Job. A reader that can rebuild the Job
holds it to the rebuild; one that cannot, because the inputs may have moved
since dispatch, holds it to the labels and annotations the claim fixes. The
webhook therefore refuses a Job the controller would not recognize as its
claim's, and the controller never touches one the webhook would refuse.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `jobclaim.Match`, then the UID written by `mutationlifecycle.Dispatch` |
| `PtahMigration` | `jobclaim.Match`, then the UID written by `mutationlifecycle.Dispatch` |

A crash between the create and the UID write is covered by the dispatch marker:
the next pass adopts the Job found under the reserved name once `jobclaim.Match`
holds it to the claim, in both families.

## Supervise

Every pass rebinds by UID, renews the Lease, and re-validates that the Job is
still the one the claim named and still owned by exactly this resource. Drift
in any of those is an uncertain outcome for a mutating claim, never a discard.

While the claim's inputs still hold, the same pass rebuilds the Job and holds
the one under the reserved name to it through `jobclaim.Match`, before it
adopts that Job and on every pass that supervises it: the epoch, the Pod
template against the admission snapshot, and the rest of the rebuilt Job. The
manager identity the Job records -- the controller image and revision and the
runner image -- is taken from the live Pod template first, because it binds
nothing and the snapshot pins it. A Job that fails the match, or a claim that
cannot rebuild its Job, is settled the way a lost Job is: a mutating claim's
run is recorded `Unknown` and never gets a second executor, and a read-only
claim runs again under a new attempt. A read-only claim whose Job admission
would collect first waits a bounded time for that Job to stop, as described
below. One whose Job admission would refuse a cleanup TTL leaves it to its own
deadline, and a Warning Event names it.
Once the inputs have moved the claim cannot rebuild its Job. The pass then
holds the Job to what the claim fixes without a rebuild: the epoch, the labels
and annotations, and the Pod template against the admission snapshot. That is
the match the controller-write webhook applies to the Job's cleanup TTL. A Job
that fails it is settled as one that fails the rebuild is, and the claim never
records its UID. A Job that passes is not harvested either: the terminal pass
settles the claim as stale or `Unknown` without reading its result.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `reconcileActive`, `validateAdoptedJobIntent` or `validateJobEnvelope`, and `jobclaim.Match` on every pass |
| `PtahMigration` | `reconcileActiveMigration`, `holdMigrationJobToItsClaim` (`validateAdoptedMigrationJobIntent` or `validateMigrationJobEnvelope`) and `jobclaim.Match` on every pass |

This decides what an operator upgrade does to work in flight. A release that
keeps the execution binding and changes only the manager identity adopts every
run its predecessor dispatched. A release that also changes the operation Job
or its Pod template cannot confirm them: a running `PtahSchema` or
`PtahMigration` Apply is recorded `Unknown` rather than adopted, and nothing
else is applied until that run is accounted for; a running read-only Job is
run again. A migration Apply whose plan was deleted while it ran is
recorded `Unknown` the same way, because its Job cannot be rebuilt without the
plan it runs. A read-only claim runs again once the Job it refused has stopped,
where admission admits that Job's cleanup TTL, so the Job gets the TTL first
(`retryPastRefusedSchemaJob`, `retryPastRefusedMigrationJob`). A claim that
never recorded the Job's UID records it first, where the Job is the one the
claim fixes. It waits no longer than the Job's own deadline and the termination
grace of its Pods, plus 30 seconds for the kubelet and the Job controller to
report, because a Job past its deadline is not `Failed` until every Pod has
terminated, and a Pod on a node that is gone may never terminate.

A read-only Job is created with its cleanup TTL (`workload.CleanupTTLSeconds`),
and an Apply's Job with none. Kubernetes counts the TTL from when the Job
finishes, so it removes a finished read whether or not its result was read, and
removes nothing that still runs. A claim whose Job the TTL collected before its
result was read runs again under a new attempt. `releaseReadOnlyJob` lets go of
a Job that carries a TTL at once, writes nothing to it and names it in no
Warning, so the waits and TTL writes this section describes serve a read-only
Job a release before 0.2.11 created, with no TTL. The waits the database Lease
needs do not depend on the TTL: a `PtahSchema` Plan or proof read that a binding
change retires, and a Plan whose database lock epoch moved, wait for their Job
by the bound below whatever it carries.

Every read-only claim that lets go of its Job settles it by that one rule,
`releaseReadOnlyJob`, in both families: one that runs again past a Job it
refused, one that suspension discards, and a migration's claim that an
execution-binding change retires. A `PtahSchema` claim that a binding change
retires is kept under the retired epoch until its Job stops, as a Plan's claim
has to be to hold its Lease, renews that Lease while it waits, and gets its TTL
then. A Plan whose database lock epoch moved while its Job ran waits for the
Job, renewing the Lease, before its result is discarded; it asks for the Lease
only while it waits, so once it waits no longer it is discarded whoever holds
the Lease, and its release, which names its own epoch, leaves a sibling's Lease
alone. Every one of these
waits keeps the same rule (`stopWaitingForJob`): no longer than the bound above
(`jobStopOverdue`), and not at all while the resource is suspended. A
`PtahSchema` claim that serves post-apply proof waits under a suspension too
(`schemaClaimMayWait`): proof outranks suspension, so suspension does not
discard that claim, and the proof keeps renewing its Lease anyway. Deletion
waits on the running Job of a claim that holds the lock by the bound alone,
suspended or not: an Apply's until the Job stops, and a Plan's or a proof
read's until the bound, where it discards the claim and records a proof it had
not taken as `Unknown`. The bound also ends the wait of a live read-only claim
over its running Job (`readJobStopOverdue`), which runs again under a new Job,
and of a claim whose recorded Job is gone over the Pods that Job owned
(`podsStillStopping`: each Pod's own deadline, or the end of its deletion grace,
plus 30 seconds). It holds whether or not the Job controller has marked the Job
`FailureTarget`: a Job controller that is down marks nothing, and a mark it does
write only brings the bound earlier. The operator dates the bound by its own
clock against the start the Job controller recorded, so a skew between the two
larger than the grace and the 30 seconds lets go of a read the Job controller
still counts as running; its Pod may read on, and nothing uses what it reads. A
Plan's wait checks the bound before it renews the Lease, so its Lease is not
renewed past the bound. A proof read's Lease is different: whether the proof is
an Observe or a Plan, the Lease is the Apply's pending observation's. The pass
renews it before it reaches the bound check (`acquirePendingObservationLock`),
and it stays held under the next attempt until a proof settles the Apply, so
for a proof the bound ends the wait for one Job, not the hold. A migration read
holds no Lease. A claim that stops waiting names the Job it leaves running in a
Warning Event: `ReadJobOverdue` for a deleted schema's read and for the two
just named, with the Pod that may still read and its node.

A claim that harvests its finished Job asks for the Job's TTL before it acts
for the Job: before it consumes the result or records the run, retries, or is
discarded because its inputs moved (`harvestJob`, `harvestMigrationJob`). An
Apply that failed, or whose outcome is unknown, asks for the TTL its log is
kept by before the run or the unknown outcome is recorded
(`retainFailedApplyJob`), and so does the cleanup of an Apply an
execution-binding change retired. The pass has proved the Job and read the
result from the Job's own Pods by then, or records that nobody did, so a
refusal, Forbidden from the controller-write webhook or Invalid from a
ValidatingAdmissionPolicy, names the Job in a Warning Event and the pass goes
on. For a failed Apply that means a refusal of the five-minute fallback as well
as of any 24 hours it asked for. Admission reads the claim, or the retirement
record, as stored, so a refusal can also say that it moved after the pass read
it, as under a second manager. The pass reads it again and goes on only where
it still names the Job (`settleHarvestedJobCleanup`); otherwise it fails, and
the next pass reads it again. Any other failure of the write fails the pass, as
everywhere a claim asks for a TTL.

Suspension prevents new operations. It does not necessarily stop one that is
already running: nothing deletes a running Job or its Pod, and a cleanup TTL
removes a Job only once it has finished. Suspension cannot discard a dispatched
mutating claim in either family. A resource suspended mid-Apply keeps its claim
and keeps renewing the Lease. A read-only claim that serves no proof is
discarded at once, because
suspension is the stop button. A Job created with its cleanup TTL is left to
it, and Kubernetes deletes the Job five minutes after it stops. A Job a release
before 0.2.11 created carries no TTL: the claim gives it one first if it has
finished, and a Warning Event names one still running, which runs until it stops
by itself or reaches its deadline and then stays until it is deleted. A claim that
an execution-binding change was holding over its Job, in either family, and a
`PtahSchema` Plan whose database lock epoch moved, are let go of the same way.
The read that proves a `PtahSchema` Apply is not discarded, because post-apply
proof outranks suspension ([Suspended, and losing a
lock](../reconciliation/#suspended-and-losing-a-lock)). It runs its Job to the
end, and one whose Job it cannot confirm waits for that Job to stop and gives it
its TTL before it reads again.

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
| `PtahSchema` | a Plan whose target matches the pending snapshot and reports no changes, in `consumeResultWithTransport` |
| `PtahMigration` | a History reading with nothing pending on the same database, in `migrationUnresolvedRunSettledBy` |

Convergence is not attribution. A schema whose uncertain Apply converges is
recorded as converged with no Apply attributed to it, because the reading
proves the database matches and proves nothing about what changed it.

## Release coordination

Release always follows the durable status write, never leads it, and the
write that retires a claim carries the release it owes as
`status.pendingLockRelease`. A crash between the write and the release leaves
the record, which the top of the next pass performs; the other order hands the
database back under a claim that still reads as live.

Each family retires a claim through one function, which clears the claim,
keeps whatever record the claim leaves and stages the release in one status
patch, then performs the release. Which release a retirement owes is decided
once, for both families, by `mutationlifecycle.Retirement`: from the claim as
it stood and from why it is going -- discarded, accounted for by its own
result, or a run nobody accounted for.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `retireClaim`, retried by `completePendingLockRelease` |
| `PtahMigration` | `retireMigrationClaim`, withheld by `dispatchedApplyMayStillWrite`, retried by `completeMigrationPendingLockRelease` |

The two families release at different points, and that difference is one input
to the decision: whether the record a mutating run leaves keeps the realm.

A schema holds the database until the account is settled. Every Apply, read or
uncertain, leaves `status.pendingObservation` carrying the claim's Lease epoch.
The pending observation renews the Lease, waits until no Apply Pod can still
run, and claims the read-only Observe and Plan that settle it. Both run under
that same Lease, and retiring either of them releases nothing while the proof
is owed. Both are bound to the target the Apply's plan names, as the Apply is:
the runner refuses either one before it starts the child when the URL names
another target, and the resource stops `Blocked` with `TargetIdentityChanged`
while it holds the Lease. Only the status write that settles the proof stages the release. No other claimant can change the database
between the Apply and the reading that accounts for it, so an intervening
mutation cannot be mistaken for this plan's convergence.

A migration holds the database until nothing its claim dispatched can still
write. A run whose result was read stages the release in the write that retires
the claim. A run retired as uncertain (`finishUncertainMigrationApply`) stages
it in that write too, and only when `dispatchedApplyMayStillWrite`, asked
before the write, says no: a Job that is not terminal, a Pod it owns that has
not stopped, and a read that could not say all count as "may still be writing",
and the Lease is left to expire instead. Nothing that can still write stops
being able to, so a release recorded on that answer is safe whenever the next
pass performs it. The reading that settles the account comes afterwards and takes no
Lease: the one `VerifyingHistory` hands to after a run that reported what it did, and
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


## Retire an execution binding

A change to what the execution binding names -- the executor image, the Ptah
version, the runner protocol or the controller-state version -- installs a new
epoch, and work authorized under the old one cannot finish under the new one.
Both families retire it; they differ in what is left over.

| | Enforcement |
| --- | --- |
| `PtahSchema` | `rotateExecutionBinding` writes `status.pendingBindingRetirement` with the epoch, `reconcileBindingRetirement` works it off, `settleRetirement` clears it |
| `PtahMigration` | `reconcileMigrationExecutionBinding` retires a claim in the write that installs the epoch, `letGoOfRetiredReadOnlyMigrationJob` holds a read-only one over its running Job first, `applyUncertainUnderBindingChange` records a dispatched Apply first |

A migration leaves no record behind, because it moves the epoch only once
nothing it retires is left. A claim goes in the write that installs the epoch.
A read-only claim over a running Job that carries no cleanup TTL, as one a
release before 0.2.11 created, holds that write back, under the binding status
still records, until the Job stops or reaches its stop bound, and the Job gets
its cleanup TTL first. A Job created with its TTL holds nothing back, and a
suspended resource does not wait. A dispatched Apply becomes an unresolved run
first, so the epoch moves on the next pass once no claim is in flight.

A schema leaves up to two things, and names them in the write that installs
the epoch: the retired plan, whose approvals are still to be marked stale, and
the Job the retired claim dispatched, by name and, once known, by UID. The plan
leaves `status.plan` in that write, so the plan status names always belongs to
the current epoch. A read-only claim that holds no database lock, an Observe
that proves nothing, a Resolve or a Verify, is retired at once over a Job
created with its cleanup TTL. Any other read-only claim stays in
`status.activeOperation` until a Job of this resource under the recorded name
has stopped, whether or not the
controller can prove that Job's envelope, so a Plan's Lease is not handed back
while its Pod can still reach the database, and it renews the Lease it holds on
every pass while it waits. It stays no longer than the Job's
stop bound, and not at all once the resource is suspended unless it serves a
proof, by the rule every read-only wait keeps, and a Warning Event names a Job
it leaves running. The claim is then retired, and a Job that carries no TTL
gets its cleanup TTL only when its envelope carries the retired epoch and
the claim's admission snapshot. A dispatched Apply, or an outcome-unknown Apply
of the retired epoch that no pass harvested, stays in
`status.pendingObservation`, and no proof is claimed until its Job is accounted
for: its UID adopted from a late create, or given up on at the Apply's
`ObserveAfter` horizon, and its cleanup TTL set once it stops. The
controller-write webhook admits either TTL only for the Job the record names,
and judges the Job by the matcher the controller uses, `jobclaim.Match`, so
declared Pod metadata neither hides a Job from the controller nor passes the
webhook unchecked.

Each obligation is removed as it is met, and the record with its last one. No
condition reason and no phase takes part in any of these decisions, so a later
refusal that rewrites a reason changes nothing. No other rotation starts while
the record is present, so it always describes exactly one retired epoch. A
rotation that arrives meanwhile waits for the Job the record names: for a
read-only claim no longer than that Job's deadline, the grace of its Pods and
30 seconds, or not at all once the resource is suspended unless the claim
serves a proof, and for an Apply no longer than the proof, which waits for the
same Job, would have waited anyway.

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
| The approval's `Consumed` condition, for a mutating claim | The `dispatchStarted` write | An approval spent with nothing marked. The same claim dispatches on the next pass under the approval it spent; a claim retired instead leaves the approval spent, which authorizes nothing |
| `dispatchStarted` | The one permitted create | A claim that says a Job may exist, with its approval already spent. The next pass adopts the Job it finds, or declares the outcome unknown: for an active claim once the Job has stayed missing for `MissingJobGrace`, for a resource being deleted at once. It never creates again |
| `jobUID` | An Event and the telemetry | Covered by the marker above: the next pass finds the Job under the reserved name and adopts its UID once `jobclaim.Match` holds it to the claim |
| The Job's cleanup TTL | The outcome status patch | A Job carrying a TTL under a live claim. The next pass re-reads the same terminal Job and reaches the same verdict |
| The outcome patch: the claim cleared, the record written and the release it owes staged, together | For a migration, the Lease release. For a schema, the proof, under the same Lease | Either a live claim or a retained record, never both and never neither. Which one decides whether the next pass supervises or proves |
| `pendingLockRelease`, staged in the patch that clears the last record holding the realm | The release itself | The realm still claimed, with a record saying so. The next pass releases it before doing anything else |
| A schema's `pendingBindingRetirement`, written in the patch that installs a new execution-binding epoch | The retired plan's approvals marked stale; the retired Job's UID adopted and its cleanup TTL set | The new epoch with the record beside it. The next pass works the record off before anything else, a Job's TTL already set counts as met, and no other rotation starts until the record is gone |

Every window is closed by a record. A migration run retired as uncertain asks
whether anything it dispatched can still write before its outcome patch, and
stages the release in that patch when nothing can; when something can, it
stages none, and the Lease expires after the executor has stopped.

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

The database goes back at a different point. A schema holds it through the
reading that settles the account; a migration hands it back once nothing its
claim dispatched can still write, and reads the history without it. One rule,
`mutationlifecycle.Retirement`, decides both, and the families differ in one
input to it. [Release coordination](#release-coordination) says why both
orders are safe.

The proof is a prioritized operation for a schema and an ordinary reading for a
migration. `reconcilePendingObservation` runs before suspension, before the
generation gate and before the realm census, so a schema owing proof cannot be
overtaken by anything. Only deletion and [a person's
settlement](../../use/recovery/#a-schema-whose-target-moved) end a proof that no
read can finish. A migration's unresolved record is consulted only when a
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
migration. A migration moves the epoch only in the write that retires what is
left, and holds that write back while a read-only claim's Job runs. A schema
keeps a read-only claim until its Job stops or reaches its stop bound, and not
at all once suspended unless the claim serves a proof, and keeps the retired plan
and the retired Apply's Job named until they are tidied, so it carries
`status.pendingBindingRetirement` until they are.
[Retire an execution binding](#retire-an-execution-binding) says what each
family retires and when.
