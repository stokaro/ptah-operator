# Restored migration history refusal journals

These readings were captured from the pinned Ptah executor, the Ptah v0.16.0
release image, on isolated PostgreSQL 17.11 and MySQL 8.4.11 servers on
2026-10-25. They exercise the migration selection guard used by the operator's
`restoredHistoryProof`.

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
  permits, so the readings here are v0.16.0's.
- Executor image: `ghcr.io/stokaro/ptah@sha256:b8a9ac7c695e7af9f66787880b0cd722017474f5d05c08a81e0db297df311cf4`
- Database: `ptah_audit_restore`; account: `ptah_audit`
- Executor client address in each isolated network: `172.19.0.3`

The PostgreSQL reading retains all 31 received SQL records from the refused
executor's address. The MySQL reading retains all 77 protocol records from
that account, including 34 Query/Execute records and the connection boundaries.
SQL, parameters and hex-encoded MySQL argument bytes are unchanged. Administrator
traffic and account setup are excluded. The probe containers, volumes and
networks were removed; the borrowed images were preserved.

## Postgresql

- Reading: [postgresql-restored-history-refusal.jsonl](postgresql-restored-history-refusal.jsonl)
- Server image: `postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`
- Reading SHA-256: `5fc37626c2c30b7265823cb62f93ab2fd924bdc65946d1b477dbd6f7b85fdca1`

## Mysql

- Reading: [mysql-restored-history-refusal.jsonl](mysql-restored-history-refusal.jsonl)
- Server image: `mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb`
- Reading SHA-256: `9eeae5c60c294b68b8604d2ae7d5cfee3da125f983c769a21aee30f3ad8576aa`

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
