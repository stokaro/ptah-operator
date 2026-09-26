---
title: Execution guarantees
description: What the operator promises about database work, where each promise is enforced for each family, and the test that proves it.
---

`PtahSchema` converges a declared schema and `PtahMigration` runs a versioned
sequence. Both run their database work the same way: they share Job
construction, framing, fingerprints and Leases, and using the same components
is not the same as keeping the same promises. This page maps each promise to
the place it is enforced in each family, the durable state that carries it, and
the test that proves it.

Read it when you need to know whether a guarantee you rely on holds for the
family you are running, and when you are changing either controller.

## Only work a decision authorized executes

A plan is executed only while the decision that authorized it still names the
exact plan, and only once.

| | `PtahSchema` | `PtahMigration` |
| --- | --- | --- |
| Enforced in | `findApproval` and the Apply claim | `findMigrationApproval` and `claimMigrationApply` |
| Bound to | plan name and UID, plan fingerprint, artifact digest, coordination and target identity digests, policy fingerprint, execution binding | the same, plus the history fingerprint the sequence was selected against |
| Spent by | `markRecordedApprovalStale` when the plan stops being current | `consumeMigrationApproval` at the dispatch boundary |
| Failure behavior | the approval goes stale; the resource waits for a decision on the new plan | the same |
| Proved by | `TestRecordedApprovalStaleIgnoresSameNameReplacementUID` | `TestMigrationApplyClaimsTheApprovedPlan` |

An approval names a UID, not just a name. A plan republished under the same
deterministic name is a different object, and an approval for the previous one
authorizes nothing.

## Inputs that moved do not execute

Every claim records the fingerprint of the inputs it was decided from, and that
fingerprint is compared again before dispatch and after the Job.

| | `PtahSchema` | `PtahMigration` |
| --- | --- | --- |
| Enforced in | the claim's input fingerprint, re-derived each pass | `migrationInputFingerprint`, re-derived each pass |
| Failure behavior | the plan is discarded and the database observed again | the claim is discarded, or the plan dropped by `discardMigrationPlan` |
| Proved by | `TestStalePlanEvidenceForcesFreshObservation` | `TestMigrationApplyDiscardsAPlanTheEvidenceNoLongerSupports` |

A dispatched Apply is the exception in both families. Once SQL may have run,
inputs that moved make the outcome uncertain rather than discardable — see
below.

## A dispatched Apply is never re-run

Once an Apply Job may exist, the operator does not create another one under a
fresh name. What that Job did becomes a question for the database.

| | `PtahSchema` | `PtahMigration` |
| --- | --- | --- |
| Enforced in | `finishUncertainApply` | `finishUncertainMigrationApply` |
| Durable evidence | `status.pendingObservation`, which owes a fresh reading | `status.unresolvedRun`, which names the attempt, its Job, its plan and the database |
| Cleared by | the observation it owes, completing | a History reading of the same database with nothing of the artifact pending, in `migrationUnresolvedRunSettledBy`; or a person, by hand |
| Failure behavior | the resource re-observes before it plans again | the resource is `Blocked`; nothing runs until the record is cleared |
| Proved by | `TestMissingDispatchedApplyJobForcesObservationWithoutRecreation` | `TestMigrationApplyIsNeverRecreatedOnceDispatched` |

The families differ here on purpose, and the difference is the one to
understand before relying on either.

Both settle from a reading, and what a reading can prove differs. A schema is
declarative: a fresh observation of the live database is a complete answer, so
a Plan with no changes settles the uncertainty whatever the Apply did. A
migration is a sequence of statements written by hand; a file that committed
part of itself cannot be re-run, and no reading tells the operator which part
committed. So a migration settles only from a reading that leaves nothing to
re-run: the same database, with every migration the artifact carries recorded
applied. A run that did not finish its sequence leaves a migration pending, and
the record then stands until a person repairs the database or clears the record
by hand. An unrelated refusal arriving later does not clear
it — the record is a field, not a condition reason.

## The database is held while a run may still write

Coordination is released when the work that held it can no longer write, not
when the controller stops watching it.

| | `PtahSchema` | `PtahMigration` |
| --- | --- | --- |
| Enforced in | `acquireApplyLock`, with the release owed durably once it is owed at all | `acquireMigrationApplyLock`, with `dispatchedApplyMayStillWrite` deciding whether it is owed yet |
| Held until | the reading that settles the Apply's account, which runs under the same Lease; the release is recorded in the write that settles it | the dispatched Job is terminal and every Pod it owns has stopped, or the Lease expires, which it cannot do before the run's absolute execution deadline has passed; the reading that settles the account runs afterwards, without the Lease |
| Failure behavior | the persisted release completes on a later pass | the same where the run's result was read; a run retired as uncertain records only a release that failed, so a crash at that instant leaves the Lease to expire |
| Proved by | `TestPersistedTargetLockReleaseRecoversAfterManagerCrash` | `TestUncertainMigrationApplyKeepsTheDatabaseWhileItsPodMayRun` |

A missing Job is insufficient proof that the old process stopped. A schema
requires the Pod-level answer before another claim may take the database, and
renews the Lease for as long as it has none. A migration requires it before it
hands the database back itself; without it the Lease runs out, and the runner
has killed the executor at its execution deadline before that can happen. Why
a migration may hand the database back before its account is settled is
[Release coordination](../mutation-lifecycle/#release-coordination).

## One database has one writer

More than one resource addressing the same database is refused unless every
claimant declares the sharing.

| | `PtahSchema` | `PtahMigration` |
| --- | --- | --- |
| Enforced in | `takeRealmCensus`, before any Job is dispatched | the same function, in the same place |
| Bound to | the coordination digest of the engine and coordination key | the same |
| Failure behavior | `Blocked` with reason `RealmConflict`; the verdict is re-taken on a bounded cadence because another resource's edit produces no event here | the same |
| Proved by | `TestSchemaBlocksOnAContestedRealmWithoutDispatchingAJob` | `TestMigrationBlocksOnAContestedRealmWithoutDispatchingAJob` |

This is the one guarantee with a single implementation for both families, and
it is the model the rest is moving toward.

## Stored state is never interpreted by a manager that predates it

Durable state carries the version of the contract that wrote it, and a manager
compiled for an older contract refuses rather than interpreting it.

| | `PtahSchema` | `PtahMigration` |
| --- | --- | --- |
| Enforced in | `VerifyStoredControllerState`, at startup and before every CRD update | the same function, over the same scan |
| Stored at | `status.executionBinding`, `status.plan`, `status.applied`, `status.pendingObservation.plan`, and the plan and approval specs | `status.executionBinding`, and the plan and approval specs |
| Failure behavior | the upgrade fails and the active release keeps running | the same |
| Proved by | `TestRuntimeVerifierRejectsStoredFutureControllerState` | the same test, over the same scan of all six kinds |

Which manager reads which state, and what an operator sees when the fence
refuses, is on [Releases and provenance](../../support/releases/).

## Where the families still differ

Which of the asymmetries above are deliberate, and which are not:

- **By design:** what settles an uncertain Apply. Both families settle from a
  reading of the database. A schema's reading is a complete answer; a
  migration's settles only when nothing of the artifact is left to re-run, so a
  run that left work pending waits for a person.
- **By design:** what an approval is bound to. A migration approval also names
  the history fingerprint, because a database that moved between planning and
  approval changes which versions are pending.
- **Closed:** the durability of a release. Both families owe it through the
  same durable record and retry it before anything else in a pass, and one
  implementation decides it for both. What still differs is the width of the
  crash window: a migration run retired as uncertain records only a release
  that failed. The rest of
  [issue #225](https://github.com/stokaro/ptah-operator/issues/225) is what
  remains.
- **Not yet aligned:** when the database is handed back. A schema holds it
  through the reading that settles the Apply; a migration releases it once
  nothing it dispatched can still write, and settles the account from a reading
  that takes no Lease. Both are safe, for the reasons
  [Release coordination](../mutation-lifecycle/#release-coordination) gives,
  and [issue #457](https://github.com/stokaro/ptah-operator/issues/457) is
  where one rule decides it for both.

Which family keeps which record is stated once, on
[Mutation lifecycle](../mutation-lifecycle/#durable-safety-state), where it is
checked against the API types. Restating it here is how the sentence above came
to be wrong for a day.
