# A generated column of another kind

`schema-plan-postgres-18.json` is what Ptah v0.35.0 printed on standard output
for `ptah schema plan --schema-file desired.sql --output plan.json --json`, and
`schema-plan-postgres-18.stderr` what it printed on standard error, both stored
byte for byte. It exited 2. The image was the v0.35.0 release image that
`support/ptah.json` pins,
`ghcr.io/stokaro/ptah@sha256:f76221499b83c232d737894daea53f8c3e1ebad20d6602f05b88ffae1134913a`,
and the server was
`postgres:18-alpine@sha256:898c60cb390d1085bc97bdd6a1ae968757421df394b071848b52b1c09c89d6a5`,
PostgreSQL 18.6, as its superuser, on an arm64 host, on November 22, 2026.

The database held the table `desired.sql` declares, created with that same
statement. PostgreSQL 18 makes a column declared `GENERATED ALWAYS AS (expr)`
with no kind `VIRTUAL` (`attgenerated` read `v`), and Ptah reads the same
declaration in a schema file as `STORED`, so the plan would have to change the
column's kind, which no statement does in place. v0.35.0 refuses it with the
code `generated-kind` and lists the column under `generated_columns`
(stokaro/ptah#6466). The v0.34.0 release image, run beside it, exited 2 with the
outcome `failed` and no code.

These files are not under `internal/dataplane/testdata`, because every file
there is a document a runner harness row prints, and
`TestCommittedCapturesHaveNoStrayFile` refuses any other. The harness runs
PostgreSQL 17 only, and no row against it can print this refusal: PostgreSQL 17
has no virtual generated column.

`schema-plan-postgres-18-v0.34.0.json` and `schema-plan-postgres-18-v0.34.0.stderr`
are the same command run with the v0.34.0 release image,
`ghcr.io/stokaro/ptah@sha256:8be4bebe660fcc427028ce854f815f5e5ae24854134d80d6ed9f00ab3aaaa1dc`,
against the same server image, PostgreSQL 18.6, as its superuser, on an arm64
host, on November 23, 2026, stored byte for byte. It exited 2 with the outcome
`failed`, no refusal code and the same sentence. It read the same
`desired.sql`, the database held the table that file declares, created with that
statement, and `attgenerated` read `v`. The runner row that reads this capture
holds a failed plan with no code to its log line.
