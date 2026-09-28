## Examples

A `PtahMigration` runs an ordered sequence and records what it ran. Where a
`PtahSchema` compares a declaration against the database, this one asks the
database which versions it already has and applies the rest in order.

### The smallest resource that runs

An artifact holding the sequence, the policy it must satisfy, and the database
to run it against. `spec.policy.apply` defaults to `OnApproval`: the resource
reads the history, plans the versions that are missing, and waits.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigration
metadata:
  name: orders
  namespace: application
spec:
  target:
    engine: PostgreSQL
    coordinationKey: production/application-primary
    urlFrom:
      name: application-database
      key: url
  artifact:
    ociRef: oci://ghcr.io/example/orders-migrations:1.4.0
    verificationPolicyFrom:
      name: ptah-migration-verification-policy
      key: policy.yaml
```

### Beside a PtahSchema, over one database

Both resources live in one namespace, name the same `coordinationKey` and set
`sharedRealm`, so they take turns under one lease rather than running at once.
The operator checks that a person decided to share; it cannot check that the
areas they write to are really disjoint.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigration
metadata:
  name: orders
  namespace: application
spec:
  target:
    engine: PostgreSQL
    coordinationKey: production/application-primary
    sharedRealm: true
    urlFrom:
      name: application-database
      key: url
  artifact:
    ociRef: oci://ghcr.io/example/orders-migrations@sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae
    verificationPolicyFrom:
      name: ptah-migration-verification-policy
      key: policy.yaml
  policy:
    # A migration artifact carries arbitrary SQL, and no analyzer calls
    # arbitrary SQL safe, so the conservative setting is also the default.
    apply: OnApproval
    # The wait for the database's own migration lock. This is not the
    # Kubernetes Lease: two controllers that never run at the same time still
    # need the database to serialize them.
    lockTimeout: 5m
  interval: 10m
  execution:
    activeDeadlineSeconds: 900
    serviceAccountName: ptah-execution
```

### An engine that will not run DDL inside a transaction

MySQL commits implicitly on DDL, so a file that fails halfway leaves what ran
in place. `transactionMode: none` says that plainly instead of promising a
rollback the engine will not perform.

There is no setting that makes such a failure recover on its own. The operator
records what it cannot account for in `status.unresolvedRun` and stops, and a
person decides what the interrupted file did before anything runs against that
database again; [Operations](../../use/operations/) has the procedure. A checkpoint
is not that answer either -- it decides where a new database starts, not what
an interrupted one has run.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigration
metadata:
  name: orders
  namespace: application
spec:
  target:
    engine: MySQL
    coordinationKey: production/orders-primary
    urlFrom:
      name: orders-database
      key: url
  artifact:
    ociRef: oci://ghcr.io/example/orders-migrations:1.4.0
    verificationPolicyFrom:
      name: ptah-migration-verification-policy
      key: policy.yaml
  policy:
    apply: OnApproval
    transactionMode: none
  suspend: false
```

### A database another namespace manages too

A key reaches no further than its namespace, so a database that resources in
two namespaces manage is named by a [PtahRealm](../ptahrealm/) an
administrator created instead. `realmRef` replaces `coordinationKey`; a target
names one or the other. The realm has to list this resource's namespace, or the
resource is refused with reason `RealmNotAuthorized` and runs nothing, and a
realm that allows sharing still needs `sharedRealm` from every claimant.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigration
metadata:
  name: reporting-views
  namespace: reporting
spec:
  target:
    engine: PostgreSQL
    realmRef:
      name: production-application-primary
    sharedRealm: true
    urlFrom:
      name: application-database-reporting
      key: url
  artifact:
    ociRef: oci://ghcr.io/example/reporting-migrations:2.0.1
    verificationPolicyFrom:
      name: ptah-migration-verification-policy
      key: policy.yaml
```

### In a namespace with a service mesh or a policy engine

A migration's Pods are held to their Job template the way a schema's are, and
`podMetadata` is the same declaration: the annotation that opts the Pods out
of sidecar injection, and the label a policy engine requires. A Pod refused
anyway is reported as `Progressing=False` with reason `PodAdmissionRefused`.
[Execution](../execution/#meshes-and-policy-engines) has the contract.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigration
metadata:
  name: orders
  namespace: application
spec:
  target:
    engine: PostgreSQL
    coordinationKey: production/application-primary
    urlFrom:
      name: application-database
      key: url
  artifact:
    ociRef: oci://ghcr.io/example/orders-migrations:1.4.0
    verificationPolicyFrom:
      name: ptah-migration-verification-policy
      key: policy.yaml
  execution:
    podMetadata:
      labels:
        acme.example/team: platform
      annotations:
        sidecar.istio.io/inject: "false"
```
