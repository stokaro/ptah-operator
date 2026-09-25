---
title: Events
description: Every Event the operator records, what it says, and which resource carries it.
---

`kubectl describe ptahschema orders` prints these under the status, and they are
the fastest way to see what the operator has just done. They are diagnostic
text: read them to understand, and match on Conditions to decide.
[Condition reasons](../condition-reasons/) is the interface for deciding, and
says why.

An Event is recorded on the resource a person would look at, which is not always
the one the operator was working on: an approval that went stale carries the
Event, because that is the object whose author has to act.

Event messages are bounded. A failure long enough to be truncated is recorded in
full on the resource's own status and in the operation's Job.

| Reason | Type | Recorded on | What it says |
| --- | --- | --- | --- |
| `ApplyOutcomeUnknown` | Warning | `PtahSchema` | An Apply ended with no proof of whether it changed the database, so a read-only observation starts to establish what happened. |
| `ApprovalAccepted` | Normal | `PtahSchema` | An authenticated approval was accepted for the current immutable plan. |
| `ApprovalRequired` | Normal | `PtahSchema` | The current immutable plan needs an approval before anything applies it. |
| `ApprovalRevoked` | Warning | `PtahSchema` | The recorded approval became invalid before the Apply Job was created, so no Job was created. |
| `ApprovalStale` | Warning | `PtahSchemaApproval` | This approval no longer matches the plan it was written for, and will not be consumed. |
| `ArtifactVerificationRefused` | Warning | `PtahSchema` | Verification refused the resolved artifact, so no plan is produced from it. |
| `ExecutionBindingChanged` | Warning | `PtahSchema`, `PtahMigration` | The runtime identity the evidence was produced under was retired. A schema closes the approval boundary and refreshes read-only; a migration discards the operation claim. |
| `LeaseContinuityLost` | Warning | `PtahSchema` | Database lock ownership was not continuous, so the result it covered is discarded and the proof restarts. |
| `MigrationJobCleanupDeferred` | Warning | `PtahMigration` | A Job's cleanup could not be scheduled, so the Job keeps its own deadline rather than relying on one. |
| `MigrationRunFinished` | Normal or Warning | `PtahMigration` | An Apply finished and the message names what it did. The type follows the outcome: Applied and UpToDate are Normal, and Failed, Partial and Unknown are Warnings. |
| `MigrationRunUncertain` | Warning | `PtahMigration` | A migration run ended without establishing what it committed, and the resource records that until something resolves it. |
| `OperationCompleted` | Normal | `PtahSchema` | An operation finished and its result was accepted. |
| `OperationFailed` | Warning | `PtahSchema`, `PtahMigration` | An operation failed. The message carries the failure. |
| `OperationRetried` | Warning | `PtahMigration` | An attempt failed and another will follow. The message names the attempt number. |
| `OperationStarted` | Normal | `PtahSchema`, `PtahMigration` | A Job was created for the named operation. |
| `PlanStale` | Warning | `PtahSchema` | The plan stopped matching what it was computed against, so nothing applies it. |
| `ProtectedTableRefused` | Warning | `PtahSchema` | A plan would change a table `spec.policy.protectedTables` fences off, so no plan is published. |
| `ResultReadTimedOut` | Warning | `PtahSchema`, `PtahMigration` | Reading an operation's result took longer than the bound it is given. |
| `TargetLockReleaseOwed` | Warning | `PtahMigration` | The database lock was not released, and the release the claim owes will be retried. |
| `UnresolvedRunDiscarded` | Warning | `PtahMigration` | Deleting this resource discarded the record of a run nobody accounted for. The message names the outcome, the version, and the database. |
| `VerificationPolicyInvalidated` | Warning | `PtahSchema` | The verification policy stopped matching the plan, so artifact and plan verification were invalidated. |

## A Warning is not always a fault

Most of these say the operator refused to proceed, which is the behavior they
are there for: `ProtectedTableRefused`, `ApprovalRevoked` and `PlanStale` are
each a decision not to change a database on evidence that no longer holds.

The ones that ask for a person are `MigrationRunUncertain`,
`ApplyOutcomeUnknown` and `TargetLockReleaseOwed`: each says something about the
database is not yet established, and each has a page saying what settles it --
[the mutation lifecycle](../../reference/mutation-lifecycle/) for the first two,
and [Operations](../../use/operations/) for the lock.
