package e2e

import (
	"fmt"
	"slices"
	"strings"
)

// databasesPage is the page that documents the authority a login needs.
const databasesPage = "docs/site/src/content/docs/support/databases.md"

// The page's sections that show a person the statements to run.
const (
	postgreSQLAuthoritySection = "## PostgreSQL authority"
	mySQLAuthoritySection      = "## MySQL authority"
)

// documentedPostgreSQLLogin is the statements the page's PostgreSQL authority
// section shows, for login on database: a login with no administrative
// attribute, CONNECT and TEMPORARY on the database, and USAGE and CREATE on its
// public schema. What the login creates afterwards it owns, which is the last item the
// page lists. A drift row creates its login with these statements, and
// TestDocumentedLoginIsTheOneThePageShows holds them to the page, so the login
// a row proves and the one the page tells a person to create are one list.
func documentedPostgreSQLLogin(login, database string) []string {
	return []string{
		"CREATE ROLE " + login + " LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"GRANT CONNECT, TEMPORARY ON DATABASE " + database + " TO " + login,
		"GRANT USAGE, CREATE ON SCHEMA public TO " + login,
	}
}

// documentedMySQLPrivileges is the grant the page's MySQL authority section
// shows, in its order.
var documentedMySQLPrivileges = []string{
	"SELECT", "CREATE", "ALTER", "DROP", "INDEX", "REFERENCES", "TRIGGER", "EXECUTE", "SHOW VIEW",
}

// documentedMySQLGrant is the page's grant on database to account, an account
// name as MySQL spells it: 'user'@'host'.
func documentedMySQLGrant(database, account string) string {
	return "GRANT " + strings.Join(documentedMySQLPrivileges, ", ") + " ON `" + mysqlGrantDatabase(database) + "`.* TO " + account
}

// mysqlApplyLogTable is the table a schema Apply keeps its log in since Ptah
// v0.28.0, in the database it targets. The first Apply that sends a statement
// creates it.
const mysqlApplyLogTable = "ptah_schema_apply_log"

// documentedMySQLApplyLogGrant is the page's grant of INSERT on the schema
// apply log of database to account, the step it shows for once an Apply has
// created the table. A table-level grant reads the database name as written,
// so unlike documentedMySQLGrant it escapes nothing: MySQL 8.4.11 refuses
// `e2e\_x`.`ptah_schema_apply_log` with error 1146, as a table that does not
// exist.
func documentedMySQLApplyLogGrant(database, account string) string {
	return "GRANT INSERT ON `" + database + "`.`" + mysqlApplyLogTable + "` TO " + account
}

// mysqlGrantDatabase is database as a database-level grant has to name it.
// With partial_revokes off, MySQL's default, a grant reads an unescaped _ or %
// in the name as a wildcard, so a grant on e2e_drift.* also covers e2eXdrift.
// information_schema stores the name as the grant wrote it, escapes included.
func mysqlGrantDatabase(database string) string {
	return strings.NewReplacer("_", `\_`, "%", `\%`).Replace(database)
}

// documentedPostgreSQLLoginQuery reads, as the server's superuser in database,
// what login holds there: each role attribute that bypasses a privilege check,
// LOGIN, how many roles it is a member of, its CONNECT, CREATE and TEMPORARY on
// the database, and its USAGE and CREATE on the public schema.
func documentedPostgreSQLLoginQuery(login, database string) string {
	return "SELECT r.rolsuper, r.rolcreatedb, r.rolcreaterole, r.rolreplication, r.rolbypassrls, r.rolcanlogin, " +
		"(SELECT count(*) FROM pg_auth_members m WHERE m.member = r.oid), " +
		"has_database_privilege(r.oid, '" + database + "', 'CONNECT'), " +
		"has_database_privilege(r.oid, '" + database + "', 'CREATE'), " +
		"has_database_privilege(r.oid, '" + database + "', 'TEMPORARY'), " +
		"has_schema_privilege(r.oid, 'public', 'USAGE'), has_schema_privilege(r.oid, 'public', 'CREATE') " +
		"FROM pg_roles r WHERE r.rolname = '" + login + "'"
}

// documentedPostgreSQLLoginReading is what documentedPostgreSQLLoginQuery reads
// for the login the page documents, once the fixture revoked what PUBLIC holds
// on a new database: no attribute but LOGIN, no role, CONNECT and TEMPORARY on
// the database, USAGE and CREATE on the schema. extensions is the one grant a
// row may add, CREATE on the database, which installs a trusted extension.
func documentedPostgreSQLLoginReading(extensions bool) string {
	create := "f"
	if extensions {
		create = "t"
	}
	return "f|f|f|f|f|t|0|t|" + create + "|t|t|t"
}

// documentedMySQLAccountQuery reads, as root, what account holds: its
// privileges on database, as the grant names it, how many grants it holds on
// any other name, a wildcard that matches database included, its global
// privileges, and how many table and column grants it holds. The query runs
// with backslash escapes on, so each backslash the name holds is doubled.
func documentedMySQLAccountQuery(user, database string) string {
	grantee := "'''" + user + "''@''%'''"
	database = strings.ReplaceAll(mysqlGrantDatabase(database), `\`, `\\`)
	return "SELECT CONCAT_WS('|', " +
		"(SELECT COALESCE(GROUP_CONCAT(PRIVILEGE_TYPE ORDER BY PRIVILEGE_TYPE SEPARATOR ','), '') " +
		"FROM information_schema.SCHEMA_PRIVILEGES WHERE GRANTEE = " + grantee + " AND TABLE_SCHEMA = '" + database + "'), " +
		"(SELECT COUNT(*) FROM information_schema.SCHEMA_PRIVILEGES WHERE GRANTEE = " + grantee +
		" AND TABLE_SCHEMA <> '" + database + "'), " +
		"(SELECT COALESCE(GROUP_CONCAT(PRIVILEGE_TYPE ORDER BY PRIVILEGE_TYPE SEPARATOR ','), '') " +
		"FROM information_schema.USER_PRIVILEGES WHERE GRANTEE = " + grantee + "), " +
		"(SELECT COUNT(*) FROM information_schema.TABLE_PRIVILEGES WHERE GRANTEE = " + grantee + "), " +
		"(SELECT COUNT(*) FROM information_schema.COLUMN_PRIVILEGES WHERE GRANTEE = " + grantee + "))"
}

// documentedMySQLAccountReading is what documentedMySQLAccountQuery reads for an
// account that holds the page's grant and nothing else: the grant's privileges
// on the database, in MySQL's order, and USAGE alone everywhere else.
func documentedMySQLAccountReading() string {
	return documentedMySQLPrivilegesReading() + "|0|USAGE|0|0"
}

// documentedMySQLAccountReadingWithApplyLog is what documentedMySQLAccountQuery
// reads once the account also holds the page's grant on the schema apply log:
// the same privileges on the database, and one table grant.
func documentedMySQLAccountReadingWithApplyLog() string {
	return documentedMySQLPrivilegesReading() + "|0|USAGE|1|0"
}

// documentedMySQLPrivilegesReading is the page's privileges as
// information_schema lists them, sorted.
func documentedMySQLPrivilegesReading() string {
	privileges := slices.Clone(documentedMySQLPrivileges)
	slices.Sort(privileges)
	return strings.Join(privileges, ",")
}

// documentedMySQLTableGrantsQuery reads, as root, every table-level privilege
// account holds, each as database.table.privilege.grantable, sorted.
func documentedMySQLTableGrantsQuery(user string) string {
	grantee := "'''" + user + "''@''%'''"
	return "SELECT COALESCE(GROUP_CONCAT(CONCAT_WS('.', TABLE_SCHEMA, TABLE_NAME, PRIVILEGE_TYPE, IS_GRANTABLE) " +
		"ORDER BY TABLE_SCHEMA, TABLE_NAME, PRIVILEGE_TYPE SEPARATOR ','), '') " +
		"FROM information_schema.TABLE_PRIVILEGES WHERE GRANTEE = " + grantee
}

// documentedMySQLApplyLogGrantReading is what documentedMySQLTableGrantsQuery
// reads for an account whose one table grant is the page's on the schema apply
// log of database: INSERT, without the grant option.
func documentedMySQLApplyLogGrantReading(database string) string {
	return database + "." + mysqlApplyLogTable + ".INSERT.NO"
}

// mysqlApplyLogQuery reads, in the database, the schema apply log: how many
// entries it holds, under how many runs, and each entry as
// seq,state,plan,digest, in run and sequence order. Each value is one word, so
// the answer survives the whitespace the fault query helper removes.
const mysqlApplyLogQuery = "SELECT CONCAT_WS('|', COUNT(*), COUNT(DISTINCT run_id), " +
	"COALESCE(GROUP_CONCAT(CONCAT_WS(',', seq, state, COALESCE(plan_name, '-'), COALESCE(plan_digest, '-')) " +
	"ORDER BY run_id, seq SEPARATOR ';'), '')) FROM " + mysqlApplyLogTable

// mysqlApplyLogEmpty is what mysqlApplyLogQuery reads in a log that holds no
// entry.
const mysqlApplyLogEmpty = "0|0|"

// mysqlApplyLogOfOneApply is what mysqlApplyLogQuery reads in a log that holds
// one Apply's entries and nothing else: the started entry and the applied one,
// under one run, both naming the plan by its name and the digest of its bytes.
func mysqlApplyLogOfOneApply(plan, digest string) string {
	return "2|1|1,started," + plan + "," + digest + ";2,applied," + plan + "," + digest
}

// pageSQL is the statements of each sql block in the section of page the
// heading opens, in the page's order, without their semicolons. A section that
// holds no sql block before the next heading of its level is an error, not an
// empty list, and so is a block that holds no statement or does not end.
func pageSQL(page, heading string) ([][]string, error) {
	start := strings.Index(page, "\n"+heading+"\n")
	if start < 0 {
		return nil, fmt.Errorf("the page has no section %q", heading)
	}
	section := page[start+len(heading)+2:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	var blocks [][]string
	for {
		open := strings.Index(section, "\n```sql\n")
		if open < 0 {
			break
		}
		block := section[open+len("\n```sql\n"):]
		closing := strings.Index(block, "\n```")
		if closing < 0 {
			return nil, fmt.Errorf("sql block %d of section %q does not end", len(blocks)+1, heading)
		}
		var statements []string
		for statement := range strings.SplitSeq(block[:closing], ";") {
			if statement = strings.Join(strings.Fields(statement), " "); statement != "" {
				statements = append(statements, statement)
			}
		}
		if len(statements) == 0 {
			return nil, fmt.Errorf("sql block %d of section %q holds no statement", len(blocks)+1, heading)
		}
		blocks = append(blocks, statements)
		section = block[closing+len("\n```"):]
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("section %q shows no sql block", heading)
	}
	return blocks, nil
}

// The sql blocks of the MySQL authority section a fixture runs, by their place
// in it, counted from 0: the grant on the database first, and the grant on the
// schema apply log after the SHOW_ROUTINE one.
const (
	mySQLGrantBlock         = 0
	mySQLApplyLogGrantBlock = 2
)

// pageShows holds sql block block of a section, counted from 0, to the
// statements a fixture runs, rendered with the names the page uses.
func pageShows(page, heading string, block int, want []string) error {
	blocks, err := pageSQL(page, heading)
	if err != nil {
		return err
	}
	if block >= len(blocks) {
		return fmt.Errorf("section %q shows %d sql blocks, and the fixture runs block %d", heading, len(blocks), block+1)
	}
	shown := blocks[block]
	if len(shown) != len(want) {
		return fmt.Errorf("sql block %d of section %q shows %d statements and the fixture runs %d", block+1, heading, len(shown), len(want))
	}
	for index := range want {
		if shown[index] != want[index] {
			return fmt.Errorf("sql block %d of section %q shows %q where the fixture runs %q", block+1, heading, shown[index], want[index])
		}
	}
	return nil
}
