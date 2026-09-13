---
title: Try it locally
description: From an empty machine to a table the operator created, and then to a column it added, in one command each.
---

This builds a cluster of its own, installs the operator into it, gives it a
database and a schema, and takes you from nothing to a table the operator
created. It touches nothing you already run.

Read it first if you want to know whether this operator fits before deciding
how to run it against your own database. [Install](install.md) is the other
direction: your cluster, your database, your registry.

## What you need

- Docker, local or remote. The lab builds images, so a machine with a few cores
  is much faster than a laptop.
- `kind`, `kubectl`, `helm` 4, `jq`, `git`, Go and Node.
- About 8 GB of memory and 20 GB of disk.
- A clone of this repository. The lab builds the operator from the commit you
  have checked out, so there is no release to choose and no digest to look up.

Measured on macOS against a remote Linux Docker daemon, and on Linux. Windows
is not verified: the scripts are POSIX shell throughout.

## One command to the cluster

```sh
git clone https://github.com/stokaro/ptah-operator
cd ptah-operator
make demo-up
```

That builds a four-node kind cluster on the newest Kubernetes release this
operator supports, an isolated OCI registry, a PostgreSQL of its own, and
installs the chart with the manager, runner and executor images pinned by
digest and the verified Ptah version supplied. You choose none of those: they
come from what this repository declares, which is the same pairing
[Ptah compatibility](../support/ptah.md) publishes.

Nothing here reaches your own clusters or databases. `make demo-down` removes
the cluster and the containers it created, and nothing else.

## One command to a table

Run the scenario you came for. It publishes a schema as an OCI artifact, points
a `PtahSchema` at its digest, and waits for the operator to resolve it, verify
it, observe the database, plan, apply and prove convergence:

```sh
go run ./demo/cmd/record -root . -only first-apply -output demo/.lab/first-apply.json
```

It prints each step as it runs and each check as it holds. Nothing is
published: the output goes to the lab directory, and the recording this
repository ships is left alone.

The database went from empty to holding the table:

```sh
demo/bin/lab psql '\dt'
```

Read what the operator actually ran. The SQL lives in a controller-owned
ConfigMap rather than in a status field or a log line:

```sh
NAMESPACE=$(demo/bin/lab namespace)
APPLIED=$(kubectl -n "$NAMESPACE" get ptahschema storefront \
  -o jsonpath='{.status.applied.planFingerprint}')
PLAN=$(kubectl -n "$NAMESPACE" get ptahschemaplan -o json \
  | jq -r --arg f "$APPLIED" '.items[] | select(.spec.fingerprint == $f) | .metadata.name')
CHUNK=$(kubectl -n "$NAMESPACE" get ptahschemaplan "$PLAN" \
  -o jsonpath='{.spec.chunks[0].name}')
kubectl -n "$NAMESPACE" get configmap "$CHUNK" \
  -o jsonpath='{.binaryData.chunk}' | base64 -d | jq -r '.statements[].sql'
```

The plan is found by the fingerprint the status recorded as applied, not by
picking the newest one. A converged schema carries no current plan, because
there is nothing left to do.

And the conditions, which are what the operator says rather than what a Job
did:

```sh
kubectl -n "$NAMESPACE" get ptahschema storefront \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'
```

`InSync=True` with reason `ScopedConverged` is convergence. `Applying=False`
with reason `JobCompleted` beside it is a different claim: the Job finished.

## And to a column it added

```sh
go run ./demo/cmd/record -root . -only schema-update -output demo/.lab/schema-update.json
```

That publishes a revision adding a column and a table, points the same resource
at the new digest, and converges again. The plan it derives is an `ALTER` and a
`CREATE` against the database as it actually is, not a second `CREATE TABLE` of
what is already there.

Seven more scenarios show what the operator refuses and what it does when
something goes wrong: a destructive change the policy stops, a plan waiting for
a person to approve it, an approval that has been used and cannot be used
again, drift closed without being asked, a database that is not there, and a
reconciliation suspended and resumed. `demo/scenarios/` is all of them, and

```sh
make demo-record
```

runs every one against the lab. It takes about twenty minutes and leaves the
cluster in whatever state the last scenario wanted, so run it when you want the
whole set rather than when you want to look at one.

## Reading it without running it

Every one of those sessions is recorded, checked while it runs, and published:
[recorded runs](../../demo/). The transcripts are what the commands printed on
a real cluster, and each is published only because every condition its scenario
claims held.

## When you are ready to use it for real

```sh
make demo-down
```

Then [Install](install.md) for your own cluster, [First schema](first-schema.md)
for the resource and the approval, and [Security model](../use/security.md) for
what the operator is and is not allowed to read.

The lab installs a verified combination because this repository declares one.
Doing the same against your own cluster means supplying the manager, runner and
executor digests and the Ptah version yourself, until a published release
carries them.
