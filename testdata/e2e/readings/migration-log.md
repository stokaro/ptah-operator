# Migration log readings

`migration-log-<engine>-<kind>.txt` hold what the acceptance row's query printed
for `schema_migrations_log` after one `ptah migrations up --migrations-dir
<fixtures> --json` against an empty database, with the repository's own
`testdata/e2e/migrations/<engine>` fixtures. Ptah wrote every row. Captured on
2026-11-06 with the clients the phase reads through, `psql -Atqc` and
`mysql -Nse`.

- Executor: the Ptah v0.18.0 release image,
  `ghcr.io/stokaro/ptah@sha256:9f8797986124add44d0232a17cd17e6f03cfec76b1711efc53c169483b7b4303`,
  source `6e1d430e`
- PostgreSQL: `17.11`, `postgres@sha256:67f41722b7a8cbdb868a44a4995c846eddfdc2973bccb291ce937dce88ad5675`
- MySQL: `8.4.13`, `mysql@sha256:a0225d1ea6631c8bc2385dc4c2faba17bbf1834ecf369e1659a9a382bd8d5dd7`

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
