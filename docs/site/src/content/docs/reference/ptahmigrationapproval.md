---
title: PtahMigrationApproval
description: Every field of the PtahMigrationApproval resource, generated from the API types.
---

`PtahMigrationApproval` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## Examples

An approval authorizes one plan, once. Most of the fields below are not yours
to write: the admission webhook copies the binding from the plan you named and
refuses any value that disagrees with it, then stamps who you are and when from
the authenticated request.

A migration approval binds the history as well as the plan. A sequence applied
to a database that has moved on since the plan was computed is a different
change, so the approval retires with it.

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

The same object after admission. The three fields above are unchanged;
everything else was filled in from the plan and from the request.

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
  # The history the plan was computed against. A version applied by anything
  # else in the meantime changes this, and the approval no longer matches.
  historyFingerprint: sha256:4a44dc15364204a80fe80e9039455cc1608281820fe2b24f1e5233ade6af1dd5
  artifactDigest: sha256:d4735e3a265e16eee03f59718b9b5d03019c07d8b6c51f90da3a666eec13ab35
  verificationPolicyDigest: sha256:084fed08b978af4d7d196a7446a86b58009e636b611db16211b65a9aadff29c5
  coordinationDigest: sha256:e7f6c011776e8db7cd330b54174fd76f7d0216b612387a5ffcfb81e6f0919683
  targetIdentityDigest: sha256:67586e98fad27da0b9968bc039a1ef34c939b9b8e523a8bef89d478608c5ecf6
  policyFingerprint: sha256:fcde2b2edba56bf408601fb721fe9b5c338d10ee429ea04fae5511b68fbf8fb9
  ptahVersion: v0.7.0
  executorImage: ghcr.io/stokaro/ptah@sha256:1b4f0e9851971998e732078544c96b36c3d01cedf7caa332359d6f1d83567014
  runnerImage: ghcr.io/stokaro/ptah-runner@sha256:60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752
  runnerProtocolVersion: 5
  controllerImage: ghcr.io/stokaro/ptah-operator@sha256:fd61a03af4f77d870fc21e05e7e80678095c92d808cfb3b5c279ee04c74aca13
  controllerStateVersion: 2
```

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

