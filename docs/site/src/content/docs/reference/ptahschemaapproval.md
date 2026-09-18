---
title: PtahSchemaApproval
description: Every field of the PtahSchemaApproval resource, generated from the API types.
---

`PtahSchemaApproval` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## spec

| Field | Type | What it does |
| --- | --- | --- |
| `spec.actualStateFingerprint` | `string`, required | ActualStateFingerprint is the observed database state that was planned from. A database that has moved since makes this approval stale. |
| `spec.approvedAt` | `string`, required | ApprovedAt is when that stamp was made, by the same webhook. |
| `spec.approver` | `object`, required | Approver is stamped by the mutating webhook from the authenticated request. Whatever an API client writes here is replaced. |
| `spec.approver.groups` | `[]string` | Groups the authenticated user belonged to at that moment. |
| `spec.approver.uid` | `string` | UID of that user, where the authenticator provides one. |
| `spec.approver.username` | `string`, required | Username the API server authenticated the request as. |
| `spec.artifactDigest` | `string`, required | ArtifactDigest is the OCI artifact the approved plan was computed from. |
| `spec.controllerImage` | `string` | ControllerImage is the digest-pinned manager the approved apply must be dispatched by. |
| `spec.controllerRevision` | `string` | ControllerRevision is that manager's revision. |
| `spec.controllerStateVersion` | `integer` | ControllerStateVersion is the state semantics it writes. |
| `spec.coordinationDigest` | `string`, required | CoordinationDigest is the database realm the approved apply takes its turn in. |
| `spec.desiredStateFingerprint` | `string`, required | DesiredStateFingerprint is the state the artifact declared. |
| `spec.executionBindingID` | `string` | ExecutionBindingID is the execution epoch the approved plan belongs to. It changes on every operator transition, including one that returns to byte-identical versions, so an approval cannot survive a rollout unseen. |
| `spec.executorImage` | `string`, required | ExecutorImage is the digest-pinned image it must run in. |
| `spec.mutationRequestUID` | `string`, required | MutationRequestUID records the mutating AdmissionReview that stamped the authenticated identity. Kubernetes creates a distinct AdmissionReview UID for the later validating webhook, so the validator checks this field is present while matching identity against its own authenticated UserInfo. |
| `spec.planFingerprint` | `string`, required | PlanFingerprint is the plan's complete approval identity. Everything below is the same identity written out, so a reader can see what was approved without fetching the plan, and the admission that accepts this approval checks each part against the live plan. |
| `spec.planRef` | `object`, required | PlanRef is the exact plan being approved. A plan is immutable, so this names bytes rather than an intention. |
| `spec.planRef.name` | `string`, required | Name of the referenced object in the same namespace. |
| `spec.planRef.uid` | `string`, required | UID the object had when the reference was written. An object deleted and recreated under the same name is a different object, and this says so. |
| `spec.policyFingerprint` | `string`, required | PolicyFingerprint is the spec.policy the plan was computed under, so an edited policy retires this decision instead of inheriting it. |
| `spec.ptahVersion` | `string`, required | PtahVersion is the Ptah build the approved apply must run. |
| `spec.runnerImage` | `string`, required | RunnerImage is the digest-pinned image that supervises it. |
| `spec.runnerProtocolVersion` | `integer`, required | RunnerProtocolVersion is the result-frame protocol that runner speaks. |
| `spec.schemaRef` | `object`, required | SchemaRef is the resource the approved change belongs to. |
| `spec.schemaRef.name` | `string`, required | Name of the referenced object in the same namespace. |
| `spec.schemaRef.uid` | `string`, required | UID the object had when the reference was written. An object deleted and recreated under the same name is a different object, and this says so. |
| `spec.targetIdentityDigest` | `string`, required | TargetIdentityDigest is the database the approved plan was computed against. |
| `spec.verificationPolicyDigest` | `string`, required | VerificationPolicyDigest is that policy's content at the time. |
| `spec.verificationPolicyUID` | `string`, required | VerificationPolicyUID is the verification policy object that accepted the artifact. |

## status

| Field | Type | What it does |
| --- | --- | --- |
| `status.conditions` | `[]object` | Conditions say whether the binding was Accepted, has been Consumed by an apply, or went Stale because the database, the artifact or the policy moved before it could run. |
| `status.conditions[].lastTransitionTime` | `string`, required | lastTransitionTime is the last time the condition transitioned from one status to another. This should be when the underlying condition changed. If that is not known, then using the time when the API field changed is acceptable. |
| `status.conditions[].message` | `string`, required | message is a human readable message indicating details about the transition. This may be an empty string. |
| `status.conditions[].observedGeneration` | `integer` | observedGeneration represents the .metadata.generation that the condition was set based upon. For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date with respect to the current state of the instance. |
| `status.conditions[].reason` | `string`, required | reason contains a programmatic identifier indicating the reason for the condition's last transition. Producers of specific condition types may define expected values and meanings for this field, and whether the values are considered a guaranteed API. The value should be a CamelCase string. This field may not be empty. |
| `status.conditions[].status` | `string`, required, one of `True`, `False`, `Unknown` | status of the condition, one of True, False, Unknown. |
| `status.conditions[].type` | `string`, required | type of condition in CamelCase or in foo.example.com/CamelCase. |
| `status.observedGeneration` | `integer` | ObservedGeneration is the approval generation this status was written for. An approval is immutable, so it moves only when the object is first reconciled. |

