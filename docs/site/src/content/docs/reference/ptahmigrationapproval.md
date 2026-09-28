---
title: PtahMigrationApproval
description: Every field of the PtahMigrationApproval resource, generated from the API types.
---

`PtahMigrationApproval` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## Examples

An approval authorizes one plan, once. It carries the decision -- which
migration, which plan, and that plan's fingerprint -- and who made it. The
admission webhook holds the three to the plan it names and refuses any that
disagrees, then stamps who you are and when from the authenticated request.

A migration plan's fingerprint binds the history as well as the sequence. A
sequence applied to a database that has moved on since the plan was computed
is a different change, so it gets a different plan, and the approval stays
with the one it named.

### What a person writes

Read the plan first, then copy the identifiers the operator published:

```sh
kubectl ptah migration orders -n application

kubectl -n application get ptahmigration orders \
  -o jsonpath='{.metadata.uid}{"\n"}{.status.plan.name}{"\n"}{.status.plan.uid}{"\n"}'
kubectl -n application get ptahmigrationplan <plan-name> \
  -o jsonpath='{.spec.fingerprint}{"\n"}'
```

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigrationApproval
metadata:
  name: approve-orders-14
  namespace: application
spec:
  migrationRef:
    name: orders
    uid: 8d3f6c2b-1a4e-4f90-b7c5-2e6a8d0b3f41
  planRef:
    name: ptah-mplan-19581e27de7ced00ff1ce50b
    uid: c5e8a7d2-3b41-4f6e-9a08-1d2c3b4a5e6f
  planFingerprint: sha256:19581e27de7ced00ff1ce50b2047e7a567c76b1cbaebabe5ef03f7c3017bb5b7
```

### What the cluster stores

The same object after admission. The three fields above are unchanged; the
stamp was added from the request, and nothing was copied from the plan.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigrationApproval
metadata:
  name: approve-orders-14
  namespace: application
spec:
  migrationRef:
    name: orders
    uid: 8d3f6c2b-1a4e-4f90-b7c5-2e6a8d0b3f41
  planRef:
    name: ptah-mplan-19581e27de7ced00ff1ce50b
    uid: c5e8a7d2-3b41-4f6e-9a08-1d2c3b4a5e6f
  planFingerprint: sha256:19581e27de7ced00ff1ce50b2047e7a567c76b1cbaebabe5ef03f7c3017bb5b7
  approver:
    username: jane@example.com
    uid: 1b9d6bcf-bbfd-4b2d-9b5d-ab8dfbbd4bed
    groups:
      - migration-approvers
  approvedAt: "2026-09-20T09:14:02Z"
  mutationRequestUID: 6f4b2a18-8c3e-4d5a-b1f7-2e0c9d8a7b64
```

The history the plan was computed against, the artifact, and the execution
binding the run must happen under are on the plan the approval names:

```sh
kubectl -n application get ptahmigrationplan ptah-mplan-19581e27de7ced00ff1ce50b \
  -o jsonpath='{.spec.historyFingerprint}{"\n"}{.spec.artifactDigest}{"\n"}{.spec.executionBindingID}{"\n"}'
```

## spec

| Field | Type | What it does |
| --- | --- | --- |
| `spec.approvedAt` | `string`, required | ApprovedAt is when that stamp was made. |
| `spec.approver` | `object`, required | Approver is stamped by the mutating webhook from the authenticated request; whatever a client writes here is replaced. |
| `spec.approver.groups` | `[]string` | Groups the authenticated user belonged to at that moment. |
| `spec.approver.uid` | `string` | UID of that user, where the authenticator provides one. |
| `spec.approver.username` | `string`, required | Username the API server authenticated the request as. |
| `spec.migrationRef` | `object`, required | MigrationRef is the resource the approved sequence belongs to. |
| `spec.migrationRef.name` | `string`, required | Name of the referenced object in the same namespace. |
| `spec.migrationRef.uid` | `string`, required | UID the object had when the reference was written. An object deleted and recreated under the same name is a different object, and this says so. |
| `spec.mutationRequestUID` | `string`, required | MutationRequestUID records the mutating AdmissionReview that stamped the authenticated identity, exactly as a schema approval does. |
| `spec.planFingerprint` | `string`, required | PlanFingerprint is the plan's spec.fingerprint: its complete identity, checked against the live plan before anything runs. |
| `spec.planRef` | `object`, required | PlanRef is the exact plan being approved. |
| `spec.planRef.name` | `string`, required | Name of the referenced object in the same namespace. |
| `spec.planRef.uid` | `string`, required | UID the object had when the reference was written. An object deleted and recreated under the same name is a different object, and this says so. |

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

