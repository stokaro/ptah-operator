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
| PostgreSQL | 18.x | `postgres:18-alpine@sha256:898c60cb390d1085bc97bdd6a1ae968757421df394b071848b52b1c09c89d6a5` |

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

That authority plans and applies what the login owns. It is enough to report what
another role added to the database, and not enough to change it. Owning the
database or a schema is more: PostgreSQL lets the owner drop what is in it, whoever
created that. [What the login cannot see](#what-the-login-cannot-see) says what
follows from each.

Grant extension, role, or cross-schema authority only when the desired artifact
actually manages those object kinds. Keep the login out of superuser and
cluster-wide administrative roles. Session advisory locking does not justify
broadening its DDL authority.

## MySQL authority

Use a dedicated account scoped to the one managed database. Grant the DDL verbs
the desired object kinds need, and the privileges that let the account see every
object a comparison reads. A typical table-and-index schema needs `SELECT`,
`CREATE`, `ALTER`, `DROP`, `INDEX`, and `REFERENCES` on that database, and every
schema needs `TRIGGER`, `EXECUTE`, and `SHOW VIEW` there as well:

```sql
GRANT SELECT, CREATE, ALTER, DROP, INDEX, REFERENCES, TRIGGER, EXECUTE, SHOW VIEW ON app.* TO 'ptah'@'%';
```

MySQL lists in `information_schema` only what an account holds a privilege on,
and a comparison reads an object it cannot see as absent. `TRIGGER` is the only
privilege that shows triggers, so it also lets the account create and drop them.
`EXECUTE` lets the read prove it listed every routine in the database; it also
lets the account call any routine there, including a `SQL SECURITY DEFINER` one
another account defines later. `SHOW VIEW` shows the definitions of views. Add
`CREATE VIEW`, `EVENT`, `CREATE ROUTINE`, or `ALTER ROUTINE` only if those
objects are managed.

Do not grant global administration, user-management, file, replication, or
grant-option privileges. `SHOW_ROUTINE` is the one global privilege the account
may need. `EXECUTE` does not show the body of a routine another account defined,
so where the managed database holds one, the read fails at exit 2 and Ptah's
message names the routine and `SHOW_ROUTINE`. Grant it then, and only then:

```sql
GRANT SHOW_ROUTINE ON *.* TO 'ptah'@'%';
```

`SHOW_ROUTINE` reads routine definitions in every database on the server and
writes nothing.

The operator rejects credential-bearing principal DDL and MySQL connection
parameters that can inject session SQL. `multiStatements` is always rejected.
Conventional IPv6 MySQL URLs must include an explicit port; the network DSN form
remains supported without one.

The privileges above decide which object kinds the account can see. Under the
Ptah v0.22.0 executor this build pins, a kind it cannot see stays out of the
drift report: [What the login cannot see](#what-the-login-cannot-see). Ptah
v0.23.0 refuses such a read instead, with `permission_denied`, and the operator
then says which kinds of object the account cannot see. With the grants above
both builds give the same answer, including the refusal of a routine another
account defined.

## What the login cannot see

The executor reads the database as the login in the target URL, and a comparison
reads what that login cannot see as not there. An empty `driftFindings` list
says the login found no difference. It does not say that nobody added anything.

This section describes Ptah v0.22.0, the executor this operator build pins
([Ptah compatibility](../ptah/)), measured with `schema drift` and `schema plan`,
the commands an Observe and a Plan run, against PostgreSQL 17.11 and MySQL
8.4.11. The acceptance row for it also runs on PostgreSQL 18. Another executor
build can answer differently. PostgreSQL has no gap at
this build, and
[stokaro/ptah#5497](https://github.com/stokaro/ptah/issues/5497) tracks MySQL.

### PostgreSQL

The executor reads a schema from `pg_catalog`, which lists every object to every
role, so the report does not depend on what the login holds. The login this page
describes gets the report a superuser gets, and a login that owns through a
no-login owner role reads the same:

| Created by another role | The login | The login with `pg_read_all_data` | A superuser |
| --- | --- | --- | --- |
| A table | `tables_removed` | `tables_removed` | `tables_removed` |
| That table's primary key | `constraints_removed` | `constraints_removed` | `constraints_removed` |
| A view | `views_removed` | `views_removed` | `views_removed` |
| A function | `functions_removed` | `functions_removed` | `functions_removed` |
| An index on a table the login owns | `indexes_removed` | `indexes_removed` | `indexes_removed` |
| A column on a table the login owns | `columns_removed` | `columns_removed` | `columns_removed` |
| A table in a schema the login holds no `USAGE` on | `tables_removed` | `tables_removed` | `tables_removed` |

A table the desired schema declares reads as present whoever owns it: no finding,
and the plan answers `no-changes`. Do not grant `pg_read_all_data` for this. It
changes nothing in the report, and it lets the login read the data of every table
in every database it can connect to.

The report also covers every schema the URL covers. With a URL that pins no
`search_path` that is the whole database, so the table in the last row counts as
drift, and the command warns on standard error that the plan drops it. Pin
`search_path` in the URL to leave other schemas out.

The plan drops what the declaration leaves out, and reading an object does not
give the login the authority to do that. For the login this page describes, a `DROP`
of a table, a view or a function another role owns fails with `must be owner of
table`, or of `view` or `function`, and an unknown outcome. For a table in a schema
without `USAGE` it fails with `permission denied for schema`, the same way. A `DROP
INDEX` on a table the login owns runs.

A login that owns the database or the schema can run that `DROP`, whatever privilege
it holds on the table: `has_table_privilege` reads false, the Apply succeeds, and the
table and its rows are gone. It takes `allowDestructive: true` and an approval, so
read what the plan drops before you give either to such a login.

Such a plan does not go away by itself. It is destructive, so the resource stays
blocked, and every later release of it plans the same `DROP` beside its own change:
a new column reads `[ADD COLUMN, DROP TABLE]`, and the whole plan stops. Choose one:

- A table that should stay, such as a monitoring agent's: list it in
  `spec.policy.exclude`. It takes a table, a view, a function or a sequence by name,
  a table in another schema as `other.t`, and a whole schema as `other.*`. The plan
  leaves it alone and the rest of the schema converges. `status.target.driftFindings`
  still counts it, because an Observe reads without exclusions.
- A table that should not exist: have its owner drop it.
- A table this schema should manage: suspend the resource, hand the table to the login
  with `ALTER TABLE ... OWNER TO`, and declare it before resuming.

A login with `USAGE` on no schema on its `search_path` has no current schema. The
read fails at exit 2 and says so, so an Observe fails rather than finding nothing.

### MySQL

MySQL has no catalog that lists every object to every account, so here the report
still depends on the privileges, as it did before this build. An account with
`SELECT` on the database sees its tables, views, indexes and constraints. Triggers
and routines take privileges of their own:

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
