//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// postgresqlExtensionDrift stores one of Ptah's feature categories through a
// real cluster (#678). Ptah v0.14.0 hands a PostgreSQL extension to its
// pgextension feature owner and counts a change to one under
// feature_changes:ptah.run/pgextension/extension-change; the runner's
// vocabulary, the result frame and the CRD's category enum each refused that
// before. The schema declares pg_trgm and a table, a person approves the plan
// that creates both, and once the schema converged the extension is dropped by
// hand. The next Observe has to store Ptah's category as it is. The schema
// then waits for a person to approve the plan that puts the extension back,
// and the row leaves it there.
func (d *dataPlane) postgresqlExtensionDrift() {
	d.t.Helper()
	d.logf("checking that a dropped PostgreSQL extension reaches status in Ptah's category")
	const (
		schemaName = "e2e-extension-postgresql"
		key        = "e2e/extension-postgresql"
		table      = "e2e_extension_widgets"
		extension  = "pg_trgm"
		approval   = "e2e-extension-postgresql-approval"
	)
	reference := d.registryReference("extension-postgresql")
	digest := d.publishSchema("extension-postgresql", "v1", "postgres", reference, "")
	d.createIsolatedPostgreSQLDatabase(extensionPGDatabase, extensionPGSecret, d.credentials.extensionPGURL)
	// The approval policy is the default, so the drifted schema waits for a
	// person with the reading in place rather than putting the extension back
	// before the row could read it.
	d.createSchemaResource(schemaResource{
		name: schemaName, engine: "PostgreSQL", secret: extensionPGSecret, reference: reference, coordinationKey: key,
		failureRetry: "5s", interval: reconcileInterval,
	})
	matched := d.waitForSchema(schemaName, "the extension schema to publish a plan awaiting approval",
		func(s *ptahv1alpha1.PtahSchema) bool { return awaitingApprovalOf(s, digest) })
	plan := matched.Status.Plan
	if matched.UID == "" || plan.UID == "" || plan.Fingerprint == "" {
		d.fatalf("%s published a plan with no UID or fingerprint to approve", schemaName)
	}
	d.mustCreate(approvalDocument(d.in.TestNamespace, approval, schemaName, string(matched.UID), plan.Name,
		string(plan.UID), plan.Fingerprint))
	d.waitForSchema(schemaName, "the approved plan to create the extension and converge",
		func(s *ptahv1alpha1.PtahSchema) bool { return convergedOn(s, digest) })
	if count := d.isolatedPostgreSQLExtensionCount(extensionPGDatabase, extension); count != "1" {
		d.fatalf("%s is not installed in %s after the approved plan applied", extension, extensionPGDatabase)
	}
	if count := d.isolatedPostgreSQLTableCount(extensionPGDatabase, table); count != "1" {
		d.fatalf("%s is not present in %s after the approved plan applied", table, extensionPGDatabase)
	}

	_, err := d.psql(extensionPGDatabase, "DROP EXTENSION "+extension)
	d.check(err, "drop %s from %s", extension, extensionPGDatabase)
	drifted := d.waitForSchema(schemaName, "an Observe to count the dropped extension in Ptah's category",
		func(s *ptahv1alpha1.PtahSchema) bool { return droppedExtensionObserved(s, digest) })
	d.printExtensionDriftReading(drifted)
	d.logf("PASS a dropped PostgreSQL extension is stored as %s", pgExtensionChange)
}

// isolatedPostgreSQLExtensionCount counts one installed extension in one
// database on the lifecycle's server.
func (d *dataPlane) isolatedPostgreSQLExtensionCount(database, extension string) string {
	d.t.Helper()
	count, err := d.psql(database, "SELECT count(*) FROM pg_extension WHERE extname='"+extension+"'")
	d.check(err, "count extension %s in %s", extension, database)
	return removeWhitespace(count)
}

// printExtensionDriftReading prints the status that satisfied the wait, so a
// run's log holds the document the predicate's unit test reads. It is scanned
// like every other document the phase prints, and withheld on a match.
func (d *dataPlane) printExtensionDriftReading(schema *ptahv1alpha1.PtahSchema) {
	d.t.Helper()
	reading, err := json.Marshal(map[string]any{"status": schema.Status})
	if err != nil || !d.scanner.ready() {
		return
	}
	if d.scanner.leaks(reading) {
		_, _ = fmt.Fprintf(os.Stderr, "e2e data plane: the extension drift reading is withheld: it matched a protected credential\n")
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "e2e data plane: the extension drift reading\n%s\n", reading)
}
