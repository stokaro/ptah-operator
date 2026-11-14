package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The drift rows connect as the login the page tells a person to create, and
// the MySQL drift row then grants it INSERT on the schema apply log as the
// page shows. If the page and the fixture moved apart, a row would prove a
// login nobody is told to make, and nothing in a cluster run would say so.
func TestDocumentedLoginIsTheOneThePageShows(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(databasesPage)))
	if err != nil {
		t.Fatalf("read %s: %v", databasesPage, err)
	}
	page := string(raw)
	if err := pageShows(page, postgreSQLAuthoritySection, 0, documentedPostgreSQLLogin("ptah", "app")); err != nil {
		t.Errorf("%s: %v", databasesPage, err)
	}
	if err := pageShows(page, mySQLAuthoritySection, mySQLGrantBlock, []string{documentedMySQLGrant("app", "'ptah'@'%'")}); err != nil {
		t.Errorf("%s: %v", databasesPage, err)
	}
	if err := pageShows(page, mySQLAuthoritySection, mySQLApplyLogGrantBlock, []string{documentedMySQLApplyLogGrant("app", "'ptah'@'%'")}); err != nil {
		t.Errorf("%s: %v", databasesPage, err)
	}
}

// Each refused page is the accepted one with the edit a person could make to
// the page alone.
func TestPageShowsRefusesAPageThatMovedApart(t *testing.T) {
	t.Parallel()
	const (
		mysqlGrant    = "GRANT SELECT, CREATE, ALTER, DROP, INDEX, REFERENCES, TRIGGER, EXECUTE, SHOW VIEW ON `app`.* TO 'ptah'@'%';"
		routineGrant  = "```sql\nGRANT SHOW_ROUTINE ON *.* TO 'ptah'@'%';\n```\n"
		applyLogGrant = "```sql\nGRANT INSERT ON `app`.`ptah_schema_apply_log` TO 'ptah'@'%';\n```\n"
	)
	accepted := strings.Join([]string{
		"# Databases", "",
		postgreSQLAuthoritySection, "", "The login needs only:", "",
		"```sql",
		"CREATE ROLE ptah LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;",
		"GRANT CONNECT, TEMPORARY ON DATABASE app TO ptah;",
		"GRANT USAGE, CREATE ON SCHEMA public TO ptah;",
		"```", "",
		mySQLAuthoritySection, "", "Grant:", "",
		"```sql", mysqlGrant, "```", "",
		routineGrant, "To keep the rows, once an Apply has created the table:", "",
		applyLogGrant, "## What the login cannot see", "",
	}, "\n")
	postgres := documentedPostgreSQLLogin("ptah", "app")
	mysql := []string{documentedMySQLGrant("app", "'ptah'@'%'")}
	applyLog := []string{documentedMySQLApplyLogGrant("app", "'ptah'@'%'")}
	if err := pageShows(accepted, postgreSQLAuthoritySection, 0, postgres); err != nil {
		t.Fatalf("the PostgreSQL statements were refused: %v", err)
	}
	if err := pageShows(accepted, mySQLAuthoritySection, mySQLGrantBlock, mysql); err != nil {
		t.Fatalf("the MySQL grant was refused: %v", err)
	}
	if err := pageShows(accepted, mySQLAuthoritySection, mySQLApplyLogGrantBlock, applyLog); err != nil {
		t.Fatalf("the MySQL apply log grant was refused: %v", err)
	}
	for _, row := range []struct {
		name, heading, from, to string
		block                   int
		want                    []string
	}{
		{"a PostgreSQL login made a superuser", postgreSQLAuthoritySection, "NOSUPERUSER", "SUPERUSER", 0, postgres},
		{"a PostgreSQL grant dropped", postgreSQLAuthoritySection, "GRANT CONNECT, TEMPORARY ON DATABASE app TO ptah;\n", "", 0, postgres},
		{"TEMPORARY dropped from the PostgreSQL grant", postgreSQLAuthoritySection, "CONNECT, TEMPORARY ON DATABASE", "CONNECT ON DATABASE", 0, postgres},
		{"a PostgreSQL grant added", postgreSQLAuthoritySection, "GRANT USAGE, CREATE ON SCHEMA public TO ptah;\n",
			"GRANT USAGE, CREATE ON SCHEMA public TO ptah;\nGRANT CREATE ON DATABASE app TO ptah;\n", 0, postgres},
		{"a PostgreSQL schema privilege dropped", postgreSQLAuthoritySection, "USAGE, CREATE ON SCHEMA", "USAGE ON SCHEMA", 0, postgres},
		{"the PostgreSQL statements moved out of their section", postgreSQLAuthoritySection, "```sql\nCREATE ROLE", "```text\nCREATE ROLE", 0, postgres},
		{"a MySQL privilege dropped", mySQLAuthoritySection, "EXECUTE, SHOW VIEW", "SHOW VIEW", mySQLGrantBlock, mysql},
		{"a MySQL privilege added", mySQLAuthoritySection, "SHOW VIEW ON", "SHOW VIEW, CREATE VIEW ON", mySQLGrantBlock, mysql},
		{"the MySQL grant widened to every database", mySQLAuthoritySection, "ON `app`.*", "ON *.*", mySQLGrantBlock, mysql},
		{"the MySQL grant removed, leaving the global one first", mySQLAuthoritySection, "```sql\n" + mysqlGrant + "\n```\n", "", mySQLGrantBlock, mysql},
		{"the apply log grant widened to the database", mySQLAuthoritySection, "ON `app`.`ptah_schema_apply_log`", "ON `app`.*", mySQLApplyLogGrantBlock, applyLog},
		{"the apply log grant on another table", mySQLAuthoritySection, "`app`.`ptah_schema_apply_log`", "`app`.`schema_migrations_log`", mySQLApplyLogGrantBlock, applyLog},
		{"a privilege added to the apply log grant", mySQLAuthoritySection, "GRANT INSERT ON", "GRANT INSERT, UPDATE ON", mySQLApplyLogGrantBlock, applyLog},
		{"the apply log grant to another account", mySQLAuthoritySection, "`ptah_schema_apply_log` TO 'ptah'@'%'", "`ptah_schema_apply_log` TO 'ptah'@'localhost'", mySQLApplyLogGrantBlock, applyLog},
		{"a statement added to the apply log grant's block", mySQLAuthoritySection, "`ptah_schema_apply_log` TO 'ptah'@'%';",
			"`ptah_schema_apply_log` TO 'ptah'@'%';\nGRANT UPDATE ON `app`.`ptah_schema_apply_log` TO 'ptah'@'%';", mySQLApplyLogGrantBlock, applyLog},
		{"the apply log grant removed", mySQLAuthoritySection, applyLogGrant, "", mySQLApplyLogGrantBlock, applyLog},
		{"the apply log grant moved ahead of the global one", mySQLAuthoritySection,
			routineGrant + "\nTo keep the rows, once an Apply has created the table:\n\n" + applyLogGrant,
			applyLogGrant + "\nTo keep the rows, once an Apply has created the table:\n\n" + routineGrant, mySQLApplyLogGrantBlock, applyLog},
		{"the apply log grant moved out of its section", mySQLAuthoritySection, applyLogGrant + "\n## What the login cannot see\n",
			"## What the login cannot see\n\n" + applyLogGrant, mySQLApplyLogGrantBlock, applyLog},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			page := strings.Replace(accepted, row.from, row.to, 1)
			if page == accepted {
				t.Fatalf("the edit %q changed nothing", row.from)
			}
			if err := pageShows(page, row.heading, row.block, row.want); err == nil {
				t.Errorf("a page with %s was accepted", row.name)
			}
		})
	}
}

// The readings are what the guard queries returned on the matrix images,
// PostgreSQL 17.11 and MySQL 8.4.11, as the server's superuser and as root.
// The accepted ones came from logins made the way the fixtures make them. The
// refused ones are the logins the drift rows connected as before, and the
// mistakes a fixture could make that would still let every row pass.
func TestDocumentedLoginReadingsRefuseWiderLogins(t *testing.T) {
	t.Parallel()
	const (
		documented = "f|f|f|f|f|t|0|t|f|t|t|t"
		// The image's POSTGRES_USER, which every PostgreSQL row connected as.
		superuser = "t|t|t|t|t|t|0|t|t|t|t|t"
		// The page's statements before they granted TEMPORARY, under the
		// revoke: Ptah's column probe cannot create its temporary table, and
		// the row's SQL audit refused the probe it falls back to.
		noTemporary = "f|f|f|f|f|t|0|t|f|f|t|t"
		// The statements and CREATE on the database, for a trusted extension.
		extension = "f|f|f|f|f|t|0|t|t|t|t|t"
		// An account made with the page's grant.
		mysqlDocumented = "ALTER,CREATE,DROP,EXECUTE,INDEX,REFERENCES,SELECT,SHOW VIEW,TRIGGER|0|USAGE|0|0"
		// The audit account the MySQL rows connected as: ALL PRIVILEGES on one
		// database.
		mysqlAll = "ALTER,ALTER ROUTINE,CREATE,CREATE ROUTINE,CREATE TEMPORARY TABLES,CREATE VIEW,DELETE,DROP,EVENT," +
			"EXECUTE,INDEX,INSERT,LOCK TABLES,REFERENCES,SELECT,SHOW VIEW,TRIGGER,UPDATE|0|USAGE|0|0"
		// The page's privileges granted on the name with its underscores
		// unescaped, which MySQL reads as a pattern covering other databases:
		// the query counts all nine as held on another name. Read on MySQL
		// 8.4.13 with partial_revokes off.
		mysqlUnescaped = "|9|USAGE|0|0"
	)
	if got := documentedPostgreSQLLoginReading(false); got != documented {
		t.Errorf("the documented login reads %q, and the guard expects %q", documented, got)
	}
	if got := documentedPostgreSQLLoginReading(true); got != extension {
		t.Errorf("the documented login with CREATE on the database reads %q, and the guard expects %q", extension, got)
	}
	if got := documentedMySQLAccountReading(); got != mysqlDocumented {
		t.Errorf("the documented account reads %q, and the guard expects %q", mysqlDocumented, got)
	}
	for _, reading := range []string{superuser, noTemporary, extension} {
		if reading == documentedPostgreSQLLoginReading(false) {
			t.Errorf("the PostgreSQL guard accepts %q", reading)
		}
	}
	for _, reading := range []string{superuser, noTemporary, documented} {
		if reading == documentedPostgreSQLLoginReading(true) {
			t.Errorf("the PostgreSQL extension guard accepts %q", reading)
		}
	}
	for _, reading := range []string{mysqlAll, mysqlUnescaped} {
		if reading == documentedMySQLAccountReading() {
			t.Errorf("the MySQL guard accepts %q", reading)
		}
	}
}

// A database-level grant reads an unescaped _ or % as a wildcard, so the grant
// has to escape them, and the readback has to look for the name as the grant
// stored it, escapes included.
func TestDocumentedMySQLGrantEscapesTheDatabaseName(t *testing.T) {
	t.Parallel()
	if grant := documentedMySQLGrant("e2e_drift%x", "'u'@'%'"); !strings.Contains(grant, " ON `e2e\\_drift\\%x`.* TO 'u'@'%'") {
		t.Errorf("the grant names %q", grant)
	}
	query := documentedMySQLAccountQuery("u", "e2e_drift")
	for _, want := range []string{`TABLE_SCHEMA = 'e2e\\_drift'`, `TABLE_SCHEMA <> 'e2e\\_drift'`} {
		if !strings.Contains(query, want) {
			t.Errorf("the readback does not hold %s: %s", want, query)
		}
	}
}

// A table-level grant reads the database name as written, so the page's grant
// on the schema apply log names the database without the escapes the grant on
// the database needs.
func TestDocumentedMySQLApplyLogGrantNamesTheDatabaseAsWritten(t *testing.T) {
	t.Parallel()
	const want = "GRANT INSERT ON `e2e_drift%x`.`ptah_schema_apply_log` TO 'u'@'%'"
	if grant := documentedMySQLApplyLogGrant("e2e_drift%x", "'u'@'%'"); grant != want {
		t.Errorf("the apply log grant is %q, want %q", grant, want)
	}
}

// The readings are what the queries returned as root on MySQL 8.4.11, the
// matrix image, for an account made with the page's grant on
// e2e_mysql_approved_drift and then given the page's grant on the schema apply
// log, and for accounts given the grants a fixture could run by mistake.
func TestDocumentedMySQLApplyLogGrantReadingsRefuseOtherGrants(t *testing.T) {
	t.Parallel()
	const (
		database = "e2e_mysql_approved_drift"
		// The documented account before the apply log grant.
		before = "ALTER,CREATE,DROP,EXECUTE,INDEX,REFERENCES,SELECT,SHOW VIEW,TRIGGER|0|USAGE|0|0"
		// The same account after it.
		after       = "ALTER,CREATE,DROP,EXECUTE,INDEX,REFERENCES,SELECT,SHOW VIEW,TRIGGER|0|USAGE|1|0"
		tableGrants = "e2e_mysql_approved_drift.ptah_schema_apply_log.INSERT.NO"
		// INSERT granted on the database rather than on the table.
		onTheDatabase = "ALTER,CREATE,DROP,EXECUTE,INDEX,INSERT,REFERENCES,SELECT,SHOW VIEW,TRIGGER|0|USAGE|0|0"
		// INSERT and UPDATE on the table, WITH GRANT OPTION.
		wider = "e2e_mysql_approved_drift.ptah_schema_apply_log.INSERT.YES,e2e_mysql_approved_drift.ptah_schema_apply_log.UPDATE.YES"
	)
	if got := documentedMySQLAccountReadingWithApplyLog(); got != after {
		t.Errorf("the account with the apply log grant reads %q, and the guard expects %q", after, got)
	}
	if got := documentedMySQLApplyLogGrantReading(database); got != tableGrants {
		t.Errorf("the apply log grant reads %q, and the guard expects %q", tableGrants, got)
	}
	// The guard the account is created under still refuses any table grant.
	if after == documentedMySQLAccountReading() {
		t.Errorf("the guard before the apply log grant accepts %q", after)
	}
	for _, reading := range []string{before, onTheDatabase, documentedMySQLPrivilegesReading() + "|0|USAGE|2|0"} {
		if reading == documentedMySQLAccountReadingWithApplyLog() {
			t.Errorf("the guard after the apply log grant accepts the account reading %q", reading)
		}
	}
	for _, reading := range []string{
		"", wider,
		"e2e_mysql_approved_drift.ptah_schema_apply_log.INSERT.YES",
		"e2e_mysql_approved_drift.e2e_widgets.INSERT.NO",
		"app.ptah_schema_apply_log.INSERT.NO",
	} {
		if reading == documentedMySQLApplyLogGrantReading(database) {
			t.Errorf("the guard after the apply log grant accepts the table grants %q", reading)
		}
	}
}

// The readings are what mysqlApplyLogQuery returned as root on MySQL 8.4.11
// after the drift row's Applies, replayed with the v0.28.0 image under the
// page's grants. The fresh Apply created the table and lost both entries, so
// the log read empty; the later one, after the apply log grant, kept both.
// Refused are the logs a broken grant or a reading of the wrong Apply leaves.
func TestMySQLApplyLogReadingNamesOneApply(t *testing.T) {
	t.Parallel()
	const (
		later       = "plan_4fb7fef7010b"
		laterDigest = "sha256:4b6f3784d8c9943bc8eba55d23d62c823e6fd2a879c7b4b258c11c1ca56e0b4a"
		fresh       = "plan_5f233011010c"
		freshDigest = "sha256:fb2feda1ac3b6ae2fc2270cd90f12b75a15032ff917dfd109c986852506bfdc6"
		empty       = "0|0|"
		laterOnly   = "2|1|1,started," + later + "," + laterDigest + ";2,applied," + later + "," + laterDigest
		// One Apply after the later one, of a plan that brought the database
		// back to the seed's shape: it holds both runs.
		twoApplies = "4|2|1,started,plan_48e9e4916af4,sha256:18c6dd54f50c498b253e96067f9d65f5999de5c929756e8a67aea06923fa9d02;" +
			"2,applied,plan_48e9e4916af4,sha256:18c6dd54f50c498b253e96067f9d65f5999de5c929756e8a67aea06923fa9d02;" +
			"1,started," + later + "," + laterDigest + ";2,applied," + later + "," + laterDigest
	)
	if mysqlApplyLogEmpty != empty {
		t.Errorf("the empty log reads %q, and the guard expects %q", empty, mysqlApplyLogEmpty)
	}
	if got := mysqlApplyLogOfOneApply(later, laterDigest); got != laterOnly {
		t.Errorf("the later Apply's log reads %q, and the guard expects %q", laterOnly, got)
	}
	if laterOnly == mysqlApplyLogEmpty {
		t.Errorf("the empty log guard accepts %q", laterOnly)
	}
	for name, reading := range map[string]string{
		"every entry lost":                     empty,
		"another Apply's entries":              twoApplies,
		"the outcome lost":                     "1|1|1,started," + later + "," + laterDigest,
		"an Apply that failed":                 strings.Replace(laterOnly, "2,applied,", "2,failed,", 1),
		"an Apply that ended Unknown":          strings.Replace(laterOnly, "2,applied,", "2,unknown,", 1),
		"the fresh Apply's entries":            "2|1|1,started," + fresh + "," + freshDigest + ";2,applied," + fresh + "," + freshDigest,
		"the plan's name with another digest":  strings.ReplaceAll(laterOnly, laterDigest, freshDigest),
		"the plan's digest under another name": strings.ReplaceAll(laterOnly, later, fresh),
		"entries of two runs":                  strings.Replace(laterOnly, "2|1|", "2|2|", 1),
		"the outcome entry first":              "2|1|1,applied," + later + "," + laterDigest + ";2,started," + later + "," + laterDigest,
	} {
		if reading == mysqlApplyLogOfOneApply(later, laterDigest) {
			t.Errorf("the later Apply's guard accepts %s: %q", name, reading)
		}
	}
}
