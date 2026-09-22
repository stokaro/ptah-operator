## Examples

Nobody writes a `PtahMigrationPlan`. The operator publishes one when it has
read the database's history and worked out which versions are missing, and the
object is immutable afterwards.

A migration plan names versions rather than carrying SQL: the statements stay
in the artifact, and the plan records which versions run, in which order, and
what each one's checksum was when the plan was computed. Read it with the
plugin:

```sh
kubectl ptah migration orders -n application
```

### Two versions to apply

`currentVersion` is where the database was, and `migrations` is what would run.
`historyFingerprint` binds the history the plan was computed against, so a
version applied by anything else in the meantime retires this plan instead of
letting it run against a database it no longer describes.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigrationPlan
metadata:
  name: ptah-mplan-19581e27de7ced00ff1ce50b
  namespace: application
  ownerReferences:
    - apiVersion: operator.ptah.run/v1alpha1
      kind: PtahMigration
      name: orders
      uid: 8d3f6c2b-1a4e-4f90-b7c5-2e6a8d0b3f41
      controller: true
spec:
  migrationRef:
    name: orders
    uid: 8d3f6c2b-1a4e-4f90-b7c5-2e6a8d0b3f41
  fingerprint: sha256:19581e27de7ced00ff1ce50b2047e7a567c76b1cbaebabe5ef03f7c3017bb5b7
  createdAt: "2026-09-20T09:12:44Z"
  currentVersion: 12
  historyFingerprint: sha256:4a44dc15364204a80fe80e9039455cc1608281820fe2b24f1e5233ade6af1dd5
  migrations:
    - version: 13
      versionKey: "0013"
      description: add order status index
      checksum: sha256:3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea
      transactionMode: file
    - version: 14
      versionKey: "0014"
      description: backfill order status
      checksum: sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae
      transactionMode: file
  contractVersion: 1
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

### A fresh database, bootstrapped from a checkpoint

A migration file marked `checkpoint: true` carries the whole schema up to its
version. A database created after it starts there instead of replaying
everything before it, so the plan for an empty database names the checkpoint
and the versions after it, and nothing below it. `status.history` on the
resource reports those lower versions as applied and names the checkpoint that
covers them.

A checkpoint changes where a new database starts and nothing else. A database
already at version 10 or beyond when this checkpoint arrived ignores it and
goes on from whatever it has run. It does not undo statements a failed migration
committed, clear a dirty revision, or make a backfill safe to run twice; those
are answered by the revision table and by the recovery a person performs
against it, which [Operations](../../use/operations/) describes.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigrationPlan
metadata:
  name: ptah-mplan-ef2d127de37b942baad06145
  namespace: application
  ownerReferences:
    - apiVersion: operator.ptah.run/v1alpha1
      kind: PtahMigration
      name: orders
      uid: 8d3f6c2b-1a4e-4f90-b7c5-2e6a8d0b3f41
      controller: true
spec:
  migrationRef:
    name: orders
    uid: 8d3f6c2b-1a4e-4f90-b7c5-2e6a8d0b3f41
  fingerprint: sha256:ef2d127de37b942baad06145e54b0c619a1f22327b2ebbcfbec78f5564afe39d
  createdAt: "2026-09-20T08:40:12Z"
  # Nothing has run here yet.
  currentVersion: 0
  historyFingerprint: sha256:e7f6c011776e8db7cd330b54174fd76f7d0216b612387a5ffcfb81e6f0919683
  migrations:
    # The cumulative schema through version 10, and the plan runs it: a
    # checkpoint covers the versions below itself, not itself. Versions 1 to 9
    # are absent, because this file is what puts them in place.
    - version: 10
      versionKey: "0010"
      description: cumulative schema through version 10
      checksum: sha256:7902699be42c8a8e46fbbb4501726517e86b22c56a189f7625a6da49081b2451
      transactionMode: file
      checkpoint: true
    - version: 13
      versionKey: "0013"
      description: add order status index
      checksum: sha256:3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea
      transactionMode: file
    - version: 14
      versionKey: "0014"
      description: backfill order status
      checksum: sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae
      transactionMode: file
  contractVersion: 1
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

### One version, on an engine with no DDL transaction

`transactionMode: none` is the artifact's own declaration for that file. It
says a failure partway through leaves what already ran in place, because the
engine will not roll DDL back. The operator records the version as uncertain
rather than claiming either outcome.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigrationPlan
metadata:
  name: ptah-mplan-6b51d431df5d7f141cbececc
  namespace: application
spec:
  migrationRef:
    name: orders
    uid: 8d3f6c2b-1a4e-4f90-b7c5-2e6a8d0b3f41
  fingerprint: sha256:6b51d431df5d7f141cbececcf79edf3dd861c3b4069f0b11661a3eefacbba918
  createdAt: "2026-09-20T10:03:51Z"
  currentVersion: 14
  historyFingerprint: sha256:9c56cc51b374c3ba189210d5b6d4bf57790d351c96c47c02190ecf1e430635ab
  migrations:
    - version: 15
      versionKey: "0015"
      description: widen order reference column
      checksum: sha256:4e07408562bedb8b60ce05c1decfe3ad16b72230967de01f640b7e4729b49fce
      transactionMode: none
  contractVersion: 1
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
