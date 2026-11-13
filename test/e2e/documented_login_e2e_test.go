//go:build e2e

package e2e

import (
	"strings"
)

// createDocumentedPostgreSQLLogin creates login on the lifecycle's server, as
// the server's superuser, with the statements support/databases.md shows for
// database, and holds the result to what the page grants. The password is the
// one the database container already holds, so no credential crosses the exec.
// A new database grants PUBLIC CONNECT and TEMPORARY, and every role holds what
// PUBLIC holds, so the fixture revokes both before it grants: the login then
// holds what the page grants and nothing it inherits. extensions adds CREATE on
// the database, the one grant beyond the page a row may ask for, which
// installs a trusted extension; the row that asks says why.
func (d *dataPlane) createDocumentedPostgreSQLLogin(login, database string, extensions bool) {
	d.t.Helper()
	if !safeDatabaseName.MatchString(login) || !safeDatabaseName.MatchString(database) {
		d.fatalf("unsafe documented login %s or database %s", login, database)
	}
	statements := documentedPostgreSQLLogin(login, database)
	if extensions {
		statements = append(statements, "GRANT CREATE ON DATABASE "+database+" TO "+login)
	}
	_, err := d.execIn(pgService, "sh", "-ec",
		`PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -v ON_ERROR_STOP=1 -Atq `+
			`-c "REVOKE ALL ON DATABASE $1 FROM PUBLIC" -c "$2" -c "ALTER ROLE $3 PASSWORD '$POSTGRES_PASSWORD'"`,
		"sh", database, strings.Join(statements, "; "), login)
	d.check(err, "create the documented login %s on %s", login, database)
	authority, err := d.psql(database, documentedPostgreSQLLoginQuery(login, database))
	d.check(err, "read what %s holds on %s", login, database)
	if want := documentedPostgreSQLLoginReading(extensions); removeWhitespace(authority) != want {
		d.fatalf("%s holds %s on %s, and the documented login holds %s", login, removeWhitespace(authority), database, want)
	}
}

// assertNoAuthorityOver holds a login to holding nothing on a table another
// role created: no attribute that bypasses a privilege check, no membership of
// pg_read_all_data, no privilege on the table. A row that reads such a table
// under a login that could read it passes whatever the executor reads it
// through.
func (d *dataPlane) assertNoAuthorityOver(login, database, table string) {
	d.t.Helper()
	authority, err := d.psql(database, loginAuthorityQuery(login, table))
	d.check(err, "read what %s holds on %s", login, table)
	if removeWhitespace(authority) != "f|f|f|f|f|f|f" {
		d.fatalf("%s holds more than the documented authority over %s: %s", login, table, removeWhitespace(authority))
	}
}

// loginAuthorityQuery reads, as the server's superuser, what would let the
// login see the table: the role attributes that bypass a privilege check,
// membership of pg_read_all_data, and any privilege on the table itself.
func loginAuthorityQuery(login, table string) string {
	return "SELECT rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls, " +
		"pg_has_role('" + login + "', 'pg_read_all_data', 'MEMBER'), " +
		"has_table_privilege('" + login + "', 'public." + table + "', 'SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER') " +
		"FROM pg_roles WHERE rolname='" + login + "'"
}

// assertOwnsEveryTable holds a login to owning every table in the public schema
// of database, and the schema to holding at least one: the login can alter and
// drop what the desired schema names only if it owns it.
func (d *dataPlane) assertOwnsEveryTable(login, database string) {
	d.t.Helper()
	counts, err := d.psql(database, "SELECT count(*), count(*) FILTER (WHERE tableowner = '"+login+"') "+
		"FROM pg_tables WHERE schemaname = 'public'")
	d.check(err, "count the tables %s owns in %s", login, database)
	all, owned, _ := strings.Cut(removeWhitespace(counts), "|")
	if all == "0" || all != owned {
		d.fatalf("%s owns %s of the %s tables in %s, and the row needs it to own every one", login, owned, all, database)
	}
}
