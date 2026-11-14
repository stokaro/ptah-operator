# Migration log readings

`migration-log-<engine>-<kind>.txt` hold what the acceptance row's query printed
for `schema_migrations_log` after one `ptah migrations up --migrations-dir
<fixtures> --json` against an empty database, with the repository's own
`testdata/e2e/migrations/<engine>` fixtures. Ptah wrote every row. Captured on
2026-11-14 with the clients the phase reads through, `psql -Atqc` and
`mysql -Nse`. Each run was its own container, so every run id starts with the
process ID 1.

- Executor: the Ptah v0.28.0 release image,
  `ghcr.io/stokaro/ptah@sha256:34356290733b9c7ce526312b8ce89f753c85a04c9c187d71b4cbd63af0080977`,
  source `2ee460a6`, for arm64. The v0.18.0 (`6e1d430e`), v0.22.0 (`0dd9190b`)
  and v0.23.0 (`f47b2bfa`) release images wrote the same rows in all eight runs,
  apart from the run id. v0.28.0 adds the `run_reference` and `actor_origin`
  columns, which these queries do not read
- PostgreSQL: `17.11`, `postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`
- MySQL: `8.4.11`, `mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb`

PostgreSQL query:

```sql
SELECT concat(run_id, ':', version, ':', state, ':', actor_source, ':', encode(convert_to(coalesce(actor, ''), 'UTF8'), 'hex')) FROM schema_migrations_log ORDER BY seq
```

MySQL query:

```sql
SELECT concat(run_id, ':', version, ':', state, ':', actor_source, ':', hex(coalesce(actor, ''))) FROM schema_migrations_log ORDER BY seq
```

| Kind | What the run was given | What it wrote |
| --- | --- | --- |
| `approved` | `PTAH_ACTOR=oidc:Jos%C3%A9 Garc%C3%ADa %24(HOME)`, the operator's encoding of `oidc:José García $(HOME)` | that value, source `provided` |
| `unencoded` | `PTAH_ACTOR=oidc:José García $(HOME)`, which the operator does not send | that name in UTF-8, source `provided`, on a database that can store it |
| `no-actor` | nothing, as every operator run before this row | no name, source `unknown` |
| `process-user` | `USER=ptah` and no actor | `ptah`, source `process-user` |

The Job the operator builds sets `PTAH_ACTOR`; these runs set it by hand, because
what they record is what Ptah does with the value and no cluster was involved.
The acceptance row in `test/e2e/migrations_lifecycle_e2e_test.go` proves the
operator's half.
