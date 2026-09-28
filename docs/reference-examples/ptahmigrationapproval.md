## Examples

An approval authorizes one plan, once. It carries the decision -- which
migration, which plan, and that plan's fingerprint -- and who made it. The
admission webhook holds the three to the plan it names and refuses any that
disagrees, then stamps who you are and when from the authenticated request.

A migration plan's fingerprint binds the history as well as the sequence. A
sequence applied to a database that has moved on since the plan was computed
is a different change, so it gets a different plan, and the approval stays
with the one it named.

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

The same object after admission. The three fields above are unchanged; the
stamp was added from the request, and nothing was copied from the plan.

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
```

The history the plan was computed against, the artifact, and the execution
binding the run must happen under are on the plan the approval names:

```sh
kubectl -n application get ptahmigrationplan ptah-mplan-19581e27de7ced00ff1ce50b \
  -o jsonpath='{.spec.historyFingerprint}{"\n"}{.spec.artifactDigest}{"\n"}{.spec.executionBindingID}{"\n"}'
```
