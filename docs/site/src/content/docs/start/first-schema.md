---
title: First schema
description: One worked example, from a Secret and a ConfigMap to a converged PtahSchema, with the move to a registry as the next step.
---

This page takes an installed operator to a table in your database. The schema
is a file in a ConfigMap, so it needs no registry and no verification policy.
[Read the schema from a registry](#from-a-registry) is the next step, and it is
what a production installation should read before it chooses a source.

The page walks the path
[From source to verified change](../../reference/reconciliation/#from-source-to-verified-change)
draws, with `kubectl` and `kubectl ptah` alone, and
[Stage by stage](#stage-by-stage) lists what to run and expect at each stage.
[Ptah Console](https://github.com/stokaro/ptah-console) is optional, and no step
here needs it.

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

If the URL lives in Vault or another store, a tool can write this Secret instead:
[Credentials from an external secret store](../../reference/credentials-and-admission/#external-secret-stores)
has a tested path and what a rotation does.

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

## Read what happened {#history}

You can read this at any stage, and reading it changes nothing:

```sh
kubectl -n application get ptahschema application -o jsonpath='{.status.journal}' | jq
kubectl ptah plan application --applied -n application
kubectl events -n application --for ptahschema/application
```

The journal is the resource's own record. After the run above it holds
`PlanPublished`, `ApprovalAccepted`, `ApplyFinished` with outcome `Succeeded`, and
`Verified` with outcome `InSync`, each with its time. A plan that was replaced
before anybody approved it stays there as a `PlanPublished` with nothing after
it. `--applied` prints the plan that Apply ran; without the flag the command
exits 3, because a converged schema has no current plan. The Events and the
finished Jobs go first, and the journal is bounded too:
[The lifecycle journal](../../reference/reconciliation/#the-lifecycle-journal)
says how far back each one reaches.

## Stage by stage {#stage-by-stage}

The same run, against the stages of
[From source to verified change](../../reference/reconciliation/#from-source-to-verified-change).
`kubectl ptah schema application -n application` shows the phase and every
condition below, at any stage.

| Stage | Run | Expect | Then |
| --- | --- | --- | --- |
| 1 Source | The commands in [Create what the schema reads](#create-what-the-schema-reads) | `ArtifactResolved=True`, reason `SnapshotTaken` | Wait for the plan |
| 2 Plan | `kubectl -n application wait ptahschema/application --for=condition=ApprovalRequired --timeout=5m`, then `kubectl ptah plan application -n application` | Phase `AwaitingApproval`, `PlanReady=True`, reason `Published`, and each statement with its severity | Decide |
| 3 Decision | `kubectl ptah approve schema/application -n application --wait` | `Approved:`, with the identity admission stamped | The command stays for stages 4 and 5 |
| 4 Execution | Nothing more. To watch from elsewhere, `kubectl -n application get ptahschema application -w` | Phase `Applying`, then `VerifyingConvergence` | The command ends with `reached applied` |
| 5 Verification | `kubectl ptah schema application -n application` | Phase `InSync`, `InSync=True`, reason `ScopedConverged` | Read what happened |
| 6 History | The commands in [Read what happened](#history) | The journal entries named there, and the approval with `Consumed=True`, reason `DispatchCommitted` | Make [the next version](#the-next-version) |

A stage can go another way:

- At stage 2, phase `InSync` with no plan means the database already matches, and
  the `kubectl wait` times out. There is nothing to approve.
- At stage 2, [`Blocked`](../../reference/reconciliation/#blocked-is-a-refusal-not-a-fault)
  means something refuses the plan. `kubectl ptah schema` says what, and nothing
  runs until an input changes.
- At stage 3, an `approve` that approves nothing found the plan replaced, or being
  replaced. Run it again, and it shows the current one.
- At stage 4, exit status 7 means the Apply has not ended yet and 8 that it will
  not end applied. Both say what holds the schema, and
  [Failure recovery](../../use/operations/#failure-recovery) has the rest.

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
