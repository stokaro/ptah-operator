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
