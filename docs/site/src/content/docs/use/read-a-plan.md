---
title: Read a plan
description: See the SQL the operator would run, or the SQL the last apply ran, with one read-only command.
---

The SQL the operator applies is not in a status field or a log line. It is a
document the operator publishes into immutable ConfigMaps, bound to a
`PtahSchemaPlan` by index, key, size and digest, and read back only after every
one of those bindings has held.

`kubectl ptah` is how you read it. It is a read-only client: it creates
nothing, changes nothing, starts no Job, and never connects to your database.

```sh
kubectl ptah plan storefront -n application
```

## Install it {#install}

`kubectl` runs any executable named `kubectl-<verb>` on your `PATH` as
`kubectl <verb>`, so installing the plugin is putting one file there.

Each release publishes a binary for the supported client platforms and a
`SHA256SUMS` beside them:

```sh
version=<release tag>
platform=darwin-arm64   # or linux-amd64, linux-arm64, darwin-amd64
base=https://github.com/stokaro/ptah-operator/releases/download/$version

curl -fsSLO "$base/kubectl-ptah-$platform"
curl -fsSLO "$base/SHA256SUMS"
shasum -a 256 --check --ignore-missing SHA256SUMS

install -m 0755 "kubectl-ptah-$platform" /usr/local/bin/kubectl-ptah
kubectl ptah --version
```

`kubectl plugin list` shows it once it is on `PATH`. It needs nothing else: no
checkout of this repository, no Helm release on your machine, and no files
beside it.

## Which plan

Current and applied are two different plans, and neither stands in for the
other.

| Flag | What it reads |
| --- | --- |
| `--current` (the default) | the plan the operator would run next, which is what `status.plan` names |
| `--applied` | the plan the last confirmed apply ran, which is what `status.applied` records |

A converged schema usually has no current plan, because there is nothing left
to do; a schema that has never applied anything has no applied plan. Either way
the command says so and exits with status 3 rather than printing empty SQL.

`--applied` shows the plan that ran. It is not a log of what each statement did
in the database, and it does not read the database again.

## What comes out

Without `-o` you get what the plan is, and then the SQL it holds:

```sh
kubectl ptah plan storefront --applied -n application
```

```text
Schema:         application/storefront
Plan:           applied (ptah-plan-4b44084123f0958600629632)
Fingerprint:    sha256:4b44084123f095860062963255f9f84c2921946524e187186eca71ca88cc366e
Content digest: sha256:99a821c0f52b1eb6d643633d40a7db7467b81e10a3be16982626807ef6c692ad
Dialect:        postgres
Statements:     1
Destructive:    false
Stored:         2026-09-13T08:04:05Z
Applied:        2026-09-13T08:04:24Z

-- POSTGRES TABLE: customers --
CREATE TABLE "customers" (
  "id" bigint PRIMARY KEY NOT NULL,
  "email" text NOT NULL
);
```

The statements come out as the planner wrote them, comments and all.

`-o sql` prints the statements alone, in the order the plan holds them, each
terminated once. Nothing is re-split on a semicolon: a statement may carry one
inside a string, a comment or a function body.

`-o json` prints the stored plan document byte for byte, so what you save still
matches the content digest printed above it. It is the executable plan, not the
Kubernetes object:

```sh
kubectl ptah plan storefront --applied -n application -o json > plan.json
```

Diagnostics go to standard error. Nothing reaches standard output until every
check has held, so a plan whose last chunk is corrupt leaves you with an error
rather than half its SQL.

Exit status: `0` printed, `1` could not read or verify, `2` the command line,
`3` nothing stored to print.

## What it needs to be allowed to do

Reading a plan is reading three kinds of object in one namespace. No Secret, no
Pod log, no `pods/exec`, no Job, and nothing cluster-wide:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ptah-plan-reader
  namespace: application
rules:
  - apiGroups: [operator.ptah.run]
    resources: [ptahschemas, ptahschemaplans]
    verbs: [get]
  - apiGroups: [""]
    resources: [configmaps]
    verbs: [get]
```

Add `list` on `ptahschemaplans` for a schema whose last apply predates the
`status.applied.planRef` field: without the reference the command has to find
the plan by the fingerprint the record does carry, and finding it means listing
the namespace's plans. It refuses to guess -- no match is an absence, and more
than one match is reported rather than resolved by taking the first.

:::caution
The command reads the plan chunks as you, so granting it means granting
`get` on those ConfigMaps. ConfigMaps are not secret and base64 is not
protection: anyone who can read them can read the SQL, with or without this
plugin. That is why the database URL is in a Secret and the plan is not.
:::

## How the plan is stored

Worth knowing when you are diagnosing the store rather than reading a plan.

A plan document is split into 512 KiB chunks by bytes, up to 8 MiB in total.
The split is of the serialized document, so a boundary falls wherever 512 KiB
falls -- possibly inside a SQL string, inside a JSON escape, or inside a
multi-byte character. Each chunk is an immutable ConfigMap; the plan's
`spec.chunks` binds every one by name, key, index, size and digest, and
`spec.contentDigest` binds the whole document.

Reading one chunk on its own is therefore not reading a plan, and decoding
chunks by hand is reproducing bindings that already exist. The command does
what the operator does: read every chunk, check each against its binding,
concatenate in index order, check the whole against the content digest, and
only then parse.
