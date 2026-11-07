# Documents of Ptah f47b2bfa3

Every other file in this directory is a document Ptah printed at commit
`f47b2bfa3b696c93730e7d25397987ed0a4a2562`, the commit Ptah v0.23.0 is to be
tagged on, copied byte for byte. The build ran from `git archive` of that commit
and reported version `0.23.0-pre.f47b2bfa3`. It ran on November 7, 2026
against PostgreSQL 17.11 and MySQL 8.4.11. The `drift-column-*`,
`plan-declaration-refused-*`, `validate-*` and `migrations-up-unknown-*` files
are what `make runner-harness-write` captured from that build. The
`*-permission-denied-*`, `migrations-up-commit-failed*` and `plan-partition-*`
files are what the `ptah` binary in that image printed when run directly with
`--json` and the environment a Job gives it.

The operator still pins Ptah v0.22.0 (`support/ptah.json`), so the runner
harness cannot write these documents yet, and nothing under
`internal/dataplane/testdata` may hold a document the harness did not write.
The decoder, runner and controller rows that read these files prove that this
operator reads what v0.23.0 prints, before the pin moves.

The repin replaces them: once the tag exists and `support/ptah.json` names it,
`make runner-harness-write` captures the same documents from the release image,
the rows read those captures, and this directory goes.

- `drift-column-types-narrowed-*.json`, `drift-column-identities-removed-postgres.json`:
  `schema drift` from the harness rows `differs-precision-postgres`,
  `differs-precision-mysql` and `differs-identity-postgres`, with the two drift
  categories v0.23.0 adds.
- `migrations-up-commit-failed*.json`: `migrations up` on PostgreSQL, where a
  deferred foreign key fails at commit. The plain files ran in the default mode,
  the `tx-all` files with `--tx-mode all`; `second` is the run whose second
  migration failed.
- `migrations-up-unknown-mysql-ddl.json`: `migrations up` on MySQL from the
  harness row `statement-errors/my-unique-up`, where `CREATE UNIQUE INDEX` failed
  with error 1062 and the run reports `unknown`.
- `plan-declaration-refused-*.json` and `validate-*.txt`: `schema plan` refusing a
  desired schema with an invalid foreign key, and what `schema validate` printed
  for the same file.
- `*-permission-denied-*.json`: `schema drift` and `schema plan` as MySQL accounts
  that cannot see every trigger, routine or view in the database.
- `plan-partition-*.json`: the plans `schema plan --output` saved for five
  changes to a partition of a PostgreSQL table declared in SQL.
