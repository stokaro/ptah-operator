# A plan of notes

`schema-plan-postgres-16.json` is what Ptah v0.33.0 printed for
`ptah schema plan --schema-file desired.sql --output plan.json --json`, and
`saved-plan-postgres-16.json` is the plan it saved, both stored byte for byte.
The image was `ghcr.io/stokaro/ptah@sha256:933cc38f93375e0a83ff49db0cfdcee301ab328e95d13496fb2b2be82be1f906`,
the one `support/ptah.json` pins, and the server was
`postgres:16-alpine@sha256:cbf42f0edc946c0bc1b8eeed62403734e1f25dc67a3491455d07a92fdb558b28`
on an amd64 host. Against the plan v0.32.0 saved, and v0.31.0 before it, only
the plan's `from_fingerprint`, and with it its name and digest, moved: the note,
the statements and the `to_fingerprint` are the same. The v0.32.0 image run the
same day against a fresh server still printed and saved the files this
replaces byte for byte.

The database held a table whose generated column the schema file gives another
expression:

```sql
-- in the database
CREATE TABLE tasks (id integer PRIMARY KEY, title text, code text GENERATED ALWAYS AS (upper(title)) STORED);
-- in desired.sql
CREATE TABLE tasks (id integer PRIMARY KEY, title text, code text GENERATED ALWAYS AS (lower(title)) STORED);
```

PostgreSQL 16 has no `ALTER COLUMN ... SET EXPRESSION`, so the report says
`no-changes`, its `plan_digest` is the SHA-256 of the saved plan, and the plan
holds one note and no statement. `schema apply --plan` of it, with and without
`--dry-run`, exited 0 and left the expression as it was. Against
`postgres:17-alpine` the same files plan
`ALTER TABLE "public"."tasks" ALTER COLUMN "code" SET EXPRESSION AS (lower(title))`.

These files are not under `internal/dataplane/testdata`, because every file
there is a document a runner harness row prints, and
`TestCommittedCapturesHaveNoStrayFile` refuses any other. The harness runs
PostgreSQL 17 only, and no row against it can print this plan.
