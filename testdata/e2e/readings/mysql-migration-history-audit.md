# MySQL migration history journal

The JSONL reading contains all 18 general-log records for the isolated
`ptah_audit` account during a successful Ptah `migrations status
--migrations-dir /migrations --json` invocation, captured on 2026-11-18 by
`support/qualification/probes/schema_sql_readings.py --part migrations`. The
database already held the repository's first two MySQL migrations, applied by
the same executor, so History reads an existing revision table; its statements
include everything it sends to an empty database, where it stops after asking
whether the table exists.

- MySQL: `8.4.11`
- Server image: `mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb`
- Executor: the Ptah v0.30.1 release image, source `c5fba763a6fb346fc8a35d2f336d75a91385e3b9`. The
  reading was first taken on 2026-10-10 with the v0.13.0 release image (`eb69f8c4`). On
  2026-10-21 the same probe run with the v0.14.0 release image (`e046527f`) produced this reading
  record for record, on 2026-11-03 so did the v0.17.0 release image, on 2026-11-04 the v0.18.0
  release image (`6e1d430e`), on 2026-11-06 the v0.22.0 release image (`0dd9190b`), on 2026-11-07
  the v0.23.0 release image (`f47b2bfa`), on 2026-11-13 the v0.26.0 release image (`1cf0563c`),
  on 2026-11-14 the v0.28.0 release image (`2ee460a6`), and on 2026-11-15 the v0.29.0 release
  image (`75fa9991`) and the v0.29.2 release image (`ba500e11`), and on 2026-11-16 the v0.29.3
  release image (`dfbda1b2`) and the v0.29.4 release image (`1f1ff93c`), and on 2026-11-17 the
  v0.29.5 release image (`8a634c05`). The v0.30.1 image adds five records, a second session
  that asks whether `schema_migrations_repeatable` exists, and changes no other. On 2026-11-18
  the v0.31.0 release image (`dc2decde`) and the v0.32.0 release image (`da2df4a5`)
  reproduced it record for record, and on 2026-11-19 so did the v0.33.0 release image
  (`37c98b00`)
- Client address: `172.19.0.3`
- Database: `ptah_audit_history`

The reading preserves event times, thread IDs, `user_host`, command types and
exact argument bytes encoded as hex. The 18 records comprise two Connect, five
Query, three Prepare, three Execute, three Close stmt and two Quit: 8 received
SQL execution records. No password-bearing account setup or unrelated administrator
traffic is included.

Run with the executor built from the previous pin on an empty database, the same
probe reproduced the earlier 34-record reading exactly. v0.13.0's History no
longer creates the revision table or inspects its version type and columns: it
counts the table in `information_schema.tables` first and reads its columns
from `SELECT * FROM schema_migrations WHERE 1 = 0`. The predicate keeps the
earlier statements, because the Apply it also governs still sends them, and
adds those two reads. It admits v0.30.1's count of the repeatable table as
well, in the session that asks it.

The exact statements and bound parameters are reviewed against the pinned
Ptah implementation:

- [Connection detection](https://github.com/stokaro/ptah/blob/e046527f79c58651d2082dce65b5ceb48dbc2606/dbschema/connection.go) reads the server version.
- [The migrator](https://github.com/stokaro/ptah/blob/e046527f79c58651d2082dce65b5ceb48dbc2606/migration/migrator/migrator.go) asks whether the default revision table exists, checks its engine and reads its columns from an empty result.
- [History selection](https://github.com/stokaro/ptah/blob/e046527f79c58651d2082dce65b5ceb48dbc2606/migration/migrator/revisions.go) reads the existing revision rows. MySQL table inspection also reads the session foreign-key restriction and the default revision table's CREATE statement.
- [The repeatable record](https://github.com/stokaro/ptah/blob/c5fba763a6fb346fc8a35d2f336d75a91385e3b9/migration/migrator/repeatable_store.go) counts `schema_migrations_repeatable` in `information_schema.tables` on a connection of its own, and only when the table exists checks its owner and reads its rows. The database here holds none, so that session ends at the count.

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
