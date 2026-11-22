# Schema diagnostic SQL readings

These are isolated CLI readings of the SQL the executor sends for schema
diagnostics. They witness every declaration of the e2e audit
[contract](../../../test/e2e/schema-sql-contract.json). They do not establish
Kubernetes acceptance or a production qualification result.

## Identity and procedure

Captured on October 23, 2026 with the Ptah v0.15.0 release image, taken again
on October 25, 2026 with v0.16.0, on November 2, 2026 with v0.17.0, on
November 4, 2026 with v0.18.0, on November 6, 2026 with v0.22.0, on November 7,
2026 with v0.23.0, on November 13, 2026 with v0.26.0, on November 14, 2026 with
v0.28.0, and on November 15, 2026 with the Ptah v0.29.0 release image, commit
`75fa9991177e9049b66fe2e37abec0db40f47568`, and with the Ptah v0.29.2 release
image, commit `ba500e11b2536ad7dad1720e5cbd9bf7292dba8b`, which both reproduced
every reading, so no file changed. On November 16, 2026 the Ptah v0.29.3 release
image, commit `dfbda1b29a7e4591a41334c6949dddac3a6bd69d`, reproduced every
reading again. Later that day the Ptah v0.29.4 release image, commit
`1f1ff93c5ac8fe34f80845bb1600ac6a3bcee788`, reproduced every PostgreSQL reading
and the four saved plans. In each MySQL reading one statement moved: the view
read also selects `DEFINER` and `CURRENT_USER()`, which the planner compares to
refuse a replacement that would hand a view to another account
([stokaro/ptah#6109](https://github.com/stokaro/ptah/pull/6109)). The MySQL
readings here are v0.29.4's, with the exit status and record count v0.29.3
gave, and the contract names the new statement. On November 17, 2026 the Ptah
v0.29.5 release image, commit `8a634c0574427e313392f1131382cf4346d05b0d`,
reproduced every MySQL reading and the four saved plans. In each PostgreSQL
reading, the 18 and 16 witnesses included, one statement moved: the trigger
read also selects the schema of each trigger's function, which the planner
uses to name a function that lives outside the trigger's schema
([stokaro/ptah#6129](https://github.com/stokaro/ptah/pull/6129)), and the name
of the prepared statement that carries it moved with it. The PostgreSQL
readings here are v0.29.5's, with the exit status and record count v0.29.4
gave, and the contract names the new statement. On November 18, 2026 the Ptah
v0.30.1 release image, commit `c5fba763a6fb346fc8a35d2f336d75a91385e3b9`,
reproduced both MySQL saved plans byte for byte and moved every reading, as
the comparison with v0.29.5 below says. The readings and the PostgreSQL saved
plans here are v0.30.1's, and the contract names the moved statements. Later
that day the v0.31.0 release image, commit
`dc2decdee2617a350475d0f17347c7fb09dbce97`, reproduced every MySQL reading, both
MySQL saved plans and every migration reading, and moved every PostgreSQL schema
reading, as the comparison with v0.30.1 below says. The PostgreSQL schema readings
and saved plans here are v0.31.0's, and the contract names the new statements.
The same day the v0.32.0 release image, commit
`da2df4a5e7088e3efcdec6b8f08110c23572bb74`, reproduced every reading record for
record, the PostgreSQL 18 and 16 witnesses and the migration readings included,
apart from the two apply log values every run draws anew, and all four saved
exclusion plans byte for byte. No reading or saved plan changed; the contract
and [`schema-sql-capture.json`](schema-sql-capture.json) name the new commit.
On November 19, 2026 the v0.33.0 release image, commit
`37c98b0024b136dd5079f42dd705d49ab24aeb52`, reproduced every MySQL schema
reading but the allowed Apply and both History readings, and moved every
PostgreSQL schema reading and both restored-history refusals, as the comparison
with v0.32.0 below says. The moved readings here are v0.33.0's, all four saved
exclusion plans stay, and the contract names the new statements. Later that day
the v0.33.1 release image, commit `f7c85bdc579c9e5939108a892f11e3d6d0974248`,
reproduced every PostgreSQL reading record for record, the 18 and 16 witnesses
and both PostgreSQL migration readings included, and moved every MySQL schema
reading, the MySQL History reading and the MySQL restored-history refusal, as
the comparison with v0.33.0 below says. The MySQL readings here are v0.33.1's,
all four saved exclusion plans came out byte for byte as v0.33.0 saved them,
and the contract names the new statements. On November 20, 2026 the v0.34.0
release image, commit `90e98f6ca0b3d77e66b0d5e72a6a9e22da20653a`, reproduced
every PostgreSQL reading record for record, the 18 and 16 witnesses included,
and every migration reading of both engines, and moved every MySQL schema
reading, as the comparison with v0.33.1 below says. The MySQL schema readings
here are v0.34.0's, the four saved exclusion plans are v0.34.0's, and the
contract names the new statements. On November 22, 2026 the v0.34.0 image
reproduced every checked-in reading again, and the v0.35.0 release image,
commit `1ec0b95eea95afce6f50b5912311ce4822f0636b`, reproduced the PostgreSQL
History reading and the PostgreSQL restored-history refusal and moved every
other reading, as the comparison with v0.34.0 below says. The moved readings
and the four saved exclusion plans here are v0.35.0's, and the contract names
the new statements.
[`schema-sql-capture.json`](schema-sql-capture.json) records the executor and
server images, every command's exit status and record count, and the SHA-256 of
each reading and saved plan.

The readings are written by
`support/qualification/probes/schema_sql_readings.py`, which runs each command
against a disposable PostgreSQL 17.11 or MySQL 8.4.11 server that journals every
statement it receives, one command per journal:

```sh
python3 support/qualification/probes/schema_sql_readings.py \
  --context <docker context> --executor-image <image> --out <dir>
```

Each server uses a new database `ptah_audit_schema` and the account
`ptah_audit`: PostgreSQL's bootstrap account, and MySQL's database-scoped
account created through `MYSQL_USER`. The executor's client address is rewritten
to `172.19.0.3`, the address the audit predicates name; SQL, parameters,
protocol commands, session order and timestamps stay as received. Each command
runs in its own container, and every container, anonymous volume and network the
run created is removed. The `documented-` readings use the login the databases
page documents instead, as [the documented login](#the-documented-login) says.

Before the v0.29.0 capture, the same procedure ran with the executor of the
previous pin, the Ptah v0.28.0 release image, and before the v0.28.0 capture
with v0.26.0, as it ran with v0.23.0 before the
v0.26.0 capture, with v0.22.0 before the v0.23.0 one, with v0.18.0 before the v0.22.0 one, with v0.17.0 before the
v0.18.0 one, with v0.16.0 before the v0.17.0 one, with v0.15.0 before the
v0.16.0 one and with v0.14.0 before the v0.15.0 one. Each time it reproduced
every earlier checked-in reading record for record, PostgreSQL parameters
included, and all four saved exclusion plans byte for byte. The v0.22.0 run
reproduced the three PostgreSQL 18 witnesses as well, and the v0.23.0 and
v0.26.0 runs the witnesses of PostgreSQL 18 and 16. Since v0.28.0 the allowed
Apply writes two schema apply log entries, and each carries the run's id and the
time it was written, which every run draws anew; the comparison leaves those two
values out and compares everything else, so the v0.28.0 control of the v0.29.0
capture reproduced every reading that way, its PostgreSQL 18 and 16 witnesses
included. That is what makes the new readings a statement about the executor rather
than about a different procedure.

`--contract` writes the contract from a directory of readings. Run on the
v0.14.0 readings, and again on the v0.16.0 readings, it reproduced the contract
then checked in byte for byte, and the probe's unit test holds the checked-in
contract to the checked-in readings. The v0.28.0 contract is the one it wrote
from the v0.28.0 readings, and the v0.29.0 contract differs from it only in the
Ptah commit it names.

Against v0.33.1, v0.34.0 changed the MySQL schema readings and nothing else.
Ptah does not model MySQL or MariaDB partitioning, and each schema read now asks
which tables of the database are partitioned, with a prepared
`SELECT DISTINCT TABLE_NAME FROM information_schema.PARTITIONS`, so that it can
report them as not compared instead of reading them without their partitions
([stokaro/ptah#6375](https://github.com/stokaro/ptah/pull/6375)). The read is
a Prepare, an Execute and a statement close, three records. Each of the 30 MySQL
schema readings keeps its exit status and gains only those: an Observe, a plan
validation, a refused stale Apply and the allowed Apply three, a plan six, and
each lock reading fifteen. The contract has 108 statements where it had 106:
the Prepare and the Execute are new, and no statement is dropped or changed.
The four saved exclusion plans hold the same statements; only their name and
`from_fingerprint` moved, on both engines.

Against v0.34.0, v0.35.0 changed every schema reading and both MySQL migration
readings, and no exit status. On MySQL each information_schema read is the
same read with a schema predicate that compares the name byte for byte unless
`lower_case_table_names` is set, and each command asks `SELECT DATABASE()`
once more as it connects
([stokaro/ptah#6454](https://github.com/stokaro/ptah/pull/6454)). Each MySQL
reading has one more record, and each lock reading, three commands, three. On PostgreSQL, the
18 and 16
witnesses included, each probe runs under `SAVEPOINT ptah_probe_transaction`
with `SET LOCAL standard_conforming_strings = on`, reads the transaction
timestamp and `standard_conforming_strings`, and sends its `CREATE TABLE`
prepared instead of as a simple query
([stokaro/ptah#6462](https://github.com/stokaro/ptah/pull/6462)), and each
comparison reads which roles hold `pg_read_all_data` or `pg_write_all_data`
([stokaro/ptah#6418](https://github.com/stokaro/ptah/pull/6418)). An Observe
has 67 records where it had 61, a plan 118 where it had 111, a plan
validation, a refused stale Apply and the allowed Apply one more each, and the
first Observe and plan of the empty database one and two more. The contract
has 112 statements where it had 108. The four saved exclusion plans hold the
same statements; only their name and `to_fingerprint` moved, on both engines.

Against v0.33.0, v0.33.1 changed the MySQL readings and nothing else. Ptah
reads `SELECT @@SESSION.sql_mode` when it connects and again on a pinned
session, because the mode decides whether the server reads a backslash in a
string literal as an escape
([stokaro/ptah#6295](https://github.com/stokaro/ptah/pull/6295)). Each schema
read also asks for the server's character sets and their default collations
from `information_schema.CHARACTER_SETS`, for the session's
`default_collation_for_utf8mb4`, and for the character set `utf8` names, with
`SELECT CHARSET(CONVERT('' USING utf8))`, so that it can compare a collation a
table inherits with the one the schema file declares
([stokaro/ptah#6291](https://github.com/stokaro/ptah/pull/6291)). Each of the
30 MySQL schema readings keeps its exit status and gains only those records:
an Observe four, a plan seven, a plan validation, a refused stale Apply and the
allowed Apply five, and each lock reading nineteen. The History reading reads
the mode once in each of its two sessions, two records more, and the refused
restored-history Apply six. The contract has 106 statements where it had 102:
the four reads are new, and no statement is dropped or changed.

Against v0.32.0, v0.33.0 changed two PostgreSQL reads. Ptah now writes and
compares a column's collation
([stokaro/ptah#6266](https://github.com/stokaro/ptah/pull/6266)), so the column
read also selects the collation a column declares, read from `pg_attribute`
against its type's collation through `pg_type`, `pg_collation` and
`pg_namespace`, beside the one it compares with, and the domain read selects
each domain's collation from `pg_collation`. Both are still catalog reads, and
the name of the prepared statement that carries each moved with it. Each of the
34 PostgreSQL schema readings, the 18 and 16 witnesses included, keeps its
record count and exit status. The allowed Apply's two schema apply log entries
name a new plan on both engines, because the plan's source fingerprint moved;
every other MySQL schema reading matched record for record. Each refused
restored-history Apply asks whether the revision table exists where it sent
`CREATE TABLE IF NOT EXISTS`
([stokaro/ptah#6234](https://github.com/stokaro/ptah/pull/6234)), as
[the refusal record](restored-history-refusal.md) says, and both History
readings matched. All four saved exclusion plans hold the same statements and
target fingerprint under a new source fingerprint and name. The contract keeps
102 statements: the two column reads, PostgreSQL 18's included, and the domain
read are replaced.

Against v0.30.1, v0.31.0 changed the PostgreSQL schema readings and nothing
else. Ptah now manages logical replication publications
([stokaro/ptah#6216](https://github.com/stokaro/ptah/pull/6216)). The read of
event triggers, which also listed each publication's name, now lists event
triggers alone, and four reads take the rest of its place: the schemas the
database describes, so that publications are read only where the read covers
every schema, and then `pg_publication`, `pg_publication_rel` and
`pg_publication_namespace`. Each of the 34 PostgreSQL schema readings, the 18
and 16 witnesses included, sends them once in an Observe, a plan validation or
an Apply and twice in a plan, so an Observe has four records more and a Plan
eight, and keeps its exit status. Both PostgreSQL saved exclusion plans hold the
same statements under a new name and fingerprints. Every MySQL reading, both
History readings and both restored-history refusals matched record for record,
apart from the two apply log values every run draws anew, and both MySQL saved
exclusion plans byte for byte. The contract has 102 statements where it had 98:
the four reads and the narrower read of event triggers are new, and the read of
event triggers and publications is gone.

Against v0.29.5, v0.30.1 changed every reading on both engines. Ptah now keeps
a record of repeatable migrations in the table `schema_migrations_repeatable`,
and every catalog read of a schema leaves that table out beside the migration
tables it already left out. Each of the 30 MySQL schema readings changes in
that alone and keeps its exit status and record count. Each PostgreSQL schema reading,
the 18 and 16 witnesses included, also reads `pg_db_role_setting`, once in an
Observe, a plan validation or an Apply and twice in a plan, which reads twice,
so an Observe has one record more and a Plan two. The statements a plan or an
Apply sends name their schema, as `ALTER TABLE "public"."e2e_widgets" ADD COLUMN
"enabled" boolean`, and both PostgreSQL saved exclusion plans name it and record
it as `placed_schema`
([stokaro/ptah#6132](https://github.com/stokaro/ptah/pull/6132)). Each History
reading and each restored-history refusal asks whether
`schema_migrations_repeatable` exists, once per History read: a PostgreSQL
History has one record more and its refusal three, and MySQL asks in a session
of its own, so its History has five records more and its refusal eleven. The
contract has 98 statements where it had 97: the `pg_db_role_setting` read is
new, and 17 reads gain the table, 7 on PostgreSQL and 10 on MySQL.

Against v0.28.0, v0.29.0 changed no reading on either engine, the PostgreSQL 18
and 16 witnesses included: every schema and migration journal matched record for
record, apart from the two apply log values every run draws anew, and all four
saved exclusion plans byte for byte.

Against v0.26.0, v0.28.0 changed every schema reading on both engines and no
migration reading: both migration History readings and both restored-history
refusals matched record for record. Ptah now keeps a log of each schema Apply in
the table `ptah_schema_apply_log`
([stokaro/ptah#6014](https://github.com/stokaro/ptah/pull/6014)), and each schema
read leaves that table out beside the migration tables it already left out. That
predicate is the only change: 25 of the contract's 97 statements gain it, 15 on
PostgreSQL and 10 on MySQL, no statement is added or dropped, and no record count
of a diagnostic moved. The allowed Apply creates the table and writes a `started`
row before its first statement and an `applied` row after it, so its journal
grows by six records on PostgreSQL, where it also reads the table's owner before
and after the `CREATE`, and by nine on MySQL, where the second row arrives on a
connection of its own. All four saved exclusion plans are unchanged byte for
byte.

Against v0.23.0, v0.26.0 changed every schema reading on both engines and no
migration reading but one: both migration History readings and the PostgreSQL
restored-history refusal matched record for record, and the MySQL refusal
creates its revision table with `ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`
([stokaro/ptah#5874](https://github.com/stokaro/ptah/pull/5874)). Each PostgreSQL
schema read now reads the inherit and set options of a role membership
([stokaro/ptah#5884](https://github.com/stokaro/ptah/pull/5884)) and whether the
role it runs as is a superuser, where it asked only that role's name
([stokaro/ptah#5896](https://github.com/stokaro/ptah/pull/5896)). It also reads
which foreign tables keep an extension's foreign-data wrapper in use, so that a
plan which would drop the extension under them is refused
([stokaro/ptah#5898](https://github.com/stokaro/ptah/pull/5898)), lists the
aggregates, collations, foreign tables, event triggers and publications it does
not compare, so that a report does not read as covering them
([stokaro/ptah#5908](https://github.com/stokaro/ptah/pull/5908)), and reads each
trigger's arguments ([stokaro/ptah#5986](https://github.com/stokaro/ptah/pull/5986)).
A PostgreSQL Observe has three records more and a Plan six, on the empty
database of `initial-v1` too. Each MySQL view read now selects `SECURITY_TYPE`
([stokaro/ptah#5902](https://github.com/stokaro/ptah/pull/5902)), and the MySQL
record counts did not move. The allowed Apply executes the statements it
executed before. Both saved PostgreSQL exclusion plans carry the same statements
and changed only in their source fingerprint and the plan name taken from it;
both MySQL plans are unchanged byte for byte.

Against v0.22.0, v0.23.0 changed every schema reading on both engines and no
migration reading: both migration History readings and both restored-history
refusals matched record for record. Each MySQL schema read now asks who the
session is and what it holds, with `SELECT CURRENT_USER(), VERSION()`,
`SHOW GRANTS` and a read of the `partial_revokes` and `lower_case_table_names`
settings, so that it can refuse a read of objects the account cannot see instead
of reading them as absent
([stokaro/ptah#5545](https://github.com/stokaro/ptah/pull/5545)). It also lists
the database's events, which take no part in a comparison and are named in a
report so that it does not read as covering them
([stokaro/ptah#5552](https://github.com/stokaro/ptah/pull/5552)). A MySQL Observe
has six records more and four executions more, and a Plan, which reads twice,
twelve and eight. Each PostgreSQL schema read now asks what an object's owner holds
by default apart from its grants
([stokaro/ptah#5528](https://github.com/stokaro/ptah/pull/5528)), the role it runs
as ([stokaro/ptah#5542](https://github.com/stokaro/ptah/pull/5542)) and, on a
database that has constraints, which of them a table inherits
([stokaro/ptah#5529](https://github.com/stokaro/ptah/pull/5529)). A PostgreSQL
Observe has three records more and a Plan six, or two and four on the empty
database of `initial-v1`. The table, column, index, constraint and view reads of
both engines leave out Ptah's seed ledger `schema_seeds`
([stokaro/ptah#5641](https://github.com/stokaro/ptah/pull/5641)). The allowed
Apply executes the statements it executed before, and both runner Plan journals
on MySQL grow by the same reads. Three saved exclusion plans carry the same
statements and changed only in their fingerprints and the plan name taken from
them: both PostgreSQL plans in their source and target fingerprint, the narrowed
MySQL plan in its target fingerprint. The wide MySQL plan is unchanged byte for
byte.

Against v0.18.0, v0.22.0 changed every PostgreSQL schema reading, and on MySQL
only the allowed Apply's journal. Each PostgreSQL schema command now reads
`pg_catalog` where it read `information_schema`
([stokaro/ptah#5499](https://github.com/stokaro/ptah/pull/5499)), so a role reads
the same schema whatever it holds, and it sends two reads more: the tables another
table inherits from
([stokaro/ptah#5341](https://github.com/stokaro/ptah/pull/5341)) and the relations
an extension owns
([stokaro/ptah#5409](https://github.com/stokaro/ptah/pull/5409)). An Observe has
two records more and a Plan four, since it reads twice. Planned statements lost
their comment lines and a dropped table its `IF EXISTS` and `CASCADE`
([stokaro/ptah#5341](https://github.com/stokaro/ptah/pull/5341)), which is what
moved the allowed Apply on MySQL. Every other MySQL reading, both migration History
readings and both restored-history refusals matched record for record. All four
saved exclusion plans changed: they carry the same statements in that style, and
the `scope` Ptah has recorded since v0.19.0.

Against v0.17.0, v0.18.0 changed every PostgreSQL schema reading and no other.
Observe now lists the schemas the connection covers before it reads them, as
Plan already did, so each Observe journal has one record more
([stokaro/ptah#5181](https://github.com/stokaro/ptah/pull/5181)). Four catalog
reads, of columns, tables, constraints and table grants, now find a table's
`pg_class` row with the index kinds left out
([stokaro/ptah#5170](https://github.com/stokaro/ptah/pull/5170)), and the
constraint read also leaves out a sequence's key
([stokaro/ptah#5182](https://github.com/stokaro/ptah/pull/5182)). Every MySQL
reading, both migration History readings and both restored-history refusals
matched record for record. Of the saved exclusion plans, only the narrowed
PostgreSQL one changed, and only in its name and its source fingerprint: the
exclusion now keys the excluded table in its effective schema
([stokaro/ptah#5178](https://github.com/stokaro/ptah/pull/5178)). A build of
that change's parent wrote the old fingerprint, and a build of the change the
new one.

Against v0.15.0, v0.16.0 changed nothing here: every schema reading and both
migration History readings matched record for record, and only the
restored-history refusals moved; [restored-history-refusal.md](restored-history-refusal.md)
says how.

Against v0.16.0, v0.17.0 changed every PostgreSQL reading and one MySQL
reading. Every PostgreSQL command now also reads the session's search path when
it connects, so each PostgreSQL journal has one record more; four catalog reads
were rewritten, and the column spelling probe and the planned statements lost
the comment lines in front of them. On MySQL only the allowed Apply's journal
moved, because the statements it executes lost the same comment lines; every
other MySQL reading matched record for record. All four saved exclusion plans
changed the same way: the narrowed plans carry only the column addition, and
the wide plans still drop the excluded table after it.

## Permitted work and source review

The contract carries 60 PostgreSQL statement/parameter sets and 37 MySQL
command/statement pairs. The predicate compares SQL, comments, whitespace,
parameters and MySQL protocol commands byte for byte. Only the declaration's
quoted MySQL database placeholder is expanded to the controlled fixture
identifier. Received SQL is never normalized. The one rewrite is the label of a
PostgreSQL parameter list: 16 logs `parameters: `, and the audit reads it as the
`Parameters: ` 17 and 18 log, with the values after it compared byte for byte.

Against v0.23.0, v0.26.0 changed three PostgreSQL declarations and added three,
and changed one MySQL prepared read with its execution. The count goes from 94 to
97, every new declaration for Observe, Plan and the stale Apply alike. On
PostgreSQL the membership read adds `inherit_option` and `set_option`, read
through `to_jsonb`, which gives null on a server that has no such column; the role read
`SELECT current_user` becomes
`SELECT rolname, rolsuper FROM pg_roles WHERE rolname = current_user`, and the
trigger read adds `encode(trg.tgargs, 'hex')`. The new reads are the foreign
tables of an extension's foreign-data wrapper, from `pg_extension`, `pg_depend`
and the foreign-data catalogs, the aggregates, collations and foreign tables in
the schema, and the database's event triggers and publications. On MySQL the
view read and its execution select `SECURITY_TYPE`. Each of the eight new
declarations was scanned with its comments, literals and quoted identifiers
removed: none contains a write, DDL, grant, lock or transaction-control keyword.
Each PostgreSQL one selects from `pg_catalog` relations alone, and the MySQL
pair reads `information_schema.VIEWS` as before.

Against v0.22.0, v0.23.0 changed fourteen PostgreSQL declarations, the column read
on each server line among them, and added three. On MySQL it changed five
prepared reads, each with its execution, and added three queries and one prepared
read with its execution. The count goes from 84 to 92, every new declaration for
Observe, Plan and the stale Apply alike. On PostgreSQL the column, table, index,
constraint, exclusion constraint and view reads leave out `schema_seeds`
([stokaro/ptah#5641](https://github.com/stokaro/ptah/pull/5641)). The column read
also takes an interval column's type from `format_type` when the column carries a
fields or precision modifier
([stokaro/ptah#5546](https://github.com/stokaro/ptah/pull/5546)), and the table
read no longer selects an empty `row_deletion_policy`
([stokaro/ptah#5657](https://github.com/stokaro/ptah/pull/5657)). The inheritance
read adds whether a child is a partition and its bound, and the trigger read
leaves out the copy of a row trigger PostgreSQL keeps on each partition
([stokaro/ptah#5529](https://github.com/stokaro/ptah/pull/5529)). Both role reads
and the table, schema and sequence grant reads leave out the owner's own ACL
entry, and the role reads count an owner that revoked a privilege from itself
([stokaro/ptah#5528](https://github.com/stokaro/ptah/pull/5528)). The new reads
are the owner's privileges from
[`acldefault`](https://github.com/stokaro/ptah/blob/f47b2bfa3b696c93730e7d25397987ed0a4a2562/internal/dbschema/postgres/owner_privileges.go),
the constraints a table
[inherits](https://github.com/stokaro/ptah/blob/f47b2bfa3b696c93730e7d25397987ed0a4a2562/internal/dbschema/postgres/partition.go)
from `pg_constraint`, and `SELECT current_user`. On MySQL the column, table,
index, constraint and view reads leave out `schema_seeds`. The new statements are
`SELECT CURRENT_USER(), VERSION()`, `SHOW GRANTS` and
`SHOW VARIABLES WHERE Variable_name IN ('partial_revokes', 'lower_case_table_names')`,
which [the session read](https://github.com/stokaro/ptah/blob/f47b2bfa3b696c93730e7d25397987ed0a4a2562/internal/mysqlgrants/session.go)
sends, and the prepared read of `information_schema.EVENTS`. `SHOW GRANTS` about
the session needs no privilege and names only what the session holds. Each of the
32 new declarations was scanned with its comments, literals and quoted
identifiers removed: none contains a write, DDL, grant, lock or
transaction-control keyword. Each PostgreSQL one selects from `pg_catalog`
relations and catalog functions such as `aclexplode`, `acldefault` and
`pg_get_expr` alone, and no PostgreSQL schema declaration reads
`information_schema`.

Against v0.18.0, v0.22.0 changed ten PostgreSQL declarations, the column read on
each server line among them, and added two, and it kept every MySQL one; the count
goes from 82 to 84. The column, table, constraint,
view and table grant reads select from
[`pgcatalogview`](https://github.com/stokaro/ptah/blob/0dd9190b6477830272c90fd98b2b57b603166fd1/internal/pgcatalogview/pgcatalogview.go)
rather than `information_schema`, and the column read finds an owned sequence
through `pg_depend` rather than `pg_get_serial_sequence`
([stokaro/ptah#5499](https://github.com/stokaro/ptah/pull/5499)). The grant reads
no longer leave out predefined roles
([stokaro/ptah#5412](https://github.com/stokaro/ptah/pull/5412)), the schema grant
read leaves out what the database owner holds by default
([stokaro/ptah#5430](https://github.com/stokaro/ptah/pull/5430)), and the constraint
read names the index a foreign key uses
([stokaro/ptah#5370](https://github.com/stokaro/ptah/pull/5370)). The two new reads
are of `pg_inherits` and of `pg_depend` for extension members. Each of the twelve
was scanned with its comments and literals removed: none contains a write, DDL,
grant, lock or transaction-control keyword, and each selects from `pg_catalog`
relations alone. No PostgreSQL schema declaration reads `information_schema` any
more.

Against v0.17.0, v0.18.0 changed four PostgreSQL declarations, the four reads
above, and permits the existing schema listing, which Plan and the stale Apply
already sent, to Observe as well; the count stays 81 and every MySQL
declaration is kept. The listing is
[`PostgresDescribedSchemasPredicate`](https://github.com/stokaro/ptah/blob/6e1d430e51312de325a8f0238e601547a6c473d5/internal/pgsystemschema/pgsystemschema.go)
over `pg_namespace` and `pg_depend`, and the index-kind condition is
[`relationByName`](https://github.com/stokaro/ptah/blob/6e1d430e51312de325a8f0238e601547a6c473d5/internal/dbschema/postgres/relation_by_name.go).
None of them contains a write, DDL, grant or transaction-control keyword
outside comments and literals.

Against v0.16.0, v0.17.0 added one PostgreSQL declaration and changed six,
and kept every MySQL one. The new one is the search-path read
`SELECT s FROM unnest(current_schemas(false)) WITH ORDINALITY AS path(s, n) ORDER BY n`,
which [connection setup](https://github.com/stokaro/ptah/blob/38bd6e91cfe3a70543046da9c3ada05a0460a103/internal/connector/pgconnect/pgconnect.go)
sends so that an unqualified name resolves in the order the server resolves
it. The view read adds each view's `reloptions`; the materialized view read
adds its `reloptions` and whether it is populated; the table grant read adds a
second branch that reads a materialized view's privileges from
`pg_class.relacl`; and the routine grant read adds a branch for the owner's own
`EXECUTE`. These are reads of the catalog or the session: none contains a
write, DDL, grant or transaction-control keyword outside comments and literals.
The two column spelling probes create the same temporary table as before,
without the `-- POSTGRES TABLE` comment in front of the statement.

Against v0.15.0, v0.16.0 changed nothing here. Against v0.14.0, v0.15.0 changed
nothing either. Every reading carries
the same statements in the same order; the MySQL journals differ only in their
timestamps and one thread number, and the contract only in the commit it names.
Against v0.13.0, v0.14.0 changed seven declarations and kept the count at 80. On PostgreSQL the column read takes a domain column's NOT NULL from the
column rather than from the type's effective nullability, the range type read
qualifies operator class and collation by schema, the exclusion constraint read
adds the backing index, and the row-level security policy read was rewritten for
its new owner: the same catalog, with roles as JSON, the raw command and the
permissive flag. On MySQL the `information_schema.STATISTICS` column check and
both forms of the index read add the `EXPRESSION` column. Every changed
declaration is a catalog read: none contains a write, DDL, grant or
transaction-control keyword outside comments and literals. The migration History
and restored-history readings are unchanged record for record.

The reviewed source is the pinned commit:

- PostgreSQL's [schema reader](https://github.com/stokaro/ptah/blob/1cf0563c0951fbf67f13f9b4ca7f9b49bbe2f3a3/internal/dbschema/postgres/reader.go)
  reads namespaces, columns, types, table statistics, inheritance and partitions,
  indexes, constraints and which of them are inherited, extensions and their
  members, routines, views, triggers, sequences, policies, roles, owners, grants,
  what an owner holds by default, default privileges, membership with its inherit
  and set options, and the role it runs as and whether it is a superuser, from
  PostgreSQL catalogs. It also reads each trigger's arguments, the foreign tables
  that keep an [extension](https://github.com/stokaro/ptah/blob/1cf0563c0951fbf67f13f9b4ca7f9b49bbe2f3a3/internal/dbschema/postgres/extensions.go)'s
  foreign-data wrapper in use, and the aggregates, collations, foreign tables,
  event triggers and publications it
  [does not compare](https://github.com/stokaro/ptah/blob/1cf0563c0951fbf67f13f9b4ca7f9b49bbe2f3a3/internal/dbschema/postgres/unmodeled.go). Catalog
  functions such as `pg_get_expr` return definitions; the fixture queries do not
  execute those definitions.
- PostgreSQL's [column spelling probe](https://github.com/stokaro/ptah/blob/1cf0563c0951fbf67f13f9b4ca7f9b49bbe2f3a3/internal/dbexprprobe/column_spellings.go)
  creates exactly `pg_temp.ptah_column_probe_0` with `id bigint`, with or without
  `enabled boolean` in the declared fixture variants, reads its type
  definitions, and rolls back to its savepoint. This exact temporary probe is
  permitted; permanent DDL, changed columns or defaults, arbitrary functions and
  COMMIT are not.
- MySQL's [schema reader](https://github.com/stokaro/ptah/blob/1cf0563c0951fbf67f13f9b4ca7f9b49bbe2f3a3/internal/dbschema/mysql/reader.go)
  reads columns, tables, enum columns, indexes, check and key constraints, views
  with their security type, triggers, routine parameters, routines and event names from
  `information_schema`. Before it reads, its
  [visibility check](https://github.com/stokaro/ptah/blob/1cf0563c0951fbf67f13f9b4ca7f9b49bbe2f3a3/internal/dbschema/mysql/visibility.go)
  reads the session's account, server version, own grants and the two settings
  that decide how a grant applies. Its prepared queries and their expanded
  executions are distinct entries. The
  isolated account cannot read `mysql.user`; that attempted catalog read remains
  visible and permitted, without granting it additional authority.

Tests require every declaration to have an actual reading and the source pin to
match the support catalog. Mutation checks reject arbitrary SELECT/WITH,
appended SQL, changed parameters, changed probe objects, wrong operations,
unknown clients, databases or accounts, lost records, incomplete MySQL sessions
and missing diagnostic controls. Error messages omit SQL and parameter values.

## PostgreSQL 18

An added PostgreSQL line from `support/databases.json` is held to the same
contract, so it has witnesses of its own:

```sh
python3 support/qualification/probes/schema_sql_readings.py \
  --context <docker context> --executor-image <image> --postgresql-line 18 --out <dir>
```

Running the whole capture on PostgreSQL 18.6 with the v0.18.0 image changed one
statement against the PostgreSQL 17.11 readings. Ptah's column read selects
`not_null_constraint_name` from `pg_constraint`, because 18 records a NOT NULL
there as `contype = 'n'`, where 17 selects an empty string. Record counts, exit
statuses and every other statement matched, and so did both migration History
readings and both restored-history refusals. The saved exclusion plans differ in
their source fingerprint and name and hold the same statements. The three witnesses
below were taken again with the v0.22.0 image on November 6, 2026, at 50, 89 and 47
records as on PostgreSQL 17, with the v0.23.0 image on November 7, 2026, at 53,
95 and 50, with the v0.26.0 image on November 13, 2026, at 56, 101 and 53, and
with the v0.30.1 image on November 18, 2026, at 57, 103 and 54, with the
v0.31.0 and v0.32.0 images the same day, at 61, 111 and 58, and with the
v0.33.0 and v0.33.1 images on November 19, 2026, and the v0.34.0 image on
November 20, 2026, at the same counts. Each time they
still differ from 17 in that one statement. Since v0.22.0
Ptah reads the columns from `pg_catalog`, so the two statements are the same read
with that name selected or left empty.

That read is the one PostgreSQL declaration only an added line witnesses:
`postgresql-18-schema-drift-observe`, `-drift-plan` and `-drift-stale-apply`, one
per operation. The other readings are the PostgreSQL 17 ones. The contract
permits the read on every server, and it is still a read of the catalog with
no write, DDL, grant or transaction-control keyword outside comments and literals.

## PostgreSQL 16

PostgreSQL 16 adds no declaration. On November 12, 2026 the whole PostgreSQL
capture ran on PostgreSQL 16.15
(`postgres:16-alpine@sha256:cbf42f0edc946c0bc1b8eeed62403734e1f25dc67a3491455d07a92fdb558b28`)
with the v0.23.0 image, the server image being the only change to the
procedure. All 22 readings, 1,459 records, had the record counts and exit
statuses of the PostgreSQL 17.11 ones, and every statement and parameter value
matched. What differed is how 16 logs a statement's bound parameters: 885
records carry `parameters: $1 = 'public'` where 17 writes `Parameters: $1 =
'public'`. The audits compare that detail byte for byte, so on 16 they would
refuse every statement with parameters. They read 16's label as 17's, and
nothing else of the detail changes. The saved exclusion plans differ from 17's
in their source fingerprint and name, as 18's do, and hold the same statements.
Ptah resolves 16 to its `Postgres16` preset, which differs from 17's only in
`alter_generated_column_expression`, and no fixture changes a generated column.

`postgresql-16-schema-drift-observe`, `-drift-plan` and `-drift-stale-apply`
are the line's witnesses, taken with `--postgresql-line 16` on the same day, at
53, 95 and 50 records, again with the v0.26.0 image on November 13, 2026, at
56, 101 and 53, with the v0.30.1 image on November 18, 2026, at 57, 103 and
54, with the v0.31.0 and v0.32.0 images the same day, at 61, 111 and 58, with the v0.33.0 and v0.33.1 images on November 19, 2026, and with the v0.34.0 image on November 20, 2026, at the same counts, with every statement and parameter value of the PostgreSQL 17 readings. They witness the declarations the PostgreSQL 17 readings
do, through the audit's own parser, which is what holds the label rewrite to a
reading 16 wrote.

## The documented login

A runner connects as the login
[the databases page](../../../docs/site/src/content/docs/support/databases.md)
documents, not as an administrator, and Ptah reads some catalogs differently
without one. The `documented-exclusion-wide-`, `documented-exclusion-narrow-`
and `documented-drift-` readings repeat those sequences as that login. The probe
takes the grants from the first `sql` block under the page's PostgreSQL or MySQL
authority section, so the readings are taken with what the page says, and a
change to the page changes the next capture.

On PostgreSQL the probe revokes every database privilege from `PUBLIC`, creates
`ptah_documented` with the page's statements, and seeds the fixture as that
login, which then owns the tables as the page requires. On MySQL the server
starts with root alone, and root creates `ptah_audit` with the page's
database-level grant.

The readings were taken on November 13, 2026 with the v0.23.0 image, and again
the same day with the v0.26.0 image. Each one has the record count and exit
status of its administrator counterpart. On MySQL
every statement matched. On PostgreSQL the two role reads differ: without
superuser Ptah selects `NULL::boolean AS has_password` and does not join
`pg_authid`. Those are two declarations, one for the roles the scope uses and
one for the rest, and they take the contract from 92 to 94. Every other
statement matched, the column spelling probe on `pg_temp` included.

That probe needs `TEMPORARY` on the database. Without it the temporary table
cannot be created, Ptah falls back to a probe per column and then to comparing
the column as declared, and a column it created can read as changed on every
plan ([stokaro/ptah#3617](https://github.com/stokaro/ptah/issues/3617)). The
databases page lists `TEMPORARY` for that reason.

## The readings

| Reading | Fixture and command | PostgreSQL records | MySQL records (Query/Execute) |
| --- | --- | ---: | ---: |
| `observe`, `plan` | v3 with a control row; drift and plan of fault-v1 | 57, 103 | 44 (20), 85 (39) |
| `destructive-observe`, `-plan` | the same, desired v4 | 57, 103 | 44 (20), 85 (39) |
| `exclusion-wide-observe`, `-plan` | adds `e2e_excluded_policy_keep` as the lifecycle creates it; fault-v1 | 57, 103 | 44 (20), 85 (39) |
| `exclusion-narrow-observe`, `-plan` | the same; Plan alone receives `--exclude=e2e_excluded_policy_keep` | 57, 103 | 44 (20), 85 (39) |
| `initial-v1-observe`, `-plan` | no tables; desired v1 | 50, 94 | 43 (19), 83 (37) |
| `tag-v2-observe`, `-plan`, `tag-v3-…` | v1 with one control row; desired v2 or v3 | 57, 103 | 44 (20), 85 (39) |
| `drift-plan-before` | v3; saved plan of fault-v1 | 103 | 85 (39) |
| `drift-stale-apply` | `enabled` dropped under the saved plan; Apply refused `stale-plan` (exit 2), lock timeout 60s | 54 | 52 (23) |
| `drift-observe`, `drift-plan` | the changed database, observed and planned again | 57, 103 | 44 (20), 85 (39) |
| `drift-validate-plan` | `schema apply --dry-run` of the fresh plan, lock timeout 30s | 54 | 52 (23) |
| `drift-apply-current` | the fresh plan applied, lock timeout 60s | 64 | 65 (30) |
| `documented-exclusion-wide-observe`, `-plan`, `documented-exclusion-narrow-…` | the exclusion sequences as the documented login | 57, 103 | 44 (20), 85 (39) |
| `documented-drift-stale-apply`, `-drift-observe`, `-drift-plan`, `-drift-validate-plan` | the drift sequence as the documented login | 54, 57, 103, 54 | 52 (23), 44 (20), 85 (39), 52 (23) |
| `lock-45-plan`, `lock-60-plan` | MySQL only: a runner Plan's two plan reads and dry-run validation under a 45s or 60s lock timeout | | 222 (101) |

Every Observe exited 1 with drift, every Plan and validation exited 0, the stale
Apply exited 2 and the allowed Apply exited 0. Only that final Apply restored
`enabled` and added `fault_token`; the control rows survived every sequence, and
the exclusion plans keep the excluded table's drop out of the narrowed plan.

Both native exclusion plans add the managed column and then drop the excluded
table. v0.13.0's PostgreSQL plan dropped the table's primary-key constraint
before the table, and v0.14.0's does not. Both narrowed plans contain only the
column addition. v0.14.0 also sends each planned statement with the plan's
comments in front of it, so the allowed Apply's journal shows them. v0.17.0
writes fewer of them: the `-- ALTER statements: --` and `-- POSTGRES TABLE`
and `-- MYSQL TABLE` headers are gone, and a comment that names the change,
such as `-- Add/modify columns for table: e2e_widgets`, stays. v0.22.0 writes none
of them, and drops the excluded table with `DROP TABLE` alone, without `IF EXISTS`
or `CASCADE`. v0.23.0, v0.26.0 and v0.28.0 plan the same statements.

## Operations

Observe readings witness `observe`. Plan readings, the saved-plan validation and
the runner Plan journals witness `plan`: a runner Plan validates its saved plan
with `schema apply --dry-run`, so the validation's lock statements belong to
Plan. The refused stale Apply witnesses `stale-apply`, which the runtime audit
grants only to the exact resource, Job and Pod whose result refused the approved
stale plan. The allowed Apply is not a diagnostic and grants nothing.

The lock declarations permit only:

- PostgreSQL `pg_try_advisory_lock` and `pg_advisory_unlock` with the key
  derived from `ptah_schema_apply`.
- MySQL Prepare/Execute pairs for `GET_LOCK('ptah_schema_apply', <timeout>)` and
  `RELEASE_LOCK('ptah_schema_apply')`, with the Plan timeouts 30, 45 and 60
  seconds and the stale Apply's 60.
- MySQL's exact `SELECT @@SESSION.restrict_fk_on_non_standard_key` capability
  read on the pinned session.

Neighboring timeout values, other lock names, extra SQL and wrong actors remain
refused. None of these declarations permits permanent schema or row changes.
