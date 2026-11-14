# Schema apply log readings

`schema-apply-log-<engine>-<kind>.txt` hold what the acceptance row's query
printed for `ptah_schema_apply_log` after `ptah schema plan --schema-file
<file> --output plan.json` and `ptah schema apply --plan plan.json
--auto-approve --json` against an empty database, with the repository's own
`testdata/e2e/<engine>-v1.sql` and `-v2.sql`. Ptah wrote every entry. Captured
on 2026-11-14 on the `vcluster-mac` Docker host with the clients the phase reads
through, `psql -Atqc` and `mysql -Nse`, in the same session as the
[migration log readings](migration-log.md), from the same images. Each apply
was its own container, with Ptah as its first process, so every run id starts
with the process ID 1. The PostgreSQL applies connected as the server's own
user. The MySQL applies connected as `ptah_e2e`, which holds every privilege on
its database, so it can create and write the log; the lifecycle's audit account
holds the same grant.

PostgreSQL query:

```sql
SELECT concat(run_id, ':', seq, ':', state, ':', actor_source, ':', encode(convert_to(coalesce(actor, ''), 'UTF8'), 'hex'), ':', encode(convert_to(coalesce(actor_origin, ''), 'UTF8'), 'hex'), ':', encode(convert_to(coalesce(run_reference, ''), 'UTF8'), 'hex'), ':', encode(convert_to(coalesce(plan_digest, ''), 'UTF8'), 'hex')) FROM ptah_schema_apply_log ORDER BY logged_at, run_id, seq
```

MySQL query:

```sql
SELECT concat(run_id, ':', seq, ':', state, ':', actor_source, ':', hex(coalesce(actor, '')), ':', hex(coalesce(actor_origin, '')), ':', hex(coalesce(run_reference, '')), ':', hex(coalesce(plan_digest, ''))) FROM ptah_schema_apply_log ORDER BY logged_at, run_id, seq
```

| Kind | What the applies were given | What they wrote |
| --- | --- | --- |
| `approved` | the v1 plan, `PTAH_ACTOR=kubernetes-admin`, `PTAH_ACTOR_ORIGIN=admission` and `--run-reference=sha256:9736d8870b28be7ed18bca37f8c9dc8986137ab1f85d4b3f8ec9f8c29f1b7916` | a started and an applied entry, each with that name, source `provided`, that origin, that reference and the plan's digest |
| `two-runs` | the same apply, then the v2 plan with `--run-reference=sha256:6207f4eb99a137c035a873d29b55b91bc1088fd25ae94954883b2664d778409f` | the first apply's two entries, then the second's under its own run id and reference |
| `no-attribution` | the v1 plan alone, as every operator Apply before 0.2.13 | the two entries with no name, source `unknown`, no origin and no reference |

The plan digests the entries name are the ones the apply documents reported:

| Engine | v1 | v2 |
| --- | --- | --- |
| PostgreSQL | `sha256:7fdab1f6673273a2b923f3a3a9bbeacdb78855aca6e46fcdf8cfbb6a025954ca` | `sha256:a62b3791fa07e943ab8b6fd4d84a53d46d0c2c5892e9ddbe753bfc41bd2ee47f` |
| MySQL | `sha256:af6d45fe3995e355fdb8f54e9601f521652f417ed0d529fe2278709eba6889d6` | `sha256:7730dcc98589fb2aada26d0fb9b68d8fcd6f71376c03bdea114a727a3ad83a69` |

The documents also report the reference: `apply_log` holds
`{"written": 2, "run_reference": "<the reference>"}` for each attributed apply,
and `{"written": 2}` with no reference for the apply that was given none.

The Job the operator builds sets `PTAH_ACTOR` and `PTAH_ACTOR_ORIGIN`, and the
runner passes `--run-reference`; these applies set them by hand, because what
they record is what Ptah does with the values and no cluster was involved. The
data-plane lifecycle in `test/e2e/dataplane_lifecycle_e2e_test.go` proves the
operator's half on both engines.
