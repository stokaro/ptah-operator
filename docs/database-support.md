# Database support and privileges

The initial operator support contract is deliberately narrower than every
database that the Ptah CLI can address. A database release line is supported by
the operator only when the complete OCI, observation, planning, approval,
Apply, failure-recovery, and convergence lifecycle is green on every supported
Kubernetes minor.

| Engine | Supported release line | Matrix image |
| --- | --- | --- |
| PostgreSQL | 17.x | `postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73` |
| MySQL | 8.4.x LTS | `mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb` |

The immutable references identify the current matrix environments; they do
not restrict production to one image vendor. Other release lines are
unsupported until they receive explicit matrix rows. The CRD selects an engine
but intentionally does not trust a user-supplied server version. The executor
discovers server capabilities from the live connection and refuses unsupported
DDL instead of rendering for an asserted version.

Database support is also scoped to the Ptah executor build exercised by the
matrix. Installation requires both its digest-pinned image and an explicit
version identity verified from that image's provenance. The chart has no
executor-version default and cannot silently assign an unrelated version to a
different digest; both values remain part of every plan and applied binding.

## PostgreSQL authority

Use a dedicated login that is the owner of the managed objects, or grant it
membership in a dedicated no-login owner role. PostgreSQL does not provide a
general `ALTER` privilege for objects owned by another role, so object
ownership is the reliable least-authority boundary.

The login needs only:

- `CONNECT` on the target database;
- `USAGE` on each managed schema;
- the ability to create objects in each managed schema;
- ownership of every object the desired schema may alter or drop;
- catalog visibility needed to inspect those objects.

Grant extension, role, or cross-schema authority only when the desired artifact
actually manages those object kinds. Keep the login out of superuser and
cluster-wide administrative roles. Session advisory locking does not justify
broadening its DDL authority.

## MySQL authority

Use a dedicated account scoped to the one managed database. Grant only the DDL
verbs required by the desired object kinds. A typical table-and-index schema
needs `SELECT`, `CREATE`, `ALTER`, `DROP`, `INDEX`, and `REFERENCES` on that
database. Add `CREATE VIEW` and `SHOW VIEW`, `TRIGGER`, `EVENT`, or routine
privileges only if those objects are managed.

Do not grant global administration, user-management, file, replication, or
grant-option privileges. The operator rejects credential-bearing principal DDL
and MySQL connection parameters that can inject session SQL. `multiStatements`
is always rejected. Conventional IPv6 MySQL URLs must include an explicit port;
the network DSN form remains supported without one.

## Proving a custom privilege set

Privilege requirements depend on the exact desired object kinds. Validate a
custom role against a non-production database with the same engine release and
artifact before using it in production. The proof must include observation,
plan dry-run, one representative Apply, and a subsequent no-change plan. A
permission error after Apply dispatch is outcome-unknown and requires the
normal read-only convergence proof; it is not safe evidence that nothing
changed.
