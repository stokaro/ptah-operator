---
title: PtahSchemaApproval
description: Every field of the PtahSchemaApproval resource, generated from the API types.
---

`PtahSchemaApproval` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`. `kubectl` knows it as `ptahschemaapprovals`, or `ptahapprove` for short.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## Examples

An approval authorizes one plan, once. The table below lists every field, and
most of them are not yours to write: the admission webhook copies the binding
from the plan you named and refuses any value that disagrees with it, then
stamps who you are and when from the authenticated request.

### What a person writes

Three identifiers. Read the plan first, then copy them out of the resources the
operator already published:

```sh
kubectl ptah plan application -n application

kubectl -n application get ptahschema application \
  -o jsonpath='{.metadata.uid}{"\n"}{.status.plan.name}{"\n"}{.status.plan.uid}{"\n"}'
kubectl -n application get ptahschemaplan <plan-name> \
  -o jsonpath='{.spec.fingerprint}{"\n"}'
```

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchemaApproval
metadata:
  name: approve-application-1
  namespace: application
spec:
  schemaRef:
    name: application
    uid: 4f2c9e1a-5b6d-4a7e-9c31-0d8f2b6a4e57
  planRef:
    name: application-3f79bb7b
    uid: 9a1b3c5d-7e9f-4012-83a4-5c6d7e8f9a0b
  # spec.fingerprint of that plan, not a digest of the SQL.
  planFingerprint: sha256:71c480df93d6ae2f14efe3c44baabb7d3bc5d0e2de07d0e7a9b1a6cbd5f7ca3f
```

An approval that names a plan the schema has moved past is refused. So is one
whose fingerprint does not match the plan it names: the fingerprint binds the
artifact, the observed and desired state, the policy, the target identity and
the executing images, so a change to any of them retires the approval rather
than letting it carry over.

### What the cluster stores

The same object, read back after admission. The three fields above are
unchanged; everything else was filled in.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchemaApproval
metadata:
  name: approve-application-1
  namespace: application
spec:
  schemaRef:
    name: application
    uid: 4f2c9e1a-5b6d-4a7e-9c31-0d8f2b6a4e57
  planRef:
    name: application-3f79bb7b
    uid: 9a1b3c5d-7e9f-4012-83a4-5c6d7e8f9a0b
  planFingerprint: sha256:71c480df93d6ae2f14efe3c44baabb7d3bc5d0e2de07d0e7a9b1a6cbd5f7ca3f
  # Stamped from the authenticated request, not from the document.
  approver:
    username: jane@example.com
    uid: 1b9d6bcf-bbfd-4b2d-9b5d-ab8dfbbd4bed
    groups:
      - schema-approvers
  approvedAt: "2026-09-20T09:14:02Z"
  mutationRequestUID: 6f4b2a18-8c3e-4d5a-b1f7-2e0c9d8a7b64
  # Copied from the plan. Apply reconstructs these and rehashes them; a
  # mismatch retires the approval rather than running under it.
  artifactDigest: sha256:3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea
  desiredStateFingerprint: sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae
  actualStateFingerprint: sha256:fcde2b2edba56bf408601fb721fe9b5c338d10ee429ea04fae5511b68fbf8fb9
  coordinationDigest: sha256:084fed08b978af4d7d196a7446a86b58009e636b611db16211b65a9aadff29c5
  targetIdentityDigest: sha256:e7f6c011776e8db7cd330b54174fd76f7d0216b612387a5ffcfb81e6f0919683
  policyFingerprint: sha256:67586e98fad27da0b9968bc039a1ef34c939b9b8e523a8bef89d478608c5ecf6
  executionBindingID: v1-9f8e7d6c5b4a39281706f5e4d3c2b1a0
  ptahVersion: v0.42.0
  executorImage: ghcr.io/stokaro/ptah@sha256:1b4f0e9851971998e732078544c96b36c3d01cedf7caa332359d6f1d83567014
  runnerImage: ghcr.io/stokaro/ptah-runner@sha256:60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752
  runnerProtocolVersion: 1
  controllerImage: ghcr.io/stokaro/ptah-operator@sha256:fd61a03af4f77d870fc21e05e7e80678095c92d808cfb3b5c279ee04c74aca13
  # The exact manager build, not a number.
  controllerRevision: a7d0119c0bd0d34e0b73f1d9e0e5c6aa0d9ff2b1
  controllerStateVersion: 3
```

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

