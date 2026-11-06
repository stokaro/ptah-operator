//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const (
	// loginVisibilityTable is the table the schema declares and the login owns.
	loginVisibilityTable = "e2e_login_visibility_widgets"
	// loginVisibilityHandMade is the table the server's superuser adds, which
	// the login holds no privilege on.
	loginVisibilityHandMade = "e2e_login_visibility_hand_made"
)

// postgresqlLoginVisibilityDrift runs drift detection under the login
// support/databases.md documents, with a table that login cannot read (#792).
// Every other PostgreSQL row connects as the server's superuser, or as the
// external login that owns everything in its database, so no other row meets
// an object another role created under a login that holds nothing on it. The
// schema converges as that login, the superuser adds a table, and the next
// Observe has to report the table and its primary key as a superuser's would.
// The destructive plan that follows stays refused, and the row leaves it there.
func (d *dataPlane) postgresqlLoginVisibilityDrift() {
	d.t.Helper()
	d.logf("checking that a table the documented login cannot read reaches status")
	const (
		schemaName = "e2e-login-visibility-postgresql"
		key        = "e2e/login-visibility-postgresql"
		approval   = "e2e-login-visibility-postgresql-approval"
	)
	reference := d.registryReference("login-visibility-postgresql")
	digest := d.publishSchema("login-visibility-postgresql", "v1", "postgres", reference, "")
	d.createDocumentedLoginDatabase()
	// The approval policy is the default, so the first plan waits for a person
	// and the hand-made table meets a schema that converged under this login.
	d.createSchemaResource(schemaResource{
		name: schemaName, engine: "PostgreSQL", secret: loginVisibilityPGSecret, reference: reference, coordinationKey: key,
		failureRetry: "5s", interval: reconcileInterval,
	})
	matched := d.waitForSchema(schemaName, "the login-visibility schema to publish a plan awaiting approval",
		func(s *ptahv1alpha1.PtahSchema) bool { return awaitingApprovalOf(s, digest) })
	plan := matched.Status.Plan
	if matched.UID == "" || plan.UID == "" || plan.Fingerprint == "" {
		d.fatalf("%s published a plan with no UID or fingerprint to approve", schemaName)
	}
	d.mustCreate(approvalDocument(d.in.TestNamespace, approval, schemaName, string(matched.UID), plan.Name,
		string(plan.UID), plan.Fingerprint))
	d.waitForSchema(schemaName, "the approved plan to converge under the documented login",
		func(s *ptahv1alpha1.PtahSchema) bool { return convergedOn(s, digest) })
	if owner := d.loginVisibilitySQL("SELECT tableowner FROM pg_tables WHERE schemaname='public' AND tablename='" +
		loginVisibilityTable + "'"); owner != loginVisibilityPGLogin {
		d.fatalf("%s is owned by %q after the Apply, and the row needs the login to own what it created",
			loginVisibilityTable, owner)
	}

	d.loginVisibilitySQL("CREATE TABLE " + loginVisibilityHandMade + " (id bigint PRIMARY KEY)")
	// The row is about a login that holds nothing on this table. A fixture that
	// let it read would pass at every executor.
	if authority := d.loginVisibilitySQL(loginAuthorityQuery(loginVisibilityPGLogin, loginVisibilityHandMade)); authority != "f|f|f|f|f|f|f" {
		d.fatalf("%s holds more than the documented authority over %s: %s", loginVisibilityPGLogin,
			loginVisibilityHandMade, authority)
	}
	drifted := d.waitForSchema(schemaName, "an Observe under the documented login to count the hand-made table and its primary key",
		func(s *ptahv1alpha1.PtahSchema) bool { return unreadableTableObserved(s, digest) })
	d.printLoginVisibilityDriftReading(drifted)
	d.logf("PASS a table the documented login holds nothing on is reported as tables_removed and constraints_removed")
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

// createDocumentedLoginDatabase creates the row's database and the login the
// page documents: CONNECT on the database, USAGE and CREATE on the schema, no
// administrative attribute. The password is the one the database container
// already holds, so no credential crosses the exec.
func (d *dataPlane) createDocumentedLoginDatabase() {
	d.t.Helper()
	d.createIsolatedPostgreSQLDatabase(loginVisibilityPGDatabase, loginVisibilityPGSecret, d.credentials.loginVisibilityPGURL)
	_, err := d.execIn(pgService, "sh", "-ec",
		`PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -Atqc `+
			`"CREATE ROLE $1 LOGIN PASSWORD '$POSTGRES_PASSWORD' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"`,
		"sh", loginVisibilityPGLogin)
	d.check(err, "create the documented login %s", loginVisibilityPGLogin)
	d.loginVisibilitySQL("REVOKE ALL ON DATABASE " + loginVisibilityPGDatabase + " FROM PUBLIC; " +
		"GRANT CONNECT ON DATABASE " + loginVisibilityPGDatabase + " TO " + loginVisibilityPGLogin + "; " +
		"GRANT USAGE, CREATE ON SCHEMA public TO " + loginVisibilityPGLogin)
}

// loginVisibilitySQL runs one statement in the row's database as the server's
// superuser and returns its output without whitespace.
func (d *dataPlane) loginVisibilitySQL(statement string) string {
	d.t.Helper()
	output, err := d.psql(loginVisibilityPGDatabase, statement)
	d.check(err, "run %q in %s", statement, loginVisibilityPGDatabase)
	return removeWhitespace(output)
}

// printLoginVisibilityDriftReading prints the status that satisfied the wait,
// so a run's log holds the document the predicate's unit test reads. It is
// scanned like every other document the phase prints, and withheld on a match.
func (d *dataPlane) printLoginVisibilityDriftReading(schema *ptahv1alpha1.PtahSchema) {
	d.t.Helper()
	reading, err := json.Marshal(map[string]any{"status": schema.Status})
	if err != nil || !d.scanner.ready() {
		return
	}
	if d.scanner.leaks(reading) {
		_, _ = fmt.Fprintf(os.Stderr, "e2e data plane: the login-visibility drift reading is withheld: it matched a protected credential\n")
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "e2e data plane: the login-visibility drift reading\n%s\n", reading)
}
