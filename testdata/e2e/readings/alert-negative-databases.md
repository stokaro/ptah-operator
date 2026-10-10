# Ordinary policy-control databases

These four readings describe the isolated databases of the `ordinary-policy-waits`
scenario after its ten-minute window: two Schema and two Migration resources
that observe and plan but never apply.

The first readings were collected on 2026-10-03, Kubernetes 1.37.0, Linux amd64,
with Ptah `f6e562c5`, whose History created an empty default `schema_migrations`
table. The executor is now the Ptah v0.13.0 release image, whose History asks
whether the revision table exists and never creates it: the isolated History
capture in [postgresql-migration-history-audit.md](postgresql-migration-history-audit.md)
shows a fresh database left untouched. The Migration readings were updated to
that state on 2026-10-10, so every database is empty. Application tables, a
created revision table and recorded revision rows are refused without Apply.

The reading validates the database predicate. The scenario logs the tables it
reads, which is where a native run confirms this state; it does not turn a
failed attempt into a pass or establish a complete alert or SQL-audit verdict.
