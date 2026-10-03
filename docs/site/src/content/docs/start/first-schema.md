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
approval. `-o json` gives the stored plan document, if you want the severity
the planner recorded beside each statement:

```sh
kubectl ptah plan application -n application -o json \
  | jq -r '.statements[] | "\(.severity)\t\(.sql)"'
```

An approval names the schema, the plan and that plan's fingerprint. Everything
else on it is stamped by the admission webhook from the plan being approved,
which is why this creates it from what the API server already holds rather than
from values copied by hand:

```sh
kubectl -n application get ptahschema application -o json > schema.json
kubectl -n application get ptahschemaplan \
  "$(jq -r .status.plan.name schema.json)" -o json > plan.json
jq -n --slurpfile schema schema.json --slurpfile plan plan.json '
  {
    apiVersion: "operator.ptah.run/v1alpha1", kind: "PtahSchemaApproval",
    metadata: {name: "application-first"},
    spec: {
      schemaRef: {name: $schema[0].metadata.name, uid: $schema[0].metadata.uid},
      planRef:   {name: $plan[0].metadata.name,   uid: $plan[0].metadata.uid},
      planFingerprint: $plan[0].spec.fingerprint
    }
  }' | kubectl -n application create -f -
```

Creating it requires a binding that grants the approval verbs, which the
chart's approver ClusterRole carries together with read access to the plan and
its chunks; `examples/approver-plan-reader-role.yaml` is the reader half alone.
[Exact-plan approvals](../../use/approvals/) is why the binding is separate
from the one that writes the desired state.

With the decision recorded the plan runs, and convergence is proved by a second
observation rather than by the Job finishing:

```sh
kubectl -n application get ptahschema application \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'
```

`InSync=True` with reason `ScopedConverged` confirms convergence.
`Applying=False` with reason `JobCompleted` says only that the Apply Job
finished, which is a different claim.

Watch the whole sequence, including this one, in
[the recorded runs](../../demo/).
