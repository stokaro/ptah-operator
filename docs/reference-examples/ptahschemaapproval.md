## Examples

An approval authorizes one plan, once. It carries the decision -- which
schema, which plan, and that plan's fingerprint -- and who made it. The
admission webhook holds the three to the plan it names and refuses any that
disagrees, then stamps who you are and when from the authenticated request.

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
whose fingerprint does not match the plan it names, one whose plan belongs to
another schema, and one that names a plan by a UID the plan no longer has.
The fingerprint is the plan's complete identity: it binds the artifact, the
observed and desired state, the policy, the verification policy, the target
identity and the execution binding -- the executor image, the Ptah version,
the runner protocol and the controller-state version -- and a plan is
immutable, so naming it by UID and fingerprint names every one of those. A
change to any of them produces another plan with another fingerprint, and the
approval stays with the one it named. The manager's own image and revision are
not in it: a manager release that changes only those, a patch or a security
fix, keeps the approval and applies the plan it names.

### What the cluster stores

The same object, read back after admission. The three fields above are
unchanged; the stamp was added, and nothing was copied from the plan.

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
```

What the decision was made under -- the artifact digest, the state
fingerprints, the execution binding -- is read off the plan the approval
names:

```sh
kubectl -n application get ptahschemaplan ptah-plan-71c480df93d6ae2f14efe3c4 \
  -o jsonpath='{.spec.artifactDigest}{"\n"}{.spec.executionBindingID}{"\n"}{.spec.executorImage}{"\n"}'
```
