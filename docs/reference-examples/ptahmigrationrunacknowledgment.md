## Examples

An acknowledgment settles one run a `PtahMigration` recorded in
`status.unresolvedRun`: an Apply that ended `Partial` or `Unknown`, whose
effect on the database nobody established. It carries the decision -- which
migration, and which run -- and who made it. The admission webhook refuses one
that names anything but the run the migration records right now, then stamps
who you are and when from the authenticated request.

Write one only after you have established what the run did. The controller
settles the record in your name, reads the database again, and plans whatever
is still pending; under `apply: OnApproval` that plan waits for an approval
like any other.

### What a person writes

Copy the migration's UID and the run's operation ID the operator published:

```sh
kubectl -n application get ptahmigration orders \
  -o jsonpath='{.metadata.uid}{"\n"}{.status.unresolvedRun.operationID}{"\n"}'
```

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigrationRunAcknowledgment
metadata:
  name: orders-run-accounted-for
  namespace: application
spec:
  migrationRef:
    name: orders
    uid: 8d3f6c2b-1a4e-4f90-b7c5-2e6a8d0b3f41
  operationID: sha256:4c2a7e9b1d3f5a6c8e0b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2c4e6b8d0f2a4c
```

### What the cluster stores

The same object after admission. The two fields above are unchanged, and the
stamp was added from the request.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigrationRunAcknowledgment
metadata:
  name: orders-run-accounted-for
  namespace: application
spec:
  migrationRef:
    name: orders
    uid: 8d3f6c2b-1a4e-4f90-b7c5-2e6a8d0b3f41
  operationID: sha256:4c2a7e9b1d3f5a6c8e0b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2c4e6b8d0f2a4c
  acknowledgedBy:
    username: jane@example.com
    uid: 1b9d6bcf-bbfd-4b2d-9b5d-ab8dfbbd4bed
    groups:
      - dba
  acknowledgedAt: "2026-09-20T09:14:02Z"
  mutationRequestUID: 6f4b2a18-8c3e-4d5a-b1f7-2e0c9d8a7b64
```

Once the controller has settled the run, the migration names the
acknowledgment and the identity on it, and the acknowledgment reports
`Consumed=True`:

```sh
kubectl -n application get ptahmigration orders -o jsonpath='{.status.resolvedRun}' | jq
kubectl -n application get ptahmigrationrunacknowledgment orders-run-accounted-for
```
