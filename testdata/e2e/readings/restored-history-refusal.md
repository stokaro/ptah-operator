# Restored migration history refusal journals

These readings were captured from the pinned Ptah executor on isolated
PostgreSQL 17.11 and MySQL 8.4.11 servers on 2026-09-29. They exercise the
migration selection guard used by the operator's `restoredHistoryProof`.

The probe applied the repository's two-migration fixture, then removed
migration 2's column and revision row. `migrations up --expect-sequence`
with an approved `[3]` refused the newly selected `[2, 3]` with exit code 2.
The revision table still contained only version 1. A new invocation approving
`[2, 3]` exited 0, recorded `1,2,3`, and established the expected blue row.
Both invocations used `PTAH_MIGRATION_LOCK_TIMEOUT=30s`, matching the operator
fixture. MySQL also used its explicit `--tx-mode none`.

- Ptah source: `f6e562c5b0986cd29a53a5cc01938827336b780a`; on 2026-10-10 the same
  procedure, run by `support/qualification/probes/schema_sql_readings.py --part migrations`
  with the pinned Ptah v0.13.0 image (`eb69f8c4`), produced both journals record for
  record, so they stand for that executor unchanged
- Executor image ID: `sha256:cc4fabbc0ad0d4dc6d1138df08456222232c0f38d7ef0eba4a7b40bd03946fd1`
- Operator fixture source: `85201caff7c44b9a271930331052108b34629ab9`
- Database: `ptah_audit_restore`; account: `ptah_audit`
- Executor client address in each isolated network: `172.19.0.3`

The PostgreSQL reading retains all 25 received SQL records from the refused
executor's address. The MySQL reading retains all 63 protocol records from
that account, including 28 Query/Execute records and the connection boundaries.
SQL, parameters and hex-encoded MySQL argument bytes are unchanged. Administrator
traffic and account setup are excluded. Complete raw journals and command
outputs are retained privately. The probe containers, volumes and networks
were removed; the borrowed images were preserved.

## Postgresql

- Reading: [postgresql-restored-history-refusal.jsonl](postgresql-restored-history-refusal.jsonl)
- Recorded at: `2026-09-29T20:58:10.692871+00:00`
- Server image: `postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`
- Server container: `7618b2daba20a6aeaf2d476aefb919495ee5a6e8c7219aa6b8c664577d0be3db`
- Refused executor container: `d76f011f7f53defb77d506b0f6f0dd9cbe04f853df7e4d365b45c6128c5ac9d8`
- Fresh-control executor container: `b3fc329fe3717d4efb50816d1981904b599e8b04fba5d0bea6097ba0fac55a1e`
- Reading SHA-256: `747c8c1246882ef19da6fe5e35768d9fe980ecb11a9f274459a183abe0fd1491`

## Mysql

- Reading: [mysql-restored-history-refusal.jsonl](mysql-restored-history-refusal.jsonl)
- Recorded at: `2026-09-29T20:58:48.520550+00:00`
- Server image: `mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb`
- Server container: `1ac1bca117ae647dfff9fb323c512c06934ee6b9787db523f861c8663b344bad`
- Refused executor container: `b87ca5aa681e03d0f3115ae2035cccfdb32b8ed907a582b177b6a5ef90420c99`
- Fresh-control executor container: `704734d86fdf96e55f5cce60bb4135e5994641bad9b9e07a556fbabd8d6085ae`
- Reading SHA-256: `b2743658425fdedcf3c956a5a3308832f9053a1cf9cf4c060a9a35e93cfa4524`

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
