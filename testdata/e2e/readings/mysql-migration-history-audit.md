# MySQL migration history journal

The JSONL reading contains all 34 general-log records for the isolated
`ptah_audit` account during a successful Ptah `migrations status
--migrations-dir /migrations --json` invocation. The server recorded them on
2026-09-29 at 20:31:42 UTC. The account has access only to the empty fixture
database. The fixture uses the repository's MySQL migration directory.

- MySQL: `8.4.11`
- Server image: `mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb`
- Executor image ID: `sha256:cc4fabbc0ad0d4dc6d1138df08456222232c0f38d7ef0eba4a7b40bd03946fd1`
- Ptah source: `f6e562c5b0986cd29a53a5cc01938827336b780a`
- Server container: `c8933cdeb3dd3accc155cc13fdc1556eb6832d17951c8b1b1d3e81619790dffc`
- History container: `e72da9483066329ddc7d5be21d6ec2efc036a924f11ea1c38fcce6a7dec9a745`
- Client address and thread: `172.19.0.3`, `11`
- Database: `ptah_audit_history`

The reading preserves event times, thread IDs, `user_host`, command types and
exact argument bytes encoded as hex. The 34 records comprise one Connect,
five Query, nine Prepare, nine Execute, nine Close stmt and one Quit. There
are 14 received SQL execution records; preparing a statement is not executing
it. No password-bearing account setup or unrelated administrator traffic is
included. The capture's containers, volumes and network were removed.

The exact statements and bound parameters are reviewed against the pinned
Ptah implementation:

- [Connection detection](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/dbschema/connection.go) reads the server version.
- [History initialization](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/migration/migrator/migrator.go) creates the empty default revision table and checks its engine, version type and bookkeeping columns.
- [History selection](https://github.com/stokaro/ptah/blob/f6e562c5b0986cd29a53a5cc01938827336b780a/migration/migrator/revisions.go) reads the existing revision rows. MySQL table inspection also reads the session foreign-key restriction and the default revision table's CREATE statement.

The refusal predicate checks the command type, exact SQL and expanded bound
parameters, authenticated account, connection lifecycle and exact Job/Pod
ownership. It does not grant a general SELECT or DDL exception. Creating the
empty revision table is the explicit permitted History effect.

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
