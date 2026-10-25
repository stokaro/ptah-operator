# Schema diagnostic SQL readings

These are isolated CLI readings of the SQL the executor sends for schema
diagnostics. They witness every declaration of the e2e audit
[contract](../../../test/e2e/schema-sql-contract.json). They do not establish
Kubernetes acceptance or a production qualification result.

## Identity and procedure

Captured on October 23, 2026 with the Ptah v0.15.0 release image, and taken
again on October 25, 2026 with the executor `support/ptah.json` now pins: the
Ptah v0.16.0 release image, commit `91e28d4331950e949590fd9ac404373bc17b572d`.
[`schema-sql-capture.json`](schema-sql-capture.json) records the executor and
server images, every command's exit status and record count, and the SHA-256 of
each reading and saved plan.

The readings are written by
`support/qualification/probes/schema_sql_readings.py`, which runs each command
against a disposable PostgreSQL 17.11 or MySQL 8.4.11 server that journals every
statement it receives, one command per journal:

```sh
python3 support/qualification/probes/schema_sql_readings.py \
  --context <docker context> --executor-image <image> --out <dir>
```

Each server uses a new database `ptah_audit_schema` and the account
`ptah_audit`: PostgreSQL's bootstrap account, and MySQL's database-scoped
account created through `MYSQL_USER`. The executor's client address is rewritten
to `172.19.0.3`, the address the audit predicates name; SQL, parameters,
protocol commands, session order and timestamps stay as received. Each command
runs in its own container, and every container, anonymous volume and network the
run created is removed.

Before the v0.16.0 capture, the same procedure ran with the executor of the
previous pin, the Ptah v0.15.0 release image, as it ran with v0.14.0 before
the v0.15.0 capture. Each time it reproduced every
earlier checked-in reading record for record, PostgreSQL parameters included,
and all four saved exclusion plans byte for byte. That is what makes the new
readings a statement about the executor rather than about a different procedure.

`--contract` writes the contract from a directory of readings. Run on the
v0.14.0 readings, it reproduced the earlier contract byte for byte.

Against v0.15.0, v0.16.0 changed nothing here either: every schema reading and
both migration History readings matched record for record, and the contract
changed only in the commit it names. Only the restored-history refusals moved;
[restored-history-refusal.md](restored-history-refusal.md) says how.

## Permitted work and source review

The contract carries 48 PostgreSQL statement/parameter sets and 32 MySQL
command/statement pairs. The predicate compares SQL, comments, whitespace,
parameters and MySQL protocol commands byte for byte. Only the declaration's
quoted MySQL database placeholder is expanded to the controlled fixture
identifier. Received SQL is never normalized.

Against the previous pin, v0.15.0 changed nothing here. Every reading carries
the same statements in the same order; the MySQL journals differ only in their
timestamps and one thread number, and the contract only in the commit it names.
Against v0.13.0, v0.14.0 changed seven declarations and kept the count at 80. On PostgreSQL the column read takes a domain column's NOT NULL from the
column rather than from the type's effective nullability, the range type read
qualifies operator class and collation by schema, the exclusion constraint read
adds the backing index, and the row-level security policy read was rewritten for
its new owner: the same catalog, with roles as JSON, the raw command and the
permissive flag. On MySQL the `information_schema.STATISTICS` column check and
both forms of the index read add the `EXPRESSION` column. Every changed
declaration is a catalog read: none contains a write, DDL, grant or
transaction-control keyword outside comments and literals. The migration History
and restored-history readings are unchanged record for record.

The reviewed source is the pinned commit:

- PostgreSQL's [schema reader](https://github.com/stokaro/ptah/blob/e046527f79c58651d2082dce65b5ceb48dbc2606/internal/dbschema/postgres/reader.go)
  reads namespaces, columns, types, table statistics, indexes, constraints,
  extensions, routines, views, triggers, sequences, policies, roles, owners,
  grants, default privileges and membership from PostgreSQL catalogs. Catalog
  functions such as `pg_get_expr` return definitions; the fixture queries do not
  execute those definitions.
- PostgreSQL's [column spelling probe](https://github.com/stokaro/ptah/blob/e046527f79c58651d2082dce65b5ceb48dbc2606/internal/dbexprprobe/column_spellings.go)
  creates exactly `pg_temp.ptah_column_probe_0` with `id bigint`, with or without
  `enabled boolean` in the declared fixture variants, reads its type
  definitions, and rolls back to its savepoint. This exact temporary probe is
  permitted; permanent DDL, changed columns or defaults, arbitrary functions and
  COMMIT are not.
- MySQL's [schema reader](https://github.com/stokaro/ptah/blob/e046527f79c58651d2082dce65b5ceb48dbc2606/internal/dbschema/mysql/reader.go)
  reads columns, tables, enum columns, indexes, check and key constraints, views,
  triggers, routine parameters and routines from `information_schema`. Its
  prepared queries and their expanded executions are distinct entries. The
  isolated account cannot read `mysql.user`; that attempted catalog read remains
  visible and permitted, without granting it additional authority.

Tests require every declaration to have an actual reading and the source pin to
match the support catalog. Mutation checks reject arbitrary SELECT/WITH,
appended SQL, changed parameters, changed probe objects, wrong operations,
unknown clients, databases or accounts, lost records, incomplete MySQL sessions
and missing diagnostic controls. Error messages omit SQL and parameter values.

## The readings

| Reading | Fixture and command | PostgreSQL records | MySQL records (Query/Execute) |
| --- | --- | ---: | ---: |
| `observe`, `plan` | v3 with a control row; drift and plan of fault-v1 | 46, 84 | 38 (16), 73 (31) |
| `destructive-observe`, `-plan` | the same, desired v4 | 46, 84 | 38 (16), 73 (31) |
| `exclusion-wide-observe`, `-plan` | adds `e2e_excluded_policy_keep` as the lifecycle creates it; fault-v1 | 46, 84 | 38 (16), 73 (31) |
| `exclusion-narrow-observe`, `-plan` | the same; Plan alone receives `--exclude=e2e_excluded_policy_keep` | 46, 84 | 38 (16), 73 (31) |
| `initial-v1-observe`, `-plan` | no tables; desired v1 | 40, 77 | 37 (15), 71 (29) |
| `tag-v2-observe`, `-plan`, `tag-v3-…` | v1 with one control row; desired v2 or v3 | 46, 84 | 38 (16), 73 (31) |
| `drift-plan-before` | v3; saved plan of fault-v1 | 84 | 73 (31) |
| `drift-stale-apply` | `enabled` dropped under the saved plan; Apply refused `stale-plan` (exit 2), lock timeout 60s | 44 | 46 (19) |
| `drift-observe`, `drift-plan` | the changed database, observed and planned again | 46, 84 | 38 (16), 73 (31) |
| `drift-validate-plan` | `schema apply --dry-run` of the fresh plan, lock timeout 30s | 44 | 46 (19) |
| `drift-apply-current` | the fresh plan applied, lock timeout 60s | 48 | 50 (23) |
| `lock-45-plan`, `lock-60-plan` | MySQL only: a runner Plan's two plan reads and dry-run validation under a 45s or 60s lock timeout | | 192 (81) |

Every Observe exited 1 with drift, every Plan and validation exited 0, the stale
Apply exited 2 and the allowed Apply exited 0. Only that final Apply restored
`enabled` and added `fault_token`; the control rows survived every sequence, and
the exclusion plans keep the excluded table's drop out of the narrowed plan.

Both native exclusion plans add the managed column and then drop the excluded
table. v0.13.0's PostgreSQL plan dropped the table's primary-key constraint
before the table, and v0.14.0's does not. Both narrowed plans contain only the
column addition. v0.14.0 also sends each planned statement with the plan's
comments in front of it, so the allowed Apply's journal shows them.

## Operations

Observe readings witness `observe`. Plan readings, the saved-plan validation and
the runner Plan journals witness `plan`: a runner Plan validates its saved plan
with `schema apply --dry-run`, so the validation's lock statements belong to
Plan. The refused stale Apply witnesses `stale-apply`, which the runtime audit
grants only to the exact resource, Job and Pod whose result refused the approved
stale plan. The allowed Apply is not a diagnostic and grants nothing.

The lock declarations permit only:

- PostgreSQL `pg_try_advisory_lock` and `pg_advisory_unlock` with the key
  derived from `ptah_schema_apply`.
- MySQL Prepare/Execute pairs for `GET_LOCK('ptah_schema_apply', <timeout>)` and
  `RELEASE_LOCK('ptah_schema_apply')`, with the Plan timeouts 30, 45 and 60
  seconds and the stale Apply's 60.
- MySQL's exact `SELECT @@SESSION.restrict_fk_on_non_standard_key` capability
  read on the pinned session.

Neighboring timeout values, other lock names, extra SQL and wrong actors remain
refused. None of these declarations permits permanent schema or row changes.
