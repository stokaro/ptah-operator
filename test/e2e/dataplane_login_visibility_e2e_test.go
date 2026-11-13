//go:build e2e

package e2e

import (
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
// It connects as ptah_e2e_documented, created from the page's statements and
// holding what they grant and nothing else. The schema converges as that login,
// the server's superuser adds a table, and the next Observe has to report the
// table and its primary key as a superuser's would. The destructive plan that
// follows stays refused, and the row leaves it there.
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
	d.assertNoAuthorityOver(loginVisibilityPGLogin, loginVisibilityPGDatabase, loginVisibilityHandMade)
	drifted := d.waitForSchema(schemaName, "an Observe under the documented login to count the hand-made table and its primary key",
		func(s *ptahv1alpha1.PtahSchema) bool { return unreadableTableObserved(s, digest) })
	d.printReading("login-visibility drift", map[string]any{"status": drifted.Status})
	d.logf("PASS a table the documented login holds nothing on is reported as tables_removed and constraints_removed")
}

// createDocumentedLoginDatabase creates the row's database and, from the
// statements the page shows, the login the page documents.
func (d *dataPlane) createDocumentedLoginDatabase() {
	d.t.Helper()
	d.createIsolatedPostgreSQLDatabase(loginVisibilityPGDatabase, loginVisibilityPGSecret, d.credentials.loginVisibilityPGURL)
	d.createDocumentedPostgreSQLLogin(loginVisibilityPGLogin, loginVisibilityPGDatabase, false)
}

// loginVisibilitySQL runs one statement in the row's database as the server's
// superuser and returns its output without whitespace.
func (d *dataPlane) loginVisibilitySQL(statement string) string {
	d.t.Helper()
	output, err := d.psql(loginVisibilityPGDatabase, statement)
	d.check(err, "run %q in %s", statement, loginVisibilityPGDatabase)
	return removeWhitespace(output)
}
