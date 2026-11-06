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
each scenario in its own namespace and database, on November 6, 2026. The
[comparison on the blog](https://blog.ptah.run/posts/ptah-operator-vs-schemahero/)
has the commands, the manifests and the recorded output for both sides.

## What we measured

| What we tested | SchemaHero 0.26.2 | Ptah Operator 0.2.9 |
| --- | --- | --- |
| The database changed after review | Ran the old SQL, failed 32 times in three minutes, then applied it unattended once it could | Refused before any statement ran and planned again |
| The spec changed again before approval | Ran the replaced plan, so the database no longer matched the spec | Refused the old fingerprint |
| Unattended removal of a column with data | Dropped the column and its data | Blocked the plan and kept the data |
| A hand edit to the database | No migration and no log line in five minutes | Reported in 78 seconds, by category, with a one-minute interval |
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
- [Versioned migrations](../../use/migrations/) through `PtahMigration`, for a
  team that keeps a migration directory.
- [Immutable delivery](../../use/sources/) from an OCI artifact by digest, as well
  as from a ConfigMap or Git.
- [Ptah Console](https://github.com/stokaro/ptah-console), an optional web console
  for approvers, developers, people on call and auditors.

## Where SchemaHero is simpler

`kubectl schemahero install` is one command with no values. A published release
of this operator takes [four values from its release manifest](../install/), and
making that one command is
[tracked](https://github.com/stokaro/ptah-operator/issues/789).

SchemaHero's `Database` resource has connection types for PostgreSQL, MySQL,
CockroachDB, TimescaleDB, SQLite, RQLite and Cassandra. We ran only PostgreSQL,
so we state that list from its API. This operator supports
[PostgreSQL 17 and MySQL 8.4](../../support/databases/), and
[widening that](https://github.com/stokaro/ptah-operator/issues/784) is tracked.
If your engine is outside this operator's list, SchemaHero may be your only
option of the two.

## Next

Run [Try it locally](../try-it/) to see the same behavior on a cluster of its own,
or start with [First schema](../first-schema/) on your own.
