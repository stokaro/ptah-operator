# A table revoke that a database grant keeps

`schema-plan-mysql-8.4.json` is what Ptah v0.35.1 printed on standard output
for `ptah schema plan --output /tmp/plan.json --json`, with `PTAH_SCHEMA_FILE`
naming `desired.sql`, and `schema-plan-mysql-8.4.stderr` what it printed on
standard error, both stored byte for byte. It exited 2. The image was the
v0.35.1 release image that `support/ptah.json` pins,
`ghcr.io/stokaro/ptah@sha256:4b66856f2d0780ff697155575c97e900b7b7654d8141c6368ee77f79db2c90d5`,
and the server was
`mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb`,
MySQL 8.4.11, as root, on an arm64 host, on November 24, 2026.

The database `wide` held the two tables `desired.sql` declares, created with
the same statements, and the role `wide_reader`, created with
`CREATE ROLE wide_reader` and granted `SELECT ON wide.*`. A REVOKE on one table
cannot take a privilege granted on the whole database: MySQL answers it with
ERROR 1147, and the role keeps reading the table. v0.35.1 refuses the declared
revoke with the code `declaration-refused` and names the grant to revoke
instead (stokaro/ptah#6492 and #6512). `ptah schema validate --dialect=MySQL`
on the same file, the check the runner makes next, printed nothing and exited
0 with the same image.

`schema-plan-mysql-8.4-v0.35.0.json` and `schema-plan-mysql-8.4-v0.35.0.stderr`
are the same command run with the v0.35.0 release image,
`ghcr.io/stokaro/ptah@sha256:f76221499b83c232d737894daea53f8c3e1ebad20d6602f05b88ffae1134913a`,
against a fresh server of the same image prepared the same way, on the same
day, stored byte for byte. It exited 0 with the outcome `no-changes`: the
schema read as in sync while `wide_reader` kept `SELECT` on `t3`.

These files are not under `internal/dataplane/testdata`, because every file
there is a document a runner harness row prints, and
`TestCommittedCapturesHaveNoStrayFile` refuses any other. No harness row
declares a MySQL role.
