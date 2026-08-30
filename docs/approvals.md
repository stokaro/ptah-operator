# Exact-plan approvals

An approval is an independent authorization object, not a Boolean field on the
schema. The approver must explicitly select all three stable identifiers:

- schema name and UID;
- plan name and UID;
- plan fingerprint.

The admission webhook reads the current plan directly from the API server and
fills the remaining derived bindings when they are omitted. It never silently
corrects a conflicting value. The validating webhook then checks the complete
post-mutation object against the current schema, plan storage commit, observed
database state, artifact digest, policy bytes, and execution images.

Start from [the minimal approval example](../examples/approval.yaml). Obtain the
values only after reviewing the plan:

```sh
kubectl -n application get ptahschema application \
  -o jsonpath='{.metadata.uid}{"\n"}{.status.plan.name}{"\n"}{.status.plan.uid}{"\n"}{.status.plan.fingerprint}{"\n"}'
kubectl -n application get ptahschemaplan <plan-name> -o yaml
```

Fill those values in the approval and use server-side dry run to inspect the
object after authenticated identity and derived bindings are stamped:

```sh
kubectl apply --server-side --dry-run=server -f examples/approval.yaml -o yaml
kubectl apply -f examples/approval.yaml
```

Creation is rejected if the plan is already stale, its immutable storage is
not committed, the verification-policy ConfigMap changed, or a supplied
derived field conflicts. Updates cannot change `spec`; create a new approval
for a new plan.

The chart's optional approver ClusterRole grants read access to schemas and
plans plus create access to approvals, but it has no binding. Bind approval
permission only to authenticated identities that are independent from routine
desired-state writers. Namespace-scoped Roles are preferable in multi-tenant
clusters.
