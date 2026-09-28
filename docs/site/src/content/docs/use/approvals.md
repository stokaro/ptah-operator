---
title: Exact-plan approvals
description: Why an approval names one plan, and what the operator refuses when it does not.
---

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

Approvals are required where `spec.policy.apply` asks for them, and for any
schema plan that
[changes privileges](../../reference/plans-and-approvals/#privilege-changes)
whatever it asks. That field is on the desired-state resource, so whoever may
edit a `PtahSchema` or a `PtahMigration` may also select `Always` and have the
remaining plans applied without any approval: for a schema, the ones that
destroy nothing and change no privilege; for a migration, all of them. The
chart's `applyPolicyGuard`, on by default, is what keeps that choice with the
groups it names; the
[security model](../security/#who-may-turn-the-approval-requirement-off)
says why RBAC alone does not do it, and which groups to name.

Start from the minimal approval example in `examples/approval.yaml`. Obtain the
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
least-privilege Role template in `examples/approver-plan-reader-role.yaml`,
copy all `.spec.chunks[*].name` values into `resourceNames`, and bind that Role
only to the reviewer. Replace the Role for the next plan. A broader Role that
can read every ConfigMap in an application namespace is easier to operate but
also exposes unrelated configuration.

Once the access is granted, read the plan with
[`kubectl ptah`](../read-a-plan/), which is a plugin the reviewer
[installs once](../read-a-plan/#install):

```sh
kubectl ptah plan application --current -n application
```

It reads every chunk, checks each against the digest and size the plan records,
joins them in index order and checks the whole document against
`spec.contentDigest` before printing a line. An approval of hashes without
reading the SQL they refer to is not an independent review, and neither is
reading one chunk of a plan that has several.

The chunk Role limits who reads a plan through its chunks, and the chunks are
not the only copy. The Plan Pod hands the whole document to the controller
through its log, so whoever may read Pod logs in the namespace, or the log
store a node agent ships them to, reads every plan published there without any
chunk Role. Keep `pods/log` for the people who may read every plan in the
namespace, and keep Plan Pod logs out of shared log stores;
[Pod logs carry plans](../security/#pod-logs-carry-plans) says how.

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

The chart's optional approver ClusterRole grants read access to schemas,
migrations, their plan metadata and approvals, plus create access to
`PtahSchemaApproval` and `PtahMigrationApproval`. It has no binding, no
ConfigMap permission and no Pod log permission. Bind approval permission only
to authenticated identities that are independent from routine desired-state
writers, and grant plan-chunk access separately in each application namespace.

## Refusing a self-approval

RBAC decides who *may* approve; on its own, an accepted approval proves an
authenticated approver, not a second person. `approvals.requireDistinctApprover`
closes that gap. It is a chart value, set by whoever installs the operator, not
a field on the schema or migration it protects: a switch a desired-state author
could reach from their own resource's spec would not bind that author. They
could turn it off, let their own edit retire the current plan and its binding,
have the operator replan on the new spec, and approve that identical plan
themselves, with nothing in the resource recording that the control was ever
off. Moving the switch to the chart is what #450 asked of the operator's other
approval control, the apply-mode guard; this one now matches it. It defaults to
false, so an existing installation that never sets it keeps admitting the
approvals it always did.

A mutating webhook always keeps a record of who created a schema or migration,
or who last changed its spec, in two annotations
(`operator.ptah.run/last-spec-writer-username` and `-uid`) that only that
webhook ever writes: it overwrites whatever a request carried for them, so an
author cannot name someone else, and it leaves them untouched on an update
that does not change spec, so the manager's own finalizer and status writes
never move the recorded name. When the installation's flag is true, the
approval webhook refuses an approval whose approver is exactly that identity,
or whose resource carries no recorded identity at all, with a reason that says
so.

Editing spec after a plan is awaiting approval already retires that plan and
its binding, so a fresh plan is always judged against whoever most recently
touched the spec it was computed from.

The mutating webhook that stamps the two annotations is itself installed only
while the switch is on, so a schema or migration written before an
installation turned it on carries no recorded writer. Turning the switch on
does not retroactively identify who wrote an existing resource's spec: its
approvals are refused, by the same "no spec writer is recorded" reason a
missing annotation always gets, until its spec is next changed and the
webhook has a create or update to stamp. Plan for that gap when turning the
switch on: an existing schema or migration stops taking approvals until an
edit, however small, records its first writer.

Identity here is exactly what the cluster's authentication reports for a
request: a username, and a UID where the authenticator supplies one. A
ServiceAccount counts like any other identity: the same ServiceAccount writing
the spec and later approving it is refused exactly like a person doing both.
Group membership plays no part in the comparison. A GitOps controller that
writes every schema and migration spec makes every human approver distinct by
construction, because no human ever appears as the recorded writer; the control
is aimed at direct human edits, and adds nothing where one already runs.

This is a limit worth stating plainly: impersonation, or a credential more
than one person uses, defeats the control, because the cluster's own audit
trail cannot tell those requests apart. And because the switch lives in the
chart, it does not stop an author who can also edit or upgrade this release:
that authority is a different, larger one than editing one namespace's
schemas, and RBAC over Helm releases and the operator's own namespace has to
close it. Where any of that matters, pair this control with the RBAC
separation above rather than relying on either alone.
