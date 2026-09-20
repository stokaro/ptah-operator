---
title: PtahSchemaPlan
description: Every field of the PtahSchemaPlan resource, generated from the API types.
---

`PtahSchemaPlan` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`. `kubectl` knows it as `ptahschemaplans`, or `ptahplan` for short.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## Examples

Nobody writes a `PtahSchemaPlan`. The operator publishes one when it has
compared the declaration against the database, and the object is immutable
afterwards: it is the thing an approval names and the thing Apply reconstructs
and rehashes before it runs anything.

The examples below are what `kubectl get -o yaml` returns, shortened to the
fields worth looking at. Read the SQL with the plugin rather than out of the
object -- the statements travel as chunks, and the plugin assembles them:

```sh
kubectl ptah plan application -n application
```

### A plan with changes in it

`destructive: false` is the one field to read first: it says no statement in
this plan drops or rewrites anything. `spec.fingerprint` is what an approval
must name, and it binds far more than the SQL -- the artifact, the observed and
desired state, the policy, the target identity and every executing image.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchemaPlan
metadata:
  name: application-3f79bb7b
  namespace: application
  # The plan belongs to the schema and is collected with it.
  ownerReferences:
    - apiVersion: operator.ptah.run/v1alpha1
      kind: PtahSchema
      name: application
      uid: 4f2c9e1a-5b6d-4a7e-9c31-0d8f2b6a4e57
      controller: true
spec:
  schemaRef:
    name: application
    uid: 4f2c9e1a-5b6d-4a7e-9c31-0d8f2b6a4e57
  fingerprint: sha256:71c480df93d6ae2f14efe3c44baabb7d3bc5d0e2de07d0e7a9b1a6cbd5f7ca3f
  dialect: postgres
  destructive: false
  statementCount: 4
  size: 1832
  contentDigest: sha256:3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea
  # The SQL itself, in ConfigMaps this plan owns. A plan is bounded: at most
  # 16 chunks of 512 KiB, and 8 MiB in total.
  chunks:
    - index: 0
      name: application-3f79bb7b-0
      key: plan.sql
      size: 1832
      digest: sha256:3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea
  contractVersion: 1
  artifactDigest: sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae
  verificationPolicyUID: 7c9e6679-7425-40de-944b-e07fc1f90ae7
  verificationPolicyDigest: sha256:084fed08b978af4d7d196a7446a86b58009e636b611db16211b65a9aadff29c5
  desiredStateFingerprint: sha256:19581e27de7ced00ff1ce50b2047e7a567c76b1cbaebabe5ef03f7c3017bb5b7
  actualStateFingerprint: sha256:4a44dc15364204a80fe80e9039455cc1608281820fe2b24f1e5233ade6af1dd5
  coordinationDigest: sha256:e7f6c011776e8db7cd330b54174fd76f7d0216b612387a5ffcfb81e6f0919683
  targetIdentityDigest: sha256:67586e98fad27da0b9968bc039a1ef34c939b9b8e523a8bef89d478608c5ecf6
  policyFingerprint: sha256:fcde2b2edba56bf408601fb721fe9b5c338d10ee429ea04fae5511b68fbf8fb9
  ptahVersion: v0.42.0
  executorImage: ghcr.io/stokaro/ptah@sha256:1b4f0e9851971998e732078544c96b36c3d01cedf7caa332359d6f1d83567014
  runnerImage: ghcr.io/stokaro/ptah-runner@sha256:60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752
  runnerProtocolVersion: 1
  controllerImage: ghcr.io/stokaro/ptah-operator@sha256:fd61a03af4f77d870fc21e05e7e80678095c92d808cfb3b5c279ee04c74aca13
  controllerStateVersion: 3
```

### A plan that would destroy something

Same shape, `destructive: true`. With `spec.policy.allowDestructive` left at
its default, the schema does not offer this plan for approval at all: it stays
`Blocked`, and an approval naming it is refused. Reading the plan is how you
find out what the artifact would have dropped.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchemaPlan
metadata:
  name: application-9c56cc51
  namespace: application
spec:
  schemaRef:
    name: application
    uid: 4f2c9e1a-5b6d-4a7e-9c31-0d8f2b6a4e57
  fingerprint: sha256:9c56cc51b374c3ba189210d5b6d4bf57790d351c96c47c02190ecf1e430635ab
  dialect: postgres
  destructive: true
  statementCount: 1
  size: 96
  contentDigest: sha256:6b51d431df5d7f141cbececcf79edf3dd861c3b4069f0b11661a3eefacbba918
  chunks:
    - index: 0
      name: application-9c56cc51-0
      key: plan.sql
      size: 96
      digest: sha256:6b51d431df5d7f141cbececcf79edf3dd861c3b4069f0b11661a3eefacbba918
  contractVersion: 1
  artifactDigest: sha256:d4735e3a265e16eee03f59718b9b5d03019c07d8b6c51f90da3a666eec13ab35
  verificationPolicyUID: 7c9e6679-7425-40de-944b-e07fc1f90ae7
  verificationPolicyDigest: sha256:084fed08b978af4d7d196a7446a86b58009e636b611db16211b65a9aadff29c5
  desiredStateFingerprint: sha256:4e07408562bedb8b60ce05c1decfe3ad16b72230967de01f640b7e4729b49fce
  actualStateFingerprint: sha256:4a44dc15364204a80fe80e9039455cc1608281820fe2b24f1e5233ade6af1dd5
  coordinationDigest: sha256:e7f6c011776e8db7cd330b54174fd76f7d0216b612387a5ffcfb81e6f0919683
  targetIdentityDigest: sha256:67586e98fad27da0b9968bc039a1ef34c939b9b8e523a8bef89d478608c5ecf6
  policyFingerprint: sha256:fcde2b2edba56bf408601fb721fe9b5c338d10ee429ea04fae5511b68fbf8fb9
  ptahVersion: v0.42.0
  executorImage: ghcr.io/stokaro/ptah@sha256:1b4f0e9851971998e732078544c96b36c3d01cedf7caa332359d6f1d83567014
  runnerImage: ghcr.io/stokaro/ptah-runner@sha256:60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752
  runnerProtocolVersion: 1
  controllerImage: ghcr.io/stokaro/ptah-operator@sha256:fd61a03af4f77d870fc21e05e7e80678095c92d808cfb3b5c279ee04c74aca13
  controllerStateVersion: 3
```

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

