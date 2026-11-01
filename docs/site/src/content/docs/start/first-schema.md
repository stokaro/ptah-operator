---
title: First schema
description: One worked example, from Secret and policy to a converged PtahSchema.
---

## Before you start

- The operator installed and its CRDs established: [Install](../install/), and
  its readiness check.
- A namespace for your own resources. This page uses `application`; the chart
  does not create it, and it is not the release namespace.
- A database the operator can reach, and a URL for it. The privileges it needs
  are [Databases and privileges](../../support/databases/).
- Your schema published as an OCI artifact, by `ptah schema push`, in a
  registry the cluster can read. Which Ptah build to use is
  [Ptah compatibility](../../support/ptah/); the digest that artifact prints is
  what the example points at.
- The `examples/` files named below, from the version you chose on
  [Install](../install/#choose-a-version-once). They are in the repository at
  that commit or tag, beside the chart, and an example from a different version
  can name a field this one does not have.

## Create what the schema reads

The database URL is a namespaced Secret and the verification policy a
ConfigMap, both in the namespace the resource lives in. Store the URL in a
private file with no trailing newline, outside the repository. Set
`DATABASE_URL_FILE` to its path. The command below reads that file so the
credential does not become a command argument or shell-history entry:

```sh
DATABASE_URL_FILE=/path/to/private/database-url
kubectl create namespace application
kubectl -n application create secret generic application-database \
  --from-file=url="$DATABASE_URL_FILE"
kubectl -n application create configmap ptah-verification-policy \
  --from-file=policy.yaml=examples/verification-policy.yaml
kubectl -n application patch configmap ptah-verification-policy \
  --type=merge -p '{"immutable":true}'
```

For PostgreSQL, replace the artifact placeholder in `examples/ptahschema.yaml`
and apply it:

```sh
kubectl apply -f examples/ptahschema.yaml
kubectl -n application get ptahschema application -w
```

For MySQL, use a MySQL database URL in that Secret and publish the schema with
`ptah schema push --dialect mysql`. Replace the artifact placeholder in
`examples/ptahschema-mysql.yaml` with the resulting digest, then apply that
manifest instead:

```sh
kubectl apply -f examples/ptahschema-mysql.yaml
kubectl -n application get ptahschema application -w
```

Both examples name the resource `application`, so the plan review and approval
commands below work for either engine. Choose one manifest for this resource.
The MySQL example manages a database of its own; see
[shared realms](../../reference/ptahrealm/) before adding another resource
that writes to the same database.

For private registries, add a
same-namespace `registryAuthFrom` reference; the API supports environment-key
Secrets and standard Docker config JSON Secrets. Either representation must
contain a fixed `registry` key whose authority-only `host[:port]` value exactly
matches the OCI client's effective request authority (`registry-1.docker.io`
for an `oci://docker.io/...` source). A Docker config Secret keeps its standard
`.dockerconfigjson` data in addition to that owner-controlled grant. If authenticated
`plainHTTP` is unavoidable, the authentication Secret must also contain
`allowPlainHTTP: "true"`; anonymous plain HTTP needs no Secret grant.
Verification-policy
ConfigMaps must be immutable. To change a policy, create a new ConfigMap name
and update the schema reference; delete-and-recreate is intentionally not
treated as the same policy.

What the schema reports while it converges is
[the status progression](../../use/operations/#normal-status-progression), and
what each reason means is
[condition reasons](../../troubleshoot/condition-reasons/).

## It stops, and waits for you

The example sets `apply: OnApproval`, which is also the default. The operator
resolves the artifact, observes the database and publishes a plan, and then
stops: `ApprovalRequired` becomes `True` and the phase is `AwaitingApproval`.

The operator has already connected to the database by this point — observing it
is where the plan came from, and that read is what the plan is the difference
against. What has not happened is the plan: not one statement it names has run,
and none will until the decision below is recorded.

Read the plan before approving it. The SQL is in the controller-owned
`PtahSchemaPlanChunk` objects the plan names rather than in the status, and
[`kubectl ptah`](../../use/read-a-plan/) reads it
back the way the operator does. It is a plugin you
[install once](../../use/read-a-plan/#install):

```sh
kubectl ptah plan application -n application
```

Without a selection that is the current plan, which is the one waiting for an
approval. Each statement is printed under the severity and reason the plan
records for it.

An approval names the schema with its UID, the plan with its UID and that
plan's fingerprint, and admission stamps who approved and when. `kubectl ptah
approve` shows the plan again with those identities, asks for a decision, and
creates the approval as you:

```sh
kubectl ptah approve schema/application -n application
```

It reads the schema once more before it sends anything. If a newer plan
replaced the one you read, or the operator is reading the database again and
admission would refuse an approval, it says so and approves nothing. A script
that reviewed the plan passes its fingerprint with `--plan-fingerprint`, and a
declarative workflow takes the approval as YAML with `--dry-run client` and
applies it from a repository.

Creating it requires a binding that grants the approval verbs, which the
chart's approver ClusterRole carries together with read access to the plan and
its chunks; `examples/approver-plan-reader-role.yaml` is the reader half alone.
[Exact-plan approvals](../../use/approvals/) is why the binding is separate
from the one that writes the desired state.

With the decision recorded the plan runs, and convergence is proved by a second
observation rather than by the Job finishing:

```sh
kubectl ptah schema application -n application
```

`InSync=True` with reason `ScopedConverged` confirms convergence.
`Applying=False` with reason `JobCompleted` says only that the Apply Job
finished, which is a different claim.

Watch the whole sequence, including this one, in
[the recorded runs](../../demo/).
