# Restored migration history refusal journals

These readings were captured from the Ptah executor on isolated PostgreSQL
17.11 and MySQL 8.4.11 servers: the PostgreSQL reading with the Ptah v0.17.0
release image on 2026-11-02, and the MySQL reading with the v0.16.0 release
image on 2026-10-25, which the v0.17.0 image reproduced record for record on
2026-11-02. On 2026-11-04 the v0.18.0 release image (`6e1d430e`) reproduced
both record for record, on 2026-11-06 so did the v0.22.0 release image
(`0dd9190b`), and on 2026-11-07 the v0.23.0 release image (`f47b2bfa`). On
2026-11-13 the v0.26.0 release image (`1cf0563c`) reproduced the PostgreSQL
reading record for record. On MySQL it creates the revision table with
`ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`
([stokaro/ptah#5874](https://github.com/stokaro/ptah/pull/5874)), so the MySQL
reading here is v0.26.0's; every other record matched. On 2026-11-14 the v0.28.0
release image (`2ee460a6`) reproduced both readings record for record, and on
2026-11-15 so did the v0.29.0 release image (`75fa9991`) and the v0.29.2 release
image (`ba500e11`), and on 2026-11-16 the v0.29.3 release image (`dfbda1b2`) and the
v0.29.4 release image (`1f1ff93c`), and on 2026-11-17 the v0.29.5 release image
(`8a634c05`). The readings here are the v0.30.1 release image's (`c5fba763`),
taken on 2026-11-18. Each History read of the refused Apply also asks whether
`schema_migrations_repeatable` exists: the PostgreSQL reading has three records
more and the MySQL one eleven, since MySQL asks in a session of its own. No
other record moved. Later that day the v0.31.0 release image (`dc2decde`) and
the v0.32.0 release image (`da2df4a5`) reproduced both readings record for
record. The readings here are the v0.33.0 release image's (`37c98b00`), taken
on 2026-11-19. The refused Apply asks whether the revision table exists where
it sent `CREATE TABLE IF NOT EXISTS` for it, and creates it only when the catalog
does not show it ([stokaro/ptah#6234](https://github.com/stokaro/ptah/pull/6234)).
The question is the one History already asks, so both contracts admitted it.
On MySQL it is a prepared statement, which adds two records. No other record
moved. Later that day the v0.33.1 release image (`f7c85bdc`) reproduced the
PostgreSQL reading record for record. On MySQL each of its six sessions reads
`SELECT @@SESSION.sql_mode` first
([stokaro/ptah#6295](https://github.com/stokaro/ptah/pull/6295)), which the
History contract admits, so the MySQL reading here is v0.33.1's, six records
more; no other record moved. On 2026-11-20 the v0.34.0 release image (`90e98f6c`)
reproduced both readings record for record. On 2026-11-22 the v0.35.0 release
image (`1ec0b95e`) reproduced the PostgreSQL reading record for record, and the
MySQL one with a `SELECT DATABASE()` after the first session's `sql_mode` read
([stokaro/ptah#6454](https://github.com/stokaro/ptah/pull/6454)), which the
History contract admits, so the MySQL reading here is v0.35.0's, one record
more; no other record moved. On 2026-11-23 the v0.35.0 image reproduced the
MySQL reading record for record on MySQL 9.7.4
(`mysql:9.7@sha256:1954314de304d70fe85c67204d4676891ffa11dcd7caedaf5e9bc50b5f6be446`)
and on MySQL 26.10.0
(`mysql:26.10@sha256:b401608b19ffafbc6aa3d84523acf783078c8cf01e0f7ee1dc3ca75a5701a999`).
On 2026-11-24 the v0.35.1 release image (`ff4a4368`) reproduced both readings
record for record, and the MySQL one on MySQL 26.10.0 as well. They exercise the migration selection guard used by the operator's `restoredHistoryProof`.

The probe applied the repository's two-migration fixture, then removed
migration 2's column and revision row. `migrations up --expect-sequence`
with an approved `[3]` refused the newly selected `[2, 3]` with exit code 2.
The revision table still contained only version 1. A new invocation approving
`[2, 3]` exited 0, recorded `1,2,3`, and established the expected blue row.
Both invocations used `PTAH_MIGRATION_LOCK_TIMEOUT=30s`, matching the operator
fixture. MySQL also used its explicit `--tx-mode none`.

- Procedure: `support/qualification/probes/schema_sql_readings.py --part migrations`.
  The first readings were taken from Ptah source
  `f6e562c5b0986cd29a53a5cc01938827336b780a` on 2026-09-29, and the v0.13.0,
  v0.14.0 and v0.15.0 images reproduced them record for record. The v0.16.0
  image does not: its refused Apply reads the revision table again under the
  migration lock before it refuses, which the History contract already
  permits, so the MySQL reading here is v0.16.0's. The v0.17.0 image adds one
  PostgreSQL record: it reads the session's search path when it connects,
  which the History contract now permits too.
- Executor image: `ghcr.io/stokaro/ptah@sha256:933cc38f93375e0a83ff49db0cfdcee301ab328e95d13496fb2b2be82be1f906`
  (v0.33.0) for PostgreSQL and
  `ghcr.io/stokaro/ptah@sha256:37c16165f4184ddfd80cf61da9a366fe13ab950830b095f401e91de2c2f4383c`
  (v0.33.1) for MySQL
- Database: `ptah_audit_restore`; account: `ptah_audit`
- Executor client address in each isolated network: `172.19.0.3`

The PostgreSQL reading retains all 35 received SQL records from the refused
executor's address. The MySQL reading retains all 96 protocol records from
that account, including 43 Query/Execute records and the connection boundaries.
SQL, parameters and hex-encoded MySQL argument bytes are unchanged. Administrator
traffic and account setup are excluded. The probe containers, volumes and
networks were removed; the borrowed images were preserved.

## Postgresql

- Reading: [postgresql-restored-history-refusal.jsonl](postgresql-restored-history-refusal.jsonl)
- Server image: `postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`
- Reading SHA-256: `d812020f10d1ee6366883ccbaf4107e05453e51c3b48634a5967ea77d9e8cab2`

## Mysql

- Reading: [mysql-restored-history-refusal.jsonl](mysql-restored-history-refusal.jsonl)
- Server image: `mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb`
- Reading SHA-256: `b4f213cd8d64fb0082f76892670bf27522aac34baa81873f6b1846072c98179c`

## Permitted diagnostic contract

The existing History SQL remains permitted. Only the exact refused Apply Job
may additionally execute the default migration lock's acquire/release queries
and the exact unresolved-revision query. Both the lock name/key and parameters
are fixed by the reviewed Ptah source:

- [Sequence guard](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/internal/cli/migrateup/expect_sequence.go): compares the selected sequence with the approved list under the migration lock.
- [Default migration lock](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/migration/migrator/advisory_lock.go): uses `ptah_migrate`.
- [Database locks](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/internal/dblock/dblock.go): derives PostgreSQL key `2705505214` using FNV-1a; MySQL receives the lock name and 30-second timeout. PostgreSQL also tries the lock from a witness connection to verify exclusion.
- [Revision inspection](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/migration/migrator/revisions.go): inspects unresolved revision state before executing the selection.

The predicate requires received acquire, unresolved-history and release SQL
from the exact refused Apply. MySQL Prepare records alone cannot satisfy those
requirements. A different Job, changed parameters, arbitrary SELECT/DDL/DML,
missing diagnostic execution, or an incomplete journal fails. Later History
Jobs retain their narrower read contract; they cannot supply the Apply's lock
records. Harness database assertions use separately enumerated loopback reads.

These CLI probes establish concrete journal readings and allowed controls for
the parser. They do not qualify the Kubernetes scenario, the full matrix, or
backup recovery. Cluster execution must still bind the refusal window to the
actual resource, Job and Pod identities and verify the fresh approval's effects.

An earlier probe failed before database execution because its expected-sequence
file was unreadable by the executor's non-root user; its private failure output
was retained and its resources removed. A later diagnostic capture used the
CLI's default lock timeout. The readings here are the subsequent successful
captures using the fixture's exact 30-second setting; neither earlier run is
substituted for these results.
