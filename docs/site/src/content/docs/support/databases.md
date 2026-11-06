---
title: Databases and privileges
description: The supported engines and the least authority each one needs.
---

The initial operator support contract is deliberately narrower than every
database that the Ptah CLI can address. A database release line is supported by
the operator only when the complete OCI, observation, planning, approval,
Apply, failure-recovery, and convergence lifecycle is green on every supported
Kubernetes minor.

| Engine | Supported release line | Matrix image |
| --- | --- | --- |
| PostgreSQL | 17.x | `postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73` |
| MySQL | 8.4.x LTS | `mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb` |

The immutable references identify the current matrix environments; they do
not restrict production to one image vendor. Other release lines are
unsupported. `support/databases.json` declares a line that is being added, and
the `Added database lines` workflow runs that same lifecycle on it, on a
schedule and before a release, on every supported Kubernetes minor. A line joins
this table only after that run is green on it. The CRD selects an engine but
intentionally does not trust a user-supplied server version. The executor
discovers server capabilities from the live connection and refuses unsupported
DDL instead of rendering for an asserted version.

Database support is also scoped to the Ptah executor build exercised by the
matrix. Installation requires both its digest-pinned image and a version
identity verified from that image's provenance: the chart a release publishes
carries that release's pair, and any other chart takes both as explicit values.
No chart can silently assign an unrelated version to a different digest; both
values remain part of every plan and applied binding.
Which build that is, and what has actually been run against it, is
[Ptah compatibility](../ptah/).

## PostgreSQL authority

Use a dedicated login that is the owner of the managed objects, or grant it
membership in a dedicated no-login owner role. PostgreSQL does not provide a
general `ALTER` privilege for objects owned by another role, so object
ownership is the reliable least-authority boundary.

The login needs only:

- `CONNECT` on the target database;
- `USAGE` on each managed schema;
- the ability to create objects in each managed schema;
- ownership of every object the desired schema may alter or drop.

That authority plans and applies what the login owns. It does not let the login
read everything in the database, and a table it cannot read stays out of the
drift report: [What the login cannot see](#what-the-login-cannot-see).

Grant extension, role, or cross-schema authority only when the desired artifact
actually manages those object kinds. Keep the login out of superuser and
cluster-wide administrative roles. Session advisory locking does not justify
broadening its DDL authority.

## MySQL authority

Use a dedicated account scoped to the one managed database. Grant only the DDL
verbs required by the desired object kinds. A typical table-and-index schema
needs `SELECT`, `CREATE`, `ALTER`, `DROP`, `INDEX`, and `REFERENCES` on that
database. Add `CREATE VIEW` and `SHOW VIEW`, `TRIGGER`, `EVENT`, or routine
privileges only if those objects are managed.

Do not grant global administration, user-management, file, replication, or
grant-option privileges. The operator rejects credential-bearing principal DDL
and MySQL connection parameters that can inject session SQL. `multiStatements`
is always rejected. Conventional IPv6 MySQL URLs must include an explicit port;
the network DSN form remains supported without one.

The privileges above decide which object kinds the account can see, and a kind
it cannot see stays out of the drift report:
[What the login cannot see](#what-the-login-cannot-see).

## What the login cannot see

The executor reads the database as the login in the target URL, and a comparison
reads what that login cannot see as not there. An empty `driftFindings` list
says the login found no difference. It does not say that nobody added anything.

This section describes Ptah v0.18.0, the executor this operator build pins
([Ptah compatibility](../ptah/)), measured with `schema drift` and `schema plan`,
the commands an Observe and a Plan run, against PostgreSQL 17.11 and MySQL
8.4.11. Another executor build can answer differently.
[stokaro/ptah-operator#792](https://github.com/stokaro/ptah-operator/issues/792)
tracks PostgreSQL, and
[stokaro/ptah#5497](https://github.com/stokaro/ptah/issues/5497) tracks MySQL.

### PostgreSQL

The executor reads tables, columns and constraints from `information_schema`,
which lists only what the connecting role holds a privilege on. The login this
page describes holds nothing on a table another role created. When that table
is the only difference, `schema drift` exits 0 with `"drift": false` and
`schema plan` answers `no-changes`, which is what a resource needs to stay
`InSync`. A login that owns through a no-login owner role reads the same.

| Created by another role | The login | The login with `pg_read_all_data` | A superuser |
| --- | --- | --- | --- |
| A table | not reported | `tables_removed` | `tables_removed` |
| That table's primary key | not reported | not reported | `constraints_removed` |
| A view | `views_removed` | `views_removed` | `views_removed` |
| A function | `functions_removed` | `functions_removed` | `functions_removed` |
| An index on a table the login owns | `indexes_removed` | `indexes_removed` | `indexes_removed` |

A change to a table the login owns is reported whoever made it. The same gap
covers a table in a schema the login holds no `USAGE` on. A table the desired
schema declares reads as missing when the login holds nothing on it: the report
counts `tables_added`, rated safe, the plan says `CREATE TABLE`, and its Apply
fails because the relation already exists, with an unknown outcome.

Granting `pg_read_all_data` brings the table into the report and leaves its
constraints out, because `information_schema.table_constraints` does not count
`SELECT`. The plan then holds a `DROP TABLE` the login cannot run: its Apply
fails with `must be owner of table` and an unknown outcome. The role also lets
the login read the data of every table in every database it can connect to. It
does not close this gap, so do not grant it for this.

Until the pinned executor changes, check for such a table yourself. Run this as
the login. `pg_catalog` lists every table to every role, so it needs no grant:

```sql
SELECT n.nspname, c.relname, pg_get_userbyid(c.relowner)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p')
  AND n.nspname <> 'information_schema'
  AND n.nspname NOT LIKE 'pg\_%'
  AND NOT pg_has_role(c.relowner, 'USAGE')
ORDER BY 1, 2;
```

Each row is a table the login does not own, which covers every table the report
can miss. Have its owner drop one that should not exist. For one that should
stay, suspend the resource, hand the table to the login with
`ALTER TABLE ... OWNER TO`, and declare it in the desired schema before resuming:
the plan drops what the declaration leaves out, and the login can now run that.

A superuser running `ptah schema drift` at the pinned digest gets the whole
report. Keep that role out of the resource, because an Apply runs with the
authority of the login.

### MySQL

An account with `SELECT` on the database sees its tables, views, indexes and
constraints. Triggers and routines take privileges of their own:

| Account, on the managed database unless noted | Not reported |
| --- | --- |
| `SELECT`, `CREATE`, `ALTER`, `DROP`, `INDEX`, `REFERENCES` | triggers, functions, procedures |
| The above with `TRIGGER` | functions, procedures |
| The above with `SHOW_ROUTINE`, granted globally | none of these |
| `root` | none of these |

When a trigger or a routine is the only difference, `schema drift` exits 0 with
`"drift": false` and `schema plan` answers `no-changes`. `CREATE ROUTINE` and
`ALTER ROUTINE` on the database show a routine without its body when another
account defined it: the read then fails at exit 2, naming `SHOW_ROUTINE`, so an
Observe fails too. An account with table-level grants does not see a table it
holds nothing on. A hand-made event is not reported under any account, `root`
included.

Until the pinned executor changes, add `TRIGGER` on the database and
`SHOW_ROUTINE` globally where a hand-made trigger or routine has to show up.
`TRIGGER` lets the account create and drop triggers, and `SHOW_ROUTINE` reads
routine definitions in every database. To keep the account as it is, run
`ptah schema drift` at the pinned digest as an account that holds them.

## Proving a custom privilege set

Privilege requirements depend on the exact desired object kinds. Validate a
custom role against a non-production database with the same engine release and
artifact before using it in production. The proof must include observation,
plan dry-run, one representative Apply, and a subsequent no-change plan. A
permission error after Apply dispatch is outcome-unknown and requires the
normal read-only convergence proof; it is not safe evidence that nothing
changed.
