The two small plan documents were generated on 2026-11-12 by the Ptah
v0.23.0 release image
`ghcr.io/stokaro/ptah@sha256:653100970980ff77edadbf6fa0cbf0fa26b0f179a6e2130a50ed2259f3c33489`
(commit `f47b2bfa3b696c93730e7d25397987ed0a4a2562`). It ran
`ptah schema plan --schema-file <source> --output <plan> --json` against empty
PostgreSQL 17.11 and MySQL 8.4.11 databases named `plan_size`, from the images
`support/databases.json` pins for those lines, and the source is what
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
byte sizes exactly; only the plan name and the fingerprints differed. The v0.22.0
image reproduces both documents' statements and adds the `scope` Ptah has recorded
since v0.19.0, so they measure 779 bytes on PostgreSQL and 23,373 on MySQL, where
the 64 bytes the MySQL document gains include the name of the database it read,
`plan_size`. A longer name is a longer plan, so the cluster scenario gives its five
databases names of one length (`planSizeDatabase`); a name one byte shorter than
the first measured a repeated byte at 5 bytes instead of 6, and stopped the row.
The v0.23.0 image reproduces both documents' statements and byte sizes exactly.
The MySQL document is byte-identical to the v0.22.0 one; the PostgreSQL one
differs only in the plan name and the fingerprints.

The SQL source contains 32 `<` bytes in defaults. PostgreSQL uses one TEXT
column. MySQL distributes the total over 100 VARCHAR(16000) columns in a fixed
table set: the pinned source parser cannot accept the expression required by
a MySQL TEXT default. All native plans are non-destructive CREATE TABLE
statements, with no padding or edited plan bytes in the CLI probes.

Adding one `<` byte increases the saved native JSON by six bytes; adding one
`x` increases it by one. With v0.22.0 and v0.23.0 the PostgreSQL envelope is
587 bytes and the MySQL envelope is 23,181 bytes; with v0.17.0 they were 549 and
23,117, and before it 592 and 27,517. At v0.23.0 the cluster scenario, which
calibrates from real plans, passed its exact 8 MiB and 8 MiB + 1 byte rows on
both engines on Kubernetes 1.35, 1.36 and 1.37 in CI run 43241757619. Full plans
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
