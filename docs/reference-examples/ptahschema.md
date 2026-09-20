## Examples

Three shapes, from the least a cluster accepts to the one a shared production
database needs. Every field not named here takes the default the table below
records.

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
    # Every resource that can write to this physical database must use this
    # exact key, whatever DNS alias, proxy or credential it reaches it through.
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

### Unattended, where nobody is waiting to approve

`apply: Always` skips the approval and applies what it planned. It stays safe
to leave running because the two fences below it hold: a destructive change is
refused rather than applied, and a table named in `protectedTables` is refused
even when it is not.

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
of the same `coordinationKey` has to set it: with one left `false`, all of them
are refused rather than allowed to undo each other's work.

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
