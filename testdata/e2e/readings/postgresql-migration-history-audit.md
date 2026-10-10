# PostgreSQL migration history journal

The JSONL reading contains the 9 received SQL records of a successful Ptah
`migrations status --migrations-dir /migrations --json` invocation, captured on
2026-10-10 by `support/qualification/probes/schema_sql_readings.py --part
migrations`. PostgreSQL's JSON logging collector produced the records. Only the
database, client address, SQL and parameter fields are retained; SQL and
parameter bytes are unchanged. The database already held the repository's first
two PostgreSQL migrations, so History reads an existing revision table; its
statements include everything it sends to an empty database, where it stops
after asking whether the table exists.

- Ptah source: `eb69f8c4435643c5c9f3fbd1c9953b9ba0f44b6f`, the v0.13.0 release image `support/ptah.json` pins
- Server image: `postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`
- Client address: `10.244.3.97`
- Isolated database: `ptah_e2e_retarget`

The database name and client address are the ones the earlier reading carried
from its cluster run, so the tests' identities stay. Run with the executor built
from the previous pin on an empty database, the same probe reproduced that
earlier 17-record reading exactly.

The permitted statements are reviewed against the pinned Ptah implementation:

- [Connection detection](https://github.com/stokaro/ptah/blob/eb69f8c4435643c5c9f3fbd1c9953b9ba0f44b6f/dbschema/connection.go) reads the server version, current schema and TimescaleDB presence. The PostgreSQL driver's ping sends `-- ping`.
- [Metadata ownership](https://github.com/stokaro/ptah/blob/eb69f8c4435643c5c9f3fbd1c9953b9ba0f44b6f/migration/migrator/metadata_owner.go) checks ownership of the exact default revision table.
- [The migrator](https://github.com/stokaro/ptah/blob/eb69f8c4435643c5c9f3fbd1c9953b9ba0f44b6f/migration/migrator/migrator.go) counts the default table in `information_schema.tables` and reads its columns from `SELECT * FROM "schema_migrations" WHERE 1 = 0`.
- [History selection](https://github.com/stokaro/ptah/blob/eb69f8c4435643c5c9f3fbd1c9953b9ba0f44b6f/migration/migrator/revisions.go) reads the existing revision rows in order.

v0.13.0's History no longer creates the table or checks its version type and
columns. The predicate keeps those statements, because the Apply it also
governs still sends them, and adds the two reads above. Additional statements,
changed table/schema/column parameters, arbitrary SELECT or WITH statements,
and failed unauthorized SQL are refused.

This reading validates the parser and the allowed diagnostic control. It does
not establish that a later approval-refusal scenario passed. That result
requires its complete server journal window, exact migration/Job/Pod
identities, and the matching freshly approved Apply.
