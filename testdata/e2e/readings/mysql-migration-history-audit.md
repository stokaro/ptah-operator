# MySQL migration history journal

The JSONL reading contains all 13 general-log records for the isolated
`ptah_audit` account during a successful Ptah `migrations status
--migrations-dir /migrations --json` invocation, captured on 2026-10-10 by
`support/qualification/probes/schema_sql_readings.py --part migrations`. The
database already held the repository's first two MySQL migrations, applied by
the same executor, so History reads an existing revision table; its statements
include everything it sends to an empty database, where it stops after asking
whether the table exists.

- MySQL: `8.4.11`
- Server image: `mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb`
- Executor: the Ptah v0.13.0 release image `support/ptah.json` pins
- Ptah source: `eb69f8c4435643c5c9f3fbd1c9953b9ba0f44b6f`
- Client address: `172.19.0.3`
- Database: `ptah_audit_history`

The reading preserves event times, thread IDs, `user_host`, command types and
exact argument bytes encoded as hex. The 13 records comprise one Connect, five
Query, two Prepare, two Execute, two Close stmt and one Quit: 7 received SQL
execution records. No password-bearing account setup or unrelated administrator
traffic is included.

Run with the executor built from the previous pin on an empty database, the same
probe reproduced the earlier 34-record reading exactly. v0.13.0's History no
longer creates the revision table or inspects its version type and columns: it
counts the table in `information_schema.tables` first and reads its columns
from `SELECT * FROM schema_migrations WHERE 1 = 0`. The predicate keeps the
earlier statements, because the Apply it also governs still sends them, and
adds those two reads.

The exact statements and bound parameters are reviewed against the pinned
Ptah implementation:

- [Connection detection](https://github.com/stokaro/ptah/blob/eb69f8c4435643c5c9f3fbd1c9953b9ba0f44b6f/dbschema/connection.go) reads the server version.
- [The migrator](https://github.com/stokaro/ptah/blob/eb69f8c4435643c5c9f3fbd1c9953b9ba0f44b6f/migration/migrator/migrator.go) asks whether the default revision table exists, checks its engine and reads its columns from an empty result.
- [History selection](https://github.com/stokaro/ptah/blob/eb69f8c4435643c5c9f3fbd1c9953b9ba0f44b6f/migration/migrator/revisions.go) reads the existing revision rows. MySQL table inspection also reads the session foreign-key restriction and the default revision table's CREATE statement.

The refusal predicate checks the command type, exact SQL and expanded bound
parameters, authenticated account, connection lifecycle and exact Job/Pod
ownership. It does not grant a general SELECT or DDL exception.

Each approval-input scenario uses a new account scoped to its database. Its
audit starts before the migration exists and includes all traffic from that
account or any identified migration Pod address, including attempts to change
the selected database. Complete before/after journal multisets must retain
every prior row, including duplicates. Unknown clients, altered statements,
ambiguous session ordering and missing initial History SQL fail the audit.

This isolated CLI reading validates the parser and allowed History control.
It is not Kubernetes approval-refusal evidence. That result requires the
scenario's full journal window, migration/Job/Pod identities and freshly
authorized successful Apply on the qualified artifacts.
