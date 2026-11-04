The two small plan documents were generated on 2026-11-02 by the Ptah
v0.17.0 release image
`ghcr.io/stokaro/ptah@sha256:181e279720916ed455bf9fe8f1620b8e0d71dd37cc127b3ae662317697c92548`
(commit `38bd6e91cfe3a70543046da9c3ada05a0460a103`). It ran
`ptah schema plan --schema-file <source> --output <plan> --json` against empty
PostgreSQL 17.11 and MySQL 8.4.11 databases, and the source is what
`e2e-handcraft-oci plan-size-schema <engine> 32 0` writes
(`test/e2e/handcraftoci`). These are standalone native CLI fixture
measurements; they do not supply Kubernetes or production acceptance.

The first fixtures came from an executor built from Ptah commit
`f6e562c5b0986cd29a53a5cc01938827336b780a` with this repository's
`Dockerfile.executor`. The v0.16.0 release image, run the same way, reproduced
their statements and byte sizes exactly; only the plan name and the
fingerprints, which identify the database the plan read, differed. v0.17.0 no
longer writes the `-- POSTGRES TABLE: <name> --` or `-- MYSQL TABLE: <name> --`
line in front of each `CREATE TABLE`, so its documents are shorter: 741 bytes
instead of 784 on PostgreSQL, and 23,309 instead of 27,709 on MySQL. On
2026-11-04 the v0.18.0 release image
`ghcr.io/stokaro/ptah@sha256:9f8797986124add44d0232a17cd17e6f03cfec76b1711efc53c169483b7b4303`
(commit `6e1d430e51312de325a8f0238e601547a6c473d5`), run the same way against
PostgreSQL 17.11 and MySQL 8.4.13, reproduced both documents' statements and
byte sizes exactly; only the plan name and the fingerprints differed.

The SQL source contains 32 `<` bytes in defaults. PostgreSQL uses one TEXT
column. MySQL distributes the total over 100 VARCHAR(16000) columns in a fixed
table set: the pinned source parser cannot accept the expression required by
a MySQL TEXT default. All native plans are non-destructive CREATE TABLE
statements, with no padding or edited plan bytes in the CLI probes.

Adding one `<` byte increases the saved native JSON by six bytes; adding one
`x` increases it by one. With v0.17.0 the PostgreSQL envelope is 549 bytes and
the MySQL envelope is 23,117 bytes, where they were 592 and 27,517. Full plans
generated from those envelopes measured exactly 8,388,608 and 8,388,609 bytes
on both engines, and each 8,388,608-byte plan applied from its saved file on a
fresh database. With the `f6e562c5` executor, native OCI-layout publication and
pull also preserved those sizes after canonical HCL conversion, publication ran
with a 256 MiB memory limit and native Plan and Apply with 512 MiB, PostgreSQL
executed its default in an inserted row, and MySQL retained all 100 exact
defaults. Those were not measured again with v0.17.0; the cluster scenario
covers them.

The predicate unit tests expand only these fixtures' default literals while
preserving the other native bytes. The predicate finds each table's statement
by the `CREATE TABLE` that opens it, quoted as the dialect quotes it, and not
by a comment line the planner may write or drop. The cluster scenario
calibrates with real operator-produced plans, publishes generated SQL, and
requires actual native Plan, storage, approval, projection and Apply evidence.
Its oversized refusal must report a saved file of exactly 8,388,609 bytes and
no executable storage, unauthorized SQL or Apply.
