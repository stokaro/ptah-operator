# Condition reasons

Kubernetes Conditions are the operator's stable machine interface. Consumers
should evaluate the complete `type`, `status`, and `reason` tuple and use
`observedGeneration` to decide whether it describes the current spec. Condition
messages are bounded diagnostic text and may become more detailed without an
API version change.

The Go API exports every value below as a typed `ConditionReason` constant.
Existing values are never repurposed. Adding a reason is backward compatible;
renaming, removing, or changing the meaning of one requires an API compatibility
decision.

| Reason | Stable meaning |
| --- | --- |
| `Active` | Reconciliation is not suspended. |
| `ApplyDisabled` | Policy records plans but forbids Apply. |
| `ApplyOutcomeUnknown` | Apply failed without proof of whether mutation occurred. |
| `ApplyPending` | A non-destructive plan is eligible for automatic Apply. |
| `ApprovalRevoked` | A reserved approval became invalid before dispatch. |
| `ApprovedPlan` | The exact current approved plan is applying. |
| `ArtifactUnverified` | No plan is usable because artifact verification did not succeed. |
| `AwaitingApproval` | The current immutable plan needs a new exact approval. |
| `ConfigurationError` | Controller-side configuration or declared input is invalid. |
| `ConvergedAfterUnknownOutcome` | Read-only proof found convergence after an unattributable Apply outcome. |
| `CurrentPlan` | An approval exactly matches the current immutable plan. |
| `DesiredStateChanged` | New desired inputs arrived after older work completed. |
| `DestructiveChangesDisabled` | A destructive plan is blocked by policy. |
| `DigestPinned` | A requested OCI reference resolved to immutable content. |
| `DispatchCommitted` | One exact approval was consumed by an Apply dispatch boundary. |
| `ExecutionBindingChanged` | Evidence or approval belongs to a retired runtime identity. |
| `InputsChanged` | An operation result was discarded because its desired inputs changed. |
| `InSync` | Independent read-only planning proved convergence. |
| `JobCompleted` | Apply exited and independent convergence proof remains pending. |
| `LeaseContinuityLost` | Database lock ownership was not continuous, so evidence was discarded. |
| `NoChanges` | Scoped planning produced no executable statements. |
| `NotRequired` | Policy allows this non-destructive plan without separate approval. |
| `Observed` | A database observation completed successfully. |
| `OperationFailed` | A read-only or mutating operation failed. |
| `OperationInProgress` | A durable operation claim is active. |
| `OutcomeUnknown` | Apply attribution is uncertain and only read-only proof may proceed. |
| `Pending` | Resolved artifact content has not yet been verified. |
| `PlanNoLongerCurrent` | An approval's plan binding is no longer current. |
| `PlanReady` | An exact immutable plan is ready for approval. |
| `PolicyBlocked` | Destructive-change policy blocks the current plan. |
| `PolicyChanged` | Verification policy identity or bytes changed. |
| `PolicyRefused` | Artifact verification policy explicitly refused the artifact. |
| `PolicySatisfied` | Artifact type and verification policy checks succeeded. |
| `ProofInputsChanged` | Post-Apply proof will restart from its durable immutable binding. |
| `Published` | Exact plan bytes were committed to immutable storage. |
| `RefreshFailed` | A previously resolved source could not be refreshed. |
| `Refreshing` | The requested source is being resolved again. |
| `RefreshSuspended` | Source refresh stopped before dispatch because reconciliation was suspended. |
| `Requested` | The spec requests suspension. |
| `ResolveFailed` | No immutable source resolution is available. |
| `Satisfied` | Current plan approval requirements passed final validation. |
| `ScopedChanges` | The authoritative managed scope differs from desired state. |
| `ScopedConverged` | The authoritative managed scope has no changes. |
| `ScopedPlanPending` | Raw observation completed and scoped planning remains pending. |
| `SourceFreshnessUnknown` | Currentness cannot be proven because source refresh did not complete. |
| `SourceRefreshPending` | Dependent currentness is unknown during source refresh. |
| `SourceResolutionUnknown` | Convergence cannot be evaluated without a resolved source. |
| `SourceUnresolved` | A plan cannot be produced without a resolved source. |
| `Stale` | The current plan became stale before Apply. |
| `StaleObservation` | Database state must be observed again before planning. |
| `StalePlan` | Ready is false because the plan became stale before Apply. |
| `Succeeded` | The latest operation cleared the reconciliation-failure Condition. |
| `SupersededApproval` | A duplicate approval lost to another approval for the same plan. |
| `SupportedEngine` | The selected database engine has an implemented operator lifecycle. |
| `Suspended` | Reconciliation is suspended and no new operation may start. |
| `UnsupportedEngine` | The selected engine has no implemented operator lifecycle. |
| `VerifyingConvergence` | Apply completed but independent read-only proof has not. |
| `Waiting` | The controller is waiting for an approval bound to the current plan. |

## Unsupported engines

`spec.target.engine` accepts a bounded engine identifier so GitOps tools can
store desired state before that engine is implemented. An unknown value does
not cause database or registry access. After all previously dispatched Apply
work and its mandatory read-only proof reach a safe boundary, the controller
sets all of the following for the current generation:

- `EngineSupported=False` with reason `UnsupportedEngine`;
- `Ready=False` with reason `UnsupportedEngine`;
- `phase=Blocked` and `status.observedGeneration=metadata.generation`.

The same boundary marks `DatabaseReachable`, `DriftDetected`, and `InSync`
unknown, makes `PlanReady`, `ApprovalRequired`, and `Applying` false, and
prevents new operation Jobs. If an older plan exists, the controller first
persists this non-authorizing status, then marks approvals for the schema stale
in bounded passes and clears the current plan pointer. An approval CREATE that
crossed the status fence is retired on its watch-triggered reconciliation even
after that pointer is gone. Changing the spec back to `PostgreSQL` or `MySQL`
sets `EngineSupported=True` with reason `SupportedEngine` and restarts the
complete workflow at Resolve.
