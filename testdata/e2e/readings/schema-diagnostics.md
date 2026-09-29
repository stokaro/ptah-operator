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

Each engine used a new database `ptah_audit_schema`, the account `ptah_audit`,
and its `testdata/e2e/<engine>-v3.sql` seed. PostgreSQL used the container's
bootstrap account (`POSTGRES_USER`); MySQL used the database-scoped account
created through `MYSQL_USER`. The control row
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

The [contract](../../../test/e2e/schema-sql-contract.json) carries 47 distinct
PostgreSQL statements/parameter sets and 29 MySQL command/statement pairs.
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
  creates exactly `pg_temp.ptah_column_probe_0` with `id bigint`, with or without
  `enabled boolean` in the declared fixture variants, reads its type definitions, and rolls back to its savepoint.
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

## Destructive and exclusion policy readings

The [policy capture manifest](schema-policy-diagnostics.json) records six further
isolated runs on September 29, 2026, at 22:09–22:12 UTC. It includes the server
and executor identities, each command's container identity and exit status, and
a SHA-256 for every selected journal and native exclusion plan. The source pin,
server images, account setup, and journal selection follow the procedure above.
These readings validate predicates; they are not Kubernetes acceptance.

Each run started from the v3 seed and the same populated control row. The
`destructive` variant planned v4. The exclusion variants also created
`e2e_excluded_policy_keep` with a primary key and one preserved row, and planned
fault-v1. Only the narrowed Plan command received
`--exclude=e2e_excluded_policy_keep`; Observe received no exclusion, matching
the runner. No planned SQL was applied, and both control rows remained intact.
All task-owned containers, anonymous volumes, networks, and credential files
were removed after capture.

Every variant produced 45/82 PostgreSQL Observe/Plan records, or 36/69 MySQL
protocol records containing 14/27 Query/Execute records. The v4 PostgreSQL
fixture adds one exact diagnostic declaration: the temporary column-spelling
probe with only `id bigint`. Both operations witnessed it. The transaction and
savepoint rollback contract remains unchanged; no permanent mutation is allowed.

The native PostgreSQL exclusion plan first adds the managed column, then drops
the excluded table's primary-key constraint, then drops that table. MySQL has
only the addition and table drop. Both narrowed plans contain only the same
column addition. The document predicate accepts their leading line comments
and the exact PostgreSQL constraint removal, and refuses unrelated or appended
statements. Received SQL auditing still compares every byte without removing
comments or normalizing statements.

The destructive and exclusion runtime scenarios audit one uninterrupted window
from resource creation through all refused decisions. Actual diagnostic results
bind each required control to its resource, Job, Pod, and published plan. The
final database equality check follows this audit, before fresh authorization.

## Initial planning and mutable-tag readings

The [lifecycle capture manifest](schema-lifecycle-diagnostics.json) records six
isolated runs at 22:30–22:33 UTC on September 29, 2026, with the same source pin,
images, account setup, and cleanup procedure. It includes every command's
identity and exit status and the hashes of twelve diagnostic journals.
`initial-v1` starts with no tables and plans v1. `tag-v2` and `tag-v3` start from
v1 with one `(701, 'identity-control')` row and plan the corresponding revision.
The empty case remains empty; the populated cases preserve the row and do not
add `note`. All Observe commands returned 1 and all Plan commands returned 0.

The empty PostgreSQL case has 39 Observe and 75 Plan SQL records; populated
cases have 45 and 82. All MySQL cases have 36/69 protocol records containing
14/27 received Query/Execute records. Every statement already belongs to the
then-current 69-entry diagnostic contract; these fixtures added witnesses, not
permissions. The native Plan validation correction below adds seven declarations.

The schema lifecycle audits its initial decision, approved lock-policy edit,
and approved transaction-mode edit before the first allowed Apply. A separate
continuous window covers v2 planning and the admitted approval invalidated by
the v3 tag move. This later window preserves the target credential and records
its starting Job UIDs under the existing status-write barrier. Earlier Jobs
cannot provide a new SQL control, and any traffic from an unidentified client
fails. Every required Observe/Plan control names its actual result Job and Pod.
An earlier complete MySQL session is permitted before the window; missing
Connect/Quit records inside the new window still fail. These are implemented
proofs awaiting cluster execution on the final identities.

## Native Plan validation and stale-plan readings

The runner validates a saved plan with `schema apply --dry-run` before publishing
it. The original diagnostic captures invoked only `schema plan`, so their SQL
contract omitted the validation command's lock queries and MySQL session check.
`TestSchemaSQLPlanValidation` fails against that original contract on both engines
using the actual validation journals below.

[Capture metadata](schema-drift-diagnostics.json) records all twelve journals,
SHA-256 values, commands, fixture/plan hashes, container identities, and outcomes.
The executor is still the pinned Ptah source above; this capture used executor
image `sha256:7170574df0a11770b9bf9b32adc43e83d34bdb233fbe53a17df3540c10052ca5`,
PostgreSQL 17.11 and MySQL 8.4.11. Each engine began at its `*-v3.sql` fixture with
one preserved row. The `*-fault-v1.sql` desired schema added `fault_token`.

The sequence saved the initial plan, dropped `enabled` as a separate harness
fault, refused the old plan with `stale-plan`, observed and planned the changed
database, validated that fresh plan, and applied it. Only the final Apply restored
`enabled` and added `fault_token`; all diagnostic/refusal steps left both columns
absent, and the row survived the whole sequence. The account and database stayed
unchanged. Plan validation used `PTAH_LOCK_TIMEOUT=30s`, matching the workload's
observation timeout; the two Apply commands used the fault fixture's `60s`.

| Command | PostgreSQL received SQL | MySQL received Query/Execute | Expected result |
| --- | ---: | ---: | --- |
| Initial Plan | 82 | 27 | Saved plan, exit 0 |
| Stale Apply | 43 | 17 | `refused` / `stale-plan`, exit 2 |
| Observe | 45 | 14 | Drift, exit 1 |
| Replacement Plan | 82 | 27 | Saved plan, exit 0 |
| Saved-plan validation | 43 | 17 | `dry-run`, exit 0 |
| Fresh Apply | 47 | 21 | `applied`, exit 0 |

The added Plan declarations permit only:

- PostgreSQL `pg_try_advisory_lock` and `pg_advisory_unlock`, both with the exact
  key `1237737229` derived from `ptah_schema_apply`.
- MySQL Prepare/Execute pairs for `GET_LOCK('ptah_schema_apply', 30)` and
  `RELEASE_LOCK('ptah_schema_apply')`.
- MySQL's exact `SELECT @@SESSION.restrict_fk_on_non_standard_key` capability
  read on the pinned session.

The pinned source's [lock implementation](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/internal/dblock/dblock.go)
acquires, verifies and releases the named advisory lock; PostgreSQL hashes the
name with FNV-1a. Its [session setup](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/dbschema/connection.go)
reads MySQL's foreign-key capability before it inspects the target. None of
these declarations permits permanent schema or row changes.

The tests require every declaration to have a witness. They also reject different
lock identities/timeouts, broader SQL, these validation queries attributed to
Observe or Apply, and the fresh Apply's actual DDL attributed to Plan. These CLI
readings support the audit predicate; they do not prove the Kubernetes approval
and recovery sequence. The AB-04 runtime refusal windows now use the separate
`stale-apply` permission below and still need final-candidate execution.

The first capture attempt failed before SQL because a copied plan was unreadable
by the unprivileged executor. A second completed capture used the CLI's default
lock timeout. Both remain private diagnostic records; the checked-in readings
come from the third capture with the operator's explicit timeouts. Every attempt
removed its own containers, anonymous volumes and network. The executor image
belongs to the separate active data-plane run and was left intact.

The `stale-apply` declarations cover exactly the queries observed in the two
stale-plan journals: 40 PostgreSQL statement/parameter combinations and 29 MySQL
command/statement combinations. Existing inspection and lock declarations gain
that operation only where the stale-plan reading witnesses them. The only new
statement is MySQL `SELECT GET_LOCK('ptah_schema_apply', 60)`, matching the
Apply fixture's explicit lock timeout. It does not widen Plan's timeout.

The runtime audit grants this permission only to the exact resource, Job and Pod
whose result refused the approved stale plan. It requires received lock, column
inspection and unlock executions from that Apply. Observe/Plan Jobs retain
their own permissions and cannot supply the Apply's required evidence. Tests
remove each execution from the actual journals, substitute each workload UID,
change SQL/parameters and check the fresh Apply's actual column additions.
Every permitted operation/statement pair still needs an actual pinned-source
witness. This is evidence about the audit predicate; the Kubernetes scenarios
remain unqualified until their final-candidate runs complete.
