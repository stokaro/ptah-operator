# PostgreSQL migration history journal

The JSONL reading contains the 17 received SQL records from the successful
History control in run `020-recovery-pg-01679c74`, Kubernetes 1.37.0 on Linux
amd64, recorded on 2026-09-29. PostgreSQL's JSON logging collector produced
the records. Only the database, client address, SQL and parameter fields are
retained; SQL and parameter bytes are unchanged.

- Operator source: `01679c74f24f554de9fddcb577bb36a1d6f70b0f`
- Ptah source: `f6e562c5b0986cd29a53a5cc01938827336b780a`
- History Job UID: `62f5acc8-e754-4677-9dd9-d59d219b4af0`
- Pod UID: `79ab9aa4-ba76-4b85-8faa-f6e10837d43e`
- Client address: `10.244.3.97`
- Isolated database: `ptah_e2e_retarget`

The permitted statements are reviewed against the pinned Ptah implementation:

- [Connection detection](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/dbschema/connection.go) reads the server version, current schema and TimescaleDB presence. The PostgreSQL driver's ping sends `-- ping`.
- [Metadata ownership](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/migration/migrator/metadata_owner.go) checks ownership of the exact default revision table.
- [History initialization](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/migration/migrator/migrator.go) creates the empty default table if absent, checks its version type and inspects its bookkeeping columns.
- [History selection](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/migration/migrator/revisions.go) reads the existing revision rows in order.

The predicate allows the exact SQL and bound parameters for this fixture.
Additional statements, changed table/schema/column parameters, arbitrary
SELECT or WITH statements, and failed unauthorized SQL are refused. The
table creation is an explicit permitted History effect, not evidence of
zero SQL or arbitrary DDL permission.

This reading validates the parser and the allowed diagnostic control. It
does not establish that a later approval-refusal scenario passed. That result
requires its complete server journal window, exact migration/Job/Pod
identities, and the matching freshly approved Apply.
