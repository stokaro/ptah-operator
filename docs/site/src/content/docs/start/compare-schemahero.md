---
title: Compared with SchemaHero
description: How Ptah Operator and SchemaHero differ when a database moves after review, a statement fails, or a change would remove data, measured on one cluster.
---

Ptah Operator and SchemaHero both let a team declare a database schema as
Kubernetes resources and have a controller change the database. Choose between
them by what must hold between a reviewed plan and the statement that runs.

Ptah Operator binds an approval to the state of the database and refuses to run
when that state has moved. SchemaHero binds an approval to the text of the SQL
and runs that text. Most of the differences below follow from that.

We build Ptah Operator, so check the evidence. We ran released Ptah Operator
0.2.9 and SchemaHero 0.26.2 on one Kubernetes 1.37 cluster with PostgreSQL 17.11,
each scenario in its own namespace and database, on November 6, 2026. Both
connected as an [ordinary login](../../support/databases/#postgresql-authority), and
hand edits ran as a separate superuser. The
[comparison on the blog](https://blog.ptah.run/posts/ptah-operator-vs-schemahero/)
has the commands, the manifests and the recorded output for both sides.

## Where the SQL lives

SchemaHero generates SQL from a `Table` manifest and stores it in a `Migration`
object in the cluster, so the reviewed SQL does not reach your repository.
[`PtahMigration`](../../use/migrations/) takes numbered SQL files from a ConfigMap,
Git or an OCI artifact, and the database records each version it applied in
`schema_migrations`. After we deleted a cluster and applied the same manifests to a
new one, the operator read that history and ran nothing again. SchemaHero had no
migrations and no record in the database.

Approvals are Kubernetes objects, so who approved is lost with the cluster in both
products. Keeping that in the database is
[tracked](https://github.com/stokaro/ptah-operator/issues/800). `PtahSchema` also
computes its SQL and keeps its plans in the cluster.

## What we measured

| What we tested | SchemaHero 0.26.2 | Ptah Operator 0.2.9 |
| --- | --- | --- |
| Where the reviewed SQL lives | In a `Migration` object in the cluster | In your repository, with `PtahMigration` |
| The cluster is lost | No SQL and no record of what ran | Files in the repository, history in the database, and the next plan starts from where the database stands |
| Renaming a column with data | Lost the data and stopped halfway | A reviewed `RENAME COLUMN` kept every row |
| The database changed after review | Ran the old SQL, failed 32 times in three minutes, then applied it unattended once it could | Refused before any statement ran and planned again |
| The spec changed again before approval | Ran the replaced plan, so the database no longer matched the spec | Refused the old fingerprint |
| Unattended removal of a column with data | Dropped the column and its data | Blocked the plan and kept the data |
| A hand edit to the database | No migration and no log line in five minutes | Reported in 81 seconds, by category, with a one-minute interval, except a table the login cannot read ([tracked](https://github.com/stokaro/ptah-operator/issues/792)) |
| A statement that fails | Retried 30 times in two minutes, then ran unattended once it could | Warned in the plan, did not retry, and required a new approval |
| Who approved | Recorded any name passed to `--actor` | Recorded the Kubernetes identity, and can refuse the author |
| PostgreSQL objects in one desired schema | No fields for check constraints, partial or expression indexes, enums, generated columns, row-level security, policies, comments or partitioning | All of these created and converged, with a partitioned parent table only: a schema file cannot yet declare `PARTITION OF` ([tracked](https://github.com/stokaro/ptah/issues/5506)) |
| A GIN index | `type: gin` built a B-tree | Built a GIN index |
| Adding a column beside a composite foreign key | Also dropped and recreated the key | One `ADD COLUMN` |

These are the results of one run with PostgreSQL. They show what each product
did in that scenario, and they are not a statement about every workload.

## What only Ptah Operator has

- [Exact-plan approvals](../../use/approvals/): an approval is a Kubernetes
  object that names one plan, and admission records who created it.
  `approvals.requireDistinctApprover` refuses the author of the change.
- A guard on unattended apply: `applyPolicyGuard` keeps `apply: Always` with the
  groups you name, and a plan that changes privileges waits for an approval
  even then.
- [Immutable delivery](../../use/sources/) from an OCI artifact by digest, as well
  as from a ConfigMap or Git.
- [Ptah Console](https://github.com/stokaro/ptah-console), an optional web console
  for approvers, developers, people on call and auditors.

## Where SchemaHero is simpler

`kubectl schemahero install` is one command with no values. From v0.2.10 a published
release of this operator is [one `helm` command with no values](../install/), with
every image still pinned by digest, and a release before that takes four values
from its release manifest. An install command in the plugin is
[tracked](https://github.com/stokaro/ptah-operator/issues/789).

SchemaHero's `Database` resource has connection types for PostgreSQL, MySQL,
CockroachDB, TimescaleDB, SQLite, RQLite and Cassandra. We ran only PostgreSQL,
so we state that list from its API. This operator supports
[PostgreSQL 17 and 18 and MySQL 8.4](../../support/databases/), and
[widening that](https://github.com/stokaro/ptah-operator/issues/784) is tracked.
If your engine is outside this operator's list, SchemaHero may be your only
option of the two.

## Next

Run [Try it locally](../try-it/) to see the same behavior on a cluster of its own,
or start with [First schema](../first-schema/) on your own.
