//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// targetReachability proves DatabaseReachable through a real server. A schema and
// a migration read their URL from a Secret whose password the data plane's
// PostgreSQL refuses, and each has to report that on the condition, with the
// authentication reason and the operator's sentence, in place of a True left over
// or of no condition at all. Correcting the Secret has to bring each back to True
// through the operation's own retry, with nothing else touched.
//
// Both waits read the condition and not a phase: a resource retrying its
// operation is Failed for part of every cycle and not for the rest.
func (c *configMapRun) targetReachability() {
	c.t.Helper()
	c.schemaReachability()
	c.migrationReachability()
	c.logf("PASS a schema and a migration reported the refused credentials on DatabaseReachable and recovered when the Secret was corrected")
}

func (c *configMapRun) schemaReachability() {
	c.t.Helper()
	url := reachabilityURLKey(reachabilitySchemaSecret)
	schema := &ptahv1alpha1.PtahSchema{}
	read := func() (int64, []metav1.Condition, error) {
		err := c.get(reachabilitySchema, schema)
		return schema.Generation, schema.Status.Conditions, err
	}
	c.createDatabaseWithPassword(reachabilitySchemaDatabase, reachabilitySchemaSecret, reachabilityWrongPassword)
	c.check(c.apply(c.sourceDocument(reachabilitySchemaSource, map[string]string{"schema.sql": configMapSchemaRevisions[0]})),
		"apply ConfigMap %s", reachabilitySchemaSource)
	c.check(c.create(map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahSchema",
		"metadata": map[string]any{"namespace": c.in.TestNamespace, "name": reachabilitySchema},
		"spec": map[string]any{
			"target": map[string]any{
				"engine": "PostgreSQL", "coordinationKey": "e2e/" + reachabilitySchemaDatabase,
				"urlFrom": map[string]any{"name": reachabilitySchemaSecret, "key": "url"},
			},
			"desired": map[string]any{"configMap": map[string]any{
				"name":  reachabilitySchemaSource,
				"items": []any{map[string]any{"key": "schema.sql", "path": "schema.sql"}},
			}},
			"interval":  configMapInterval,
			"policy":    map[string]any{"apply": "OnApproval"},
			"execution": map[string]any{"activeDeadlineSeconds": int64(600), "failureRetryInterval": "5s"},
		},
	}), "create PtahSchema %s", reachabilitySchema)
	c.waitForConditions(reachabilitySchema+" to report on DatabaseReachable that its target refused the credentials", read,
		func(generation int64, conditions []metav1.Condition) bool {
			return targetRefusedCredentials(conditions, generation, url)
		})
	c.printReachabilityReading("refused schema", schema.Status)

	c.applyDatabaseSecret(reachabilitySchemaDatabase, reachabilitySchemaSecret, c.password)
	c.waitForConditions(reachabilitySchema+" to report DatabaseReachable=True once its Secret is corrected", read,
		func(generation int64, conditions []metav1.Condition) bool {
			return targetAnsweredAgain(conditions, generation)
		})
	c.printReachabilityReading("recovered schema", schema.Status)
}

func (c *configMapRun) migrationReachability() {
	c.t.Helper()
	url := reachabilityURLKey(reachabilityMigrationSecret)
	migration := &ptahv1alpha1.PtahMigration{}
	read := func() (int64, []metav1.Condition, error) {
		err := c.get(reachabilityMigration, migration)
		return migration.Generation, migration.Status.Conditions, err
	}
	c.createDatabaseWithPassword(reachabilityMigrationDatabase, reachabilityMigrationSecret, reachabilityWrongPassword)
	data := map[string]string{}
	items := []any{}
	for _, file := range configMapMigrationFiles {
		data[file.key] = file.body
		items = append(items, map[string]any{"key": file.key, "path": file.path})
	}
	c.check(c.apply(c.sourceDocument(reachabilityMigrationSource, data)), "apply ConfigMap %s", reachabilityMigrationSource)
	c.check(c.create(map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahMigration",
		"metadata": map[string]any{"namespace": c.in.TestNamespace, "name": reachabilityMigration},
		"spec": map[string]any{
			"target": map[string]any{
				"engine": "PostgreSQL", "coordinationKey": "e2e/" + reachabilityMigrationDatabase,
				"urlFrom": map[string]any{"name": reachabilityMigrationSecret, "key": "url"},
			},
			"artifact":  map[string]any{"configMap": map[string]any{"name": reachabilityMigrationSource, "items": items}},
			"interval":  configMapInterval,
			"policy":    map[string]any{"apply": "OnApproval"},
			"execution": map[string]any{"activeDeadlineSeconds": int64(600), "failureRetryInterval": "5s"},
		},
	}), "create PtahMigration %s", reachabilityMigration)
	c.waitForConditions(reachabilityMigration+" to report on DatabaseReachable that its target refused the credentials", read,
		func(generation int64, conditions []metav1.Condition) bool {
			return targetRefusedCredentials(conditions, generation, url)
		})
	c.printReachabilityReading("refused migration", migration.Status)

	c.applyDatabaseSecret(reachabilityMigrationDatabase, reachabilityMigrationSecret, c.password)
	c.waitForConditions(reachabilityMigration+" to report DatabaseReachable=True once its Secret is corrected", read,
		func(generation int64, conditions []metav1.Condition) bool {
			return targetAnsweredAgain(conditions, generation)
		})
	c.printReachabilityReading("recovered migration", migration.Status)
}

// waitForConditions waits for one reading of a resource's conditions that
// satisfies done, through the failed attempts this row exists to read. The object
// read closes over holds the document that matched when it returns.
func (c *configMapRun) waitForConditions(
	what string,
	read func() (int64, []metav1.Condition, error),
	done func(generation int64, conditions []metav1.Condition) bool,
) {
	c.t.Helper()
	c.wait(what, func() (bool, string) {
		generation, conditions, err := read()
		if err != nil {
			return false, err.Error()
		}
		return done(generation, conditions), reachabilityReading(conditions)
	})
}

// printReachabilityReading prints the status that satisfied a wait, so a run's log
// holds the document a predicate test can read. A status carries no credential by
// contract, which this phase's failure output already relies on.
func (c *configMapRun) printReachabilityReading(name string, status any) {
	c.t.Helper()
	reading, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "e2e ConfigMap sources: the %s reading\n%s\n", name, reading)
}
