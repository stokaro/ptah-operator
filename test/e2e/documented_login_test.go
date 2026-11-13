package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The drift rows connect as the login the page tells a person to create. If
// the page and the fixture moved apart, a row would prove a login nobody is
// told to make, and nothing in a cluster run would say so.
func TestDocumentedLoginIsTheOneThePageShows(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(databasesPage)))
	if err != nil {
		t.Fatalf("read %s: %v", databasesPage, err)
	}
	page := string(raw)
	if err := pageShows(page, postgreSQLAuthoritySection, documentedPostgreSQLLogin("ptah", "app")); err != nil {
		t.Errorf("%s: %v", databasesPage, err)
	}
	if err := pageShows(page, mySQLAuthoritySection, []string{documentedMySQLGrant("app", "'ptah'@'%'")}); err != nil {
		t.Errorf("%s: %v", databasesPage, err)
	}
}

// Each refused page is the accepted one with the edit a person could make to
// the page alone.
func TestPageShowsRefusesAPageThatMovedApart(t *testing.T) {
	t.Parallel()
	const mysqlGrant = "GRANT SELECT, CREATE, ALTER, DROP, INDEX, REFERENCES, TRIGGER, EXECUTE, SHOW VIEW ON `app`.* TO 'ptah'@'%';"
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
		"```sql", "GRANT SHOW_ROUTINE ON *.* TO 'ptah'@'%';", "```", "",
		"## What the login cannot see", "",
	}, "\n")
	postgres := documentedPostgreSQLLogin("ptah", "app")
	mysql := []string{documentedMySQLGrant("app", "'ptah'@'%'")}
	if err := pageShows(accepted, postgreSQLAuthoritySection, postgres); err != nil {
		t.Fatalf("the PostgreSQL statements were refused: %v", err)
	}
	if err := pageShows(accepted, mySQLAuthoritySection, mysql); err != nil {
		t.Fatalf("the MySQL grant was refused: %v", err)
	}
	for _, row := range []struct {
		name, heading, from, to string
		want                    []string
	}{
		{"a PostgreSQL login made a superuser", postgreSQLAuthoritySection, "NOSUPERUSER", "SUPERUSER", postgres},
		{"a PostgreSQL grant dropped", postgreSQLAuthoritySection, "GRANT CONNECT, TEMPORARY ON DATABASE app TO ptah;\n", "", postgres},
		{"TEMPORARY dropped from the PostgreSQL grant", postgreSQLAuthoritySection, "CONNECT, TEMPORARY ON DATABASE", "CONNECT ON DATABASE", postgres},
		{"a PostgreSQL grant added", postgreSQLAuthoritySection, "GRANT USAGE, CREATE ON SCHEMA public TO ptah;\n",
			"GRANT USAGE, CREATE ON SCHEMA public TO ptah;\nGRANT CREATE ON DATABASE app TO ptah;\n", postgres},
		{"a PostgreSQL schema privilege dropped", postgreSQLAuthoritySection, "USAGE, CREATE ON SCHEMA", "USAGE ON SCHEMA", postgres},
		{"the PostgreSQL statements moved out of their section", postgreSQLAuthoritySection, "```sql\nCREATE ROLE", "```text\nCREATE ROLE", postgres},
		{"a MySQL privilege dropped", mySQLAuthoritySection, "EXECUTE, SHOW VIEW", "SHOW VIEW", mysql},
		{"a MySQL privilege added", mySQLAuthoritySection, "SHOW VIEW ON", "SHOW VIEW, CREATE VIEW ON", mysql},
		{"the MySQL grant widened to every database", mySQLAuthoritySection, "ON `app`.*", "ON *.*", mysql},
		{"the MySQL grant removed, leaving the global one first", mySQLAuthoritySection, "```sql\n" + mysqlGrant + "\n```\n", "", mysql},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			page := strings.Replace(accepted, row.from, row.to, 1)
			if page == accepted {
				t.Fatalf("the edit %q changed nothing", row.from)
			}
			if err := pageShows(page, row.heading, row.want); err == nil {
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
