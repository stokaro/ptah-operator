---
title: PtahMigrationPlan
description: Every field of the PtahMigrationPlan resource, generated from the API types.
---

`PtahMigrationPlan` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## spec

| Field | Type | What it does |
| --- | --- | --- |
| `spec.artifactDigest` | `string`, required |  |
| `spec.contractVersion` | `integer`, required | ContractVersion versions plan publication separately from the Kubernetes API version. |
| `spec.controllerImage` | `string`, required |  |
| `spec.controllerRevision` | `string`, required |  |
| `spec.controllerStateVersion` | `integer`, required |  |
| `spec.coordinationDigest` | `string`, required |  |
| `spec.createdAt` | `string`, required |  |
| `spec.currentVersion` | `integer`, required | CurrentVersion is the version the history stood at when the plan was made, published so an operator can read the plan's premise. |
| `spec.executionBindingID` | `string`, required | ExecutionBindingID is a per-transition epoch. It changes even when an operator rollout returns to byte-identical component versions. |
| `spec.executorImage` | `string`, required |  |
| `spec.fingerprint` | `string`, required | Fingerprint binds this plan to everything that decided it. An approval names it, and a changed input produces a different plan rather than a changed one. |
| `spec.historyFingerprint` | `string`, required | HistoryFingerprint is the history this plan was computed against. A history that changed between planning and execution invalidates the plan rather than being applied to. |
| `spec.migrationRef` | `object`, required | ImmutableObjectReference binds a name to the UID that existed when the reference was created, preventing delete-and-recreate aliasing. |
| `spec.migrationRef.name` | `string`, required |  |
| `spec.migrationRef.uid` | `string`, required | UID is a type that holds unique ID values, including UUIDs. Because we don't ONLY use UUIDs, this is an alias to string. Being a type captures intent and helps make sure that UIDs and names do not get conflated. |
| `spec.migrations` | `[]object`, required | Migrations is the exact sequence, in execution order, and the order is the content: a plan that applies the same migrations in another order is a different plan. So the list is atomic rather than a map keyed by version -- a map declares the order insignificant, and it would also require the version to be unique, which is the thing VersionKey exists to say it is not. |
| `spec.migrations[].checkpoint` | `boolean` | Checkpoint marks a migration whose up body is the cumulative schema at its version. |
| `spec.migrations[].checksum` | `string`, required | Checksum is what the file hashes to under the rule that decides what a revision row records. It is what makes a plan refuse an artifact whose bytes changed after the plan was published. |
| `spec.migrations[].description` | `string` |  |
| `spec.migrations[].transactionMode` | `string`, one of `file`, `none` | TransactionMode is the file's declared mode, resolved for this dialect: "file", "none", or empty where the file declares none and the engine's own mode decides. A run that stops halfway is read differently depending on it, so the plan records what was true when it was made. |
| `spec.migrations[].version` | `integer`, required | Version is the migration's numeric version. |
| `spec.migrations[].versionKey` | `string` | VersionKey is the exact revision identity. A version does not identify a row on its own: an Atlas repeatable migration carries an opaque token rather than a decimal spelling. |
| `spec.policyFingerprint` | `string`, required |  |
| `spec.ptahVersion` | `string`, required |  |
| `spec.runnerImage` | `string`, required |  |
| `spec.runnerProtocolVersion` | `integer`, required |  |
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

