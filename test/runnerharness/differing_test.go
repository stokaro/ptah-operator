package runnerharness_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/e2e/gitfixture"
	"github.com/stokaro/ptah-operator/test/runnerharness"
)

// differingRow is a database that holds something its declaration does not say.
// A comparison a release adds never fires on a database the operator converged,
// so a row over one reads the same before and after that release.
//
// The run records whatever each step answers, so a release that moves a reading
// reaches the diff rather than stopping the run that would have written it.
// want says what the reading has to hold for the row to mean what its name says,
// and TestDifferingRowsReachTheirShape holds the committed captures to it, so
// `go test` asks for a decision on a row a repin moved.
type differingRow struct {
	// name is the shape. The row set is differs-<name>-<engine>.
	name, engine string
	// declared is the file under testdata/sources/differing the artifact holds.
	declared string
	// seed is what the database holds before the first read.
	seed []string
	// urlParameters narrow what the URL covers.
	urlParameters string
	// server is what the row needs of the server rather than of its database:
	// the accounts and roles it reads as, and who owns the database. It runs
	// before any row reads anything, since an account or a role belongs to the
	// whole server. {db} is the row's database and {password} the lab's.
	server []string
	// login is the account the URL names. Empty is the server's superuser.
	login string
	// git reads the declaration from a Git-shaped source rather than an OCI
	// artifact. An artifact carries HCL, which drops what a SQL file can say
	// and HCL cannot, such as which table a partition belongs to.
	git bool
	// stored names where a step's document is kept, for the decoder rows that
	// read it by a path of their own: "<step>/<command>", or "plan/saved plan".
	stored map[string]string
	// fix is what a person runs on the database after the first plan, as the
	// documentation tells them to, on a row whose plan is empty. The row then
	// reads and plans again, as observe-fixed and plan-fixed, and both have to
	// find nothing to change.
	fix []string
	// held runs against the row's database, as the server's superuser, before
	// the first read and after the fix. It says what the seed left and what
	// the fix gave back, so a seed that stopped reaching its state fails the
	// run rather than reading as converged.
	held []heldQuery
	want differingReading
}

// heldQuery is a query whose one-line answer a row holds before its first read
// and after its fix.
type heldQuery struct {
	name, query, seeded, fixed string
}

// differingReading is what a row's captures hold, at the pin they answer.
type differingReading struct {
	// findings are the categories of the first drift report, sorted. None means
	// that report reads as converged, so observed has to show what it did read.
	findings []string
	// counts holds the first report's count for each category it names.
	counts map[string]int32
	// observed counts the first report's inventory by kind.
	observed map[string]int32
	// driftRefusal is what the first read says instead of a report.
	driftRefusal string
	// unseen is what the frames of a refused read and a refused plan say the
	// account could not see.
	unseen []string
	// statements holds a fragment of each statement of the plan, in order.
	statements []string
	// destructive and privileges are the manager's reading of the plan, which
	// decides what apply: Always runs with nobody looking. A plan with neither
	// runs unattended.
	destructive bool
	privileges  []string
	// beyondURL names the schemas the plan reaches past what the URL covers.
	beyondURL []string
	// planRefusal is what `schema plan` says when it saves no plan, and
	// planCode the code the frame carries then: invalid_plan_output for a plan
	// Ptah failed to make, plan_refused for one it refused by a code.
	planRefusal, planCode string
	// applyRefusal is what `schema apply` says when the database refuses it.
	applyRefusal string
	// driftsAfterApply says the read after the Apply still reports drift.
	driftsAfterApply bool
}

func (row differingRow) family() string { return "differs-" + row.name + "-" + row.engine }

func (row differingRow) database() string {
	return "harness_differs_" + strings.ReplaceAll(row.name, "-", "_") + "_" + row.engine
}

// url is the row's database as its login reaches it.
func (row differingRow) url(l *lab) string {
	switch {
	case row.login == "":
		return l.databaseURL(row.engine, row.database()) + row.urlParameters
	case row.engine == engineMySQL:
		return "mysql://" + row.login + ":" + l.databasePassword + "@" + mysqlHost + ":3306/" + row.database() + row.urlParameters
	default:
		return "postgres://" + row.login + ":" + l.databasePassword + "@" + postgresHost + ":5432/" + row.database() + "?sslmode=disable" + row.urlParameters
	}
}

// differingRows are the shapes a review of the Ptah v0.22.0 repin found, on each
// engine that has them. MySQL has no row for an identity or a serial column, which
// AUTO_INCREMENT stands in for, and none for a foreign table: its FEDERATED engine
// is off unless the server starts with --federated, and the lab's does not.
func differingRows() []differingRow {
	const (
		events  = "CREATE TABLE differs_events (id bigint NOT NULL PRIMARY KEY, happened_at timestamp(6) NOT NULL)"
		event   = "INSERT INTO differs_events VALUES (1, '2026-01-02 03:04:05.123456')"
		item    = "INSERT INTO differs_items (name) VALUES ('first')"
		notes   = "CREATE TABLE differs_notes (id bigint NOT NULL PRIMARY KEY, owner_name text NOT NULL)"
		invoker = "CREATE VIEW differs_notes_view WITH (security_invoker = true) AS SELECT id, owner_name FROM differs_notes"
		plain   = "CREATE VIEW differs_notes_view AS SELECT id, owner_name FROM differs_notes"
		kept    = "CREATE TABLE differs_kept (id bigint NOT NULL PRIMARY KEY)"
		parent  = "CREATE TABLE differs_parent (id bigint NOT NULL PRIMARY KEY)"
		outside = "CREATE SCHEMA differs_outside"
		scoped  = "&search_path=public"
	)
	dropped := []string{"constraints_removed", "tables_removed"}
	rows := []differingRow{
		{
			name: "precision", engine: enginePostgres, declared: "precision-postgresql.sql",
			stored: map[string]string{"observe/schema drift": "differing/drift-column-types-narrowed-postgres.json"},
			seed:   []string{events, event},
			want: differingReading{
				findings:    []string{"column_types_narrowed", "columns_modified"},
				statements:  []string{`ALTER COLUMN "happened_at" TYPE timestamp(0)`},
				destructive: true,
			},
		},
		{
			name: "precision", engine: engineMySQL, declared: "precision-mysql.sql",
			stored: map[string]string{"observe/schema drift": "differing/drift-column-types-narrowed-mysql.json"},
			seed:   []string{events, event},
			want: differingReading{
				findings:    []string{"column_types_narrowed", "columns_modified"},
				statements:  []string{"MODIFY COLUMN `happened_at` timestamp"},
				destructive: true,
			},
		},
		{
			name: "identity", engine: enginePostgres, declared: "plain-key-postgresql.sql",
			stored: map[string]string{"observe/schema drift": "differing/drift-column-identities-removed-postgres.json"},
			seed:   []string{"CREATE TABLE differs_items (id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, name text NOT NULL)", item},
			want: differingReading{
				findings:    []string{"column_identities_removed", "columns_modified"},
				statements:  []string{`ALTER COLUMN "id" DROP IDENTITY`},
				destructive: true,
			},
		},
		{
			name: "serial", engine: enginePostgres, declared: "plain-key-postgresql.sql",
			seed: []string{"CREATE TABLE differs_items (id bigserial PRIMARY KEY, name text NOT NULL)", item},
			want: differingReading{
				findings:    []string{"columns_modified", "sequences_removed"},
				statements:  []string{`ALTER COLUMN "id" DROP DEFAULT`, `DROP SEQUENCE "differs_items_id_seq"`},
				destructive: true,
			},
		},
		{
			name: "auto-increment", engine: engineMySQL, declared: "plain-key-mysql.sql",
			seed: []string{"CREATE TABLE differs_items (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, name varchar(64) NOT NULL)", item},
			want: differingReading{
				findings:    []string{"columns_modified"},
				statements:  []string{"MODIFY COLUMN `id` bigint"},
				destructive: true,
			},
		},
		// Creating a schema and a table takes nothing away, so this plan runs
		// unattended under apply: Always.
		{
			name: "missing-schema", engine: enginePostgres, declared: "missing-schema-postgresql.sql",
			want: differingReading{
				findings:   []string{"schemas_added", "tables_added"},
				statements: []string{`CREATE SCHEMA IF NOT EXISTS "differs_app"`, `CREATE TABLE "differs_app"."differs_widgets"`},
			},
		},
		// So does this one, though the URL covers public alone.
		{
			name: "missing-schema-scoped", engine: enginePostgres, declared: "missing-schema-postgresql.sql", urlParameters: scoped,
			want: differingReading{
				findings:   []string{"schemas_added", "tables_added"},
				statements: []string{`CREATE SCHEMA IF NOT EXISTS "differs_app"`, `CREATE TABLE "differs_app"."differs_widgets"`},
				beyondURL:  []string{"differs_app"},
			},
		},
		// MySQL's schema is the URL's database, so a second one is refused.
		{
			name: "missing-schema", engine: engineMySQL, declared: "missing-schema-mysql.sql",
			want: differingReading{
				driftRefusal: `the document declares database "differs_app"`,
				planRefusal:  `the document declares database "differs_app"`,
				planCode:     "invalid_plan_output",
			},
		},
		{
			name: "view-invoker-unstated", engine: enginePostgres, declared: "view-postgresql.sql",
			seed: []string{notes, invoker},
			want: differingReading{
				findings:    []string{"views_modified"},
				planRefusal: "holds security_invoker=true and the schema does not state it",
				planCode:    "plan_refused",
			},
		},
		{
			name: "view-invoker-false", engine: enginePostgres, declared: "view-invoker-false-postgresql.sql",
			seed: []string{notes, invoker},
			want: differingReading{
				findings:   []string{"views_modified"},
				statements: []string{`ALTER VIEW "differs_notes_view" SET (security_invoker=false)`},
				privileges: []string{"SecurityDefiner"},
			},
		},
		// The view's readers start to need their own rights on the table, so the
		// manager raises the option's arrival as it raises its removal, and this
		// plan waits for an approval under apply: Always.
		{
			name: "view-invoker-true", engine: enginePostgres, declared: "view-invoker-true-postgresql.sql",
			seed: []string{notes, plain},
			want: differingReading{
				findings:   []string{"views_modified"},
				statements: []string{`ALTER VIEW "differs_notes_view" SET (security_invoker=true)`},
				privileges: []string{"SecurityDefiner"},
			},
		},
		// MySQL reads a view's SQL SECURITY nowhere: the reader leaves it out and a
		// SQL schema cannot state it, so the view is read and the report is clean.
		{
			name: "view-invoker-unstated", engine: engineMySQL, declared: "view-mysql.sql",
			seed: []string{
				"CREATE TABLE differs_notes (id bigint NOT NULL PRIMARY KEY, owner_name varchar(64) NOT NULL)",
				"CREATE SQL SECURITY INVOKER VIEW differs_notes_view AS SELECT id, owner_name FROM differs_notes",
			},
			want: differingReading{observed: map[string]int32{"views": 1}},
		},
		// A foreign table is read through what it needs: Ptah names the extension
		// and plans to drop it, and the foreign table stops that drop.
		{
			name: "foreign-table", engine: enginePostgres, declared: "kept-postgresql.sql",
			seed: []string{
				kept,
				"CREATE EXTENSION postgres_fdw",
				"CREATE SERVER differs_remote FOREIGN DATA WRAPPER postgres_fdw OPTIONS (dbname 'postgres')",
				"CREATE FOREIGN TABLE differs_remote_items (id bigint NOT NULL) SERVER differs_remote",
			},
			want: differingReading{
				findings:         []string{"feature_changes:ptah.run/pgextension/extension-change"},
				statements:       []string{`DROP EXTENSION "postgres_fdw"`},
				destructive:      true,
				applyRefusal:     "cannot drop extension postgres_fdw because other objects depend on it",
				driftsAfterApply: true,
			},
		},
		// With no extension in the way nothing names the foreign table at all.
		{
			name: "foreign-table-own-wrapper", engine: enginePostgres, declared: "kept-postgresql.sql",
			seed: []string{
				kept,
				"CREATE FOREIGN DATA WRAPPER differs_wrapper",
				"CREATE SERVER differs_remote FOREIGN DATA WRAPPER differs_wrapper",
				"CREATE FOREIGN TABLE differs_remote_items (id bigint NOT NULL) SERVER differs_remote",
			},
			want: differingReading{observed: map[string]int32{"tables": 1}},
		},
		{
			name: "dependent-view", engine: enginePostgres, declared: "kept-postgresql.sql", urlParameters: scoped,
			seed: []string{kept, parent, outside, "CREATE VIEW differs_outside.parent_ids AS SELECT id FROM public.differs_parent"},
			want: differingReading{
				findings:         dropped,
				statements:       []string{`DROP TABLE "differs_parent"`},
				destructive:      true,
				applyRefusal:     "cannot drop table differs_parent because other objects depend on it (SQLSTATE 2BP01)",
				driftsAfterApply: true,
			},
		},
		{
			name: "dependent-key", engine: enginePostgres, declared: "kept-postgresql.sql", urlParameters: scoped,
			seed: []string{kept, parent, outside, "CREATE TABLE differs_outside.child (parent_id bigint NOT NULL REFERENCES public.differs_parent (id))"},
			want: differingReading{
				findings:         dropped,
				statements:       []string{`DROP TABLE "differs_parent"`},
				destructive:      true,
				applyRefusal:     "cannot drop table differs_parent because other objects depend on it (SQLSTATE 2BP01)",
				driftsAfterApply: true,
			},
		},
		// MySQL lets a table go from under a view, and nothing reports the view
		// left behind in the other database.
		{
			name: "dependent-view", engine: engineMySQL, declared: "kept-mysql.sql",
			seed: []string{
				kept, parent,
				"CREATE DATABASE differs_outside_view",
				"CREATE VIEW differs_outside_view.parent_ids AS SELECT id FROM harness_differs_dependent_view_mysql.differs_parent",
			},
			want: differingReading{
				findings:    dropped,
				statements:  []string{"DROP TABLE `differs_parent`"},
				destructive: true,
			},
		},
		{
			name: "dependent-key", engine: engineMySQL, declared: "kept-mysql.sql",
			seed: []string{
				kept, parent,
				"CREATE DATABASE differs_outside_key",
				"CREATE TABLE differs_outside_key.child (parent_id bigint NOT NULL, FOREIGN KEY (parent_id) REFERENCES harness_differs_dependent_key_mysql.differs_parent (id))",
			},
			want: differingReading{
				findings:         dropped,
				statements:       []string{"DROP TABLE `differs_parent`"},
				destructive:      true,
				applyRefusal:     "Cannot drop table 'differs_parent' referenced by a foreign key constraint",
				driftsAfterApply: true,
			},
		},
		// MySQL builds an index for a foreign key and drops it once another index
		// begins with the key's columns, so Ptah replaces it in the statement that
		// adds the unique key (stokaro/ptah#5695). Ptah rates that statement safe,
		// and the manager holds it as destructive for its DROP INDEX.
		{
			name: "fk-index-takeover", engine: engineMySQL, declared: "fk-unique-mysql.sql",
			seed: []string{
				"CREATE TABLE differs_parent (id bigint NOT NULL PRIMARY KEY)",
				"CREATE TABLE differs_child (id bigint NOT NULL PRIMARY KEY, parent_id bigint NOT NULL, " +
					"CONSTRAINT differs_child_parent FOREIGN KEY (parent_id) REFERENCES differs_parent (id))",
			},
			want: differingReading{
				findings:    []string{"constraints_added", "indexes_removed"},
				statements:  []string{"ALTER TABLE `differs_child` DROP INDEX `differs_child_parent`, ADD CONSTRAINT `differs_child_parent_unique` UNIQUE (`parent_id`)"},
				destructive: true,
			},
		},
	}
	return append(rows, append(partitionRows(), authorityRows()...)...)
}

// partitionRows move a PostgreSQL partition, declared in SQL from a Git-shaped
// source. Ptah rates every statement of these plans a warning. The manager
// holds the two that take the partition's rows out of what its parent returns,
// and runs the bound change and the attachment unattended under apply: Always.
func partitionRows() []differingRow {
	seed := []string{
		"CREATE TABLE differs_events (id bigint NOT NULL, at date NOT NULL, note text) PARTITION BY RANGE (at)",
		"CREATE TABLE differs_e1 PARTITION OF differs_events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')",
		"CREATE TABLE differs_other (id bigint NOT NULL, at date NOT NULL, note text) PARTITION BY RANGE (at)",
		"CREATE TABLE differs_loose (id bigint NOT NULL, at date NOT NULL, note text)",
		"INSERT INTO differs_events VALUES (1, '2026-06-01', 'in-e1')",
		"INSERT INTO differs_loose VALUES (2, '2030-06-01', 'in-loose')",
	}
	const (
		moved  = "feature_changes:ptah.run/postgres/table-partition-member-change"
		detach = `ALTER TABLE "differs_events" DETACH PARTITION "differs_e1"`
	)
	row := func(name string, want differingReading) differingRow {
		return differingRow{
			name: name, engine: enginePostgres, declared: name + "-postgresql.sql", seed: seed, git: true,
			stored: map[string]string{"plan/saved plan": "differing/plan-" + name + ".json"},
			want:   want,
		}
	}
	return []differingRow{
		row("partition-bound", differingReading{
			findings: []string{moved},
			statements: []string{detach,
				`ALTER TABLE "differs_events" ATTACH PARTITION "differs_e1" FOR VALUES FROM ('2026-01-01') TO ('2028-01-01')`},
		}),
		row("partition-detach", differingReading{
			findings:    []string{moved},
			statements:  []string{detach},
			destructive: true,
		}),
		row("partition-parent", differingReading{
			findings: []string{moved},
			statements: []string{detach,
				`ALTER TABLE "differs_other" ATTACH PARTITION "differs_e1" FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`},
			destructive: true,
		}),
		row("partition-attach", differingReading{
			findings:   []string{moved},
			statements: []string{`ALTER TABLE "differs_events" ATTACH PARTITION "differs_loose" FOR VALUES FROM ('2030-01-01') TO ('2031-01-01')`},
		}),
		// What `pg_dump --schema-only --no-owner --no-privileges` of PostgreSQL 17
		// writes for a partitioned table with a key and an index: each partition
		// as a plain table, then its attachment, and a copy of the key and the
		// index on each partition, attached to the parent's. Ptah reads it as the
		// declaration PARTITION OF makes (stokaro/ptah#5722), so the database it
		// was dumped from reads clean and plans nothing.
		{
			name: "partition-dump", engine: enginePostgres, declared: "partition-dump-postgresql.sql", git: true,
			seed: []string{
				"CREATE TABLE differs_events (id bigint NOT NULL, at date NOT NULL, note text DEFAULT 'x', PRIMARY KEY (id, at)) PARTITION BY RANGE (at)",
				"CREATE TABLE differs_events_2026 PARTITION OF differs_events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')",
				"CREATE TABLE differs_events_rest PARTITION OF differs_events DEFAULT",
				"CREATE INDEX differs_events_note ON differs_events (note)",
			},
			want: differingReading{observed: map[string]int32{"tables": 3, "constraints": 1, "indexes": 1}},
		},
		// The new bound leaves the row out, so the attachment fails and the
		// transaction takes the detachment back with it.
		row("partition-bound-excludes-row", differingReading{
			findings: []string{moved},
			statements: []string{detach,
				`ALTER TABLE "differs_events" ATTACH PARTITION "differs_e1" FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`},
			applyRefusal:     `partition constraint of relation "differs_e1" is violated by some row (SQLSTATE 23514)`,
			driftsAfterApply: true,
		}),
	}
}

// authorityRows read as an account that is not the server's superuser: the
// MySQL login the databases page documents, with and without what it needs to
// see every object, and a PostgreSQL login that owns the database it reads.
func authorityRows() []differingRow {
	const (
		// before is the MySQL login the databases page documented before Ptah
		// v0.23.0, and documented is the one it documents now.
		before     = "SELECT, CREATE, ALTER, DROP, INDEX, REFERENCES"
		documented = before + ", TRIGGER, EXECUTE, SHOW VIEW"
		kept       = "CREATE TABLE differs_kept (id bigint NOT NULL PRIMARY KEY)"
		notes      = "CREATE TABLE differs_notes (id bigint NOT NULL PRIMARY KEY, owner_name varchar(64) NOT NULL)"
		view       = "CREATE VIEW differs_notes_view AS SELECT id, owner_name FROM differs_notes"
		function   = "CREATE FUNCTION differs_hand_fn() RETURNS int DETERMINISTIC RETURN 1"
		procedure  = "CREATE PROCEDURE differs_hand_proc() SELECT 1"
		trigger    = "CREATE TRIGGER differs_hand_trigger BEFORE INSERT ON differs_kept FOR EACH ROW SET NEW.id = NEW.id"
		refused    = "cannot see everything in database"
	)
	account := func(name, grants string) []string {
		return []string{
			"CREATE USER '" + name + "'@'%' IDENTIFIED BY '{password}'",
			"GRANT " + grants + " ON {db}.* TO '" + name + "'@'%'",
		}
	}
	owner := func(suffix string) []string {
		return []string{
			"CREATE ROLE harness_owner_" + suffix + " LOGIN PASSWORD '{password}'",
			"CREATE ROLE harness_grantee_" + suffix,
			"ALTER DATABASE {db} OWNER TO harness_owner_" + suffix,
		}
	}
	return []differingRow{
		// Without TRIGGER and a way to list routines, the read cannot prove it
		// saw every trigger and routine, so it refuses an empty database too.
		{
			name: "login-before", engine: engineMySQL, declared: "kept-mysql.sql", seed: []string{kept},
			server: account("harness_login_before", before), login: "harness_login_before",
			stored: map[string]string{
				"observe/schema drift": "differing/drift-permission-denied-triggers-routines.json",
				"plan/schema plan":     "differing/plan-permission-denied-triggers-routines.json",
			},
			want: differingReading{
				driftRefusal: refused, planRefusal: refused, planCode: string(dataplane.TargetPermissionDenied),
				unseen: []string{dataplane.UnseenTriggers, dataplane.UnseenRoutines},
			},
		},
		// The same login refuses a view too: the read names each kind it cannot
		// see, as the first row of the databases page's MySQL table says.
		{
			name: "login-before-view", engine: engineMySQL, declared: "view-mysql.sql", seed: []string{notes, view},
			server: account("harness_login_before_view", before), login: "harness_login_before_view",
			want: differingReading{
				driftRefusal: refused, planRefusal: refused, planCode: string(dataplane.TargetPermissionDenied),
				unseen: []string{dataplane.UnseenViews, dataplane.UnseenTriggers, dataplane.UnseenRoutines},
			},
		},
		// The documented login reads a view and changes the table under it.
		{
			name: "login-documented", engine: engineMySQL, declared: "login-mysql.sql", seed: []string{notes, view},
			server: account("harness_login_documented", documented), login: "harness_login_documented",
			want: differingReading{
				findings:   []string{"columns_added"},
				statements: []string{"ADD COLUMN `note` varchar(64)"},
			},
		},
		// EXECUTE lists only the routines the account could call, and reads the
		// body of only its own, so another definer's routine is refused by name.
		{
			name: "login-other-definer", engine: engineMySQL, declared: "kept-mysql.sql", seed: []string{kept, function},
			server: account("harness_login_definer", documented), login: "harness_login_definer",
			stored: map[string]string{"observe/schema drift": "differing/drift-permission-denied-named-routines.json"},
			want: differingReading{
				driftRefusal: "the bodies of routines differs_hand_fn",
				planRefusal:  "the bodies of routines differs_hand_fn", planCode: string(dataplane.TargetPermissionDenied),
				unseen: []string{dataplane.UnseenRoutines},
			},
		},
		// SHOW_ROUTINE on *.* shows the bodies of routines another account defined,
		// so the documented login with that one global grant reads the trigger and
		// both routines root made, and reads them as removed: the fourth row of the
		// databases page's MySQL table. The login holds no ALTER ROUTINE, which
		// that page adds only where routines are managed, so the Apply stops at
		// the first DROP FUNCTION.
		{
			name: "login-show-routine", engine: engineMySQL, declared: "kept-mysql.sql", seed: []string{kept, trigger, function, procedure},
			server: append(account("harness_login_show_routine", documented), "GRANT SHOW_ROUTINE ON *.* TO 'harness_login_show_routine'@'%'"),
			login:  "harness_login_show_routine",
			want: differingReading{
				findings: []string{"functions_removed", "triggers_removed"},
				counts:   map[string]int32{"functions_removed": 2, "triggers_removed": 1},
				statements: []string{
					"DROP FUNCTION `harness_differs_login_show_routine_mysql`.`differs_hand_fn`",
					"DROP PROCEDURE `harness_differs_login_show_routine_mysql`.`differs_hand_proc`",
					"DROP TRIGGER `differs_hand_trigger`",
				},
				destructive:      true,
				applyRefusal:     "alter routine command denied to user 'harness_login_show_routine'",
				driftsAfterApply: true,
			},
		},
		{
			name: "login-no-show-view", engine: engineMySQL, declared: "view-mysql.sql", seed: []string{notes, view},
			server: account("harness_login_no_view", before+", TRIGGER, EXECUTE"), login: "harness_login_no_view",
			stored: map[string]string{"observe/schema drift": "differing/drift-permission-denied-named-views.json"},
			want: differingReading{
				driftRefusal: "the definitions of views differs_notes_view",
				planRefusal:  "the definitions of views differs_notes_view", planCode: string(dataplane.TargetPermissionDenied),
				unseen: []string{dataplane.UnseenViews},
			},
		},
		// The login owns the database and the table, and the declaration names
		// it. Ptah before stokaro/ptah#5508 planned to revoke the owner's own
		// privileges on the table here.
		{
			name: "owner-existing", engine: enginePostgres, declared: "owner-a-postgresql.sql",
			server: owner("a"), login: "harness_owner_a",
			seed: []string{"SET ROLE harness_owner_a; CREATE TABLE differs_owned (id integer NOT NULL PRIMARY KEY); GRANT SELECT ON differs_owned TO harness_grantee_a"},
			want: differingReading{observed: map[string]int32{"tables": 1}},
		},
		// What an Apply by Ptah before stokaro/ptah#5508 left: the owner holds
		// none of its own privileges on its table. v0.23.0 leaves an owner's
		// privileges out of the comparison, so the read is clean and the plan
		// empty, and they stay that way once the login grants itself the table
		// again, which is the way back the 0.2.10 release notes give.
		{
			name: "owner-revoked", engine: enginePostgres, declared: "owner-c-postgresql.sql",
			server: owner("c"), login: "harness_owner_c",
			seed: []string{"SET ROLE harness_owner_c; CREATE TABLE differs_owned (id integer NOT NULL PRIMARY KEY); " +
				"GRANT SELECT ON differs_owned TO harness_grantee_c; REVOKE ALL ON differs_owned FROM harness_owner_c"},
			fix: []string{"SET ROLE harness_owner_c; GRANT ALL ON TABLE differs_owned TO harness_owner_c"},
			held: []heldQuery{
				{
					name:   "the table's ACL",
					query:  "SELECT relacl::text FROM pg_class WHERE oid = 'differs_owned'::regclass",
					seeded: "{harness_grantee_c=r/harness_owner_c}",
					fixed:  "{harness_grantee_c=r/harness_owner_c,harness_owner_c=arwdDxtm/harness_owner_c}",
				},
				{
					name: "what the owner may do with its table",
					query: "SELECT count(*) FROM unnest(ARRAY['SELECT', 'INSERT', 'UPDATE', 'DELETE', 'TRUNCATE', 'REFERENCES', 'TRIGGER', 'MAINTAIN']) AS privilege " +
						"WHERE has_table_privilege('harness_owner_c', 'differs_owned', privilege)",
					seeded: "0", fixed: "8",
				},
			},
			want: differingReading{observed: map[string]int32{"tables": 1}},
		},
		// On an empty database the plan creates the table and grants it, and the
		// next read finds nothing left to revoke.
		{
			name: "owner-empty", engine: enginePostgres, declared: "owner-b-postgresql.sql",
			server: owner("b"), login: "harness_owner_b",
			want: differingReading{
				findings:   []string{"grants_added", "tables_added"},
				statements: []string{`CREATE TABLE "differs_owned"`, `GRANT SELECT ON TABLE "differs_owned" TO "harness_grantee_b"`},
				privileges: []string{"Grant"},
			},
		},
	}
}

// prepareDifferingServer makes each row's database and what the row needs of
// the server, before any row reads: a read that ran while another row created
// an account could see the server change under it.
func prepareDifferingServer(t *testing.T, l *lab) {
	t.Helper()
	for _, row := range differingRows() {
		if len(row.server) == 0 {
			continue
		}
		l.createDatabase(t, row.engine, row.database())
		administrative := "postgres"
		if row.engine == engineMySQL {
			administrative = "mysql"
		}
		for _, statement := range row.server {
			l.sql(t, row.engine, administrative, strings.NewReplacer("{db}", row.database(), "{password}", l.databasePassword).Replace(statement))
		}
	}
}

func differingDatabases(t *testing.T, l *lab) {
	t.Helper()
	for _, row := range differingRows() {
		t.Run(row.family(), func(t *testing.T) {
			t.Parallel()
			runDifferingRow(t, l, row)
		})
	}
}

// runDifferingRow reads the database, plans, and applies the plan if there is one,
// then reads again. The Apply is what shows a dependent stopping a drop and a
// statement taking the database to its declaration.
func runDifferingRow(t *testing.T, l *lab, row differingRow) {
	t.Helper()
	family, database := row.family(), row.database()
	if len(row.server) == 0 {
		l.createDatabase(t, row.engine, database)
	}
	l.sql(t, row.engine, database, row.seed...)
	var run *schemaRun
	source := func(*rowSpec) {}
	if row.git {
		// The fetch has nothing to add, so the file is already on the volume
		// it would have left it on.
		volume := l.name(family)
		l.fillSource(t, volume, map[string][]byte{"schema/schema.sql": harnessFixture(t, "differing/"+row.declared)})
		run = l.newSchema(t, family, row.engine, row.url(l), operatorv1alpha1.SourceSpec{Git: l.gitHTTPS(gitfixture.SchemaPath, operatorv1alpha1.GitRef{Branch: "main"})})
		commit, content := fetchShapeRevision(t)
		git := run.schema.Spec.Desired.Git
		run.schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{Git: &operatorv1alpha1.GitSourceRevision{
			URL: git.URL, Ref: git.Ref, Path: git.Path, Commit: commit, ContentDigest: content,
		}}
		source = func(spec *rowSpec) { spec.sharedSource = volume }
	} else {
		reference := l.publish(t, "schema", "schemas/"+family, "v1", "--schema-file", "/fixtures/differing/"+row.declared, "--dialect", engineFile(row.engine))
		run = l.newSchema(t, family, row.engine, row.url(l), ociSource(reference))
		run.resolve("resolve")
		run.schema.Spec.Desired.OCIRef = run.schema.Status.Source.ResolvedReference
		run.schema.Status.Source.RequestedReference = run.schema.Status.Source.ResolvedReference
		run.verify("verify", fixture(t, "verification-policy-digest-pin.yaml"))
	}
	recordAny := func(spec *rowSpec) { spec.anyOutcome = true }
	keep := func(step string) func(*rowSpec) {
		return func(spec *rowSpec) {
			for key, file := range row.stored {
				switch kept, command, _ := strings.Cut(key, "/"); {
				case kept != step:
				case command == "saved plan":
					spec.savedPlan = file
				default:
					storedAs(command, file)(spec)
				}
			}
		}
	}
	row.hold(t, l, "before the first read", func(query heldQuery) string { return query.seeded })
	run.observe("observe", recordAny, source, keep("observe"))
	planned := run.plan("plan", nil, recordAny, source, keep("plan"))
	if planned.plan == nil {
		if len(row.fix) == 0 {
			return
		}
		l.sql(t, row.engine, database, row.fix...)
		row.hold(t, l, "after the fix", func(query heldQuery) string { return query.fixed })
		run.observe("observe-fixed", recordAny, source, keep("observe-fixed"))
		run.plan("plan-fixed", nil, recordAny, source, keep("plan-fixed"))
		return
	}
	run.publishPlan("plan", planned.plan)
	run.apply("apply", recordAny, keep("apply"))
	run.observe("observe-applied", recordAny, source, keep("observe-applied"))
}

// hold fails the row when one of its held queries answers other than want says.
func (row differingRow) hold(t *testing.T, l *lab, when string, want func(heldQuery) string) {
	t.Helper()
	for _, query := range row.held {
		if got, expected := l.query(t, row.engine, row.database(), query.query), want(query); got != expected {
			t.Errorf("%s: %s reads %q %s, want %q", row.family(), query.name, got, when, expected)
		}
	}
}

// check says what the captures miss of the row's reading, or nil.
func (row differingRow) check(captures runnerharness.Captures) error {
	return errors.Join(row.checkFirstRead(captures), row.checkPlan(captures), row.checkApply(captures), row.checkFix(captures))
}

// checkFix holds the reads after a row's fix to a database that reads clean and
// plans nothing, and refuses them on a row that names no fix.
func (row differingRow) checkFix(captures runnerharness.Captures) error {
	observe, plan := row.family()+"/observe-fixed", row.family()+"/plan-fixed"
	if len(row.fix) == 0 {
		for _, name := range []string{observe, plan} {
			if _, err := capturedRow(captures, name); err == nil {
				return fmt.Errorf("%s: a row that names no fix read after one", name)
			}
		}
		return nil
	}
	var problems []error
	child, err := capturedChild(captures, observe, "schema drift")
	if err != nil {
		problems = append(problems, err)
	} else if report, err := dataplane.DecodeDrift(captures.Documents[child.Stdout], child.Exit); err != nil {
		problems = append(problems, fmt.Errorf("%s: %w", observe, err))
	} else if report.Drift || len(report.Findings) != 0 {
		problems = append(problems, fmt.Errorf("%s: the read after the fix says drift=%v with %d findings, want it clean", observe, report.Drift, len(report.Findings)))
	}
	record, err := capturedRow(captures, plan)
	switch {
	case err != nil:
		problems = append(problems, err)
	case frameField(record, "planOutcome") != string(runner.PlanOutcomeNoChanges):
		problems = append(problems, fmt.Errorf("%s: the plan after the fix is %q, want %s", plan, frameField(record, "planOutcome"), runner.PlanOutcomeNoChanges))
	}
	return errors.Join(problems...)
}

func (row differingRow) checkFirstRead(captures runnerharness.Captures) error {
	child, err := capturedChild(captures, row.family()+"/observe", "schema drift")
	if err != nil {
		return err
	}
	document := captures.Documents[child.Stdout]
	if row.want.driftRefusal != "" {
		failure, err := dataplane.DecodeDriftFailure(document)
		if err != nil {
			return fmt.Errorf("%s: the first read refuses nothing: %w", row.family(), err)
		}
		if !strings.Contains(failure.Message, row.want.driftRefusal) {
			return fmt.Errorf("%s: the first read refuses with %q, want %q", row.family(), failure.Message, row.want.driftRefusal)
		}
		record, err := capturedRow(captures, row.family()+"/observe")
		if err != nil {
			return err
		}
		return row.checkUnseen(record)
	}
	report, err := dataplane.DecodeDrift(document, child.Exit)
	if err != nil {
		return fmt.Errorf("%s: the first read: %w", row.family(), err)
	}
	var problems []error
	categories := make([]string, 0, len(report.Findings))
	for _, finding := range report.Findings {
		categories = append(categories, finding.Category)
	}
	slices.Sort(categories)
	if !slices.Equal(categories, row.want.findings) {
		problems = append(problems, fmt.Errorf("%s: the first read finds %q, want %q", row.family(), categories, row.want.findings))
	}
	for category, want := range row.want.counts {
		index := slices.IndexFunc(report.Findings, func(finding dataplane.DriftFinding) bool { return finding.Category == category })
		switch {
		case index < 0:
			problems = append(problems, fmt.Errorf("%s: the first read has no %s to count", row.family(), category))
		case report.Findings[index].Count != want:
			problems = append(problems, fmt.Errorf("%s: the first read counts %d %s, want %d", row.family(), report.Findings[index].Count, category, want))
		}
	}
	if len(row.want.findings) == 0 && len(row.want.observed) == 0 {
		problems = append(problems, fmt.Errorf("%s: a clean report has to show what it read, and the row names no inventory", row.family()))
	}
	for kind, want := range row.want.observed {
		index := slices.IndexFunc(report.Inventory, func(entry dataplane.DriftInventoryEntry) bool { return entry.Kind == kind })
		switch {
		case index < 0:
			problems = append(problems, fmt.Errorf("%s: the first read has no inventory of %s", row.family(), kind))
		case report.Inventory[index].Observed != want:
			problems = append(problems, fmt.Errorf("%s: the first read's inventory observes %d %s, want %d", row.family(), report.Inventory[index].Observed, kind, want))
		}
	}
	return errors.Join(problems...)
}

func (row differingRow) checkPlan(captures runnerharness.Captures) error {
	name := row.family() + "/plan"
	record, err := capturedRow(captures, name)
	if err != nil {
		return err
	}
	if row.want.planRefusal != "" {
		child, err := capturedChild(captures, name, "schema plan")
		if err != nil {
			return err
		}
		var problems []error
		if row.want.planCode == "" {
			problems = append(problems, fmt.Errorf("%s: the row names a refusal and no code for it", name))
		}
		if !strings.Contains(child.Stderr, row.want.planRefusal) {
			problems = append(problems, fmt.Errorf("%s: `schema plan` says %q, want the refusal %q", name, child.Stderr, row.want.planRefusal))
		}
		if code := frameCode(record); code != row.want.planCode {
			problems = append(problems, fmt.Errorf("%s: the frame says %q, want %s", name, code, row.want.planCode))
		}
		return errors.Join(append(problems, row.checkUnseen(record))...)
	}
	if len(row.want.statements) == 0 {
		if outcome := frameField(record, "planOutcome"); outcome != string(runner.PlanOutcomeNoChanges) {
			return fmt.Errorf("%s: the plan outcome is %q, want %s", name, outcome, runner.PlanOutcomeNoChanges)
		}
		return nil
	}
	if record.SavedPlan == "" {
		return fmt.Errorf("%s: the row saved no plan", name)
	}
	plan, err := dataplane.DecodePlan(captures.Documents[record.SavedPlan], record.Engine)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	var problems []error
	if len(plan.Statements) != len(row.want.statements) {
		problems = append(problems, fmt.Errorf("%s: the plan holds %d statements, want %d", name, len(plan.Statements), len(row.want.statements)))
	}
	for index, fragment := range row.want.statements {
		if index < len(plan.Statements) && !strings.Contains(plan.Statements[index].SQL, fragment) {
			problems = append(problems, fmt.Errorf("%s: statement %d is %q, want %q in it", name, index, plan.Statements[index].SQL, fragment))
		}
	}
	if plan.Destructive != row.want.destructive {
		problems = append(problems, fmt.Errorf("%s: the manager reads destructive=%v, want %v", name, plan.Destructive, row.want.destructive))
	}
	if !slices.Equal(plan.PrivilegeChanges, row.want.privileges) {
		problems = append(problems, fmt.Errorf("%s: the manager reads the privilege changes %q, want %q", name, plan.PrivilegeChanges, row.want.privileges))
	}
	if !slices.Equal(plan.SchemasBeyondURL, row.want.beyondURL) {
		problems = append(problems, fmt.Errorf("%s: the plan reaches past the URL to %q, want %q", name, plan.SchemasBeyondURL, row.want.beyondURL))
	}
	return errors.Join(problems...)
}

// checkApply holds the rows a saved plan leads to, and refuses them on a row that
// should have stopped at its plan.
func (row differingRow) checkApply(captures runnerharness.Captures) error {
	apply := row.family() + "/apply"
	if len(row.want.statements) == 0 {
		if _, err := capturedRow(captures, apply); err == nil {
			return fmt.Errorf("%s: a row that plans nothing applied", apply)
		}
		return nil
	}
	record, err := capturedRow(captures, apply)
	if err != nil {
		return err
	}
	child, err := capturedChild(captures, apply, "schema apply")
	if err != nil {
		return err
	}
	var problems []error
	switch code := frameCode(record); {
	case row.want.applyRefusal == "" && code != "":
		problems = append(problems, fmt.Errorf("%s: the frame says %q, want no error", apply, code))
	case row.want.applyRefusal != "" && code != "apply_outcome_unknown":
		problems = append(problems, fmt.Errorf("%s: the frame says %q, want apply_outcome_unknown", apply, code))
	case !strings.Contains(child.Stderr, row.want.applyRefusal):
		problems = append(problems, fmt.Errorf("%s: `schema apply` says %q, want the refusal %q", apply, child.Stderr, row.want.applyRefusal))
	}
	after, err := capturedChild(captures, row.family()+"/observe-applied", "schema drift")
	if err != nil {
		return errors.Join(append(problems, err)...)
	}
	report, err := dataplane.DecodeDrift(captures.Documents[after.Stdout], after.Exit)
	if err != nil {
		return errors.Join(append(problems, fmt.Errorf("%s: the read after the Apply: %w", row.family(), err))...)
	}
	if report.Drift != row.want.driftsAfterApply {
		problems = append(problems, fmt.Errorf("%s: the read after the Apply says drift=%v, want %v", row.family(), report.Drift, row.want.driftsAfterApply))
	}
	return errors.Join(problems...)
}

// checkUnseen holds a refused read's frame to the kinds of object the row's
// account cannot see, and to none where the row names none.
func (row differingRow) checkUnseen(record runnerharness.Row) error {
	frame, _ := record.Result.(map[string]any)
	failure, _ := frame["error"].(map[string]any)
	listed, _ := failure["unseenObjects"].([]any)
	var unseen []string
	for _, kind := range listed {
		text, _ := kind.(string)
		unseen = append(unseen, text)
	}
	if !slices.Equal(unseen, row.want.unseen) {
		return fmt.Errorf("%s: the frame says the account cannot see %q, want %q", record.Name, unseen, row.want.unseen)
	}
	return nil
}

func capturedRow(captures runnerharness.Captures, name string) (runnerharness.Row, error) {
	index := slices.IndexFunc(captures.Rows, func(row runnerharness.Row) bool { return row.Name == name })
	if index < 0 {
		return runnerharness.Row{}, fmt.Errorf("no row %s", name)
	}
	return captures.Rows[index], nil
}

func capturedChild(captures runnerharness.Captures, row, command string) (runnerharness.Child, error) {
	record, err := capturedRow(captures, row)
	if err != nil {
		return runnerharness.Child{}, err
	}
	index := slices.IndexFunc(record.Children, func(child runnerharness.Child) bool { return child.Command == command })
	if index < 0 {
		return runnerharness.Child{}, fmt.Errorf("%s: no `ptah %s` ran", row, command)
	}
	return record.Children[index], nil
}

func frameField(row runnerharness.Row, field string) string {
	frame, _ := row.Result.(map[string]any)
	value, _ := frame[field].(string)
	return value
}

func frameCode(row runnerharness.Row) string {
	frame, _ := row.Result.(map[string]any)
	failure, _ := frame["error"].(map[string]any)
	code, _ := failure["code"].(string)
	return code
}

// Each row's committed reading holds what the row is named for, and the manager's
// rating of its plan. A repin that moves one writes its captures, and this asks
// whether the move is wanted. Ptah's own rating stays out of it: the diff shows
// that, and what decides an unattended Apply is the manager's.
func TestDifferingRowsReachTheirShape(t *testing.T) {
	t.Parallel()
	_, captures := committedCaptures(t)
	declared := map[string]bool{}
	for _, row := range differingRows() {
		declared[row.family()] = true
		t.Run(row.family(), func(t *testing.T) {
			t.Parallel()
			if err := row.check(captures); err != nil {
				t.Error(err)
			}
		})
	}
	if len(declared) != len(differingRows()) {
		t.Errorf("%d rows name %d families", len(differingRows()), len(declared))
	}
	captured := map[string]bool{}
	for _, row := range captures.Rows {
		if family, _, found := strings.Cut(row.Name, "/"); found && strings.HasPrefix(family, "differs-") {
			captured[family] = true
		}
	}
	if !maps.Equal(captured, declared) {
		t.Errorf("the captures hold the row sets %v, and differingRows declares %v", slices.Sorted(maps.Keys(captured)), slices.Sorted(maps.Keys(declared)))
	}
}

// The check refuses a reading that missed its shape. Each reading below is a
// committed capture moved where it does not belong, or edited the way a release
// could, so the check meets what a real run printed.
func TestDifferingCheckRefusesAReadingThatMissedItsShape(t *testing.T) {
	t.Parallel()
	_, committed := committedCaptures(t)
	rows := map[string]differingRow{}
	for _, row := range differingRows() {
		rows[row.family()] = row
	}
	editRow := func(captures *runnerharness.Captures, name string, edit func(*runnerharness.Row)) {
		t.Helper()
		index := slices.IndexFunc(captures.Rows, func(row runnerharness.Row) bool { return row.Name == name })
		if index < 0 {
			t.Fatalf("no row %s", name)
		}
		captures.Rows[index].Children = slices.Clone(captures.Rows[index].Children)
		edit(&captures.Rows[index])
	}
	// useDocument puts what one row's child printed, and the status it exited
	// with, in another's place. The decoder holds a report to its status.
	useDocument := func(captures *runnerharness.Captures, from, into, command string) {
		t.Helper()
		source, err := capturedChild(*captures, from, command)
		if err != nil {
			t.Fatal(err)
		}
		editRow(captures, into, func(row *runnerharness.Row) {
			index := slices.IndexFunc(row.Children, func(child runnerharness.Child) bool { return child.Command == command })
			if index < 0 {
				t.Fatalf("%s: no `ptah %s` ran", into, command)
			}
			row.Children[index].Exit = source.Exit
			captures.Documents[row.Children[index].Stdout] = captures.Documents[source.Stdout]
		})
	}
	// editDocument rewrites what one row's child printed, the way a release
	// that read the database differently would have printed it.
	editDocument := func(captures *runnerharness.Captures, name, command string, edit func(map[string]any)) {
		t.Helper()
		child, err := capturedChild(*captures, name, command)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(captures.Documents[child.Stdout]))
		decoder.UseNumber()
		var document map[string]any
		if err := decoder.Decode(&document); err != nil {
			t.Fatal(err)
		}
		edit(document)
		encoded, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		captures.Documents[child.Stdout] = encoded
	}
	// setEntry sets one field of the entry of a document's list whose key
	// field has the given value.
	setEntry := func(document map[string]any, list, key, value, field string, to any) {
		t.Helper()
		entries, _ := document[list].([]any)
		for _, entry := range entries {
			if fields, _ := entry.(map[string]any); fields[key] == value {
				fields[field] = to
				return
			}
		}
		t.Fatalf("the document's %s holds no %s %q", list, key, value)
	}
	usePlan := func(captures *runnerharness.Captures, from, into string) {
		t.Helper()
		source, err := capturedRow(*captures, from+"/plan")
		if err != nil {
			t.Fatal(err)
		}
		target, err := capturedRow(*captures, into+"/plan")
		if err != nil {
			t.Fatal(err)
		}
		captures.Documents[target.SavedPlan] = captures.Documents[source.SavedPlan]
	}
	for _, mistake := range []struct {
		name, row, want string
		edit            func(*runnerharness.Captures)
	}{
		{
			name: "a first read that found the database converged", row: "differs-precision-postgres", want: "the first read finds",
			edit: func(captures *runnerharness.Captures) {
				useDocument(captures, "differs-precision-postgres/observe-applied", "differs-precision-postgres/observe", "schema drift")
			},
		},
		{
			name: "a first read of a database that holds no view", row: "differs-view-invoker-unstated-mysql", want: "no inventory of views",
			edit: func(captures *runnerharness.Captures) {
				useDocument(captures, "differs-foreign-table-own-wrapper-postgres/observe", "differs-view-invoker-unstated-mysql/observe", "schema drift")
			},
		},
		{
			name: "a foreign table counted among the tables", row: "differs-foreign-table-own-wrapper-postgres", want: "observes 2 tables",
			edit: func(captures *runnerharness.Captures) {
				useDocument(captures, "differs-dependent-view-postgres/observe", "differs-foreign-table-own-wrapper-postgres/observe", "schema drift")
			},
		},
		{
			name: "a report where a refusal belongs", row: "differs-missing-schema-mysql", want: "the first read refuses nothing",
			edit: func(captures *runnerharness.Captures) {
				useDocument(captures, "differs-precision-mysql/observe", "differs-missing-schema-mysql/observe", "schema drift")
			},
		},
		{
			name: "a plan of another shape", row: "differs-precision-postgres", want: `want "ALTER COLUMN`,
			edit: func(captures *runnerharness.Captures) {
				usePlan(captures, "differs-identity-postgres", "differs-precision-postgres")
			},
		},
		{
			name: "a narrowing the manager reads as safe", row: "differs-precision-postgres", want: "the manager reads destructive=false",
			edit: func(captures *runnerharness.Captures) {
				usePlan(captures, "differs-missing-schema-postgres", "differs-precision-postgres")
			},
		},
		{
			name: "a plan the manager holds that should run unattended", row: "differs-missing-schema-postgres", want: "the manager reads destructive=true",
			edit: func(captures *runnerharness.Captures) {
				usePlan(captures, "differs-precision-postgres", "differs-missing-schema-postgres")
			},
		},
		{
			name: "a change of authority the manager does not read", row: "differs-view-invoker-false-postgres", want: "the manager reads the privilege changes",
			edit: func(captures *runnerharness.Captures) {
				usePlan(captures, "differs-missing-schema-postgres", "differs-view-invoker-false-postgres")
			},
		},
		{
			name: "a URL that covered the schema after all", row: "differs-missing-schema-scoped-postgres", want: "the plan reaches past the URL",
			edit: func(captures *runnerharness.Captures) {
				usePlan(captures, "differs-missing-schema-postgres", "differs-missing-schema-scoped-postgres")
			},
		},
		{
			name: "a statement more than the row names", row: "differs-identity-postgres", want: "the plan holds 2 statements, want 1",
			edit: func(captures *runnerharness.Captures) {
				usePlan(captures, "differs-serial-postgres", "differs-identity-postgres")
			},
		},
		{
			name: "a refusal that lost its sentence", row: "differs-view-invoker-unstated-postgres", want: "want the refusal",
			edit: func(captures *runnerharness.Captures) {
				editRow(captures, "differs-view-invoker-unstated-postgres/plan", func(row *runnerharness.Row) {
					row.Children[1].Stderr = "Planned schema changes:\n"
				})
			},
		},
		{
			name: "a refusal under another code", row: "differs-view-invoker-unstated-postgres", want: "want plan_refused",
			edit: func(captures *runnerharness.Captures) {
				editRow(captures, "differs-view-invoker-unstated-postgres/plan", func(row *runnerharness.Row) {
					row.Result = map[string]any{"error": map[string]any{"code": "invalid_plan_output"}}
				})
			},
		},
		{
			name: "a refused read that names fewer objects", row: "differs-login-before-mysql", want: "the frame says the account cannot see",
			edit: func(captures *runnerharness.Captures) {
				editRow(captures, "differs-login-before-mysql/observe", func(row *runnerharness.Row) {
					row.Result = map[string]any{"error": map[string]any{"code": "permission_denied", "unseenObjects": []any{"routines"}}}
				})
			},
		},
		{
			name: "a refused plan that names no objects", row: "differs-login-before-mysql", want: "the frame says the account cannot see",
			edit: func(captures *runnerharness.Captures) {
				editRow(captures, "differs-login-before-mysql/plan", func(row *runnerharness.Row) {
					row.Result = map[string]any{"error": map[string]any{"code": "permission_denied"}}
				})
			},
		},
		{
			name: "a moved partition the manager runs unattended", row: "differs-partition-detach-postgres", want: "the manager reads destructive=false",
			edit: func(captures *runnerharness.Captures) {
				usePlan(captures, "differs-partition-bound-postgres", "differs-partition-detach-postgres")
			},
		},
		{
			name: "a plan where the row reads no change", row: "differs-foreign-table-own-wrapper-postgres", want: "the plan outcome is",
			edit: func(captures *runnerharness.Captures) {
				editRow(captures, "differs-foreign-table-own-wrapper-postgres/plan", func(row *runnerharness.Row) {
					row.Result = map[string]any{"planOutcome": string(runner.PlanOutcomeChanges)}
				})
			},
		},
		{
			name: "an Apply the dependent did not stop", row: "differs-dependent-view-postgres", want: "want apply_outcome_unknown",
			edit: func(captures *runnerharness.Captures) {
				editRow(captures, "differs-dependent-view-postgres/apply", func(row *runnerharness.Row) {
					row.Result = map[string]any{"operation": "apply"}
				})
			},
		},
		{
			name: "an Apply refused for another reason", row: "differs-dependent-view-postgres", want: "want the refusal",
			edit: func(captures *runnerharness.Captures) {
				editRow(captures, "differs-dependent-view-postgres/apply", func(row *runnerharness.Row) {
					row.Children[0].Stderr = "error: connection reset"
				})
			},
		},
		{
			name: "an Apply that left the database differing", row: "differs-precision-postgres", want: "the read after the Apply says drift=true",
			edit: func(captures *runnerharness.Captures) {
				useDocument(captures, "differs-precision-postgres/observe", "differs-precision-postgres/observe-applied", "schema drift")
			},
		},
		{
			name: "an Apply where the plan should have stopped the row", row: "differs-view-invoker-unstated-mysql", want: "a row that plans nothing applied",
			edit: func(captures *runnerharness.Captures) {
				captures.Rows = append(captures.Rows, runnerharness.Row{Name: "differs-view-invoker-unstated-mysql/apply"})
			},
		},
		{
			name: "a routine the read did not count", row: "differs-login-show-routine-mysql", want: "counts 1 functions_removed, want 2",
			edit: func(captures *runnerharness.Captures) {
				editDocument(captures, "differs-login-show-routine-mysql/observe", "schema drift", func(document map[string]any) {
					setEntry(document, "findings", "category", "functions_removed", "count", 1)
				})
			},
		},
		{
			name: "a read that found no routine to count", row: "differs-login-show-routine-mysql", want: "has no functions_removed to count",
			edit: func(captures *runnerharness.Captures) {
				editDocument(captures, "differs-login-show-routine-mysql/observe", "schema drift", func(document map[string]any) {
					setEntry(document, "findings", "category", "functions_removed", "category", "views_removed")
				})
			},
		},
		{
			name: "a refused read that does not name the view", row: "differs-login-before-view-mysql", want: "the frame says the account cannot see",
			edit: func(captures *runnerharness.Captures) {
				editRow(captures, "differs-login-before-view-mysql/observe", func(row *runnerharness.Row) {
					row.Result = map[string]any{"error": map[string]any{"code": "permission_denied", "unseenObjects": []any{"triggers", "routines"}}}
				})
			},
		},
		{
			name: "a key takeover the manager runs unattended", row: "differs-fk-index-takeover-mysql", want: "the manager reads destructive=false",
			edit: func(captures *runnerharness.Captures) {
				usePlan(captures, "differs-login-documented-mysql", "differs-fk-index-takeover-mysql")
			},
		},
		{
			name: "a dump whose index copies read as indexes of their own", row: "differs-partition-dump-postgres", want: "observes 3 indexes, want 1",
			edit: func(captures *runnerharness.Captures) {
				editDocument(captures, "differs-partition-dump-postgres/observe", "schema drift", func(document map[string]any) {
					setEntry(document, "inventory", "kind", "indexes", "observed", 3)
				})
			},
		},
		{
			name: "a fix the database did not take", row: "differs-owner-revoked-postgres", want: "the read after the fix says drift=true",
			edit: func(captures *runnerharness.Captures) {
				useDocument(captures, "differs-precision-postgres/observe", "differs-owner-revoked-postgres/observe-fixed", "schema drift")
			},
		},
		{
			name: "a plan after the fix that grants the owner back", row: "differs-owner-revoked-postgres", want: "the plan after the fix is",
			edit: func(captures *runnerharness.Captures) {
				editRow(captures, "differs-owner-revoked-postgres/plan-fixed", func(row *runnerharness.Row) {
					row.Result = map[string]any{"planOutcome": string(runner.PlanOutcomeChanges)}
				})
			},
		},
		{
			name: "a fix that stopped reading", row: "differs-owner-revoked-postgres", want: "no row differs-owner-revoked-postgres/plan-fixed",
			edit: func(captures *runnerharness.Captures) {
				captures.Rows = slices.DeleteFunc(captures.Rows, func(row runnerharness.Row) bool { return row.Name == "differs-owner-revoked-postgres/plan-fixed" })
			},
		},
		{
			name: "a read after a fix the row does not name", row: "differs-owner-existing-postgres", want: "a row that names no fix read after one",
			edit: func(captures *runnerharness.Captures) {
				captures.Rows = append(captures.Rows, runnerharness.Row{Name: "differs-owner-existing-postgres/observe-fixed"})
			},
		},
		{
			name: "a row set that stopped running", row: "differs-serial-postgres", want: "no row differs-serial-postgres/plan",
			edit: func(captures *runnerharness.Captures) {
				captures.Rows = slices.DeleteFunc(captures.Rows, func(row runnerharness.Row) bool { return row.Name == "differs-serial-postgres/plan" })
			},
		},
	} {
		t.Run(mistake.name, func(t *testing.T) {
			t.Parallel()
			row, declared := rows[mistake.row]
			if !declared {
				t.Fatalf("no row %s", mistake.row)
			}
			captures := runnerharness.Captures{Rows: slices.Clone(committed.Rows), Documents: maps.Clone(committed.Documents)}
			mistake.edit(&captures)
			err := row.check(captures)
			if err == nil || !strings.Contains(err.Error(), mistake.want) {
				t.Fatalf("check() = %v, want a refusal holding %q", err, mistake.want)
			}
		})
	}
}
