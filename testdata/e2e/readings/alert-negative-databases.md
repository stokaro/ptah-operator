# Ordinary policy-control databases

These four readings were collected after the failed `ordinary-policy-waits`
scenario on 2026-10-03, Kubernetes 1.37.0, Linux amd64, Docker context
`diabolocom`. Runtime source: `3e6dd38e660b43a50c529f2045ee879fc72143b9`.
Ptah source: `f6e562c5b0986cd29a53a5cc01938827336b780a`.

The isolated Schema databases are empty. Each Migration database contains only
an empty default `schema_migrations` base table. The pinned History operation
may initialize that table; [the existing History audit](postgresql-migration-history-audit.md)
documents the exact contract. Application tables and recorded revision rows
are not permitted without Apply.

The reading validates the database predicate. It does not turn the failed
native attempt into a pass or establish a complete alert or SQL-audit verdict.
