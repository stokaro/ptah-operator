# A role membership of other options

`schema-drift-postgres-17.json` is what Ptah v0.35.0 printed on standard output
for `ptah schema drift --schema-file desired.yaml --format json`, stored byte
for byte but for the `sources` and `database_url` lines, which are removed as
they are from the other drift fixtures. It exited 1, drift found, with nothing
on standard error. The image was the v0.35.0 release image that
`support/ptah.json` pins,
`ghcr.io/stokaro/ptah@sha256:f76221499b83c232d737894daea53f8c3e1ebad20d6602f05b88ffae1134913a`,
and the server was
`postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`,
PostgreSQL 17.11, read as its superuser on an arm64 host on November 22, 2026.

The database held the table and both roles `desired.yaml` declares, with
`GRANT harness_readers TO harness_vault WITH ADMIN OPTION`. The declaration
states the membership with no option, which means the default, `admin` false,
so since v0.35.0 Ptah reports it under `role_memberships_modified`
(stokaro/ptah#6424). `schema plan` of the same file planned
`GRANT "harness_readers" TO "harness_vault" WITH ADMIN FALSE`.

A runner harness row cannot print this yet: every file under
`internal/dataplane/testdata` is a document a harness row prints, and no row
declares roles in a YAML schema.
