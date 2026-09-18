---
title: PtahMigrationApproval
description: Every field of the PtahMigrationApproval resource, generated from the API types.
---

`PtahMigrationApproval` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## spec

| Field | Type | What it does |
| --- | --- | --- |
| `spec.approvedAt` | `string`, required | ApprovedAt is when that stamp was made. |
| `spec.approver` | `object`, required | Approver is stamped by the mutating webhook from the authenticated request; whatever a client writes here is replaced. |
| `spec.approver.groups` | `[]string` | Groups the authenticated user belonged to at that moment. |
| `spec.approver.uid` | `string` | UID of that user, where the authenticator provides one. |
| `spec.approver.username` | `string`, required | Username the API server authenticated the request as. |
| `spec.artifactDigest` | `string`, required | ArtifactDigest is the OCI migration artifact the sequence comes from. |
| `spec.controllerImage` | `string`, required | ControllerImage is the digest-pinned manager that must dispatch the run. |
| `spec.controllerRevision` | `string`, required | ControllerRevision is that manager's revision. |
| `spec.controllerStateVersion` | `integer`, required | ControllerStateVersion is the state semantics it writes. |
| `spec.coordinationDigest` | `string`, required | CoordinationDigest is the database realm the approved run takes its turn in. |
| `spec.executionBindingID` | `string`, required | ExecutionBindingID is the execution epoch the approved plan belongs to, which changes on every operator transition. |
| `spec.executorImage` | `string`, required | ExecutorImage is the digest-pinned image it runs in. |
| `spec.historyFingerprint` | `string`, required | HistoryFingerprint is the recorded history the plan was computed against. Somebody else's run moves it, and this is what notices. |
| `spec.migrationRef` | `object`, required | MigrationRef is the resource the approved sequence belongs to. |
| `spec.migrationRef.name` | `string`, required | Name of the referenced object in the same namespace. |
| `spec.migrationRef.uid` | `string`, required | UID the object had when the reference was written. An object deleted and recreated under the same name is a different object, and this says so. |
| `spec.mutationRequestUID` | `string`, required | MutationRequestUID records the mutating AdmissionReview that stamped the authenticated identity, exactly as a schema approval does. |
| `spec.planFingerprint` | `string`, required | PlanFingerprint is the plan's complete identity. The fields below are that identity written out, so the decision can be read without fetching the plan and checked against the live one before anything runs. |
| `spec.planRef` | `object`, required | PlanRef is the exact plan being approved. |
| `spec.planRef.name` | `string`, required | Name of the referenced object in the same namespace. |
| `spec.planRef.uid` | `string`, required | UID the object had when the reference was written. An object deleted and recreated under the same name is a different object, and this says so. |
| `spec.policyFingerprint` | `string`, required | PolicyFingerprint is the spec.policy it was computed under. |
| `spec.ptahVersion` | `string`, required | PtahVersion is the Ptah build the approved run must use. |
| `spec.runnerImage` | `string`, required | RunnerImage is the digest-pinned image that supervises it. |
| `spec.runnerProtocolVersion` | `integer`, required | RunnerProtocolVersion is the result-frame protocol that runner speaks. |
| `spec.targetIdentityDigest` | `string`, required | TargetIdentityDigest is the database it was computed against. |
| `spec.verificationPolicyDigest` | `string`, required | VerificationPolicyDigest is that policy's content at the time. |
| `spec.verificationPolicyUID` | `string`, required | VerificationPolicyUID is the policy object that accepted the artifact. |

## status

| Field | Type | What it does |
| --- | --- | --- |
| `status.conditions` | `[]object` | Conditions say whether the binding was Accepted, has been Consumed by a run, or went Stale because the history, the artifact or the policy moved first. |
| `status.conditions[].lastTransitionTime` | `string`, required | lastTransitionTime is the last time the condition transitioned from one status to another. This should be when the underlying condition changed. If that is not known, then using the time when the API field changed is acceptable. |
| `status.conditions[].message` | `string`, required | message is a human readable message indicating details about the transition. This may be an empty string. |
| `status.conditions[].observedGeneration` | `integer` | observedGeneration represents the .metadata.generation that the condition was set based upon. For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date with respect to the current state of the instance. |
| `status.conditions[].reason` | `string`, required | reason contains a programmatic identifier indicating the reason for the condition's last transition. Producers of specific condition types may define expected values and meanings for this field, and whether the values are considered a guaranteed API. The value should be a CamelCase string. This field may not be empty. |
| `status.conditions[].status` | `string`, required, one of `True`, `False`, `Unknown` | status of the condition, one of True, False, Unknown. |
| `status.conditions[].type` | `string`, required | type of condition in CamelCase or in foo.example.com/CamelCase. |
| `status.observedGeneration` | `integer` | ObservedGeneration is the approval generation this status was written for. An approval is immutable, so it moves once. |

