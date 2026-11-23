# PostgreSQL migration history journal

The JSONL reading contains the 11 received SQL records of a successful Ptah
`migrations status --migrations-dir /migrations --json` invocation, captured on
2026-11-18 by `support/qualification/probes/schema_sql_readings.py --part
migrations`. PostgreSQL's JSON logging collector produced the records. Only the
database, client address, SQL and parameter fields are retained; SQL and
parameter bytes are unchanged. The database already held the repository's first
two PostgreSQL migrations, so History reads an existing revision table; its
statements include everything it sends to an empty database, where it stops
after asking whether the table exists.

- Ptah source: `c5fba763a6fb346fc8a35d2f336d75a91385e3b9`, the v0.30.1 release image. The reading was
  first taken on 2026-10-10 with the v0.13.0 release image (`eb69f8c4`), and the v0.14.0, v0.15.0 and
  v0.16.0 images reproduced it record for record. The v0.17.0 image adds one record, the search-path
  read below, and changes no other. On 2026-11-04 the v0.18.0 release image (`6e1d430e`) reproduced it
  record for record, on 2026-11-06 so did the v0.22.0 release image (`0dd9190b`), on 2026-11-07
  the v0.23.0 release image (`f47b2bfa`), on 2026-11-13 the v0.26.0 release image (`1cf0563c`),
  on 2026-11-14 the v0.28.0 release image (`2ee460a6`), and on 2026-11-15 the v0.29.0 release
  image (`75fa9991`) and the v0.29.2 release image (`ba500e11`), and on 2026-11-16 the v0.29.3
  release image (`dfbda1b2`) and the v0.29.4 release image (`1f1ff93c`), and on 2026-11-17 the
  v0.29.5 release image (`8a634c05`). The v0.30.1 image adds one record, the count of
  `schema_migrations_repeatable` below, and changes no other. On 2026-11-18 the v0.31.0 release
  image (`dc2decde`) and the v0.32.0 release image (`da2df4a5`) reproduced it record for
  record, and on 2026-11-19 so did the v0.33.0 release image (`37c98b00`) and the v0.33.1
  release image (`f7c85bdc`), on 2026-11-20 the v0.34.0 release image (`90e98f6c`), and on
  2026-11-22 the v0.35.0 release image (`1ec0b95e`)
- Server image: `postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`
- Client address: `10.244.3.97`
- Isolated database: `ptah_e2e_retarget`

The database name and client address are the ones the earlier reading carried
from its cluster run, so the tests' identities stay. Run on an empty database
with the executor built from the pin before v0.13.0, Ptah commit `f6e562c5`
(`v0.9.0-73-gf6e562c5b`), the same probe reproduced that earlier 17-record
reading exactly.

The permitted statements are reviewed against the Ptah implementation of v0.14.0, the search-path
read against v0.17.0, and the repeatable count against v0.30.1:

- [Connection setup](https://github.com/stokaro/ptah/blob/38bd6e91cfe3a70543046da9c3ada05a0460a103/internal/connector/pgconnect/pgconnect.go) reads the session's search path, `SELECT s FROM unnest(current_schemas(false)) WITH ORDINALITY AS path(s, n) ORDER BY n` ([the query](https://github.com/stokaro/ptah/blob/38bd6e91cfe3a70543046da9c3ada05a0460a103/internal/schemaselection/schemaselection.go)), so it resolves an unqualified name in the order the server does. It reads session state and nothing else.
- [Connection detection](https://github.com/stokaro/ptah/blob/e046527f79c58651d2082dce65b5ceb48dbc2606/dbschema/connection.go) reads the server version, current schema and TimescaleDB presence. The PostgreSQL driver's ping sends `-- ping`.
- [Metadata ownership](https://github.com/stokaro/ptah/blob/e046527f79c58651d2082dce65b5ceb48dbc2606/migration/migrator/metadata_owner.go) checks ownership of the exact default revision table.
- [The migrator](https://github.com/stokaro/ptah/blob/e046527f79c58651d2082dce65b5ceb48dbc2606/migration/migrator/migrator.go) counts the default table in `information_schema.tables` and reads its columns from `SELECT * FROM "schema_migrations" WHERE 1 = 0`.
- [History selection](https://github.com/stokaro/ptah/blob/e046527f79c58651d2082dce65b5ceb48dbc2606/migration/migrator/revisions.go) reads the existing revision rows in order.
- [The repeatable record](https://github.com/stokaro/ptah/blob/c5fba763a6fb346fc8a35d2f336d75a91385e3b9/migration/migrator/repeatable_store.go) counts `schema_migrations_repeatable` in `information_schema.tables` in the current schema, and only when the table exists checks its owner and reads its rows. The database here holds none, so History stops at the count.

v0.13.0's History no longer creates the table or checks its version type and
columns. The predicate keeps those statements, because the Apply it also
governs still sends them, and admits the two reads v0.13.0 sends in their
place: the count of the default table in `information_schema.tables` and
`SELECT * FROM "schema_migrations" WHERE 1 = 0`. It admits v0.17.0's
search-path read as well, and v0.30.1's count of the repeatable table. Additional statements, changed table/schema/column
parameters, arbitrary SELECT or WITH statements, and failed unauthorized SQL
are refused. PostgreSQL 16.15 logs this History with the same statements and
values and labels its parameter lists `parameters: ` where 17 writes
`Parameters: `. The audit reads 16's label as 17's, so the same predicate holds
on both, and a changed value is refused under either label. PostgreSQL 15.19
logs it the same way as 16.15.

This reading validates the parser and the allowed diagnostic control. It does
not establish that a later approval-refusal scenario passed. That result
requires its complete server journal window, exact migration/Job/Pod
identities, and the matching freshly approved Apply.
