---
title: PtahSchemaApproval
description: Every field of the PtahSchemaApproval resource, generated from the API types.
---

`PtahSchemaApproval` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## spec

| Field | Type | What it does |
| --- | --- | --- |
| `spec.actualStateFingerprint` | `string`, required |  |
| `spec.approvedAt` | `string`, required |  |
| `spec.approver` | `object`, required | ApprovalIdentity is stamped from the authenticated admission request. The API client does not choose these fields. |
| `spec.approver.groups` | `[]string` |  |
| `spec.approver.uid` | `string` |  |
| `spec.approver.username` | `string`, required |  |
| `spec.artifactDigest` | `string`, required |  |
| `spec.controllerImage` | `string` |  |
| `spec.controllerRevision` | `string` |  |
| `spec.controllerStateVersion` | `integer` |  |
| `spec.coordinationDigest` | `string`, required |  |
| `spec.desiredStateFingerprint` | `string`, required |  |
| `spec.executionBindingID` | `string` |  |
| `spec.executorImage` | `string`, required |  |
| `spec.mutationRequestUID` | `string`, required | MutationRequestUID records the mutating AdmissionReview that stamped the authenticated identity. Kubernetes creates a distinct AdmissionReview UID for the later validating webhook, so the validator checks this field is present while matching identity against its own authenticated UserInfo. |
| `spec.planFingerprint` | `string`, required |  |
| `spec.planRef` | `object`, required | ImmutableObjectReference binds a name to the UID that existed when the reference was created, preventing delete-and-recreate aliasing. |
| `spec.planRef.name` | `string`, required |  |
| `spec.planRef.uid` | `string`, required | UID is a type that holds unique ID values, including UUIDs. Because we don't ONLY use UUIDs, this is an alias to string. Being a type captures intent and helps make sure that UIDs and names do not get conflated. |
| `spec.policyFingerprint` | `string`, required |  |
| `spec.ptahVersion` | `string`, required |  |
| `spec.runnerImage` | `string`, required |  |
| `spec.runnerProtocolVersion` | `integer`, required |  |
| `spec.schemaRef` | `object`, required | ImmutableObjectReference binds a name to the UID that existed when the reference was created, preventing delete-and-recreate aliasing. |
| `spec.schemaRef.name` | `string`, required |  |
| `spec.schemaRef.uid` | `string`, required | UID is a type that holds unique ID values, including UUIDs. Because we don't ONLY use UUIDs, this is an alias to string. Being a type captures intent and helps make sure that UIDs and names do not get conflated. |
| `spec.targetIdentityDigest` | `string`, required |  |
| `spec.verificationPolicyDigest` | `string`, required |  |
| `spec.verificationPolicyUID` | `string`, required | UID is a type that holds unique ID values, including UUIDs. Because we don't ONLY use UUIDs, this is an alias to string. Being a type captures intent and helps make sure that UIDs and names do not get conflated. |

## status

| Field | Type | What it does |
| --- | --- | --- |
| `status.conditions` | `[]object` |  |
| `status.conditions[].lastTransitionTime` | `string`, required | lastTransitionTime is the last time the condition transitioned from one status to another. This should be when the underlying condition changed. If that is not known, then using the time when the API field changed is acceptable. |
| `status.conditions[].message` | `string`, required | message is a human readable message indicating details about the transition. This may be an empty string. |
| `status.conditions[].observedGeneration` | `integer` | observedGeneration represents the .metadata.generation that the condition was set based upon. For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date with respect to the current state of the instance. |
| `status.conditions[].reason` | `string`, required | reason contains a programmatic identifier indicating the reason for the condition's last transition. Producers of specific condition types may define expected values and meanings for this field, and whether the values are considered a guaranteed API. The value should be a CamelCase string. This field may not be empty. |
| `status.conditions[].status` | `string`, required, one of `True`, `False`, `Unknown` | status of the condition, one of True, False, Unknown. |
| `status.conditions[].type` | `string`, required | type of condition in CamelCase or in foo.example.com/CamelCase. |
| `status.observedGeneration` | `integer` |  |

