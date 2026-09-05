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

The plan resource contains immutable chunk names and digests; exact SQL is in
those controller-owned ConfigMaps. The built-in approver ClusterRole does not
grant cluster-wide ConfigMap access. Before review, a namespace administrator
must grant `get` on every current chunk name to the approver. Start from the
[least-privilege Role template](../examples/approver-plan-reader-role.yaml),
copy all `.spec.chunks[*].name` values into `resourceNames`, and bind that Role
only to the reviewer. Replace the Role for the next plan. A broader Role that
can read every ConfigMap in an application namespace is easier to operate but
also exposes unrelated configuration.

After access is granted, verify every chunk against its recorded digest and
review the chunks in ascending `.spec.chunks[*].index` order. An approval of
hashes without inspecting the referenced SQL is not an independent review.

Fill those values in the approval and use server-side dry run to inspect the
object after authenticated identity and derived bindings are stamped:

```sh
kubectl apply --server-side --dry-run=server -f examples/approval.yaml -o yaml
kubectl apply -f examples/approval.yaml
```

Creation is rejected if the plan is already stale, its immutable storage is
not committed, the verification-policy ConfigMap changed, or a supplied
derived field conflicts. Creation is also rejected unless the schema is
currently waiting for exactly one approval and no operation or recorded
approval already owns that decision. Concurrent duplicates are retired, and
the accepted approval is consumed only at the persisted Apply dispatch
boundary. Updates cannot change `spec`; create a new approval for a new plan.

The chart's optional approver ClusterRole grants read access to schemas and
plan metadata plus create access to approvals, but it has no binding and no
ConfigMap permission. Bind approval permission only to authenticated identities
that are independent from routine desired-state writers, and grant plan-chunk
access separately in each application namespace.
