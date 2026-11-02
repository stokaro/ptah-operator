---
title: Exact-plan approvals
description: Why an approval names one plan, and what the operator refuses when it does not.
---

An approval is an independent authorization object, not a Boolean field on the
schema. The approver must explicitly select all three stable identifiers:

- schema name and UID;
- plan name and UID;
- plan fingerprint.

Those three are the whole of what an approval says about the plan. The plan's
fingerprint is its complete identity -- the artifact, the observed and desired
state, the policy, the verification policy, the target and the execution
binding -- and a plan is immutable, so naming it by UID and fingerprint names
every one of them; the approval carries no copy. The mutating webhook reads the
plan directly from the API server, holds the three identifiers to it, and
stamps who approved and when. The validating webhook then checks the object
against the current schema, the plan's storage commit, the observed database
state, the artifact digest, the policy bytes and the execution binding.

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

An approval names the schema with its UID, the plan with its UID and the plan's
fingerprint, and nothing else a person writes: admission stamps who approved
and when. Review the plan first.

The plan resource contains immutable chunk names and digests; the exact SQL is
in the `PtahSchemaPlanChunk` objects it names, which the controller writes with
the plan and which are deleted with it. Reading them takes one RBAC rule, `get`
on `ptahschemaplanchunks` in the namespace, and it covers every plan published
there, the next one included. The chart's approver ClusterRole carries it. A
reviewer who is not bound to that role can start from
`examples/approver-plan-reader-role.yaml`, a namespace Role that reads the
schema, its plans and their chunks and nothing else. Neither reaches a
ConfigMap, so reading plans does not mean reading the namespace's application
configuration.

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

The rule on the chunks is not the only way to a plan's bytes. The Plan Pod
hands the whole document to the controller through its log, sealed to the
manager's own key, so the log holds ciphertext and `pods/log` grants nothing a
plan approval needs;
[Pod logs carry a sealed plan](../security/#pod-logs-carry-plans) has the
detail. An Apply Pod holds no Kubernetes credential and the kubelet mounts no
custom resource, so just before it creates the Apply Job the operator copies
the plan into immutable ConfigMaps of the chunks' names, owned by the plan, and
the Pod mounts those. Whoever may read ConfigMaps in the namespace reads the
plans that reached an Apply there; a plan waiting for a person has no
ConfigMap at all.

`kubectl ptah approve` turns the review into the approval. It shows the
current plan again with the identities the approval will carry, asks for a
decision, and creates the approval as you, so admission records your identity:

```sh
kubectl ptah approve schema/application -n application
```

It reads the schema once more before it sends anything, and approves nothing if
a newer plan replaced the one shown, the schema was deleted and created again,
or the operator would not admit an approval now, as while it reads the database
again at its interval. The answer it prints is the API server's: the approval
admission stored with the identity it stamped, or the refusal in RBAC's or
admission's own words.

- `--plan-fingerprint <fingerprint>` approves without asking, and only if the
  current plan carries that fingerprint. It is for a script that reviewed the
  plan; there is no option that approves whatever plan is current.
- `--dry-run server` lets admission check the approval and stores nothing.
- `--dry-run client` prints the approval as YAML with the identifiers filled in
  and sends nothing, for a workflow that keeps decisions in a repository. It
  takes the same decision as an approval: the fingerprint of the plan you
  reviewed, or the review answered with yes. The object carries the API
  version, the kind, a name made of the schema's name and the start of the
  fingerprint, the namespace and the three references, and nothing else.
  `examples/approval.yaml` is the same object written by hand.

Create a printed approval with `kubectl create -f`, as the person who decided.
Admission records whoever creates the object as its approver, so the identity
on the approval is the identity that sent it, not the one that printed it. A
controller that applies approvals from a repository is recorded as their
approver, and with `approvals.requireDistinctApprover` on, an approval it
creates for a schema whose spec it also wrote is refused as a self-approval.
An approval is immutable and authorizes one run, so it is created once; it is
not a desired state to apply again.

Exit status 4 means nothing was approved, 5 that RBAC or the API server
refused the approval, and 6 that the request was sent and no answer said
whether it was stored. After a 6, `kubectl ptah schema application -n
application` lists the approvals that name the schema; read it before
approving again.

Creation is rejected if the plan is already stale, its immutable storage is
not committed, the verification-policy ConfigMap changed, or the approval
names a plan of another schema, a plan UID the plan no longer has, or a
fingerprint the plan does not carry. Creation is also rejected unless the schema is
currently waiting for exactly one approval and no operation or recorded
approval already owns that decision. Concurrent duplicates are retired, and
the accepted approval is consumed only as the Apply crosses its dispatch
boundary, just before the dispatch marker is written. Updates cannot change `spec`; create a new approval for a new plan.

What became of an approval is on the approval: `Accepted`, `Consumed` and
`Stale`, each with a reason and a message.
[`kubectl ptah schema`](../read-a-plan/#schema) lists the approvals that name a
schema with those conditions, the identity admission stamped and whether each
names the current plan, so an approval of an earlier plan is still there to
read after the plan changed:

```sh
kubectl ptah schema application -n application
```

`kubectl describe ptahschemaapproval <name>` shows one approval whole.

The chart's optional approver ClusterRole grants read access to schemas,
migrations, their plans, the chunks a schema plan's SQL is stored in, and
approvals, plus create access to `PtahSchemaApproval`, `PtahMigrationApproval`
and `PtahMigrationRunAcknowledgment`, the decision that settles a migration run
nobody accounted for. It has no binding, no ConfigMap permission and no Pod log
permission. Bind it in each application namespace, and only to authenticated
identities that are independent from routine desired-state writers.

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
never move the recorded name. A change to `spec.suspend` alone does not count
either. Suspending and resuming change when the operator acts, not what it
plans, and counting them would let whoever paused a resource move the record
off its author, who could then approve their own change. When the
installation's flag is true, the approval webhook refuses an approval whose
approver is exactly that identity, or whose resource carries no recorded
identity at all, with a reason that says so.

Editing spec after a plan is awaiting approval already retires that plan and
its binding, so a fresh plan is always judged against whoever most recently
changed the spec it was computed from.

The mutating webhook that stamps the two annotations is itself installed only
while the switch is on, so a schema or migration written before an
installation turned it on carries no recorded writer. Turning the switch on
does not retroactively identify who wrote an existing resource's spec: its
approvals are refused, by the same "no spec writer is recorded" reason a
missing annotation always gets, until its spec is next changed and the
webhook has a create or update to stamp. Plan for that gap when turning the
switch on: an existing schema or migration stops taking approvals until an
edit of its spec other than `spec.suspend`, however small, records its first
writer.

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
