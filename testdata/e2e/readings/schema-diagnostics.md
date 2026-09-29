# Schema diagnostic SQL readings

These are isolated CLI readings used to validate the SQL predicate for
`PtahSchema` same-name replacement. They do not establish Kubernetes acceptance
or a production qualification result.

## Identity and procedure

Captured on September 29, 2026, at 21:31–21:32 UTC, using Ptah commit
`f6e562c5b0986cd29a53a5cc01938827336b780a`, the executor pinned by
`support/ptah.json`. The Linux amd64 executor image ID was
`sha256:cc4fabbc0ad0d4dc6d1138df08456222232c0f38d7ef0eba4a7b40bd03946fd1`.

| Engine | Server version | Server image digest |
| --- | --- | --- |
| PostgreSQL | 17.11 | `postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73` |
| MySQL | 8.4.11 | `mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb` |

Each engine used a new database `ptah_audit_schema`, an isolated database-scoped
account `ptah_audit`, and its `testdata/e2e/<engine>-v3.sql` seed. The control row
was `(701, 'identity-control', 'preserved-identity-row')` in `e2e_widgets`.
The desired schema was `<engine>-fault-v1.sql`, which adds `fault_token`.
Credentials were passed in a private environment file and removed after capture.

The executor ran these native commands separately:

```text
ptah schema drift --schema-file /desired.sql --format json
ptah schema plan --schema-file /desired.sql --output /tmp/plan.json --json
```

Observe returned exit code 1, reporting the expected drift. Plan returned 0.
Both left the control row unchanged and `fault_token` absent. Each command's
container was removed before the next command; both used client IP `172.19.0.3`.
All task-owned containers, anonymous volumes, and networks were removed.

| Reading | Executor container ID | Journal records | Received Query/Execute records |
| --- | --- | ---: | ---: |
| PostgreSQL Observe | `36145a6cc7e311ccc0a9e67a44b2675c0052b425f11dffc2d80716219abef287` | 45 | 45 |
| PostgreSQL Plan | `931760be4d186d203fd3559cdec52bb4f2348d87294975b459e93360d68bd965` | 82 | 82 |
| MySQL Observe | `12b4517c749b7f50bdda28e0caa40b1a1d2c7bebcac2a3ce73221e861bc966e3` | 36 | 14 |
| MySQL Plan | `f087990369c76b8f125336c26dca5581e9586488b878989e8dad5c47fe6dfde2` | 69 | 27 |

PostgreSQL JSON rows preserve database, numeric client address, received SQL,
error statement, and bind parameters. MySQL rows preserve timestamp, thread,
authenticated account/client, protocol command, and hexadecimal argument bytes,
including Connect, Prepare, Execute, Close stmt, and Quit. The readings contain
only the isolated executor's records. Unrelated harness records and credentials
are excluded from these checked-in fixtures. Runtime refusal auditing examines
the complete journal window and fails on unidentified scoped traffic.

| File | SHA-256 |
| --- | --- |
| `postgresql-schema-observe.jsonl` | `fb41c4b27b7b5afcf4b083a9bb568d8ab7cbbd74009bcd75f49a59f28f23e3bc` |
| `postgresql-schema-plan.jsonl` | `a45a4752a443be528963e1d6ca6cdc06a44a3d423135e45c66aa887bac4c4591` |
| `mysql-schema-observe.jsonl` | `09532b02e7872160ebaf085b70b876a57196cc660fa9d50a404e4834aab2c4ab` |
| `mysql-schema-plan.jsonl` | `07da04dbaf4d6eb3a9dc01ec321d32ab550ac5f02612a2b3b224f02e40f0b9d5` |

## Permitted work and source review

The [contract](../../../test/e2e/schema-sql-contract.json) carries 44 distinct
PostgreSQL statements/parameter sets and 24 MySQL command/statement pairs.
Both operations have witnesses for each applicable declaration; PostgreSQL's
additional schema-list query belongs to Plan only. The predicate compares SQL,
comments, whitespace, parameters, and MySQL protocol commands byte for byte.
Only the declaration's quoted MySQL database placeholder is expanded to the
controlled fixture identifier. Received SQL is never normalized.

The reviewed source is the pinned commit above:

- PostgreSQL's [schema reader](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/internal/dbschema/postgres/reader.go)
  reads namespaces, columns, types, table statistics, indexes, constraints,
  extensions, routines, views, triggers, sequences, policies, roles, owners,
  grants, and membership from PostgreSQL catalogs. Catalog functions such as
  `pg_get_expr` return definitions; the fixture queries do not execute those
  definitions. The fixed `WITH` queries also read catalogs.
- PostgreSQL's [column spelling probe](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/internal/dbexprprobe/column_spellings.go)
  creates exactly `pg_temp.ptah_column_probe_0` with `id bigint` and
  `enabled boolean`, reads its type definitions, and rolls back to its savepoint.
  The [transaction wrapper](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/dbschema/probe_session.go)
  rolls back and discards its session. This exact temporary probe is permitted;
  permanent DDL, changed columns/defaults, arbitrary functions, and COMMIT are not.
- MySQL's [schema reader](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/internal/dbschema/mysql/reader.go)
  reads columns, tables, enum columns, indexes, check/key constraints, views,
  triggers, routine parameters, and routines from `information_schema`.
  The ten prepared queries and their expanded executions are distinct entries.
  The fixed version/database probes and [role visibility queries](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/internal/dbschema/mysql/roles.go)
  complete this fixture's contract. The isolated account cannot read `mysql.user`;
  that attempted catalog read remains visible and permitted, without granting it
  additional authority.

Tests require every declaration to have an actual reading and the source pin
to match the support catalog. Mutation checks reject arbitrary SELECT/WITH,
appended SQL, changed parameters, changed probe objects, wrong operations,
unknown clients/databases/accounts, lost records, incomplete MySQL sessions,
and missing diagnostic controls. Error messages omit SQL and parameter values.

The runtime row requires exact Pod-to-Job-to-resource ownership and positive
Observe and Plan SQL from both the old and new resource UIDs. No application
SQL is permitted before fresh approval. The fresh approval must execute once,
produce received SQL, add the requested column, and preserve the populated row.
