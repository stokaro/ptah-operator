# Migration log readings

`migration-log-<engine>-<kind>.txt` hold what the acceptance row's query printed
for `schema_migrations_log` after one `ptah migrations up --migrations-dir
<fixtures> --json` against an empty database, with the repository's own
`testdata/e2e/migrations/<engine>` fixtures. Ptah wrote every row. Captured on
2026-11-14 on the `vcluster-mac` Docker host with the clients the phase reads
through, `psql -Atqc` and `mysql -Nse`. Each run was its own container, with
Ptah as its first process, so every run id starts with the process ID 1. The
PostgreSQL runs connected as the server's own user, the MySQL runs as the
application user `ptah_e2e`, which holds every privilege on its database, as
the lifecycle's login does.

- Executor: the Ptah v0.28.0 release image,
  `ghcr.io/stokaro/ptah@sha256:34356290733b9c7ce526312b8ce89f753c85a04c9c187d71b4cbd63af0080977`,
  source `2ee460a6`, for arm64
- PostgreSQL: `17.11`, `postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`
- MySQL: `8.4.11`, `mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb`

PostgreSQL query:

```sql
SELECT concat(run_id, ':', version, ':', state, ':', actor_source, ':', encode(convert_to(coalesce(actor, ''), 'UTF8'), 'hex'), ':', encode(convert_to(coalesce(actor_origin, ''), 'UTF8'), 'hex'), ':', encode(convert_to(coalesce(run_reference, ''), 'UTF8'), 'hex')) FROM schema_migrations_log ORDER BY seq
```

MySQL query:

```sql
SELECT concat(run_id, ':', version, ':', state, ':', actor_source, ':', hex(coalesce(actor, '')), ':', hex(coalesce(actor_origin, '')), ':', hex(coalesce(run_reference, ''))) FROM schema_migrations_log ORDER BY seq
```

The run reference is `sha256:0ec6a440c2633fe95ed0d9b16a34c3c65509dfeb2d81031a40e8202918171748`
wherever a run was given one: a value of the form the operator's operation IDs
take.

| Kind | What the run was given | What it wrote |
| --- | --- | --- |
| `approved` | `PTAH_ACTOR=oidc:Jos%C3%A9 Garc%C3%ADa %24(HOME)`, the operator's encoding of `oidc:José García $(HOME)`, `PTAH_ACTOR_ORIGIN=admission` and `--run-reference` | that value, source `provided`, origin `admission`, and the reference |
| `unencoded` | `PTAH_ACTOR=oidc:José García $(HOME)`, which the operator does not send, with the same origin and reference | that name in UTF-8, on a database that can store it, with the origin and the reference |
| `no-reference` | the encoded `PTAH_ACTOR` alone, as a 0.2.12 Job gives it | that value, source `provided`, no origin and no reference |
| `no-actor` | nothing, as every operator run before 0.2.12 | no name, source `unknown` |
| `process-user` | `USER=ptah` and no actor | `ptah`, source `process-user` |

The Job the operator builds sets `PTAH_ACTOR` and `PTAH_ACTOR_ORIGIN`, and the
runner passes `--run-reference`; these runs set them by hand, because what they
record is what Ptah does with the values and no cluster was involved. The
acceptance rows in `test/e2e/migrations_lifecycle_e2e_test.go` and
`test/e2e/migrations_isolation_e2e_test.go` prove the operator's half.
