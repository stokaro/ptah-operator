package e2e

import (
	"errors"
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
// attribute, CONNECT on the database, and USAGE and CREATE on its public
// schema. What the login creates afterwards it owns, which is the last item the
// page lists. A drift row creates its login with these statements, and
// TestDocumentedLoginIsTheOneThePageShows holds them to the page, so the login
// a row proves and the one the page tells a person to create are one list.
func documentedPostgreSQLLogin(login, database string) []string {
	return []string{
		"CREATE ROLE " + login + " LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"GRANT CONNECT ON DATABASE " + database + " TO " + login,
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
	return "GRANT " + strings.Join(documentedMySQLPrivileges, ", ") + " ON " + database + ".* TO " + account
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
// on a new database: no attribute but LOGIN, no role, CONNECT and nothing else
// on the database, USAGE and CREATE on the schema. extensions is the one grant a
// row may add, CREATE on the database, which installs a trusted extension.
func documentedPostgreSQLLoginReading(extensions bool) string {
	create := "f"
	if extensions {
		create = "t"
	}
	return "f|f|f|f|f|t|0|t|" + create + "|f|t|t"
}

// documentedMySQLAccountQuery reads, as root, what account holds: its
// privileges on database, how many grants it holds on another database, its
// global privileges, and how many table and column grants it holds.
func documentedMySQLAccountQuery(user, database string) string {
	grantee := "'''" + user + "''@''%'''"
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
	privileges := slices.Clone(documentedMySQLPrivileges)
	slices.Sort(privileges)
	return strings.Join(privileges, ",") + "|0|USAGE|0|0"
}

// pageSQL is the statements of the first sql block in the section of page the
// heading opens, without their semicolons. A section that holds no sql block
// before the next heading of its level is an error, not an empty list.
func pageSQL(page, heading string) ([]string, error) {
	start := strings.Index(page, "\n"+heading+"\n")
	if start < 0 {
		return nil, fmt.Errorf("the page has no section %q", heading)
	}
	section := page[start+len(heading)+2:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	open := strings.Index(section, "\n```sql\n")
	if open < 0 {
		return nil, fmt.Errorf("section %q shows no sql block", heading)
	}
	block := section[open+len("\n```sql\n"):]
	closing := strings.Index(block, "\n```")
	if closing < 0 {
		return nil, fmt.Errorf("the sql block of section %q does not end", heading)
	}
	var statements []string
	for statement := range strings.SplitSeq(block[:closing], ";") {
		if statement = strings.Join(strings.Fields(statement), " "); statement != "" {
			statements = append(statements, statement)
		}
	}
	if len(statements) == 0 {
		return nil, errors.New("the sql block of section " + heading + " holds no statement")
	}
	return statements, nil
}

// pageShows holds the first sql block of a section to the statements a fixture
// runs, rendered with the names the page uses.
func pageShows(page, heading string, want []string) error {
	shown, err := pageSQL(page, heading)
	if err != nil {
		return err
	}
	if len(shown) != len(want) {
		return fmt.Errorf("section %q shows %d statements and the fixture runs %d", heading, len(shown), len(want))
	}
	for index := range want {
		if shown[index] != want[index] {
			return fmt.Errorf("section %q shows %q where the fixture runs %q", heading, shown[index], want[index])
		}
	}
	return nil
}
