---
title: First schema
description: One worked example, from a Secret and a ConfigMap to a converged PtahSchema, with the move to a registry as the next step.
---

This page takes an installed operator to a table in your database. The schema
is a file in a ConfigMap, so it needs no registry and no verification policy.
[Read the schema from a registry](#from-a-registry) is the next step, and it is
what a production installation should read before it chooses a source.

## Before you start

- The operator installed and its CRDs established: [Install](../install/), and
  its readiness check.
- A namespace for your own resources. This page uses `application`; the chart
  does not create it, and it is not the release namespace.
- A database the operator can reach, and a URL for it. Use a login that is not
  a superuser. On PostgreSQL this page needs `CONNECT` on the database and
  `USAGE` and `CREATE` on the schema, and the tables it creates belong to that
  login. [Databases and privileges](../../support/databases/) has the rest.
- `kubectl ptah`, the plugin that reads the plan and records your decision. You
  [install it once](../../use/read-a-plan/#install).

## Create what the schema reads

A `PtahSchema` reads the database URL from a Secret and the desired schema from
a ConfigMap, both in its own namespace. Store the URL in a private file with no
trailing newline, outside the repository. Set `DATABASE_URL_FILE` to its path.
The command below reads that file so the credential does not become a command
argument or shell-history entry:

```sh
DATABASE_URL_FILE=/path/to/private/database-url
kubectl create namespace application
kubectl -n application create secret generic application-database \
  --from-file=url="$DATABASE_URL_FILE"
```

The schema is the state you want the database to reach, not a list of changes.
The file's extension tells Ptah its format:

```sh
cat > schema.sql <<'SQL'
CREATE TABLE customers (
  id BIGINT PRIMARY KEY,
  email VARCHAR(320) NOT NULL
);
SQL
kubectl -n application create configmap application-schema \
  --from-file=schema.sql=schema.sql
```

The resource names the database and the file. Every resource in this namespace
that can change this database uses the same `coordinationKey`, which is a label
you choose and holds no secret. Apply it and wait for the plan:

```sh
kubectl apply -f - <<'YAML'
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
    configMap:
      name: application-schema
      items:
        - key: schema.sql
          path: schema.sql
  policy:
    apply: OnApproval
YAML
kubectl -n application wait ptahschema/application \
  --for=condition=ApprovalRequired --timeout=5m
```

For MySQL, use a MySQL database URL in the Secret and set `engine` to `MySQL`.
Everything else on this page is the same. Choose one engine for this resource;
[shared realms](../../reference/ptahrealm/) cover more than one resource writing
to the same database.

What the schema reports while it converges is
[the status progression](../../use/operations/#normal-status-progression), and
what each reason means is
[condition reasons](../../troubleshoot/condition-reasons/).

## It stops, and waits for you

The manifest sets `apply: OnApproval`, which is also the default. The operator
copies the file into a snapshot, observes the database and publishes a plan, and
then stops: `ApprovalRequired` becomes `True` and the phase is `AwaitingApproval`.

The operator has already connected to the database by this point — observing it
is where the plan came from, and that read is what the plan is the difference
against. What has not happened is the plan: not one statement it names has run,
and none will until the decision below is recorded.

Read the plan before approving it. The SQL is in the controller-owned
`PtahSchemaPlanChunk` objects the plan names rather than in the status, and
[`kubectl ptah`](../../use/read-a-plan/) reads it back the way the operator does:

```sh
kubectl ptah plan application -n application
```

Without a selection that is the current plan, which is the one waiting for an
approval. Each statement is printed under the severity and reason the plan
records for it.

An approval names the schema with its UID, the plan with its UID and that
plan's fingerprint, and admission stamps who approved and when. `kubectl ptah
approve` shows the plan again with those identities, asks for a decision, and
creates the approval as you. `--wait` keeps it there until the plan has run:

```sh
kubectl ptah approve schema/application -n application --wait
```

It reads the schema once more before it sends anything. If a newer plan
replaced the one you read, it says so and approves nothing. The operator reads
the database again at its interval, and admission refuses an approval while it
does; an answer given then is sent when that read ends, if the same plan is
waiting again. A script that reviewed the plan passes its fingerprint with
`--plan-fingerprint`.
`--dry-run client` prints the approval as YAML instead, to keep in a
repository and create later with `kubectl create -f`. Admission records
whoever creates it as the approver, so create it as the person who decided:
a controller that applies it is recorded in their place.

Creating it requires a binding that grants the approval verbs, which the
chart's approver ClusterRole carries together with read access to the plan and
its chunks; `examples/approver-plan-reader-role.yaml` is the reader half alone.
[Exact-plan approvals](../../use/approvals/) is why the binding is separate
from the one that writes the desired state.

With the decision recorded the plan runs, and convergence is proved by a second
observation rather than by the Job finishing. That is what `--wait` waits for:
an Apply of the plan you approved, recorded after the approval, and the
operator's reading of the database afterwards. If that takes longer than five
minutes, or the Apply fails, it says which, with the phase and the condition
holding the schema; [Waiting for the operator](../../use/operations/#wait) has
the rest. Then read where the schema stands:

```sh
kubectl ptah schema application -n application
```

`InSync=True` with reason `ScopedConverged` confirms convergence.
`Applying=False` with reason `JobCompleted` says only that the Apply Job
finished, which is a different claim.

Watch the whole sequence, including this one, in
[the recorded runs](../../demo/).

## The next version

A change to the schema is a change to the file. Edit it, replace the ConfigMap,
and review the plan it leads to:

```sh
cat > schema.sql <<'SQL'
CREATE TABLE customers (
  id BIGINT PRIMARY KEY,
  email VARCHAR(320) NOT NULL,
  name VARCHAR(200)
);
SQL
kubectl -n application create configmap application-schema \
  --from-file=schema.sql=schema.sql --dry-run=client -o yaml |
  kubectl replace -f -
kubectl -n application wait ptahschema/application \
  --for=condition=ApprovalRequired --timeout=5m
kubectl ptah approve schema/application -n application --wait
```

The operator reads an edit to a selected key at once, and the plan is the
difference between the new file and the database as it is now.
[Deliver from a ConfigMap or Git](../../use/sources/#from-a-configmap) covers
generated names, Kustomize and Helm.

## Next: read the schema from a registry {#from-a-registry}

A ConfigMap is trusted by Kubernetes RBAC: whoever may write it may change the
desired state. That is enough to see the operator work, and it suits a team
whose manifests already carry the files. An OCI artifact adds what a production
installation usually wants: content that cannot change under a digest, a tag
resolved once and never again, and a verification policy that decides which
artifacts the operator reads at all.
[Deliver from a ConfigMap or Git](../../use/sources/) says how to choose.

This step needs a registry the cluster can read and the Ptah CLI. Which Ptah
build to use is [Ptah compatibility](../../support/ptah/). The `examples/` files
named below come from the version you chose on
[Install](../install/#choose-a-version-once). They are in the repository at that
commit or tag, beside the chart, and an example from a different version can
name a field this one does not have.

Publish the same file as an artifact. The example policy below refuses an
artifact that does not say where it came from. A push from GitHub Actions records
that from the runner's environment; anywhere else, name the repository and the
commit yourself:

```sh
export PTAH_OCI_SOURCE=https://github.com/example/application
export PTAH_OCI_REVISION="$(git rev-parse HEAD)"
ptah schema push oci://ghcr.io/example/application-schema:1.0.0 \
  --schema-file schema.sql --dialect postgres
```

Create the verification policy, which is a ConfigMap beside the schema:

```sh
kubectl -n application create configmap ptah-verification-policy \
  --from-file=policy.yaml=examples/verification-policy.yaml
kubectl -n application patch configmap ptah-verification-policy \
  --type=merge -p '{"immutable":true}'
```

Replace the artifact placeholder in `examples/ptahschema.yaml` with the digest
the push printed, and apply it. It names the same resource, so this moves the
schema you already have from the ConfigMap to the artifact:

```sh
kubectl apply -f examples/ptahschema.yaml
kubectl -n application get ptahschema application -w
```

The artifact holds what the database already has, so the operator reports
`InSync` again with no plan to approve.

For MySQL, publish with `ptah schema push --dialect mysql`, replace the artifact
placeholder in `examples/ptahschema-mysql.yaml`, and apply that manifest instead:

```sh
kubectl apply -f examples/ptahschema-mysql.yaml
kubectl -n application get ptahschema application -w
```

Both examples name the resource `application`, so the commands on this page work
for either engine. The MySQL example manages a database of its own; see
[shared realms](../../reference/ptahrealm/) before adding another resource that
writes to the same database.

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

From here a change to the schema is a new artifact. Publish it with
`--format reference` added, point the schema at it, and review the plan it leads
to:

```sh
REF=$(ptah schema push oci://ghcr.io/example/application-schema:1.5.0 \
  --schema-file schema.sql --dialect postgres --format reference)
kubectl ptah set-source schema/application -n application --oci "$REF" --for approval
kubectl ptah approve schema/application -n application --wait
```

`--format reference` prints the reference of the artifact the push wrote,
pinned by its digest, which is the only kind of reference `set-source` takes.
It came in Ptah v0.17.0; an older push prints the digest in its receipt
instead.
It changes `spec.desired.ociRef` and nothing else: the verification policy you
created above holds the new artifact too. `--for approval` waits until the
operator has planned from it, and the plan is the difference between the new
schema and the database as it is now. With `approvals.requireDistinctApprover`
on, someone else approves it, since `set-source` records you as the schema's
last spec writer. [Moving to a new artifact](../../use/operations/#set-source)
has what `set-source` refuses and the same change made with `kubectl edit`.
