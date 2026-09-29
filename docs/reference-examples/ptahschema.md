## Examples

These examples cover approved changes, unattended operation, shared databases,
and workload admission. Every field not named here takes the default the table
below records.

### The smallest resource that runs

A target, a desired-state artifact, and the policy the artifact must satisfy.
Nothing applies by itself: `spec.policy.apply` defaults to `OnApproval`, so
this resource plans, reports the drift it found, and waits for a
`PtahSchemaApproval` naming that plan.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchema
metadata:
  name: application
  namespace: application
spec:
  target:
    engine: PostgreSQL
    # Every resource in this namespace that can write to this physical
    # database must use this exact key, whatever DNS alias, proxy or credential
    # it reaches it through. The key reaches no further than the namespace.
    coordinationKey: production/application-primary
    urlFrom:
      name: application-database
      key: url
  desired:
    ociRef: oci://ghcr.io/example/application-schema:1.4.0
    verificationPolicyFrom:
      name: ptah-verification-policy
      key: policy.yaml
```

### An approved change on MySQL

Publish the desired schema with `ptah schema push --dialect mysql` and use
the resulting artifact digest. The Secret must name the MySQL database; the
operator account needs the [database-scoped privileges](../../support/databases/#mysql-authority)
for the objects the schema manages. The plan review and `PtahSchemaApproval`
flow are the same as for PostgreSQL.

MySQL DDL can commit before a later statement fails. An approved plan can
therefore leave partial effects; inspect the recorded outcome and the actual
database before authorizing recovery. Approval does not make DDL atomic.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchema
metadata:
  name: application-mysql
  namespace: application
spec:
  target:
    engine: MySQL
    coordinationKey: production/application-mysql-primary
    urlFrom:
      name: application-mysql-database
      key: url
  desired:
    ociRef: oci://ghcr.io/example/application-schema-mysql@sha256:3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea
    verificationPolicyFrom:
      name: ptah-verification-policy
      key: policy.yaml
  policy:
    apply: OnApproval
    allowDestructive: false
```

### Unattended, where nobody is waiting to approve

`apply: Always` skips the approval and applies what it planned. It stays safe
to leave running because the fences below it hold: a destructive change is
refused rather than applied, a table named in `protectedTables` is refused even
when it is not, and a plan that changes privileges -- a grant, a role, an owner,
a row-security policy, a `SECURITY DEFINER` function -- waits for an approval
the way it would under `OnApproval`.

Suitable for a development or staging database. On a production database it
means an artifact push is a schema change with no person between the two.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchema
metadata:
  name: application
  namespace: staging
spec:
  target:
    engine: PostgreSQL
    coordinationKey: staging/application-primary
    urlFrom:
      name: application-database
      key: url
  desired:
    ociRef: oci://ghcr.io/example/application-schema:main
    verificationPolicyFrom:
      name: ptah-verification-policy
      key: policy.yaml
  interval: 2m
  policy:
    apply: Always
    # A plan that drops or rewrites anything is refused, not applied.
    allowDestructive: false
    # Report drift only where it destroys something, so an unattended resource
    # is not noisy about additions it is about to make anyway.
    driftSeverity: destructive
    # Tables the declaration does not own. Anything else changes them.
    exclude:
      - schema_migrations
```

### A database more than one resource manages

`sharedRealm` is the declaration that taking turns is intended. Every claimant
of the same `coordinationKey` in this namespace has to set it: with one left
`false`, all of them are refused rather than allowed to undo each other's work.
When the other claimant lives in another namespace, the database is named by a
[PtahRealm](../ptahrealm/) instead, and the realm has to allow the sharing too.

`protectedTables` is a fence with no override. A plan that would touch one of
these leaves the resource `Blocked` with reason `ProtectedTable`, and no
approval and no policy lifts it.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchema
metadata:
  name: application
  namespace: application
spec:
  target:
    engine: PostgreSQL
    coordinationKey: production/application-primary
    # The PtahMigration over the same database says this too.
    sharedRealm: true
    urlFrom:
      name: application-database
      key: url
  desired:
    # A digest pins the artifact for good; a tag is resolved to one per cycle.
    ociRef: oci://ghcr.io/example/application-schema@sha256:3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea
    verificationPolicyFrom:
      name: ptah-verification-policy
      key: policy.yaml
  policy:
    apply: OnApproval
    allowDestructive: false
    protectedTables:
      - billing_ledger
      - audit_log
  interval: 10m
  execution:
    activeDeadlineSeconds: 900
    # The identity the operation Jobs run as, which is never the controller's
    # own: a Job that could act as the controller could write the status that
    # judges it.
    serviceAccountName: ptah-execution
```

### In a namespace with a service mesh or a policy engine

Every operation Pod is held to exactly what its Job template carries, so a
sidecar a mesh injects, or a label a policy engine requires, would otherwise
refuse the Pod before it runs. `podMetadata` is what the Pods carry beside the
operator's own labels: the annotation that opts them out of injection, and
the label the policy wants to see. A Pod refused anyway is reported as
`Ready=False` with reason `PodAdmissionRefused`, with the API server's
refusal in the message. Keys under `ptah.run`, `kubernetes.io` and `k8s.io`
are refused, which keeps the operator's own metadata and the chart's
selectors out of reach. [Execution](../execution/#meshes-and-policy-engines)
has the contract.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchema
metadata:
  name: application
  namespace: application
spec:
  target:
    engine: PostgreSQL
    coordinationKey: production/application-primary
    urlFrom:
      name: application-database
      key: url
  desired:
    ociRef: oci://ghcr.io/example/application-schema:1.4.0
    verificationPolicyFrom:
      name: ptah-verification-policy
      key: policy.yaml
  execution:
    podMetadata:
      labels:
        # A policy engine that requires every Pod to name its owner.
        acme.example/team: platform
      annotations:
        # Istio's per-Pod opt-out: nothing runs beside the credential.
        sidecar.istio.io/inject: "false"
```
