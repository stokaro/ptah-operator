---
title: Conditions
description: Which Conditions each resource publishes, and what each one says.
---

Conditions are the operator's stable machine interface. Evaluate the complete
`type`, `status` and `reason`, and use `observedGeneration` to decide whether
what you read describes the current spec.

[Condition reasons](../condition-reasons/) says what every reason means. This
page says which Conditions each kind publishes, which is what a consumer
matching on one needs to know before it can wait for it. Messages are bounded
diagnostic text and may become more detailed without an API version change;
[Events](../events/) are the other diagnostic surface.

Three statuses are used. `Unknown` is not an error: it is the operator saying it
has no current evidence, most often while a proof is outstanding or after a
retired execution binding made the last evidence historical.

## PtahSchema

| Condition | What it says |
| --- | --- |
| `Applying` | True while the exact current plan is applying. False when no Apply is authorized -- a contested realm, an unsupported engine, an uncertain outcome, a retired binding, or a completed Job whose convergence proof is pending. |
| `ApprovalRequired` | True while the current plan needs an approval bound to its fingerprint. False when policy does not require one for this plan, or refuses the plan outright. |
| `ArtifactResolved` | True when the desired OCI reference resolved to immutable content. False when it could not be resolved. Unknown while a refresh is outstanding, or when the retained digest belongs to a retired execution binding. |
| `ArtifactVerified` | True when the artifact's type and verification policy were satisfied. False when policy refused it, when resolved content is not verified yet, or when the policy changed under it. |
| `DatabaseReachable` | True when the database schema was observed. Unknown for an unsupported engine, and when the retained observation belongs to a retired execution binding. |
| `DriftDetected` | True when the authoritative managed scope differs from desired state, false when it has no changes. Unknown until drift is observed under the current execution binding. |
| `EngineSupported` | True when the selected engine has an implemented operator lifecycle. False when it does not, and then no plan is produced and no database is reached. |
| `InSync` | True when independent read-only planning proved convergence. False when the managed scope differs from the verified artifact, or newer desired inputs arrived after older work converged. Unknown while scoped planning, a source refresh or a restarted proof is outstanding. |
| `PlanReady` | True when exact plan bytes are stored in immutable chunks. False for each refusal that prevents a plan -- an unverified artifact, a fenced table, a stale generation, a changed policy, a lost lock epoch. Unknown while source freshness is unestablished. |
| `Ready` | True when the schema is in sync. False carrying the reason that holds it, which is the field to read first. |
| `ReconciliationFailed` | True when an operation failed, carrying the failure text. False when the latest operation completed. |
| `Suspended` | True when the spec requests suspension and no new database operation may start. False when reconciliation is active. |

## PtahMigration

| Condition | What it says |
| --- | --- |
| `ApprovalRequired` | True while a plan waits for the exact approval its policy requires. False when the requirements are satisfied, when the apply policy is `Never`, or while the realm is contested and nothing is approvable. |
| `ArtifactVerified` | True when the resolved artifact satisfied its verification policy. Unknown when it resolved to a digest and has not been verified yet. |
| `Blocked` | True for a history this artifact cannot continue, which the controller never resolves by writing to the database. False when the history continues the artifact. |
| `Progressing` | True while work is in flight, including the read-back that confirms what a run did. False when nothing is pending, when operations are suspended, when the apply policy is `Never`, or when an operation was retired because an execution component changed. |
| `Ready` | True when the history matches the artifact and nothing is pending. False carrying what holds it: a dirty or modified revision row, a migration out of order or ahead of the artifact, a plan awaiting approval, a lost lock epoch, or an Apply whose outcome a person has to establish. |

## PtahSchemaApproval

| Condition | What it says |
| --- | --- |
| `Accepted` | True when the approval exactly matches the current immutable plan. False when it does not, carrying the reason it was refused. |
| `Consumed` | True when the exact plan was committed to one Apply Job dispatch. An approval is consumed once and is never reused. |
| `Stale` | True when the plan this approval was written for is no longer current, so it will not be consumed. False while the approved plan is current. |

## PtahMigrationApproval

| Condition | What it says |
| --- | --- |
| `Consumed` | True when the approved plan was dispatched. |

A migration approval publishes only this one. Whether it matches the plan is
reported on the `PtahMigration` through `ApprovalRequired` rather than on the
approval, so a consumer waiting for a decision watches the migration.

## PtahSchemaPlan

| Condition | What it says |
| --- | --- |
| `Ready` | True when the plan's immutable chunks are stored and verified. Approval and Apply both require it. |

## PtahMigrationPlan

None. A migration plan is an immutable artifact, and whether it is still the
plan to apply is reported on the `PtahMigration` that published it.
