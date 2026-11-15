The two small plan documents were generated on 2026-11-14 by the Ptah
v0.28.0 release image
`ghcr.io/stokaro/ptah@sha256:34356290733b9c7ce526312b8ce89f753c85a04c9c187d71b4cbd63af0080977`
(commit `2ee460a688d63f34e853b1eee4228a2463a6baa8`), for arm64. On 2026-11-15
the v0.29.0 release image
`ghcr.io/stokaro/ptah@sha256:1a33d8ef8b6f06226d6ebd202b8ef8620227d7d3346b66a5991afd04844e17e8`
(commit `75fa9991177e9049b66fe2e37abec0db40f47568`), run the same way, wrote
both documents byte for byte, so they stay. Later that day the v0.29.2 release
image
`ghcr.io/stokaro/ptah@sha256:f3272308b3d28008f9d574690fe1543637c3f4c09f60028437ae27780d719d2a`
(commit `ba500e11b2536ad7dad1720e5cbd9bf7292dba8b`) ran the same command through
the lab of `support/qualification/probes/schema_sql_readings.py` rather than the
setup these were written in. It wrote the PostgreSQL document
byte for byte, and the MySQL one with only the plan name and the
`from_fingerprint` changed, which identify the database the plan read. The
v0.29.0 image, run through the same lab, wrote both documents byte for byte as
v0.29.2 did. Each run was
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
differs only in the plan name and the fingerprints. The v0.28.0 image does the
same again: its MySQL document is byte-identical to the v0.23.0 one, and its
PostgreSQL one differs only in the plan name and the fingerprints.

The SQL source contains 32 `<` bytes in defaults. PostgreSQL uses one TEXT
column. MySQL distributes the total over 100 VARCHAR(16000) columns in a fixed
table set: the pinned source parser cannot accept the expression required by
a MySQL TEXT default. All native plans are non-destructive CREATE TABLE
statements, with no padding or edited plan bytes in the CLI probes.

Adding one `<` byte increases the saved native JSON by six bytes; adding one
`x` increases it by one. With v0.22.0, v0.23.0, v0.28.0, v0.29.0 and v0.29.2 the PostgreSQL envelope is
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
