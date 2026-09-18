---
title: PtahSchemaPlan
description: Every field of the PtahSchemaPlan resource, generated from the API types.
---

`PtahSchemaPlan` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## spec

| Field | Type | What it does |
| --- | --- | --- |
| `spec.actualStateFingerprint` | `string`, required | ActualStateFingerprint is the observed database state the plan was computed from. Drift since then retires the plan rather than applying it. |
| `spec.artifactDigest` | `string`, required | ArtifactDigest is the OCI artifact this plan was computed from, pinned to content rather than to the tag it was resolved through. |
| `spec.chunks` | `[]object`, required | Chunks are the immutable ConfigMaps the plan bytes are stored in. The plan is the concatenation of their contents in index order, and nothing reads them without checking each digest and size. |
| `spec.chunks[].digest` | `string`, required | Digest of this chunk's bytes, checked when the plan is read back. |
| `spec.chunks[].index` | `integer`, required | Index of this chunk in the plan, counting from zero. The chunks are concatenated in this order and in no other. |
| `spec.chunks[].key` | `string`, required | Key inside that ConfigMap the chunk bytes are stored under. |
| `spec.chunks[].name` | `string`, required | Name of the immutable ConfigMap holding this chunk. |
| `spec.chunks[].size` | `integer`, required | Size of this chunk in bytes, checked with the digest. |
| `spec.contentDigest` | `string`, required | ContentDigest is the digest of the plan bytes the chunks reconstruct. |
| `spec.contractVersion` | `integer`, required | ContractVersion versions plan publication and reconstruction separately from the Kubernetes API version. |
| `spec.controllerImage` | `string` | ControllerImage is the digest-pinned manager that published this plan. It, ControllerRevision and ControllerStateVersion are required by the current plan contract and stay optional on the wire only so legacy v1 and v2 plans can still be read and retired during an upgrade. |
| `spec.controllerRevision` | `string` | ControllerRevision is that manager's revision, which distinguishes two deployments of the same image. |
| `spec.controllerStateVersion` | `integer` | ControllerStateVersion is the state semantics that manager writes, so a plan is never applied by a controller that reads status differently. |
| `spec.coordinationDigest` | `string`, required | CoordinationDigest is the database realm this plan takes its turn in: the engine and the coordination key, hashed. Resources that share it never run against the database at the same time. |
| `spec.desiredStateFingerprint` | `string`, required | DesiredStateFingerprint is the state the verified artifact declared when the plan was computed. |
| `spec.destructive` | `boolean`, required | Destructive says the plan drops or rewrites something. Such a plan needs spec.policy.allowDestructive and an approval naming these exact bytes. |
| `spec.dialect` | `string`, required | Dialect is the SQL dialect the statements are written in. |
| `spec.executionBindingID` | `string` | ExecutionBindingID is a per-transition epoch. It changes even when an operator rollout returns to byte-identical component versions. |
| `spec.executorImage` | `string`, required | ExecutorImage is the digest-pinned image that ran Ptah. |
| `spec.fingerprint` | `string`, required | Fingerprint is the complete approval identity of this plan: every binding below hashed together. An approval names this value, and an apply runs only while the live bindings still produce it. |
| `spec.policyFingerprint` | `string`, required | PolicyFingerprint is the spec.policy the plan was computed under. Editing the policy -- a protected table included -- retires a plan waiting for a person rather than letting it apply under rules nobody approved. |
| `spec.ptahVersion` | `string`, required | PtahVersion is the Ptah build that computed this plan, as the executor image reports it. |
| `spec.runnerImage` | `string`, required | RunnerImage is the digest-pinned image that supervised the executor and returned its result. |
| `spec.runnerProtocolVersion` | `integer`, required | RunnerProtocolVersion is the result-frame protocol that runner speaks. A runner answering in another version has its result rejected rather than interpreted. |
| `spec.schemaRef` | `object`, required | SchemaRef is the PtahSchema this plan was computed for. |
| `spec.schemaRef.name` | `string`, required | Name of the referenced object in the same namespace. |
| `spec.schemaRef.uid` | `string`, required | UID the object had when the reference was written. An object deleted and recreated under the same name is a different object, and this says so. |
| `spec.size` | `integer`, required | Size is the length of those bytes, summed across the chunks. |
| `spec.statementCount` | `integer`, required | StatementCount is how many statements the plan holds. Read them with kubectl ptah plan rather than by fetching the chunks. |
| `spec.targetIdentityDigest` | `string`, required | TargetIdentityDigest identifies the database this plan was computed against without carrying anything that could reach it. |
| `spec.verificationPolicyDigest` | `string`, required | VerificationPolicyDigest is that policy's content, so replacing the object or editing it in place both retire the plan. |
| `spec.verificationPolicyUID` | `string`, required | VerificationPolicyUID is the policy object that accepted the artifact. |

## status

| Field | Type | What it does |
| --- | --- | --- |
| `status.conditions` | `[]object` | Conditions carry Ready, which is what approval and apply require. |
| `status.conditions[].lastTransitionTime` | `string`, required | lastTransitionTime is the last time the condition transitioned from one status to another. This should be when the underlying condition changed. If that is not known, then using the time when the API field changed is acceptable. |
| `status.conditions[].message` | `string`, required | message is a human readable message indicating details about the transition. This may be an empty string. |
| `status.conditions[].observedGeneration` | `integer` | observedGeneration represents the .metadata.generation that the condition was set based upon. For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date with respect to the current state of the instance. |
| `status.conditions[].reason` | `string`, required | reason contains a programmatic identifier indicating the reason for the condition's last transition. Producers of specific condition types may define expected values and meanings for this field, and whether the values are considered a guaranteed API. The value should be a CamelCase string. This field may not be empty. |
| `status.conditions[].status` | `string`, required, one of `True`, `False`, `Unknown` | status of the condition, one of True, False, Unknown. |
| `status.conditions[].type` | `string`, required | type of condition in CamelCase or in foo.example.com/CamelCase. |
| `status.observedGeneration` | `integer` | ObservedGeneration is the plan generation this status was written for. |
| `status.publishedChunks` | `[]object` | PublishedChunks are the ConfigMaps that were found to exist, by UID, before Ready became true. Chunk publication is not transactional, so this is the record that every chunk the manifest names was really written. |
| `status.publishedChunks[].index` | `integer`, required | Index of the chunk this record is for. |
| `status.publishedChunks[].name` | `string`, required | Name of the ConfigMap that was verified. |
| `status.publishedChunks[].uid` | `string`, required | UID it had when it was verified, so a chunk deleted and recreated is not mistaken for the one the plan was published with. |

