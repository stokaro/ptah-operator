---
title: PtahMigrationPlan
description: Every field of the PtahMigrationPlan resource, generated from the API types.
---

`PtahMigrationPlan` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## spec

| Field | Type | What it does |
| --- | --- | --- |
| `spec.artifactDigest` | `string`, required | ArtifactDigest is the OCI migration artifact this plan reads its files from, pinned to content rather than to the tag it was resolved through. |
| `spec.contractVersion` | `integer`, required | ContractVersion versions plan publication separately from the Kubernetes API version. |
| `spec.controllerImage` | `string`, required | ControllerImage is the digest-pinned manager that published this plan. |
| `spec.controllerRevision` | `string`, required | ControllerRevision is that manager's revision, which distinguishes two deployments of the same image. |
| `spec.controllerStateVersion` | `integer`, required | ControllerStateVersion is the state semantics that manager writes. |
| `spec.coordinationDigest` | `string`, required | CoordinationDigest is the database realm this plan takes its turn in. |
| `spec.createdAt` | `string`, required | CreatedAt is when the controller published this plan. |
| `spec.currentVersion` | `integer`, required | CurrentVersion is the version the history stood at when the plan was made, published so an operator can read the plan's premise. |
| `spec.executionBindingID` | `string`, required | ExecutionBindingID is a per-transition epoch. It changes even when an operator rollout returns to byte-identical component versions. |
| `spec.executorImage` | `string`, required | ExecutorImage is the digest-pinned image that ran Ptah. |
| `spec.fingerprint` | `string`, required | Fingerprint binds this plan to everything that decided it. An approval names it, and a changed input produces a different plan rather than a changed one. |
| `spec.historyFingerprint` | `string`, required | HistoryFingerprint is the history this plan was computed against. A history that changed between planning and execution invalidates the plan rather than being applied to. |
| `spec.migrationRef` | `object`, required | MigrationRef is the PtahMigration this plan was computed for. |
| `spec.migrationRef.name` | `string`, required | Name of the referenced object in the same namespace. |
| `spec.migrationRef.uid` | `string`, required | UID the object had when the reference was written. An object deleted and recreated under the same name is a different object, and this says so. |
| `spec.migrations` | `[]object`, required | Migrations is the exact sequence, in execution order, and the order is the content: a plan that applies the same migrations in another order is a different plan. So the list is atomic rather than a map keyed by version -- a map declares the order insignificant, and it would also require the version to be unique, which is the thing VersionKey exists to say it is not. |
| `spec.migrations[].checkpoint` | `boolean` | Checkpoint marks a migration whose up body is the cumulative schema at its version. |
| `spec.migrations[].checksum` | `string`, required | Checksum is what the file hashes to under the rule that decides what a revision row records. It is what makes a plan refuse an artifact whose bytes changed after the plan was published. |
| `spec.migrations[].description` | `string` | Description is the migration's own description, as the artifact spells it. |
| `spec.migrations[].transactionMode` | `string`, one of `file`, `none` | TransactionMode is the file's declared mode, resolved for this dialect: "file", "none", or empty where the file declares none and the engine's own mode decides. A run that stops halfway is read differently depending on it, so the plan records what was true when it was made. |
| `spec.migrations[].version` | `integer`, required | Version is the migration's numeric version. |
| `spec.migrations[].versionKey` | `string` | VersionKey is the exact revision identity. A version does not identify a row on its own: an Atlas repeatable migration carries an opaque token rather than a decimal spelling. |
| `spec.policyFingerprint` | `string`, required | PolicyFingerprint is the spec.policy the plan was computed under, so an edited policy retires a plan waiting for a person. |
| `spec.ptahVersion` | `string`, required | PtahVersion is the Ptah build that computed the sequence. |
| `spec.runnerImage` | `string`, required | RunnerImage is the digest-pinned image that supervised it. |
| `spec.runnerProtocolVersion` | `integer`, required | RunnerProtocolVersion is the result-frame protocol that runner speaks. |
| `spec.targetIdentityDigest` | `string`, required | TargetIdentityDigest identifies the database it was computed against without carrying anything that could reach it. |
| `spec.verificationPolicyDigest` | `string`, required | VerificationPolicyDigest is that policy's content at the time. |
| `spec.verificationPolicyUID` | `string`, required | VerificationPolicyUID is the policy object that accepted the artifact. |

## status

| Field | Type | What it does |
| --- | --- | --- |
| `status.conditions` | `[]object` | Conditions carry Current, which says the plan still matches the artifact and the history it was computed against, and Executed, which says its sequence ran. |
| `status.conditions[].lastTransitionTime` | `string`, required | lastTransitionTime is the last time the condition transitioned from one status to another. This should be when the underlying condition changed. If that is not known, then using the time when the API field changed is acceptable. |
| `status.conditions[].message` | `string`, required | message is a human readable message indicating details about the transition. This may be an empty string. |
| `status.conditions[].observedGeneration` | `integer` | observedGeneration represents the .metadata.generation that the condition was set based upon. For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date with respect to the current state of the instance. |
| `status.conditions[].reason` | `string`, required | reason contains a programmatic identifier indicating the reason for the condition's last transition. Producers of specific condition types may define expected values and meanings for this field, and whether the values are considered a guaranteed API. The value should be a CamelCase string. This field may not be empty. |
| `status.conditions[].status` | `string`, required, one of `True`, `False`, `Unknown` | status of the condition, one of True, False, Unknown. |
| `status.conditions[].type` | `string`, required | type of condition in CamelCase or in foo.example.com/CamelCase. |
| `status.observedGeneration` | `integer` | ObservedGeneration is the plan generation this status was written for. |

