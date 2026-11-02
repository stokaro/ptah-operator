# PostgreSQL migration history journal

The JSONL reading contains the 10 received SQL records of a successful Ptah
`migrations status --migrations-dir /migrations --json` invocation, captured on
2026-11-02 by `support/qualification/probes/schema_sql_readings.py --part
migrations`. PostgreSQL's JSON logging collector produced the records. Only the
database, client address, SQL and parameter fields are retained; SQL and
parameter bytes are unchanged. The database already held the repository's first
two PostgreSQL migrations, so History reads an existing revision table; its
statements include everything it sends to an empty database, where it stops
after asking whether the table exists.

- Ptah source: `38bd6e91cfe3a70543046da9c3ada05a0460a103`, the v0.17.0 release image `support/ptah.json`
  now pins. The reading was first taken on 2026-10-10 with the v0.13.0 release image (`eb69f8c4`), and
  the v0.14.0, v0.15.0 and v0.16.0 images reproduced it record for record. The v0.17.0 image adds one
  record, the search-path read below, and changes no other
- Server image: `postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`
- Client address: `10.244.3.97`
- Isolated database: `ptah_e2e_retarget`

The database name and client address are the ones the earlier reading carried
from its cluster run, so the tests' identities stay. Run on an empty database
with the executor built from the pin before v0.13.0, Ptah commit `f6e562c5`
(`v0.9.0-73-gf6e562c5b`), the same probe reproduced that earlier 17-record
reading exactly.

The permitted statements are reviewed against the Ptah implementation of v0.14.0, and the search-path
read against v0.17.0:

- [Connection setup](https://github.com/stokaro/ptah/blob/38bd6e91cfe3a70543046da9c3ada05a0460a103/internal/connector/pgconnect/pgconnect.go) reads the session's search path, `SELECT s FROM unnest(current_schemas(false)) WITH ORDINALITY AS path(s, n) ORDER BY n` ([the query](https://github.com/stokaro/ptah/blob/38bd6e91cfe3a70543046da9c3ada05a0460a103/internal/schemaselection/schemaselection.go)), so it resolves an unqualified name in the order the server does. It reads session state and nothing else.
- [Connection detection](https://github.com/stokaro/ptah/blob/e046527f79c58651d2082dce65b5ceb48dbc2606/dbschema/connection.go) reads the server version, current schema and TimescaleDB presence. The PostgreSQL driver's ping sends `-- ping`.
- [Metadata ownership](https://github.com/stokaro/ptah/blob/e046527f79c58651d2082dce65b5ceb48dbc2606/migration/migrator/metadata_owner.go) checks ownership of the exact default revision table.
- [The migrator](https://github.com/stokaro/ptah/blob/e046527f79c58651d2082dce65b5ceb48dbc2606/migration/migrator/migrator.go) counts the default table in `information_schema.tables` and reads its columns from `SELECT * FROM "schema_migrations" WHERE 1 = 0`.
- [History selection](https://github.com/stokaro/ptah/blob/e046527f79c58651d2082dce65b5ceb48dbc2606/migration/migrator/revisions.go) reads the existing revision rows in order.

v0.13.0's History no longer creates the table or checks its version type and
columns. The predicate keeps those statements, because the Apply it also
governs still sends them, and admits the two reads v0.13.0 sends in their
place: the count of the default table in `information_schema.tables` and
`SELECT * FROM "schema_migrations" WHERE 1 = 0`. It admits v0.17.0's
search-path read as well. Additional statements, changed table/schema/column
parameters, arbitrary SELECT or WITH statements, and failed unauthorized SQL
are refused.

This reading validates the parser and the allowed diagnostic control. It does
not establish that a later approval-refusal scenario passed. That result
requires its complete server journal window, exact migration/Job/Pod
identities, and the matching freshly approved Apply.
