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
    name: ptah-plan-71c480df93d6ae2f14efe3c4
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
    name: ptah-plan-71c480df93d6ae2f14efe3c4
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
  artifactDigest: sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae
  desiredStateFingerprint: sha256:19581e27de7ced00ff1ce50b2047e7a567c76b1cbaebabe5ef03f7c3017bb5b7
  actualStateFingerprint: sha256:4a44dc15364204a80fe80e9039455cc1608281820fe2b24f1e5233ade6af1dd5
  coordinationDigest: sha256:e7f6c011776e8db7cd330b54174fd76f7d0216b612387a5ffcfb81e6f0919683
  targetIdentityDigest: sha256:67586e98fad27da0b9968bc039a1ef34c939b9b8e523a8bef89d478608c5ecf6
  policyFingerprint: sha256:fcde2b2edba56bf408601fb721fe9b5c338d10ee429ea04fae5511b68fbf8fb9
  executionBindingID: v1-9f8e7d6c5b4a39281706f5e4d3c2b1a0
  ptahVersion: v0.7.0
  executorImage: ghcr.io/stokaro/ptah@sha256:1b4f0e9851971998e732078544c96b36c3d01cedf7caa332359d6f1d83567014
  runnerImage: ghcr.io/stokaro/ptah-runner@sha256:60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752
  runnerProtocolVersion: 5
  controllerImage: ghcr.io/stokaro/ptah-operator@sha256:fd61a03af4f77d870fc21e05e7e80678095c92d808cfb3b5c279ee04c74aca13
  # The exact manager build, not a number.
  controllerRevision: a7d0119c0bd0d34e0b73f1d9e0e5c6aa0d9ff2b1
  controllerStateVersion: 2
```
