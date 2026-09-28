---
title: PtahMigrationRunAcknowledgment
description: Every field of the PtahMigrationRunAcknowledgment resource, generated from the API types.
---

`PtahMigrationRunAcknowledgment` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## Examples

An acknowledgment settles one run a `PtahMigration` recorded in
`status.unresolvedRun`: an Apply that ended `Partial` or `Unknown`, whose
effect on the database nobody established. It carries the decision -- which
migration, and which run -- and who made it. The admission webhook refuses one
that names anything but the run the migration records right now, then stamps
who you are and when from the authenticated request.

Write one only after you have established what the run did. The controller
settles the record in your name, reads the database again, and plans whatever
is still pending; under `apply: OnApproval` that plan waits for an approval
like any other.

### What a person writes

Copy the migration's UID and the run's operation ID the operator published:

```sh
kubectl -n application get ptahmigration orders \
  -o jsonpath='{.metadata.uid}{"\n"}{.status.unresolvedRun.operationID}{"\n"}'
```

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigrationRunAcknowledgment
metadata:
  name: orders-run-accounted-for
  namespace: application
spec:
  migrationRef:
    name: orders
    uid: 8d3f6c2b-1a4e-4f90-b7c5-2e6a8d0b3f41
  operationID: sha256:4c2a7e9b1d3f5a6c8e0b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2c4e6b8d0f2a4c
```

### What the cluster stores

The same object after admission. The two fields above are unchanged, and the
stamp was added from the request.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigrationRunAcknowledgment
metadata:
  name: orders-run-accounted-for
  namespace: application
spec:
  migrationRef:
    name: orders
    uid: 8d3f6c2b-1a4e-4f90-b7c5-2e6a8d0b3f41
  operationID: sha256:4c2a7e9b1d3f5a6c8e0b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2c4e6b8d0f2a4c
  acknowledgedBy:
    username: jane@example.com
    uid: 1b9d6bcf-bbfd-4b2d-9b5d-ab8dfbbd4bed
    groups:
      - dba
  acknowledgedAt: "2026-09-20T09:14:02Z"
  mutationRequestUID: 6f4b2a18-8c3e-4d5a-b1f7-2e0c9d8a7b64
```

Once the controller has settled the run, the migration names the
acknowledgment and the identity on it, and the acknowledgment reports
`Consumed=True`:

```sh
kubectl -n application get ptahmigration orders -o jsonpath='{.status.resolvedRun}' | jq
kubectl -n application get ptahmigrationrunacknowledgment orders-run-accounted-for
```

## spec

| Field | Type | What it does |
| --- | --- | --- |
| `spec.acknowledgedAt` | `string`, required | AcknowledgedAt is when that stamp was made, by the same webhook. |
| `spec.acknowledgedBy` | `object`, required | AcknowledgedBy is stamped by the mutating webhook from the authenticated request. Whatever an API client writes here is replaced. |
| `spec.acknowledgedBy.groups` | `[]string` | Groups the authenticated user belonged to at that moment. |
| `spec.acknowledgedBy.uid` | `string` | UID of that user, where the authenticator provides one. |
| `spec.acknowledgedBy.username` | `string`, required | Username the API server authenticated the request as. |
| `spec.migrationRef` | `object`, required | MigrationRef is the resource whose unresolved run this settles. |
| `spec.migrationRef.name` | `string`, required | Name of the referenced object in the same namespace. |
| `spec.migrationRef.uid` | `string`, required | UID the object had when the reference was written. An object deleted and recreated under the same name is a different object, and this says so. |
| `spec.mutationRequestUID` | `string`, required | MutationRequestUID records the mutating AdmissionReview that stamped the identity, exactly as an approval does. |
| `spec.operationID` | `string`, required | OperationID is the status.unresolvedRun.operationID this acknowledges: one Apply attempt, and no other. |

## status

| Field | Type | What it does |
| --- | --- | --- |
| `status.conditions` | `[]object` | Conditions say whether the acknowledgment settled the run it names (Consumed), or named a run the migration was not waiting on (Stale). |
| `status.conditions[].lastTransitionTime` | `string`, required | lastTransitionTime is the last time the condition transitioned from one status to another. This should be when the underlying condition changed. If that is not known, then using the time when the API field changed is acceptable. |
| `status.conditions[].message` | `string`, required | message is a human readable message indicating details about the transition. This may be an empty string. |
| `status.conditions[].observedGeneration` | `integer` | observedGeneration represents the .metadata.generation that the condition was set based upon. For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date with respect to the current state of the instance. |
| `status.conditions[].reason` | `string`, required | reason contains a programmatic identifier indicating the reason for the condition's last transition. Producers of specific condition types may define expected values and meanings for this field, and whether the values are considered a guaranteed API. The value should be a CamelCase string. This field may not be empty. |
| `status.conditions[].status` | `string`, required, one of `True`, `False`, `Unknown` | status of the condition, one of True, False, Unknown. |
| `status.conditions[].type` | `string`, required | type of condition in CamelCase or in foo.example.com/CamelCase. |
| `status.observedGeneration` | `integer` | ObservedGeneration is the generation this status was written for. An acknowledgment is immutable, so it moves once. |

