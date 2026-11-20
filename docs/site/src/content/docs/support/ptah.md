---
title: Ptah compatibility
description: Which Ptah build this operator runs, what has been verified, and what nobody has measured.
---

The operator does not contain Ptah. It runs one, as a job whose container image
and version identity the installation binds, so "which Ptah works with this
operator" is a question about that executor and not about the CLI a person used
to build the OCI artifact.

`support/ptah.json` is the machine-readable answer, and it is canonical: the
lifecycle suite takes the image it runs as the executor out of this file, and
the table below is generated from it.

## The table

Each operator version has a row in the summary and a section with the detail.
A declared range, a verified build and the absence of both mean different
things, and [the next section](#three-states-kept-apart) says what.

<!-- BEGIN GENERATED COMPATIBILITY -->
**Axis:** Operator version to the Ptah build the operator executes, identified by the digest-pinned executorImage and the ptahVersion bound beside it. It is not the Ptah CLI a person used to build the OCI artifact. The runner is not a third axis: it is built from the operator source this row names, and the exact protocol both halves speak is in runnerProtocolVersion.

**Last measured:** 2026-11-20.

| Operator | Declared range | Verified builds | Guide |
| --- | --- | --- | --- |
| `edge` | No range | `v0.34.0` | [edge](https://operator.ptah.run/edge/) |
| `v0.2.15` | No range | `v0.33.1` | [v0.2.15](https://operator.ptah.run/v0.2.15/) |
| `v0.2.14` | No range | `v0.29.2` | [v0.2.14](https://operator.ptah.run/v0.2.14/) |
| `v0.2.13` | No range | `v0.29.0` | [v0.2.13](https://operator.ptah.run/v0.2.13/) |
| `v0.2.12` | No range | `v0.26.0` | [v0.2.12](https://operator.ptah.run/v0.2.12/) |
| `v0.2.11` | No range | `v0.23.0` | [v0.2.11](https://operator.ptah.run/v0.2.11/) |
| `v0.2.10` | No range | `v0.23.0` | [v0.2.10](https://operator.ptah.run/v0.2.10/) |
| `v0.2.9` | No range | `v0.17.0` | [v0.2.9](https://operator.ptah.run/v0.2.9/) |
| `v0.2.8` | No range | `v0.16.0` | [v0.2.8](https://operator.ptah.run/v0.2.8/) |
| `v0.2.7` | No range | `v0.15.0` | [v0.2.7](https://operator.ptah.run/v0.2.7/) |
| `v0.2.6` | No range | `v0.14.0` | [v0.2.6](https://operator.ptah.run/v0.2.6/) |
| `v0.2.5` | No range | `v0.13.0` | [v0.2.5](https://operator.ptah.run/v0.2.5/) |
| `v0.2.4` | No range | `v0.13.0` | [v0.2.4](https://operator.ptah.run/v0.2.4/) |
| `v0.2.3` | No range | `v0.13.0` | [v0.2.3](https://operator.ptah.run/v0.2.3/) |
| `v0.2.2` | No range | `v0.13.0` | [v0.2.2](https://operator.ptah.run/v0.2.2/) |
| `v0.2.1` | No range | `v0.13.0` | [v0.2.1](https://operator.ptah.run/v0.2.1/) |
| `v0.2.0` | No range | `v0.13.0` | [v0.2.0](https://operator.ptah.run/v0.2.0/) |

### edge

The development state. Its guide is at [https://operator.ptah.run/edge/](https://operator.ptah.run/edge/).

**Declared:** No range is claimed. The chart in the source tree and the chart asset ship no executor default, and installing either requires both a digest-pinned executorImage and the ptahVersion verified from that image's provenance. The chart a release publishes to its OCI registry carries that release's pair, and refuses half an override. The supported build is exactly the one the lifecycle matrix exercised.

**Verified:**

- Ptah `v0.34.0`. Commit [`90e98f6ca0b3d77e66b0d5e72a6a9e22da20653a`](https://github.com/stokaro/ptah/commit/90e98f6ca0b3d77e66b0d5e72a6a9e22da20653a), runner protocol version 1, evidence `kubernetes-e2e`. The complete OCI resolution and verification, observation, planning, approval, apply, failure-recovery and convergence lifecycle against PostgreSQL and MySQL, on every supported Kubernetes minor, for both artifact formats this operator reads: application/vnd.stokaro.ptah.schema.v1 for a declared schema, whose declared rows converge on both engines, and application/vnd.stokaro.ptah.migrations.v1 for a versioned migration directory, whose history read, approval gate and applied sequence run against PostgreSQL and MySQL.

**Limitations:**

- The verified build is the release image of the Ptah version under Verified, pinned by digest and run as Ptah published it: the operator builds no executor of its own. It contains stokaro/ptah#3777 and #3794, the complete `--json` documents of `schema plan` and `schema apply` that the operator reads on every declared-schema Plan and Apply. An executor without #3777, v0.9.0 included, rejects `--json` with exit code 2.
- The verified build contains stokaro/ptah#5173, which names in every document why a command could not open its target database: plan contract 3, apply 4, migration status and run 2, and a drift report of version 1. The operator reads those versions and no others, so an executor without #5173, v0.17.0 included, fails every Observe, Plan, Apply and migration read on a document it refuses.
- The verified build contains stokaro/ptah#5499, which reads a PostgreSQL schema through pg_catalog for every role, so the login the databases page documents reports a table another role created, as a superuser does. An executor without #5499, v0.21.0 included, leaves that table out of the report under that login, and the data-plane row that creates one fails.
- The verified build contains stokaro/ptah#5545: a MySQL read that cannot see every trigger, routine or view definition in the database fails with permission_denied and names what it could not see, where an executor without #5545, v0.22.0 included, reported those objects as absent. The login the databases page documents holds TRIGGER, EXECUTE and SHOW VIEW on the database for that reason, and the narrower login it documented before fails every Observe and Plan at this build.
- The verified build contains stokaro/ptah#5954 and #5981: each PostgreSQL view statement of a saved plan records its effect, the view's security_invoker and security_barrier before the statement and after it, and the operator decides the security_invoker hold from it. An executor without them, v0.23.0 included, records none, and the operator reads the option from the SQL, so under apply: Always a plan that changes the body of a view that states security_invoker waits for an approval even when the option keeps its value. An operator that does not declare the field, 0.2.11 included, refuses every plan this build writes with a PostgreSQL view statement as invalid_plan_output.
- The verified build contains stokaro/ptah#6098 and #6099: each view effect of a plan that read the database also records before_plan, what the view held when the plan started, and each MySQL or MariaDB view statement records its SQL SECURITY mode as sql_security. The operator reads neither, so a PostgreSQL view a CASCADE may have taken is still held whatever it states, and every MySQL view a plan replaces is still raised as SecurityDefiner.
- The verified build contains stokaro/ptah#5902: a MySQL plan keeps the SQL SECURITY mode a declaration states. From v0.26.0 to v0.29.2 it replaces a view the database holds as SQL SECURITY INVOKER and the declaration leaves without a mode as a DEFINER view. A replacement that names no definer makes the login that runs it the view's definer whatever the mode, and the operator holds every MySQL and MariaDB CREATE OR REPLACE VIEW and ALTER VIEW for an approval under apply: Always.
- The verified build contains stokaro/ptah#6105: a MySQL plan that would replace a view the database holds as SQL SECURITY INVOKER, where the declaration names no mode, is refused with the code view-security-option, which the Plan reports as plan_refused at every retry while an Observe reports views_modified. An executor without #6105, v0.29.2 included, replaces such a view as a DEFINER view.
- The verified build contains stokaro/ptah#6109 and #6124: a MySQL or MariaDB plan that would replace a view running as its definer before and after the change keeps that definer. Where the plan's login is the definer, the replacement names it in a DEFINER clause, which the operator raises as Definer beside SecurityDefiner. Where another account is, the plan is refused with the code view-definer, which the Plan reports as plan_refused at every retry, naming each view, its definer and the login, while an Observe reports views_modified. An executor without them, v0.29.3 included, replaces such a view as the login and makes the login its definer.
- The verified build contains stokaro/ptah#6124 and #6126: before the first statement, schema apply and migrations up read the login's grants and refuse a statement that names another account as a definer the grants show the login may not name. On MySQL that takes SET_ANY_DEFINER, with ALLOW_NONEXISTENT_DEFINER as well for an account the server lacks, and SUPER, SET_USER_ID and ALL PRIVILEGES pass too; on MariaDB it takes SET USER, and a login that holds only SUPER is left to the server. A migration run refused there applies none of its files and writes no revision row, and the Pod log names the file, the definer, the login and the privilege. A schema Apply meets it only where the plan's login has come to match another MySQL account than the plan's read did, as 'ptah'@'%' and 'ptah'@'10.244.1.%' can from Pods on different nodes: the plan names no definer but the account its read ran as, and the runner refuses an Apply under another login before Ptah runs. Such an Apply ends as apply_refused with the code definer-privilege, and the Pod log carries Ptah's sentence. The check reads every file of a run together and reads the executable comments mysqldump writes as the server does, so a definer inside /*!50013 ... */ is refused before the first file too. It does not ask whether a definer exists where a file of the run creates or renames an account or a role, and on MariaDB it leaves DEFINER=CURRENT_ROLE and a host-less definer to the server; there, and wherever the grants do not decide, the server decides, and a statement it refuses with error 1227 leaves the files before it applied. Measured on MySQL 8.4 with each release image's migrations up --tx-mode none, v0.29.4, which lacks #6126, refused before its first file a run whose first file creates the definer a later view names, which v0.29.5 applies, and ran the file before a view whose definer sits in /*!50013 ... */, which the server then refused with error 1227. An executor without #6124, v0.29.3 included, runs the files before such a statement, and the server refuses that statement with error 1227.
- The verified build contains stokaro/ptah#6119: a saved plan's fingerprint records of each table's rows only whether it holds any, so rows written to a table that holds some leave a waiting plan valid, and the first row of an empty table still makes it stale. Measured on PostgreSQL 17 with the release images, a plan v0.29.3 saved against a table with rows is stale under v0.29.4 with no write in between; the execution binding retires every such plan before an Apply could use it. An executor without #6119, v0.29.3 included, reported its own plan stale after 2,000 rows and an ANALYZE.
- The verified build contains stokaro/ptah#6118 and #6123: a PostgreSQL plan that removes a table whose policy reads a view the plan rebuilds drops the policy first, and the rebuild's drops carry no CASCADE. An executor without them, v0.29.3 included, writes DROP VIEW ... CASCADE, which takes the policy, and the Apply stops at DROP POLICY with SQLSTATE 42704 and rolls back.
- The verified build contains stokaro/ptah#6129, #6131 and #6136: the PostgreSQL read selects the schema of the function each trigger runs and names a function outside the schema the read resolves names in with that schema, which a declaration naming the function without a schema still matches. The function Ptah generates for a trigger is the trigger's own in whatever schema holds it, so a plan that removes the trigger drops the function there, and one that replaces the trigger keeps it there rather than creating a second one. The trigger query of every PostgreSQL schema reading moved with #6129, and no plan, drift or apply document of the runner harness moved. An executor without them, v0.29.4 included, reads the function's name alone.
- The verified build contains stokaro/ptah#6014: a schema Apply that sends a statement writes a started and an outcome entry to ptah_schema_apply_log in the target database, as the login in the schema's Secret, and creates the table on the first such Apply. Ptah's own reads leave the table out. A write the database refuses changes neither the outcome nor the exit status, and of the apply_log the apply document reports the operator reads only the run reference. The MySQL privileges the databases page documents hold no INSERT, so there the first such Apply creates the table and loses its entries, and later Applies write theirs once the login holds INSERT on the table, as the databases page shows. Every Apply of either family passes the claim's operation ID as --run-reference, with the approver or the apply-always name in PTAH_ACTOR and admission or policy in PTAH_ACTOR_ORIGIN, and the runner refuses a document whose entries carry another reference, so an entry names the approver, where the name came from and the operation. A migration log an earlier build created gains the nullable run_reference and actor_origin columns on the first run that writes to it. An executor without #6014, v0.26.0 included, keeps no apply log.
- The verified build contains stokaro/ptah#5994: `migrations up --check-drift` replays the applied history into the shadow database PTAH_SHADOW_DB names, compares it with the target under the migration lock before any pending migration, and reports drift_check in the run document. The operator passes it for a migration whose policy names a drift check, with one --drift-exclude for each selector policy.driftCheck.exclude names, so an object the migrations do not create and the list does not name reads as drift. An executor without #5994, v0.26.0 included, refuses the flag and writes no run document, so such an Apply applies nothing and is recorded Unknown until a history read settles it.
- Ptah v0.33.1 and v0.34.0 compare a view by its database-qualified name when `migrations up --check-drift` reads MySQL or MariaDB, and stokaro/ptah#6416 found the same of a stored function and, on MariaDB, a sequence, while tables and triggers compare by their own names. The shadow database always has a name other than the target's, since Ptah refuses a shadow named like the target as the target, so each such object an applied migration created reads as drift, a view as missing_view <shadow>.<view> beside extra_view <target>.<view>, and every later Apply of a PtahMigration whose policy names a drift check stops with SchemaDrifted and applies nothing, with a message that blames a change made outside the migrations. Measured on MySQL 8.4.11 with the v0.34.0 release image, a target s5 whose first migration created the view item_names, checked against the shadow s5_shadow on a server of its own, reported the drift_check outcome drifted with missing_view s5_shadow.item_names and extra_view s5.item_names; Ptah measured the same view on v0.33.1, and the function and the sequence on MySQL 8.4 and MariaDB 11.8. The operator measured no routine, no sequence and no MariaDB server, and nobody measured a stored procedure. A view named in policy.driftCheck.exclude reads as neither, and the check then compares nothing of it. The fix is stokaro/ptah#6416, which no Ptah release contains yet.
- The verified build contains stokaro/ptah#6052 and #6055: before a migration run applies any file, it asks every file it selected what the file's transaction or its own text refuses, such as a CREATE INDEX CONCURRENTLY inside the transaction the file runs in or transaction control in a file Ptah wraps in one, and a refusal there ends the run failed with nothing applied and no revision row. The run document does not say which file it refused, so the operator's message names a migration only when the run selected one, and the Pod log holds Ptah's sentence, which names the file. An executor without them, v0.28.0 included, applies the files before the refused one first, and for some refusals leaves that one failed with a dirty revision row.
- The verified build contains stokaro/ptah#6070: an exclude selector names a PostgreSQL row-level security policy through its table, as notes.notes_owner[type=policy], and one that names it by its schema and its own name, such as public.notes_owner[type=policy], names nothing, so a PtahSchema plans that policy, and a drift check compares it, as if no selector named it. An executor without #6070, v0.29.0 included, reads the second spelling as the policy and the first as nothing. Since stokaro/ptah#6069 a drift check warns on standard error about a selector that matched nothing. The runner reads no standard error of a migration Apply, so the warning reaches neither the resource nor the Pod log, as Ptah's warning about a schema plan's selector never has.
- The verified build contains stokaro/ptah#6097: on a PostgreSQL URL that pins no search_path, a plan that would create a table the declaration names without a schema in a current schema other than public, and drop the table of that name from another schema, is refused as declaration-refused, which the Plan reports as plan_refused. An executor without #6097, v0.29.2 included, plans the move and drops the other table with its rows.
- The verified build contains stokaro/ptah#6095: a PostgreSQL dev database's user mappings are listed through pg_user_mappings, so a Plan whose spec.dev connects as an ordinary role compares rather than failing with permission denied for table pg_user_mapping, as an executor without #6095, v0.29.2 included, does. The acceptance suites run no dev database, so this is Ptah's measurement.
- The verified build contains stokaro/ptah#6132, #6189 and #6204: on a PostgreSQL URL every statement of a plan names the schema it places each object in, and the saved plan records that schema as placed_schema, which the operator's plan decoder declares. A drift report names objects the same way, and the operator reads only its kinds of difference. The operator reports a fault of the schema file in the words of schema validate, which name a table without its schema. An executor before v0.30.0 writes the name alone, and an operator before 0.2.15 refuses a plan that carries placed_schema as a field it does not declare.
- The verified build contains stokaro/ptah#6166, #6196, #6198, #6199 and #6201: a PostgreSQL view or materialized view may declare its owner, and a plan that drops and makes again a view or materialized view another role owns gives it back with ALTER VIEW or ALTER MATERIALIZED VIEW ... OWNER TO, which the operator raises as Ownership. schema apply, its dry run included, refuses a plan that gives a view to a role the server lacks, one the login may not give it to, or one with no CREATE on the view's schema before any statement, with missing-owner-role, owner-transfer or owner-schema-privilege, and writes no schema apply log entry. schema plan does not check owners, so the Plan reads the code from the schema apply dry run it runs on the saved plan, and names the statement and the role. v0.30.0 reports such a refusal with no code, and writes the CREATE of a rebuild that gives a view back with no effect, which the operator holds as SecurityDefiner.
- The verified build contains stokaro/ptah#6165 and #6175: a plan that finds a difference it cannot plan, such as a generated column's new expression before PostgreSQL 17, saves notes and no statement and reports no-changes with the plan's digest and path. The Plan reads it as DifferenceNotPlannable and publishes nothing. A plan's annotations are read as notes its digest covers.
- The verified build reads an R__<identity>.sql file of a migration directory as a repeatable migration and lists it in the status (stokaro/ptah#6156, #6170 and #6181). From 0.2.16 the operator plans a new or changed repeatable migration after the pending versions, binds its content digest into the plan an approval names, and passes it in --expect-sequence, which Ptah holds the run to; a repeatable migration whose last attempt failed or recorded no outcome stops the resource as RepeatableMigrationUnfinished, while an applied record no file carries stops nothing: the history names it and a RepeatableMigrationOrphaned Warning reports it once. 0.2.15 runs a versioned sequence only, and refuses a history that lists a repeatable migration as RepeatableMigrationsUnsupported before any plan. An executor before v0.30.0 reads no such file.
- The verified build contains stokaro/ptah#6205: the owner check of schema apply reads the role statements of the plan before each OWNER TO, so a membership the plan grants counts toward the transfer and a role it drops or renames counts as missing. A role the plan creates counted as present at v0.30.1 already. Measured on PostgreSQL 17, a SQL schema cannot state a role membership at either version, and the owner rows of the runner harness read as they did.
- The verified build contains stokaro/ptah#6215: the record of repeatable migrations keeps the body of each one that applied, in a column the record v0.30.x creates lacks, and migrations status reads that column wherever the record exists. Measured on PostgreSQL 17 and MySQL 8.4, neither v0.30.1 nor v0.31.0 creates the record for a directory without an R__ file, and operator releases before 0.2.16 refuse a history that lists one before any run, so none of them has made such a record.
- The verified build contains stokaro/ptah#6216: a PostgreSQL schema may declare a logical replication publication. Ptah manages publications from the first one a schema declares. Until then it plans nothing for the ones the database holds and lists them under not_compared, which the operator does not read; from then on a plan drops each one the schema does not declare, which Ptah rates destructive. The operator raises no privilege change for a publication statement. Ptah reads publications only when the read covers every schema of the database. Measured on PostgreSQL 17, under a URL that pins search_path in a database with another schema, a declared publication is left out of every plan, and schema drift reports it as undecided with failed true and drift false. The schema then reads DriftUndecided: DriftDetected is Unknown, InSync and Ready are False, no plan is published, and Ready and a Warning Event name the publication and Ptah's reason.
- The verified build contains stokaro/ptah#6219: every command that compares a desired document checks its indexes after it places names and before its selection, so a PostgreSQL plan on a URL that pins no search_path names the table of an index fault with its schema, as drift has since v0.30.1. The operator reports the fault in the words of schema validate, which name the table without one. The runner passes spec.policy.exclude to schema plan only, and measured on PostgreSQL 17 a plan that excludes the table of a faulty index stopped on the fault at v0.31.0 as it does at v0.32.0, so the selector change of schema drift reaches no operator command.
- The verified build contains stokaro/ptah#6223: a migration file whose first bytes are a UTF-8 byte order mark runs without the mark, a directive on its first line is read, and the revision row records the checksum of the text without the mark. Measured on PostgreSQL 17 and MySQL 8.4, v0.29.2 and v0.31.0 send the mark to the server, which refuses the first statement, so such a file never applied: its failed row reads HistoryDirty at either version, and once that row is removed v0.32.0 applies the file. A row ptah migrations repair --force recorded at v0.31.0 or earlier holds the checksum with the mark, so v0.32.0 reads that migration as modified and the resource is Blocked with HistoryModified until ptah migrations repair --version N --force writes the row again. A file with a mark and a no_transaction directive reports transaction_mode none in the status, where v0.31.0 reported the default.
- The verified build contains stokaro/ptah#6266, #6277 and #6287: a PostgreSQL column's collation is written and compared, a MySQL column's declared collation is compared, and Ptah rates a collation change a warning. Measured on PostgreSQL 17.11 and MySQL 8.4.11 with the release images, v0.32.0 creates a PostgreSQL column the declaration gives a COLLATE without it and reads no drift, and v0.33.0 reads columns_modified on that unchanged database and plans ALTER TABLE ... ALTER COLUMN ... TYPE ... COLLATE; a column that holds a collation the declaration does not state is planned back to the database default. The operator rates every ALTER COLUMN ... TYPE and MODIFY COLUMN destructive whatever Ptah rates it, so allowDestructive: false blocks such a plan, and with allowDestructive the plan waits for an approval under apply: Always; it raises no privilege change. A MySQL column's declared COLLATE, a database default collation other than the server's and a table's CHARSET and COLLATE read no drift at either version, and a MySQL column whose collation differs from the declared one reads drift only at v0.33.0. The v0.33.1 release image reads each of these as v0.33.0 does.
- The verified build contains stokaro/ptah#6247, #6256 and #6264: an exclude selector that names a constraint keeps it. Measured with schema plan --exclude on PostgreSQL 17.11 and MySQL 8.4.11, v0.32.0 plans to drop a primary key or a UNIQUE constraint named with [type=index], the index of a UNIQUE constraint named with [type=constraint], a CHECK named with [type=check] and, on PostgreSQL, an EXCLUDE constraint under every selector, and on MySQL plans MODIFY COLUMN on the columns of an excluded key; v0.33.0 plans none of them. An untyped selector on a UNIQUE or CHECK constraint, and [type=constraint] on a CHECK, keep the constraint at both versions.
- The verified build contains stokaro/ptah#6232: migrations up --allow-dirty refuses a revision whose statement outcome is unknown, or whose file has a different statement count than the run recorded, and names ptah migrations repair --version N --force, where v0.32.0 named the repair without --force, which refuses that row. Measured on PostgreSQL 17.11 and MySQL 8.4.13; every other repair refusal the recovery page quotes reads as at v0.32.0, and no operator message quotes either form.
- The verified build contains stokaro/ptah#6248, #6259, #6272 and #6284: a column default in parentheses or built from operators is read, and MySQL expression defaults are compared by operator precedence. Measured on PostgreSQL 17.11 and MySQL 8.4.11, v0.32.0 refuses a schema file that writes DEFAULT (1 + 2) on either engine, and reads a MySQL datetime column it created with DEFAULT CURRENT_TIMESTAMP or now() as drift, planning MODIFY COLUMN at every Plan, which the operator rates destructive; v0.33.0 reads the same database as in sync. The PostgreSQL defaults v0.32.0 accepts read the same at both versions.
- The verified build contains stokaro/ptah#6250 and #6254: a PostgreSQL plan that changes the type of a column a stored generated column reads drops the generated column, changes the type and adds the column back with its indexes, after a guard SELECT that fails when the drop would take something the plan does not restate. Measured on PostgreSQL 17.11, v0.32.0 plans ALTER COLUMN ... TYPE, which the server refuses for such a column, and v0.33.0's plan applied; the operator rates its DROP COLUMN destructive.
- The verified build contains stokaro/ptah#6233: the sentence for a PostgreSQL foreign key whose referenced columns no key covers goes on to say that a primary key, UNIQUE constraint or unique index must key exactly those columns. The operator reports it under desired_schema_invalid as schema validate prints it, and the runner harness holds the longer sentence. From 0.2.16 it stops the PtahSchema Blocked with SchemaInvalid and reads the file again at its interval; 0.2.15 tries the Observe or Plan again.
- The verified build contains stokaro/ptah#6234: a migration Apply asks whether the revision table exists and sends CREATE TABLE IF NOT EXISTS only for a table the catalog does not show. Measured with the readings probe on PostgreSQL 17.11 and MySQL 8.4.11, the refused restored-history Apply sends that question where v0.32.0 sent the CREATE, the question History already asks, so the acceptance audits admit it; on MySQL it adds two protocol records.
- The verified build contains stokaro/ptah#6313: a migration directory is read whole or refused. Measured with migrations status against an in-memory SQLite database, the read a History makes of the files alone, the v0.29.2 and v0.33.0 release images leave out a top-level .sql file that a directory of Ptah's ten-digit files does not name, such as seed.sql, notes.sql, a name with .UP.sql, .DOWN.sql or .SQL, a version of fewer than ten digits and a macOS ._ file, and run the migrations beside it; in a directory read as Atlas migrations they leave out a .sql file that starts with no version and read R__x.sql as a versioned migration. v0.33.1 refuses each such directory and names the files. A file that does not end in .sql and a subdirectory are still ignored, and a down file without its up file is still refused. The operator reports the refusal of a History as desired_source_unreadable and stops the PtahMigration Blocked with MigrationDirectoryUnreadable, where it tried the read again at each failure retry. ptah migrations pull refuses such an artifact as well at v0.33.1, so under 0.2.15 a PtahMigration with an OCI source stops at its fetch-migrations step instead, and is tried again without the file named; from 0.2.16 the fetch writes the artifact unread and the History read names the file.
- The verified build contains stokaro/ptah#6406: ptah migrations pull --skip-validation writes an OCI migration artifact without reading its files as migrations, and still checks the manifest, the artifact type, the format annotation, each layer and each file's name and path. From 0.2.16 the fetch-migrations step of a History and of an Apply passes it, so the History read, in the container that holds no registry credential, names a file the directory's format does not read, and the PtahMigration stops Blocked with MigrationDirectoryUnreadable. The runner harness measures it with the v0.34.0 release image on an artifact published without ptah migrations push that holds a migration and a seed.sql: the pull exits 0 and says it did not validate the directory, and the History reports desired_source_unreadable naming seed.sql. An artifact that holds no SQL file is still refused at the pull, with migration artifact contains no migration files, so such a PtahMigration still fails at its fetch-migrations step with the step and its exit code alone in status, and is tried again at each failure retry; only kubectl logs job/<job> -c fetch-migrations names the reason. An executor before v0.34.0 refuses the flag with unknown flag: --skip-validation.
- The verified build contains stokaro/ptah#6388: a directory of Ptah's ten-digit files that holds two files for one direction of a version is refused, and the files are named. Ptah measured that earlier releases applied the one that loaded last and never ran the other. Measured with migrations status against an in-memory SQLite database, v0.33.1 lists version 2 of a directory holding 0000000002_create_b.up.sql and 0000000002_create_c.up.sql once, as pending, and v0.34.0 refuses it with duplicate up migration for version 2: 0000000002_create_b.up.sql and 0000000002_create_c.up.sql. The operator reports the refusal of a History as desired_source_unreadable and stops the PtahMigration Blocked with MigrationDirectoryUnreadable, whatever versions the database records.
- The verified build contains stokaro/ptah#6299 and #6345: a PostgreSQL schema file that gives a date or time column a relative time literal for its default, 'now', 'today', 'tomorrow' or 'yesterday', is refused as a fault of the file, because the server reads the literal once, when it creates the table. Measured on PostgreSQL 17.11 with the release images, v0.29.2 creates such a column with the moment it ran as the default, v0.33.0 reads that as drift and plans ALTER COLUMN ... SET DEFAULT 'now' at every Plan, and v0.33.1 refuses the file at its line and column. The operator reports desired_source_unreadable with Ptah's sentence. From 0.2.16 it stops the PtahSchema Blocked with SchemaSourceUnreadable and reads the file again at its interval; 0.2.15 tries the Observe or Plan again.
- The verified build contains stokaro/ptah#6345, whose refusal of a relative time default also covers a PostgreSQL column typed by a domain the file declares over a date or time type, as created_at moment DEFAULT 'now' after CREATE DOMAIN moment AS timestamp. Measured on PostgreSQL 17.12 with both release images, Ptah v0.33.1 and v0.34.0 refuse it as a fault of the schema: schema validate names the column's type as the file does, on a moment column, and schema drift and schema plan with the domain's schema, on a public.moment column. From 0.2.16 the operator allows for that schema, reports desired_schema_invalid with schema validate's sentence, stops the PtahSchema Blocked with SchemaInvalid and reads the file again at its interval, and a runner harness row holds the readings; 0.2.15 does not tie the two sentences, so an Observe fails as child_exit and a Plan as plan_refused with the code declaration-refused, each tried again at every failure retry.
- The verified build contains stokaro/ptah#6386: the refusal of a relative time default also covers a quoted type name, as "pg_catalog"."timestamptz" DEFAULT 'now', an array of a date or time type, as date[] DEFAULT '{now}', and a domain's own default, as CREATE DOMAIN moment AS timestamp DEFAULT 'now'. Measured with schema validate --dialect=postgres, v0.33.1 accepts all three and v0.34.0 refuses each; v0.34.0 accepts ('now'::text)::date, which the server evaluates for each row, where v0.33.1 refused it. The quoted type and the array are faults of the file, in the words v0.33.1 uses for a column, which the operator reports as desired_source_unreadable with Ptah's sentence. The domain is a fault of the schema, named domain "moment" by schema validate and domain "public.moment" by schema drift and schema plan. The operator allows for the schema drift and plan place a domain in, as it does for a table, and reports desired_schema_invalid with schema validate's sentence, which stops the PtahSchema Blocked with SchemaInvalid and reads the file again at its interval. Runner harness rows hold all three readings.
- The verified build contains stokaro/ptah#6342, #6356 and #6370: a saved plan is written for a MySQL session whose sql_mode does not name NO_BACKSLASH_ESCAPES, and schema apply --plan refuses, before any statement, a plan with a backslash in a literal on a session whose sql_mode does. Ptah reads the session's sql_mode and does not set it, and the operator refuses a sql_mode parameter in a MySQL URL, so on a server whose global sql_mode names NO_BACKSLASH_ESCAPES every Plan of such a schema fails in its dry run. Under 0.2.15 with v0.33.1 it fails with invalid_plan_output: plan validation reported failed rather than a dry run, and neither the condition nor the Pod log carries Ptah's sentence; from 0.2.16 with v0.34.0 it fails with plan_refused and the code backslash-mode. Measured on MySQL 8.4.11 with the release images, v0.33.0 on such a server stored DEFAULT 'C:\\temp' with its backslash doubled, and v0.29.2 and v0.33.0 on a server in the default mode wrote COMMENT 'a\\b' with one backslash, which MySQL reads as an escape, and then read the column as drift at every Plan. v0.33.1 writes the comment as declared, so its first plan against such a column is a MODIFY COLUMN, which the operator rates destructive, and the column then reads in sync.
- The verified build contains stokaro/ptah#6400: schema apply --plan --json, with and without --dry-run, on a MySQL or MariaDB session whose sql_mode reads a plan's backslashes differently, prints the outcome refused with the refusal code backslash-mode, where v0.33.1 printed failed with a message alone. From 0.2.16 the operator reads the dry run's refusal as it reads an owner refusal: the Plan fails with plan_refused: ptah refused to plan the change (backslash-mode), and Ptah's sentence, the session's sql_mode names NO_BACKSLASH_ESCAPES, and a statement holds a backslash, goes to the Pod log. An approved plan whose Apply meets a server that took the mode after the Plan fails with apply_refused and the same code, before any statement. The runner harness measures both with the v0.34.0 release image on MySQL 8.4.11, against a server of its own whose global sql_mode it sets. Ptah's schema apply --dev-url rehearsal refuses the same way, and no operation the operator runs reaches it.
- The verified build contains stokaro/ptah#6375: Ptah does not model MySQL or MariaDB partitioning (stokaro/ptah#6357), and refuses a declaration that partitions a table, which v0.33.1 planned without its partitions. Measured on MySQL 8.4.11 with the release images, schema validate accepts a table declared with PARTITION BY RANGE at v0.33.1 and refuses it at its line and column at v0.34.0, and schema drift of a table partitioned outside Ptah, against a declaration without the partitions, finds no drift at either, where v0.34.0 also lists the table under not_compared. A runner harness row holds the operator to Ptah's words, which it reports as desired_source_unreadable. The operator does not read not_compared, so a PtahSchema whose database holds a partitioned table reads InSync and nothing in its status says the partitions were not compared. A second row reads a MySQL table partitioned outside Ptah and holds the read to no drift and to the one not_compared entry, table partitioning differs_events, so a release that reports the partitions as drift, or stops listing them, moves it. Each MySQL schema read also asks information_schema.PARTITIONS which tables are partitioned, a statement the SQL audit admits.
- The verified build contains stokaro/ptah#6382: a MySQL or MariaDB plan creates a view after the views it reads, where earlier releases created new views in the order of their names. Measured on MySQL 8.4.11 with the release images, schema apply of a table and two views, vo_a reading vo_b, against an empty database fails at v0.33.1 with Error 1146 (42S02): Table 'vo_old.vo_b' doesn't exist and succeeds at v0.34.0, which creates vo_b first. A Plan's dry run sends no statement of the plan, so under v0.33.1 only the Apply met the error.
- The verified build contains stokaro/ptah#6381: a MySQL or MariaDB CHECK, generated column and expression default are compared in the form the server stores them. Measured on MySQL 8.4.11 with the release images, after schema apply of a table with the expression default concat('caf', 'é'), a generated column concat(name, ' it''s') and CHECK (name <> 'naïve'), a second plan at v0.33.1 modified both columns and dropped and added the CHECK, which the operator rates destructive, and at v0.34.0 found the schema in sync.
- The verified build contains stokaro/ptah#6390, #6392 and #6397: a MySQL schema file that declares two CHECK constraints of one name, which the server keeps one of per schema, is refused, and CHECKs of one name are compared by what they test. Measured with schema validate --dialect=mysql, v0.33.1 accepts a table declaring CONSTRAINT ck_qty CHECK (qty > 0) and CONSTRAINT ck_qty CHECK (qty < 100), and v0.34.0 refuses it, naming both. The folding of equal CHECKs and of CHECK names beyond ASCII (#6392 and #6397) was not measured here.
- The verified build contains stokaro/ptah#6353: a PostgreSQL column default keeps, when it is compared, a cast that can change its value. Measured on PostgreSQL 17.11 with the release images, a numeric column whose default the database holds as 1.5, against a declaration of DEFAULT (1.5::integer), which stores 2, reads in sync at v0.33.1 and as columns_modified at v0.34.0, so a PtahSchema in that state that read InSync under 0.2.15 reports the drift after the upgrade.
- The verified build contains stokaro/ptah#6391 and #6395: a plan records under omissions each declaration its statements leave out, such as a MySQL table option planned for PostgreSQL, and each command that plans warns of them on stderr. The runner harness measures two with the v0.34.0 release image: a PostgreSQL plan of a table declared with ENGINE=InnoDB records table public.differs_omitted: table option ENGINE beside the commented statement -- POSTGRES: table option ENGINE=InnoDB is not supported by this target; skipped., and a MySQL plan of an index declared with fillfactor records index differs_omitted_name: index storage parameter fillfactor. The operator's strict plan decoder accepts omissions and decides nothing from them, and the runner reads no stderr of a plan, so no condition or Event names an omission; the stored plan keeps them as Ptah wrote them. Ptah also stops reading a comment it writes from declared text as a directive (#6402) and keeps a literal that holds a delimiter directive line intact (#6405); neither was measured here.
- The verified build contains stokaro/ptah#6312, #6320 and #6341: a PostgreSQL generated column's expression is compared in the form the server stores, and PostgreSQL 18 VIRTUAL generated columns are read and planned. Measured with the release images, a stored generated column v0.29.2 created from qty * price + 1 or lower('L-' || id::text) reads as drift at v0.29.2 and v0.33.0, and v0.33.0 plans ALTER COLUMN ... SET EXPRESSION at every Plan on PostgreSQL 17.11 and 18, and saves a plan of notes on 16.15, which the operator reads as DifferenceNotPlannable; v0.33.1 reads the column in sync on all three. On PostgreSQL 18, v0.33.0 refuses a schema that declares a VIRTUAL generated column, and v0.33.1 plans it and reads it back in sync. Ptah reads a generated column declared without a kind as STORED, so a column the server holds as VIRTUAL, which PostgreSQL 18 makes of GENERATED ALWAYS AS without STORED, fails the Plan of a schema that declares it without a kind, and the runner reports child_exit.
- The migration machine contracts -- `migrations status --json` carrying contract_version and a per-migration record, and `migrations up --json` carrying an outcome -- reached Ptah after v0.3.0-201, and `migrations up --expect-sequence`, which the operator passes on every migration Apply, reached it in v0.9.0. A build older than v0.9.0 resolves and verifies a migration artifact and then fails at its own history document or at the flag every migration Apply passes, so the versioned workflow needs at least v0.9.0 rather than any build that has the commands.
- The verified scope is the operator's database support window, the release lines on that version's databases page. Other engines Ptah addresses are unverified here, whatever the Ptah build.
- PostgreSQL 18.x is evidenced by the Added database lines workflow rather than by the lifecycle matrix every push runs: run 42440618274 passed on every supported Kubernetes minor at Ptah v0.23.0.
- PostgreSQL 16.x is evidenced by the Added database lines workflow rather than by the lifecycle matrix every push runs: run 43292445627 passed on every supported Kubernetes minor at Ptah v0.23.0.
- Run 43574056611 of the same workflow passed both lines on every supported Kubernetes minor at Ptah v0.26.0.
- Run 43687878804 of the same workflow passed both lines on every supported Kubernetes minor at Ptah v0.29.0.
- Run 43780370228 of the same workflow passed both lines on every supported Kubernetes minor at Ptah v0.29.2.
- Run 44640143851 of the same workflow passed both lines on every supported Kubernetes minor at Ptah v0.33.1.
- The published verified set is a repository document. It is not compiled into the manager, and nothing compares a running executor's ptahVersion against it, so a combination outside this set is outside what the matrix measured rather than something the operator refuses. What is refused before a database is mutated is a change of any bound execution component between the plan and the apply -- Ptah, the executor image, the runner protocol version, the controller-state version -- checked before the Job is created. The manager's image and revision and the runner image are recorded and not bound: a manager release that changes only them applies the plans and approvals its predecessor left. What is refused after a Job has run is a runner speaking another protocol version, which is a rejected result frame rather than a prevented mutation. Pinning both the executor image and its ptahVersion at install is a requirement on whoever installs, not a check at runtime.

### v0.2.15

A released version. Its guide is at [https://operator.ptah.run/v0.2.15/](https://operator.ptah.run/v0.2.15/).

**Declared:** the same as [`edge`](#edge).

**Verified:**

- Ptah `v0.33.1`. Commit [`f7c85bdc579c9e5939108a892f11e3d6d0974248`](https://github.com/stokaro/ptah/commit/f7c85bdc579c9e5939108a892f11e3d6d0974248), runner protocol version 1, evidence `kubernetes-e2e`. The scope is the same as for [`edge`](#edge).

**Limitations:** the same as [`edge`](#edge) without its number 38, number 39, number 42, number 44, number 45, number 46, number 47, number 48, number 49 and number 50 limitations.

### v0.2.14

A released version. Its guide is at [https://operator.ptah.run/v0.2.14/](https://operator.ptah.run/v0.2.14/).

**Declared:** the same as [`edge`](#edge).

**Verified:**

- Ptah `v0.29.2`. Commit [`ba500e11b2536ad7dad1720e5cbd9bf7292dba8b`](https://github.com/stokaro/ptah/commit/ba500e11b2536ad7dad1720e5cbd9bf7292dba8b), runner protocol version 1, evidence `kubernetes-e2e`. The scope is the same as for [`edge`](#edge).

**Limitations:** the same as [`edge`](#edge) without its sixth, eighth, ninth, tenth, eleventh, twelfth, thirteenth, sixteenth, nineteenth, twentieth, twenty-first, twenty-second, twenty-third, twenty-fourth, twenty-fifth, twenty-sixth, twenty-seventh, twenty-eighth, twenty-ninth, thirtieth, number 31, number 32, number 33, number 34, number 35, number 36, number 37, number 38, number 39, number 40, number 41, number 42, number 43, number 44, number 45, number 46, number 47, number 48, number 49, number 50, number 51 and number 59 limitations.

### v0.2.13

A released version. Its guide is at [https://operator.ptah.run/v0.2.13/](https://operator.ptah.run/v0.2.13/).

**Declared:** the same as [`edge`](#edge).

**Verified:**

- Ptah `v0.29.0`. Commit [`75fa9991177e9049b66fe2e37abec0db40f47568`](https://github.com/stokaro/ptah/commit/75fa9991177e9049b66fe2e37abec0db40f47568), runner protocol version 1, evidence `kubernetes-e2e`. The scope is the same as for [`edge`](#edge).

**Limitations:** the same as [`edge`](#edge) without its sixth, eighth, ninth, tenth, eleventh, twelfth, thirteenth, sixteenth, eighteenth, nineteenth, twentieth, twenty-first, twenty-second, twenty-third, twenty-fourth, twenty-fifth, twenty-sixth, twenty-seventh, twenty-eighth, twenty-ninth, thirtieth, number 31, number 32, number 33, number 34, number 35, number 36, number 37, number 38, number 39, number 40, number 41, number 42, number 43, number 44, number 45, number 46, number 47, number 48, number 49, number 50, number 51, number 58 and number 59 limitations.

### v0.2.12

A released version. Its guide is at [https://operator.ptah.run/v0.2.12/](https://operator.ptah.run/v0.2.12/).

**Declared:** the same as [`edge`](#edge).

**Verified:**

- Ptah `v0.26.0`. Commit [`1cf0563c0951fbf67f13f9b4ca7f9b49bbe2f3a3`](https://github.com/stokaro/ptah/commit/1cf0563c0951fbf67f13f9b4ca7f9b49bbe2f3a3), runner protocol version 1, evidence `kubernetes-e2e`. The scope is the same as for [`edge`](#edge).

**Limitations:** the same as [`edge`](#edge) without its sixth, eighth, ninth, tenth, eleventh, twelfth, thirteenth, fourteenth, fifteenth, sixteenth, seventeenth, eighteenth, nineteenth, twentieth, twenty-first, twenty-second, twenty-third, twenty-fourth, twenty-fifth, twenty-sixth, twenty-seventh, twenty-eighth, twenty-ninth, thirtieth, number 31, number 32, number 33, number 34, number 35, number 36, number 37, number 38, number 39, number 40, number 41, number 42, number 43, number 44, number 45, number 46, number 47, number 48, number 49, number 50, number 51, number 57, number 58 and number 59 limitations.

### v0.2.11

A released version. Its guide is at [https://operator.ptah.run/v0.2.11/](https://operator.ptah.run/v0.2.11/).

**Declared:** the same as [`edge`](#edge).

**Verified:**

- Ptah `v0.23.0`. Commit [`f47b2bfa3b696c93730e7d25397987ed0a4a2562`](https://github.com/stokaro/ptah/commit/f47b2bfa3b696c93730e7d25397987ed0a4a2562), runner protocol version 1, evidence `kubernetes-e2e`. The scope is the same as for [`edge`](#edge).

**Limitations:** the same as [`edge`](#edge) without its fifth, sixth, seventh, eighth, ninth, tenth, eleventh, twelfth, thirteenth, fourteenth, fifteenth, sixteenth, seventeenth, eighteenth, nineteenth, twentieth, twenty-first, twenty-second, twenty-third, twenty-fourth, twenty-fifth, twenty-sixth, twenty-seventh, twenty-eighth, twenty-ninth, thirtieth, number 31, number 32, number 33, number 34, number 35, number 36, number 37, number 38, number 39, number 40, number 41, number 42, number 43, number 44, number 45, number 46, number 47, number 48, number 49, number 50, number 51, number 56, number 57, number 58 and number 59 limitations.

### v0.2.10

A released version. Its guide is at [https://operator.ptah.run/v0.2.10/](https://operator.ptah.run/v0.2.10/).

**Declared:** the same as [`edge`](#edge).

**Verified:** the same as [`v0.2.11`](#v0211).

**Limitations:** the same as [`edge`](#edge) without its fifth, sixth, seventh, eighth, ninth, tenth, eleventh, twelfth, thirteenth, fourteenth, fifteenth, sixteenth, seventeenth, eighteenth, nineteenth, twentieth, twenty-first, twenty-second, twenty-third, twenty-fourth, twenty-fifth, twenty-sixth, twenty-seventh, twenty-eighth, twenty-ninth, thirtieth, number 31, number 32, number 33, number 34, number 35, number 36, number 37, number 38, number 39, number 40, number 41, number 42, number 43, number 44, number 45, number 46, number 47, number 48, number 49, number 50, number 51, number 55, number 56, number 57, number 58 and number 59 limitations.

### v0.2.9

A released version. Its guide is at [https://operator.ptah.run/v0.2.9/](https://operator.ptah.run/v0.2.9/).

**Declared:** No range is claimed. The chart ships no executor default, installation requires both a digest-pinned executorImage and the ptahVersion verified from that image's provenance, and the supported build is exactly the one the lifecycle matrix exercised.

**Verified:**

- Ptah `v0.17.0`. Commit [`38bd6e91cfe3a70543046da9c3ada05a0460a103`](https://github.com/stokaro/ptah/commit/38bd6e91cfe3a70543046da9c3ada05a0460a103), runner protocol version 1, evidence `kubernetes-e2e`. The scope is the same as for [`edge`](#edge).

**Limitations:** the same as [`edge`](#edge) without its second, third, fourth, fifth, sixth, seventh, eighth, ninth, tenth, eleventh, twelfth, thirteenth, fourteenth, fifteenth, sixteenth, seventeenth, eighteenth, nineteenth, twentieth, twenty-first, twenty-second, twenty-third, twenty-fourth, twenty-fifth, twenty-sixth, twenty-seventh, twenty-eighth, twenty-ninth, thirtieth, number 31, number 32, number 33, number 34, number 35, number 36, number 37, number 38, number 39, number 40, number 41, number 42, number 43, number 44, number 45, number 46, number 47, number 48, number 49, number 50, number 51, number 54, number 55, number 56, number 57, number 58 and number 59 limitations.

### v0.2.8

A released version. Its guide is at [https://operator.ptah.run/v0.2.8/](https://operator.ptah.run/v0.2.8/).

**Declared:** the same as [`v0.2.9`](#v029).

**Verified:**

- Ptah `v0.16.0`. Commit [`91e28d4331950e949590fd9ac404373bc17b572d`](https://github.com/stokaro/ptah/commit/91e28d4331950e949590fd9ac404373bc17b572d), runner protocol version 1, evidence `kubernetes-e2e`. The scope is the same as for [`edge`](#edge).

**Limitations:** the same as [`v0.2.9`](#v029).

### v0.2.7

A released version. Its guide is at [https://operator.ptah.run/v0.2.7/](https://operator.ptah.run/v0.2.7/).

**Declared:** the same as [`v0.2.9`](#v029).

**Verified:**

- Ptah `v0.15.0`. Commit [`6ba0a93a44d3decd6782d88e57b88151edd4c788`](https://github.com/stokaro/ptah/commit/6ba0a93a44d3decd6782d88e57b88151edd4c788), runner protocol version 1, evidence `kubernetes-e2e`. The scope is the same as for [`edge`](#edge).

**Limitations:** the same as [`v0.2.9`](#v029).

### v0.2.6

A released version. Its guide is at [https://operator.ptah.run/v0.2.6/](https://operator.ptah.run/v0.2.6/).

**Declared:** the same as [`v0.2.9`](#v029).

**Verified:**

- Ptah `v0.14.0`. Commit [`e046527f79c58651d2082dce65b5ceb48dbc2606`](https://github.com/stokaro/ptah/commit/e046527f79c58651d2082dce65b5ceb48dbc2606), runner protocol version 1, evidence `kubernetes-e2e`. The scope is the same as for [`edge`](#edge).

**Limitations:** the same as [`v0.2.9`](#v029).

### v0.2.5

A released version. Its guide is at [https://operator.ptah.run/v0.2.5/](https://operator.ptah.run/v0.2.5/).

**Declared:** the same as [`v0.2.9`](#v029).

**Verified:**

- Ptah `v0.13.0`. Commit [`eb69f8c4435643c5c9f3fbd1c9953b9ba0f44b6f`](https://github.com/stokaro/ptah/commit/eb69f8c4435643c5c9f3fbd1c9953b9ba0f44b6f), runner protocol version 1, evidence `kubernetes-e2e`. The scope is the same as for [`edge`](#edge).

**Limitations:** the same as [`v0.2.9`](#v029).

### v0.2.4

A released version. Its guide is at [https://operator.ptah.run/v0.2.4/](https://operator.ptah.run/v0.2.4/).

**Declared:** the same as [`v0.2.9`](#v029).

**Verified:** the same as [`v0.2.5`](#v025).

**Limitations:** the same as [`v0.2.9`](#v029).

### v0.2.3

A released version. Its guide is at [https://operator.ptah.run/v0.2.3/](https://operator.ptah.run/v0.2.3/).

**Declared:** the same as [`v0.2.9`](#v029).

**Verified:** the same as [`v0.2.5`](#v025).

**Limitations:** the same as [`v0.2.9`](#v029).

### v0.2.2

A released version. Its guide is at [https://operator.ptah.run/v0.2.2/](https://operator.ptah.run/v0.2.2/).

**Declared:** the same as [`v0.2.9`](#v029).

**Verified:** the same as [`v0.2.5`](#v025).

**Limitations:** the same as [`v0.2.9`](#v029).

### v0.2.1

A released version. Its guide is at [https://operator.ptah.run/v0.2.1/](https://operator.ptah.run/v0.2.1/).

**Declared:** the same as [`v0.2.9`](#v029).

**Verified:** the same as [`v0.2.5`](#v025).

**Limitations:** the same as [`v0.2.9`](#v029).

### v0.2.0

A released version. Its guide is at [https://operator.ptah.run/v0.2.0/](https://operator.ptah.run/v0.2.0/).

**Declared:** the same as [`v0.2.9`](#v029).

**Verified:** the same as [`v0.2.5`](#v025).

**Limitations:** the same as [`v0.2.9`](#v029).

### Evidence

- `kubernetes-e2e`: The full lifecycle suite in test/e2e, run by the kubernetes-e2e job of .github/workflows/ci.yml on every pull request and on the weekly default-branch run. hack/e2e-kind.sh and the workflow both take the image from this file, so the Ptah build the suite runs and the claim published here are one declaration.
<!-- END GENERATED COMPATIBILITY -->

## Three states, kept apart

A compatibility table usually blurs three different claims. This one keeps them
in separate fields, because the difference is what a reader is actually asking
about.

**Declared** is a promise. `declared.range` names the Ptah versions an operator
version is meant to work with. Where it is `null`, nothing is promised, and
`declared.statement` says why.

**Verified** is a measurement. Each `verified` entry names a Ptah build the
operator was actually run against, the evidence behind it, and what ran.
`ptahRelease` is `null` when the build is not a released Ptah, and the table
then names the build by `ptahDescribe`. `ptahImage` is the Ptah release image
that ran, pinned by digest: the suite copies it unchanged and builds no
executor, so what the measurement covers is exactly that image. An operator
release names the same image, and
[Releases and provenance](../releases/#the-executor) says how it is checked
against this row.

**Absent** is neither. A combination no row mentions is untested, and untested
is not incompatible. A version with nothing verified carries
`unverifiedReason` naming the check that has not run; the verifier refuses an
empty list that says nothing, because an empty list reads as "works with
everything".

## What each field means

| Field | Meaning |
| --- | --- |
| `operator` | `edge` for the development state, or an exact `vMAJOR.MINOR.PATCH` release. |
| `stage` | `development` or `released`. |
| `documentation.published` | Whether a guide exists for this operator version. The table links to it only when it does. |
| `documentation.source` | The revision that guide is built from: `master` for the development state, and its own tag for a release. |
| `documentation.fixRevision` | An exact commit that replaces the tag as the build revision for one release. |
| `documentation.fixReason` | What that fix corrects. Required beside a fix revision. |
| `declared.range` | The promised Ptah versions, or `null`. |
| `declared.statement` | Why there is no range, when there is none. |
| `verified[].ptahRelease` | The released Ptah version, or `null` when the verified build is not a release. |
| `verified[].ptahCommit` | The exact Ptah commit the suite built its executor from. |
| `verified[].ptahDescribe` | What `git describe --tags --always` calls that commit in a complete checkout, so a reader sees something other than forty hex characters. A tag name carries no commit to compare, so the verifier accepts one only when it repeats this row's `ptahRelease`. |
| `verified[].runnerProtocolVersion` | The frame version the executor and the operator spoke in that run. Not a third axis — the runner is built from the operator source the row names — and checked against the constant rather than trusted. |
| `verified[].evidence` | A key into `evidence`, which says what ran and how the tested build and this claim stay one declaration. |
| `verified[].scope` | What the run covered. |
| `limitations` | Known constraints on the rows above. |

## One pin, not two

The image in `verified[].ptahImage` is the executor the lifecycle suite runs,
and `verified[].ptahCommit` is the commit it was built from. `hack/e2e-kind.sh`
reads both from this file, and the `kubernetes-e2e` job takes the commit from
the same place through the support job's output. The release workflow reads
them here too, through `hack/releaseverify`, when it names the executor in its
manifest. Nothing else may write them down:

```sh
make verify-ptah-support
```

The guard refuses a catalog that is malformed or self-contradictory, and it
refuses the lifecycle script, the CI or release workflow, or the executor
recipe carrying the commit a second time. A literal in any of them would keep
working on the day it and the catalog disagreed, and the table on this page
would then name a build nothing ran.

The command performs no network requests.

## Changing the claim

Changing which Ptah the suite verifies is one edit to `verified[].ptahCommit`
plus its `ptahDescribe`, and the matrix jobs then run against that build. Do not
raise `lastVerified` without a run behind it; the field records when the claim
was last measured, not when the file was last touched.

The drift finding categories the pinned build can emit are recorded in
`support/ptah-drift-categories.json`, with the digests of the Ptah files that
produce them: the drift command, the schema classifier and its feature
findings, the row summary and the undecided note. The record lists the fixed
categories and, separately, the qualified ones Ptah writes bare or followed by
`:<kind>`. The job that builds the shared task images compares the record
with the pinned source. A new pin that changed any of those files fails there
until the record is rewritten:

```sh
go run ./hack/ptahdriftcategories -ptah <Ptah checkout> -write
```

A unit test holds the operator's vocabulary to the recorded lists, because the
operator refuses a drift report that names a category it does not know. A
category the new build adds therefore joins the vocabulary, the `PtahSchema`
status pattern and the CRD schema version in the same change. The pattern keeps
every category an earlier release stored, so a summary written before an
upgrade still validates. A pin that leaves those files alone needs no edit.

Any change to the file changes the table, so regenerate it in the same commit:

```sh
cd docs/site
npm run compatibility:write
```

Adding an operator release adds a row with its own `verified` list. A release
that has not been run against any Ptah build gets `unverifiedReason` rather than
an empty list, and the table then shows a version nobody has measured instead of
a version that works with everything.

## Repinning Ptah

A repin moves `verified[].ptahImage` and the fields beside it to another Ptah
release. Do these in one change, in this order.

Make the catalog edit [Changing the claim](#changing-the-claim) describes, since
everything below reads the pin from it. Then regenerate the captures from the new
image, and read the diff:

```sh
DOCKER_CONTEXT=<context> make runner-harness-write
```

The target builds the runner from this commit and runs it inside the pinned
image, in a container with the shape of an operation Pod, against PostgreSQL,
MySQL, an OCI registry and the end-to-end Git server. It runs every runner
operation from OCI and Git sources, and the Git fetch before an operation, on
schemas that carry an added column, an extension, a policy on a new and on an
existing table, declared rows, a source it cannot load, and every target fault the
failure kinds name. It rewrites what changed under `internal/dataplane/testdata`: a document, the
arguments, an exit status, a line of standard error, a result frame. On a pin
that moved nothing it changes nothing, so the diff is what the release changed. A
document the operator's decoders refuse fails the run instead of being written:
fix the decoder first, and move any contract counter the
[API compatibility page](../api-compatibility/#contract-counters) asks for.

A verification's standard error is compared and never forwarded. On a refusal
`ptah oci verify` writes its report to standard output and the same findings to
standard error, and exits 2. The runner reads that as the policy's refusal, and
passes on the requirement codes alone, only when the text is what
`dataplane.VerifyRefusalDiagnostics` makes of the report; anything else reads as
`invalid_verification_output`. A release that rewords that sentence moves the
`schema-verify-refused` rows, and the capture fails until the function agrees.

Read the `differs-*` rows first. They run a database that differs from its
declaration, in the ways `differingRows` in `test/runnerharness/differing_test.go`
lists, because a comparison a release adds never fires on a database the operator
converged. Each records whatever its steps answer, so a moved outcome reaches the
diff. `go test ./test/runnerharness` then fails on a row whose reading no longer
holds the shape it is named for, or whose plan the manager now rates differently.
Decide whether the move is wanted, then change `want` beside the row. A plan the
manager reads as neither destructive nor a change of privileges runs unattended
under `apply: Always`, so read any row that moves there as a finding.

`make runner-harness` runs the same rows and compares. A change of shape is any
difference left once the volatile values are masked: a field added, removed or
retyped, an element added or removed, a contract version, an outcome, an exit
status, a refusal line, an argument. The volatile values are declared in
`test/runnerharness/shape.go`, and each is what a second run on one pin moved:
the times and durations a migration records, an address, the digest of an
artifact, whose manifest records when it was pushed, the correlation id on a log
line, and the random part of a name the runner makes under `TMPDIR`. A plan's
digest, a fingerprint and a Git content digest are held. CI runs it on every pull
request and every push to `master`, and a commit that moved the pin without new
captures fails in `go test` before that job starts.

On CI's own Docker a run takes a few minutes. Over SSH from a loaded machine it
has taken over twenty, and `RUNNER_HARNESS_TIMEOUT` sets the bound, an hour by
default. A run that passes the bound or is killed leaves nothing on the daemon:
a process it started removes everything the run made.

Recapture the SQL readings under `testdata/e2e/readings`, which record every
statement each schema and migration command sent to the database. Run the probe
once with `--compare` and the executor the readings were taken with, to show the
procedure reproduces them, then with the new one:

```sh
python3 support/qualification/probes/schema_sql_readings.py \
  --context <context> --executor-image <image> --out testdata/e2e/readings
```

Recalibrate `support/capacity/input-calibration.json`, since a release that
changes what a plan holds changes how much input fits a limit. Run the verifier
once per engine with the release's binary, on the Docker host, because copying the
binary over a client's link runs past the probe's step limit. Write the commit and
the report digests into the file, and any repeated characters a band now needs,
and add the measurement to `support/qualification/0.2.0-capacity-inputs.md`:

```sh
python3 support/qualification/probes/capacity_fixture_verify.py \
  --engine <engine> --docker-context <context> \
  --ptah-binary <binary> --ptah-sha256 <digest> --output <new directory>
```

Search the non-test files in `test/e2e` for literal Ptah output, and recapture
every reading their unit tests hold them to. A predicate that matches a header a
new release no longer writes stays green offline as long as its fixture came from
the old one:

```sh
grep -rnE 'ptah |-- (POSTGRES|MYSQL)|contract_version' test/e2e --include='*.go' --exclude='*_test.go'
```

Then let the matrix run against the new build. The harness does not replace
kind: admission, RBAC, the Job and Pod lifecycle, NetworkPolicy and durable result
delivery are proven there.

## Correcting a release guide

A release's pages are built from that release's own tag, so a correction cannot
arrive by editing master. Assign the corrected revision to the release instead:

```json
"documentation": {
  "published": true,
  "source": "v0.1.0",
  "fixRevision": "<exact 40-character commit>",
  "fixReason": "the install command named a chart path that never shipped"
}
```

The Git tag does not move and no new binary is released. The revision is a
commit rather than a branch because the published pages must stay the ones
somebody named, and the reason is required because a fix with no stated
correction is indistinguishable from a rebuild.

What a fix may carry is a correction to a command or an explanation. What it may
not carry is a feature from development into an older guide. The offline guard
holds the shape; the publish workflow holds the property it cannot see, that the
commit descends from the release it is assigned to.

Most releases need no entry. This is the explicit exception rather than a
documentation branch per release.

## Where it is published

The table on this page is the published one. The documentation gate renders it
from `support/ptah.json` again and refuses a page that says something else, so
the catalog and the table change in one commit or not at all.

Each guide renders the file at the revision it was built from, so a release
guide shows the catalog as it stood at that release's tag. The development
guide is rebuilt from master whenever the file changes, and
[operator.ptah.run/support/ptah/](https://operator.ptah.run/support/ptah/)
always opens its copy. That is the address to link to from elsewhere.

Database engines are a separate axis and live in
[database support](../databases/); Kubernetes versions live in
[Kubernetes support](../kubernetes/).
