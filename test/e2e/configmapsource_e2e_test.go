//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/operate"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
	"github.com/stokaro/ptah-operator/internal/sourcesnapshot"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// TestConfigMapSources is the configmap-sources phase.
func TestConfigMapSources(t *testing.T) {
	run, inputs := harness.Begin(t, phases.ConfigMapSources)
	c := newConfigMapRun(t, run, inputs)
	for _, scenario := range []struct {
		name string
		body func()
	}{
		{"configmap-schema", c.schemaFromConfigMap},
		{"configmap-schema-edits", c.schemaEdits},
		{"configmap-migration", c.migrationFromConfigMap},
		{"configmap-snapshot-refused", c.replacedSnapshotRefused},
		{"configmap-unreadable-source", c.unreadableSources},
		{"database-reachable-kind", c.targetReachability},
		{"configmap-refused-statement", c.refusedStatements},
	} {
		if !run.Scenario(scenario.name, c.scenario(scenario.body)) {
			return
		}
	}
	c.finish()
	run.Logf("e2e ConfigMap sources: PASS a schema and a migration sequence from ConfigMaps, with no registry")
}

// configMapRun is what the ConfigMap source proofs share. Each scenario runs
// as a subtest, and t is that subtest while it runs.
type configMapRun struct {
	t      *testing.T
	parent *testing.T
	ctx    context.Context
	in     phases.ConfigMapSourceInputs

	cluster  *harness.Cluster
	engine   migrationEngine
	password string
}

func newConfigMapRun(t *testing.T, run *harness.Run, in phases.ConfigMapSourceInputs) *configMapRun {
	t.Helper()
	c := &configMapRun{t: t, parent: t, ctx: run.Context(), in: in}
	t.Cleanup(c.cleanup)
	if info, err := os.Stat(in.Kubeconfig); err != nil || !info.Mode().IsRegular() {
		c.fatalf("E2E_KUBECONFIG does not name a file")
	}
	var err error
	if c.engine, err = migrationEngineFor("postgresql"); err != nil {
		c.fatalf("%v", err)
	}
	if c.cluster, err = harness.Connect(in.Kubeconfig); err != nil {
		c.fatalf("%v", err)
	}
	secret := &corev1.Secret{}
	if err := c.get(c.engine.sourceSecret, secret); err != nil {
		c.fatalf("the data plane PostgreSQL Secret %s could not be read", c.engine.sourceSecret)
	}
	if c.password = string(secret.Data["password"]); c.password == "" {
		c.fatalf("the data plane PostgreSQL Secret carries no password")
	}
	return c
}

func (c *configMapRun) scenario(body func()) func(*testing.T) {
	return func(t *testing.T) {
		c.t = t
		defer func() { c.t = c.parent }()
		body()
	}
}

func (c *configMapRun) fatalf(format string, arguments ...any) {
	c.t.Helper()
	c.t.Fatalf("e2e ConfigMap sources: "+format, arguments...)
}

func (c *configMapRun) logf(format string, arguments ...any) {
	c.t.Helper()
	c.t.Logf("e2e ConfigMap sources: "+format, arguments...)
}

func (c *configMapRun) check(err error, format string, arguments ...any) {
	c.t.Helper()
	if err != nil {
		c.fatalf("%s: %v", fmt.Sprintf(format, arguments...), err)
	}
}

func (c *configMapRun) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: c.in.TestNamespace, Name: name}
}

func (c *configMapRun) get(name string, object client.Object) error {
	return c.cluster.Client.Get(c.ctx, c.key(name), object)
}

func (c *configMapRun) create(document map[string]any) error {
	return c.cluster.Client.Create(c.ctx, &unstructured.Unstructured{Object: document},
		client.FieldOwner(harness.FieldOwner), client.FieldValidation("Strict"))
}

func (c *configMapRun) apply(document map[string]any) error {
	return c.cluster.Client.Patch(c.ctx, &unstructured.Unstructured{Object: document}, client.Apply, //nolint:staticcheck // The typed Apply needs generated apply configurations the API does not ship.
		client.FieldOwner(harness.FieldOwner), client.ForceOwnership, client.FieldValidation("Strict"))
}

// wait polls until check holds and fails the scenario when the bound ends.
func (c *configMapRun) wait(what string, check func() (bool, string)) {
	c.t.Helper()
	err := harness.Wait(c.ctx, what, waitTimeout, 3*time.Second, func(context.Context) (bool, string, error) {
		done, observed := check()
		return done, observed, nil
	})
	c.check(err, "wait for %s", what)
}

func (c *configMapRun) schema() *ptahv1alpha1.PtahSchema {
	c.t.Helper()
	schema := &ptahv1alpha1.PtahSchema{}
	c.check(c.get(configMapSchema, schema), "read PtahSchema %s", configMapSchema)
	return schema
}

func (c *configMapRun) migration() *ptahv1alpha1.PtahMigration {
	c.t.Helper()
	migration := &ptahv1alpha1.PtahMigration{}
	c.check(c.get(configMapMigration, migration), "read PtahMigration %s", configMapMigration)
	return migration
}

// waitForSchema waits for one reading that satisfies done and returns that
// reading, so a claim is asserted against the document that matched.
func (c *configMapRun) waitForSchema(what string, done func(*ptahv1alpha1.PtahSchema) bool) *ptahv1alpha1.PtahSchema {
	c.t.Helper()
	return c.waitForSchemaThrough(what, false, done)
}

// waitForSchemaThrough is waitForSchema that, when retrying is set, waits
// through a failed attempt rather than ending the scenario on it: a proof that
// could not read its snapshot fails each attempt until the snapshot is put
// back.
func (c *configMapRun) waitForSchemaThrough(what string, retrying bool, done func(*ptahv1alpha1.PtahSchema) bool) *ptahv1alpha1.PtahSchema {
	c.t.Helper()
	var matched *ptahv1alpha1.PtahSchema
	c.wait(what, func() (bool, string) {
		schema := &ptahv1alpha1.PtahSchema{}
		if err := c.get(configMapSchema, schema); err != nil {
			return false, err.Error()
		}
		if schema.Status.Phase == ptahv1alpha1.PhaseFailed && !retrying {
			c.fatalf("%s failed while waiting for %s: %v", configMapSchema, what, schema.Status.Conditions)
		}
		if done(schema) {
			matched = schema
			return true, ""
		}
		return false, string(schema.Status.Phase)
	})
	return matched
}

func (c *configMapRun) waitForMigration(what string, done func(*ptahv1alpha1.PtahMigration) bool) *ptahv1alpha1.PtahMigration {
	c.t.Helper()
	var matched *ptahv1alpha1.PtahMigration
	c.wait(what, func() (bool, string) {
		migration := &ptahv1alpha1.PtahMigration{}
		if err := c.get(configMapMigration, migration); err != nil {
			return false, err.Error()
		}
		if migration.Status.Phase == ptahv1alpha1.MigrationPhaseFailed {
			c.fatalf("%s failed while waiting for %s: %v", configMapMigration, what, migration.Status.Conditions)
		}
		if done(migration) {
			matched = migration
			return true, ""
		}
		return false, string(migration.Status.Phase)
	})
	return matched
}

// settledSchema is a schema that finished a cycle and holds nothing in
// flight.
func settledSchema(schema *ptahv1alpha1.PtahSchema) bool {
	return schema.Status.ObservedGeneration == schema.Generation && schema.Status.ActiveOperation == nil &&
		schema.Status.PendingObservation == nil
}

func settledMigration(migration *ptahv1alpha1.PtahMigration) bool {
	return migration.Status.ObservedGeneration == migration.Generation && migration.Status.ActiveOperation == nil
}

// reset removes what an earlier run of this phase left, by the names this
// phase alone uses, and the two databases.
func (c *configMapRun) reset() {
	c.t.Helper()
	objects := []client.Object{
		&ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: configMapSchema}},
		&ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: configMapUnreadableSchema}},
		&ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: configMapRefusedSchema}},
		&ptahv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: configMapMigration}},
		&ptahv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: configMapUnreadableMigration}},
		&ptahv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: configMapRefusedMigration}},
		&ptahv1alpha1.PtahMigrationApproval{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: "e2e-configmap-migration-v1"}},
		&ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: reachabilitySchema}},
		&ptahv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: reachabilityMigration}},
		&ptahv1alpha1.PtahMigrationApproval{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: "e2e-configmap-refused-migration"}},
		&ptahv1alpha1.PtahSchemaApproval{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: "e2e-configmap-refused-v1"}},
		&ptahv1alpha1.PtahSchemaApproval{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: "e2e-configmap-refused-v2"}},
	}
	for _, approval := range []string{"e2e-configmap-schema-v1", "e2e-configmap-schema-v2", "e2e-configmap-schema-v3"} {
		objects = append(objects, &ptahv1alpha1.PtahSchemaApproval{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: approval}})
	}
	for _, object := range objects {
		c.deleteAndWait(object)
	}
	for _, database := range []string{configMapSchemaDatabase, configMapMigrationDatabase, configMapUnreadableDatabase,
		configMapRefusedDatabase, reachabilitySchemaDatabase, reachabilityMigrationDatabase} {
		_, err := serverSQL(c.ctx, c.cluster, c.in.TestNamespace, c.engine, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)")
		c.check(err, "drop database %s", database)
	}
}

func (c *configMapRun) deleteAndWait(object client.Object) {
	c.t.Helper()
	if err := c.cluster.Client.Delete(c.ctx, object, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil &&
		!apierrors.IsNotFound(err) {
		c.fatalf("%s was not removed: %v", object.GetName(), err)
	}
	c.wait(object.GetName()+" to be removed", func() (bool, string) {
		current := object.DeepCopyObject().(client.Object)
		err := c.cluster.Client.Get(c.ctx, client.ObjectKeyFromObject(object), current)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err == nil && object.GetUID() != "" && current.GetUID() != object.GetUID() {
			return true, ""
		}
		return false, "still present"
	})
}

// createDatabase creates a database on the data plane's server and the
// Secret its resource reads the URL from.
func (c *configMapRun) createDatabase(database, secret string) {
	c.t.Helper()
	c.createDatabaseWithPassword(database, secret, c.password)
}

// createDatabaseWithPassword is createDatabase with a URL that carries password.
func (c *configMapRun) createDatabaseWithPassword(database, secret, password string) {
	c.t.Helper()
	_, err := serverSQL(c.ctx, c.cluster, c.in.TestNamespace, c.engine, "CREATE DATABASE "+database)
	c.check(err, "create database %s", database)
	c.applyDatabaseSecret(database, secret, password)
}

// applyDatabaseSecret writes the Secret a resource reads its URL from.
func (c *configMapRun) applyDatabaseSecret(database, secret, password string) {
	c.t.Helper()
	url := c.engine.databaseURL(c.in.TestNamespace, migrationDatabaseUser, password, database)
	c.check(c.apply(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"namespace": c.in.TestNamespace, "name": secret},
		"data":     secretData(map[string]string{"url": url}),
	}), "apply Secret %s", secret)
}

// query is one value from a database, as the client printed it.
func (c *configMapRun) query(database, statement string) string {
	output, _ := databaseSQL(c.ctx, c.cluster, c.in.TestNamespace, c.engine, database, statement)
	return trimmedSQL(output)
}

func (c *configMapRun) sourceDocument(name string, data map[string]string) map[string]any {
	values := map[string]any{}
	for key, value := range data {
		values[key] = value
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"namespace": c.in.TestNamespace, "name": name},
		"data":     values,
	}
}

// editSource sets one key of a source ConfigMap and returns the
// resourceVersion the edit gave it.
func (c *configMapRun) editSource(name, key, value string) string {
	c.t.Helper()
	var version string
	c.check(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		source := &corev1.ConfigMap{}
		if err := c.get(name, source); err != nil {
			return err
		}
		source.Data[key] = value
		if err := c.cluster.Client.Update(c.ctx, source, client.FieldOwner(harness.FieldOwner)); err != nil {
			return err
		}
		version = source.ResourceVersion
		return nil
	}), "edit key %s of ConfigMap %s", key, name)
	return version
}

func (c *configMapRun) sourceUID(name string) types.UID {
	c.t.Helper()
	source := &corev1.ConfigMap{}
	c.check(c.get(name, source), "read ConfigMap %s", name)
	return source.UID
}

// assertSnapshot holds the stored snapshot to the binding the resource
// recorded, and returns it.
func (c *configMapRun) assertSnapshot(kind string, owner types.UID, binding *ptahv1alpha1.SourceSnapshotBinding) *corev1.ConfigMap {
	c.t.Helper()
	if binding == nil {
		c.fatalf("the %s binds no snapshot", kind)
	}
	stored := &corev1.ConfigMap{}
	c.check(c.get(binding.Name, stored), "read snapshot %s", binding.Name)
	c.check(snapshotOf(stored, kind, owner, binding), "snapshot %s", binding.Name)
	return stored
}

func (c *configMapRun) snapshotGone(name string) bool {
	return apierrors.IsNotFound(c.get(name, &corev1.ConfigMap{}))
}

// assertSnapshotJobs holds every operation Job of a resource that still
// exists to the snapshot shape, and refuses a Resolve or Verify Job: a
// ConfigMap source is read by the manager. It needs at least one Job of each
// operation named in want, so a list that came back empty proves nothing.
func (c *configMapRun) assertSnapshotJobs(label, name string, want ...string) {
	c.t.Helper()
	jobs := &batchv1.JobList{}
	c.check(c.cluster.Client.List(c.ctx, jobs, client.InNamespace(c.in.TestNamespace), client.MatchingLabels{label: name}),
		"list the Jobs of %s", name)
	seen := map[string]int{}
	for index := range jobs.Items {
		job := &jobs.Items[index]
		operation := job.Labels[labelOperation]
		switch operation {
		case "resolve", "verify":
			c.fatalf("%s ran a %s Job %s, and a ConfigMap source has none", name, operation, job.Name)
		case "observe", "plan", "history", "apply":
			if operation == "apply" && label == labelSchema {
				continue
			}
			c.check(snapshotJob(job), "Job %s", job.Name)
		}
		seen[operation]++
	}
	for _, operation := range want {
		if seen[operation] == 0 {
			c.fatalf("%s has no %s Job to examine; it has %v", name, operation, seen)
		}
	}
}

func (c *configMapRun) planCount(label, name string, list client.ObjectList) int {
	c.t.Helper()
	c.check(c.cluster.Client.List(c.ctx, list, client.InNamespace(c.in.TestNamespace), client.MatchingLabels{label: name}),
		"list the plans of %s", name)
	switch typed := list.(type) {
	case *ptahv1alpha1.PtahSchemaPlanList:
		return len(typed.Items)
	case *ptahv1alpha1.PtahMigrationPlanList:
		return len(typed.Items)
	}
	return -1
}

func (c *configMapRun) approveSchemaPlan(approval string, schema *ptahv1alpha1.PtahSchema) {
	c.t.Helper()
	plan := &ptahv1alpha1.PtahSchemaPlan{}
	c.check(c.get(schema.Status.Plan.Name, plan), "read plan %s", schema.Status.Plan.Name)
	c.check(configMapPlanBound(plan.Spec, schema.Status.Source.ConfigMap, schema.Status.Source.Snapshot), "plan %s", plan.Name)
	c.check(c.create(approvalDocument(c.in.TestNamespace, approval, configMapSchema, string(schema.UID),
		plan.Name, string(plan.UID), plan.Spec.Fingerprint)), "approve plan %s", plan.Name)
}

func awaitingSchemaPlan(previous string) func(*ptahv1alpha1.PtahSchema) bool {
	return func(schema *ptahv1alpha1.PtahSchema) bool {
		return schema.Status.Phase == ptahv1alpha1.PhaseAwaitingApproval && schema.Status.Plan != nil &&
			schema.Status.Plan.Name != previous && settledSchema(schema)
	}
}

func inSyncSchema(schema *ptahv1alpha1.PtahSchema) bool {
	return schema.Status.Phase == ptahv1alpha1.PhaseInSync && settledSchema(schema)
}

// schemaFromConfigMap applies a schema whose desired state is one file of a
// ConfigMap. No registry is named anywhere: the manager reads the ConfigMap,
// the plan binds the ConfigMap and the content, and every Job reads the
// checked snapshot.
func (c *configMapRun) schemaFromConfigMap() {
	c.t.Helper()
	c.reset()
	c.createDatabase(configMapSchemaDatabase, configMapSchemaSecret)
	c.check(c.apply(c.sourceDocument(configMapSchemaSource, map[string]string{
		"schema.sql": configMapSchemaRevisions[0], configMapUnselectedKey: "first\n",
	})), "apply ConfigMap %s", configMapSchemaSource)
	c.check(c.create(map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahSchema",
		"metadata": map[string]any{"namespace": c.in.TestNamespace, "name": configMapSchema},
		"spec": map[string]any{
			"target": map[string]any{
				"engine": "PostgreSQL", "coordinationKey": "e2e/" + configMapSchemaDatabase,
				"urlFrom": map[string]any{"name": configMapSchemaSecret, "key": "url"},
			},
			"desired": map[string]any{"configMap": map[string]any{
				"name":  configMapSchemaSource,
				"items": []any{map[string]any{"key": "schema.sql", "path": "schema.sql"}},
			}},
			"interval": configMapInterval,
			"policy":   map[string]any{"apply": "OnApproval"},
			"execution": map[string]any{
				"activeDeadlineSeconds": int64(600),
				// The refusal row's proof fails each attempt until its snapshot
				// is put back; a short retry keeps the wait after that short.
				"failureRetryInterval": "10s",
			},
		},
	}), "create PtahSchema %s", configMapSchema)

	planned := c.waitForSchema("the first plan", awaitingSchemaPlan(""))
	source := planned.Status.Source
	if source.ConfigMap == nil || source.ConfigMap.UID != c.sourceUID(configMapSchemaSource) || source.Digest != "" || source.Verified {
		c.fatalf("the schema's source is %#v, want the ConfigMap read and no OCI artifact", source)
	}
	c.assertSnapshot("PtahSchema", planned.UID, source.Snapshot)
	c.assertSnapshotJobs(labelSchema, configMapSchema, "observe", "plan")
	c.approveSchemaPlan("e2e-configmap-schema-v1", planned)
	c.waitForSchema("the first plan to apply", inSyncSchema)
	if tables := c.query(configMapSchemaDatabase, "SELECT count(*) FROM information_schema.tables WHERE table_name='e2e_configmap_items'"); tables != "1" {
		c.fatalf("the applied schema left %q tables named e2e_configmap_items, want 1", tables)
	}
	c.logf("PASS a schema from a ConfigMap applied with no registry, through snapshot %s", source.Snapshot.Name)
}

// schemaEdits edits the source ConfigMap twice. An edit to a key the schema
// does not select is read at once and changes nothing: the same snapshot, no
// plan. An edit to the selected key gives a new snapshot and a new plan, and
// the snapshot nothing names any more is deleted once the schema stops naming
// it.
func (c *configMapRun) schemaEdits() {
	c.t.Helper()
	before := c.waitForSchema("the schema to be in sync", inSyncSchema)
	original := *before.Status.Source.Snapshot
	plans := c.planCount(labelSchema, configMapSchema, &ptahv1alpha1.PtahSchemaPlanList{})

	version := c.editSource(configMapSchemaSource, configMapUnselectedKey, "edited\n")
	unselected := c.waitForSchema("the unselected edit to be read and settled", func(schema *ptahv1alpha1.PtahSchema) bool {
		return sourceReadAt(schema.Status.Source.ConfigMap, schema.Status.Source.Snapshot, version) && inSyncSchema(schema)
	})
	if *unselected.Status.Source.Snapshot != original {
		c.fatalf("an edit to an unselected key moved the snapshot from %#v to %#v", original, *unselected.Status.Source.Snapshot)
	}
	if after := c.planCount(labelSchema, configMapSchema, &ptahv1alpha1.PtahSchemaPlanList{}); after != plans {
		c.fatalf("an edit to an unselected key published %d plans", after-plans)
	}
	c.assertSnapshot("PtahSchema", unselected.UID, &original)

	version = c.editSource(configMapSchemaSource, "schema.sql", configMapSchemaRevisions[1])
	selected := c.waitForSchema("the selected edit to plan again", func(schema *ptahv1alpha1.PtahSchema) bool {
		return sourceReadAt(schema.Status.Source.ConfigMap, schema.Status.Source.Snapshot, version) && awaitingSchemaPlan("")(schema)
	})
	if selected.Status.Source.Snapshot.ContentDigest == original.ContentDigest || selected.Status.Source.Snapshot.Name == original.Name {
		c.fatalf("an edit to the selected key kept the snapshot %s", original.Name)
	}
	c.assertSnapshot("PtahSchema", selected.UID, selected.Status.Source.Snapshot)
	if !c.snapshotGone(original.Name) {
		c.fatalf("snapshot %s is still stored after the schema stopped naming it", original.Name)
	}
	c.approveSchemaPlan("e2e-configmap-schema-v2", selected)
	c.waitForSchema("the second plan to apply", inSyncSchema)
	if column := c.query(configMapSchemaDatabase, "SELECT count(*) FROM information_schema.columns WHERE table_name='e2e_configmap_items' AND column_name='sku'"); column != "1" {
		c.fatalf("the edited schema left %q sku columns, want 1", column)
	}
	c.logf("PASS an unselected edit planned nothing, a selected edit planned again and retired snapshot %s", original.Name)
}

// migrationFromConfigMap applies a migration sequence whose files are keys of
// a ConfigMap, and holds an edit to an unselected key to no new plan.
func (c *configMapRun) migrationFromConfigMap() {
	c.t.Helper()
	c.createDatabase(configMapMigrationDatabase, configMapMigrationSecret)
	data := map[string]string{configMapUnselectedKey: "first\n"}
	items := []any{}
	for _, file := range configMapMigrationFiles {
		data[file.key] = file.body
		items = append(items, map[string]any{"key": file.key, "path": file.path})
	}
	c.check(c.apply(c.sourceDocument(configMapMigrationSource, data)), "apply ConfigMap %s", configMapMigrationSource)
	c.check(c.create(map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahMigration",
		"metadata": map[string]any{"namespace": c.in.TestNamespace, "name": configMapMigration},
		"spec": map[string]any{
			"target": map[string]any{
				"engine": "PostgreSQL", "coordinationKey": "e2e/" + configMapMigrationDatabase,
				"urlFrom": map[string]any{"name": configMapMigrationSecret, "key": "url"},
			},
			"artifact":  map[string]any{"configMap": map[string]any{"name": configMapMigrationSource, "items": items}},
			"interval":  configMapInterval,
			"policy":    map[string]any{"apply": "OnApproval"},
			"execution": map[string]any{"activeDeadlineSeconds": int64(600)},
		},
	}), "create PtahMigration %s", configMapMigration)

	planned := c.waitForMigration("the migration plan", func(migration *ptahv1alpha1.PtahMigration) bool {
		return migration.Status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval && migration.Status.Plan != nil &&
			settledMigration(migration)
	})
	if planned.Status.Artifact == nil || planned.Status.ConfigMap == nil || planned.Status.Artifact.Digest != "" ||
		planned.Status.ConfigMap.UID != c.sourceUID(configMapMigrationSource) {
		c.fatalf("the migration's source is %#v %#v, want the ConfigMap read and no OCI artifact", planned.Status.Artifact, planned.Status.ConfigMap)
	}
	c.assertSnapshot("PtahMigration", planned.UID, planned.Status.Artifact.Snapshot)
	plan := &ptahv1alpha1.PtahMigrationPlan{}
	c.check(c.get(planned.Status.Plan.Name, plan), "read plan %s", planned.Status.Plan.Name)
	c.check(configMapMigrationPlanBound(plan.Spec, planned.Status.ConfigMap, planned.Status.Artifact.Snapshot), "plan %s", plan.Name)
	if len(plan.Spec.Migrations) != 2 {
		c.fatalf("plan %s carries %d migrations, want the 2 the ConfigMap holds", plan.Name, len(plan.Spec.Migrations))
	}
	c.check(c.create(migrationApprovalDocument(c.in.TestNamespace, "e2e-configmap-migration-v1", configMapMigration,
		string(planned.UID), plan.Name, string(plan.UID), plan.Spec.Fingerprint)), "approve plan %s", plan.Name)
	applied := c.waitForMigration("the migration plan to apply", func(migration *ptahv1alpha1.PtahMigration) bool {
		return migration.Status.Phase == ptahv1alpha1.MigrationPhaseInSync && settledMigration(migration)
	})
	if column := c.query(configMapMigrationDatabase, "SELECT count(*) FROM information_schema.columns WHERE table_name='e2e_configmap_parts' AND column_name='weight'"); column != "1" {
		c.fatalf("the applied sequence left %q weight columns, want 1", column)
	}
	c.assertSnapshotJobs(labelMigration, configMapMigration, "history", "apply")

	plans := c.planCount(labelMigration, configMapMigration, &ptahv1alpha1.PtahMigrationPlanList{})
	version := c.editSource(configMapMigrationSource, configMapUnselectedKey, "edited\n")
	unselected := c.waitForMigration("the unselected edit to be read and settled", func(migration *ptahv1alpha1.PtahMigration) bool {
		return migration.Status.Artifact != nil && sourceReadAt(migration.Status.ConfigMap, migration.Status.Artifact.Snapshot, version) &&
			migration.Status.Phase == ptahv1alpha1.MigrationPhaseInSync && settledMigration(migration)
	})
	if *unselected.Status.Artifact.Snapshot != *applied.Status.Artifact.Snapshot {
		c.fatalf("an edit to an unselected key moved the migration's snapshot")
	}
	if after := c.planCount(labelMigration, configMapMigration, &ptahv1alpha1.PtahMigrationPlanList{}); after != plans {
		c.fatalf("an edit to an unselected key published %d migration plans", after-plans)
	}
	c.logf("PASS a migration sequence from a ConfigMap applied with no registry; an unselected edit planned nothing")
}

// replacedSnapshotRefused replaces the snapshot a plan was computed from, after
// the plan is approved and before the proof that follows the Apply reads it.
// The Pod refuses the replacement before the operation container starts, the
// proof stays outstanding, and putting the exact bytes back lets it finish.
func (c *configMapRun) replacedSnapshotRefused() {
	c.t.Helper()
	before := c.waitForSchema("the schema to be in sync", inSyncSchema)
	previous := ""
	version := c.editSource(configMapSchemaSource, "schema.sql", configMapSchemaRevisions[2])
	planned := c.waitForSchema("the index plan", func(schema *ptahv1alpha1.PtahSchema) bool {
		return sourceReadAt(schema.Status.Source.ConfigMap, schema.Status.Source.Snapshot, version) && awaitingSchemaPlan(previous)(schema)
	})
	bound := *planned.Status.Source.Snapshot
	original := c.assertSnapshot("PtahSchema", before.UID, &bound)

	replacement := original.DeepCopy()
	replacement.ObjectMeta = metav1.ObjectMeta{
		Namespace: original.Namespace, Name: original.Name, Labels: original.Labels, OwnerReferences: original.OwnerReferences,
	}
	replacement.BinaryData[sourcecontent.SnapshotFileKey(0)] = []byte(configMapSchemaRevisions[2] + "\nDROP TABLE e2e_configmap_items;\n")
	c.deleteAndWait(original)
	c.check(c.cluster.Client.Create(c.ctx, replacement), "store the replaced snapshot %s", bound.Name)

	c.approveSchemaPlan("e2e-configmap-schema-v3", planned)
	var refusal string
	c.wait("the proof to refuse the replaced snapshot", func() (bool, string) {
		pods := &corev1.PodList{}
		if err := c.cluster.Client.List(c.ctx, pods, client.InNamespace(c.in.TestNamespace),
			client.MatchingLabels{labelSchema: configMapSchema, labelOperation: "observe"}); err != nil {
			return false, err.Error()
		}
		for index := range pods.Items {
			if refused, message := snapshotRefused(&pods.Items[index]); refused {
				refusal = message
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d observe Pods, none refused", len(pods.Items))
	})
	if outstanding := c.schema(); outstanding.Status.PendingObservation == nil {
		c.fatalf("the proof was settled while it could not read its snapshot")
	}

	// The source still holds the files the plan was computed from, so the
	// snapshot is rebuilt exactly as the manager wrote it.
	source := &corev1.ConfigMap{}
	c.check(c.get(configMapSchemaSource, source), "read ConfigMap %s", configMapSchemaSource)
	files, err := sourcesnapshot.Select(source, []ptahv1alpha1.ConfigMapSourceItem{{Key: "schema.sql", Path: "schema.sql"}})
	c.check(err, "select the source files")
	rebuilt, digest, err := sourcesnapshot.Build(c.in.TestNamespace,
		sourcesnapshot.Owner{Kind: "PtahSchema", Name: before.Name, UID: before.UID}, files)
	c.check(err, "rebuild the snapshot")
	if rebuilt.Name != bound.Name || digest != bound.ContentDigest {
		c.fatalf("the rebuilt snapshot %s %s is not the bound %s %s", rebuilt.Name, digest, bound.Name, bound.ContentDigest)
	}
	stored := &corev1.ConfigMap{}
	c.check(c.get(bound.Name, stored), "read the replaced snapshot")
	c.deleteAndWait(stored)
	c.check(c.cluster.Client.Create(c.ctx, rebuilt), "restore snapshot %s", bound.Name)
	c.waitForSchemaThrough("the proof to finish on the restored snapshot", true, inSyncSchema)
	if index := c.query(configMapSchemaDatabase, "SELECT count(*) FROM pg_indexes WHERE indexname='e2e_configmap_items_sku'"); index != "1" {
		c.fatalf("the applied plan left %q indexes named e2e_configmap_items_sku, want 1", index)
	}
	if tables := c.query(configMapSchemaDatabase, "SELECT count(*) FROM information_schema.tables WHERE table_name='e2e_configmap_items'"); tables != "1" {
		c.fatalf("the replaced snapshot's statement reached the database")
	}
	c.logf("PASS a snapshot replaced after approval was refused by the Pod: %s", refusal)
}

// unreadableSources delivers a schema file Ptah cannot read and a migration
// file it cannot load, each through a ConfigMap, and reads the reason where a
// person reads it. A read request is taken while the read fails. Repairing the
// file is then all it takes: the edit alone ends the failed claim, with no spec
// edit and no suspension, and the same resource plans from the repaired file, so
// a reason that is true of one revision does not outlive it. The resources are
// removed afterward: neither is a state a later scenario reads.
func (c *configMapRun) unreadableSources() {
	c.t.Helper()
	c.createDatabase(configMapUnreadableDatabase, configMapUnreadableSecret)
	target := map[string]any{
		"engine": "PostgreSQL", "coordinationKey": "e2e/" + configMapUnreadableDatabase,
		"urlFrom": map[string]any{"name": configMapUnreadableSecret, "key": "url"},
	}
	// A short retry: the proof waits through the failed attempts it reads.
	execution := map[string]any{"activeDeadlineSeconds": int64(600), "failureRetryInterval": "10s"}

	c.check(c.apply(c.sourceDocument(configMapUnreadableSchemaSource, map[string]string{"schema.sql": unreadablePartitionSchema})),
		"apply ConfigMap %s", configMapUnreadableSchemaSource)
	c.check(c.create(map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahSchema",
		"metadata": map[string]any{"namespace": c.in.TestNamespace, "name": configMapUnreadableSchema},
		"spec": map[string]any{
			"target": target,
			"desired": map[string]any{"configMap": map[string]any{
				"name":  configMapUnreadableSchemaSource,
				"items": []any{map[string]any{"key": "schema.sql", "path": "schema.sql"}},
			}},
			"interval": configMapInterval, "policy": map[string]any{"apply": "OnApproval"}, "execution": execution,
		},
	}), "create PtahSchema %s", configMapUnreadableSchema)
	schema := &ptahv1alpha1.PtahSchema{}
	var message string
	c.wait("the schema to say why it could not read its file", func() (bool, string) {
		if err := c.get(configMapUnreadableSchema, schema); err != nil {
			return false, err.Error()
		}
		failed := meta.FindStatusCondition(schema.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed)
		if failed == nil || failed.Status != metav1.ConditionTrue || failed.Reason != string(ptahv1alpha1.ReasonOperationFailed) {
			return false, fmt.Sprintf("%s, ReconciliationFailed %+v", schema.Status.Phase, failed)
		}
		message = failed.Message
		err := unreadableSourceSays(message, unreadablePartitionFile, unreadablePartitionReason)
		return err == nil, fmt.Sprint(err)
	})
	c.checkUnreadableText(message)
	c.waitForUnreadableEvent(schema.UID, "OperationFailed", unreadablePartitionFile, unreadablePartitionReason)
	retried := c.waitForUnreadableSchema("the read to fail again under its claim", func(s *ptahv1alpha1.PtahSchema) error {
		if !schemaReadRetried(s, "") {
			return fmt.Errorf("%s, operation %+v", s.Status.Phase, s.Status.ActiveOperation)
		}
		return nil
	})
	generation, failedSource := retried.Generation, schemaSourceIdentity(retried)
	request := c.requestUnreadableSchemaRead(retried)
	c.waitForUnreadableSchema("the read request to be taken while the read fails", func(s *ptahv1alpha1.PtahSchema) error {
		if !readRequestTakenWhileFailing(s, request) {
			return fmt.Errorf("lastReadRequest %q, phase %s", s.Status.LastReadRequest, s.Status.Phase)
		}
		return nil
	})
	c.waitForUnreadableSchema("the read the request started to fail in its turn", func(s *ptahv1alpha1.PtahSchema) error {
		if !schemaReadRetried(s, retried.Status.ActiveOperation.ID) {
			return fmt.Errorf("%s, operation %+v", s.Status.Phase, s.Status.ActiveOperation)
		}
		return nil
	})
	c.editSource(configMapUnreadableSchemaSource, "schema.sql", configMapSchemaRevisions[0])
	c.waitForUnreadableSchema("the schema to plan from the edit alone", func(s *ptahv1alpha1.PtahSchema) error {
		return schemaReadTheRepair(s, generation, failedSource)
	})
	c.deleteAndWait(schema)
	c.logf("PASS a schema from a ConfigMap said why its file could not be read: %s", message)

	data := map[string]string{}
	items := []any{}
	for _, file := range unreadableMigrationFiles {
		data[file.key] = file.body
		items = append(items, map[string]any{"key": file.key, "path": file.path})
	}
	c.check(c.apply(c.sourceDocument(configMapUnreadableMigrationSource, data)), "apply ConfigMap %s", configMapUnreadableMigrationSource)
	c.check(c.create(map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahMigration",
		"metadata": map[string]any{"namespace": c.in.TestNamespace, "name": configMapUnreadableMigration},
		"spec": map[string]any{
			"target": target, "artifact": map[string]any{"configMap": map[string]any{"name": configMapUnreadableMigrationSource, "items": items}},
			"interval": configMapInterval, "policy": map[string]any{"apply": "OnApproval"}, "execution": execution,
		},
	}), "create PtahMigration %s", configMapUnreadableMigration)
	migration := &ptahv1alpha1.PtahMigration{}
	c.wait("the migration to say why it could not load its file", func() (bool, string) {
		if err := c.get(configMapUnreadableMigration, migration); err != nil {
			return false, err.Error()
		}
		progressing := meta.FindStatusCondition(migration.Status.Conditions, ptahv1alpha1.ConditionMigrationProgressing)
		if progressing == nil || progressing.Reason != string(ptahv1alpha1.ReasonOperationFailed) {
			return false, fmt.Sprintf("%s, Progressing %+v", migration.Status.Phase, progressing)
		}
		message = progressing.Message
		err := unreadableSourceSays(message, unreadableMigrationFile, unreadableMigrationReason)
		return err == nil, fmt.Sprint(err)
	})
	c.checkUnreadableText(message)
	c.waitForUnreadableEvent(migration.UID, "OperationRetried", unreadableMigrationFile, unreadableMigrationReason)
	c.wait("the migration's read to fail again under its claim", func() (bool, string) {
		if err := c.get(configMapUnreadableMigration, migration); err != nil {
			return false, err.Error()
		}
		return migrationReadRetried(migration, ""), fmt.Sprintf("%s, operation %+v", migration.Status.Phase, migration.Status.ActiveOperation)
	})
	generation = migration.Generation
	failedSource = migration.Status.Artifact.Snapshot.ContentDigest
	c.editSource(configMapUnreadableMigrationSource, unreadableMigrationFiles[0].key, "CREATE TABLE e2e_unreadable_events (id bigint);\n")
	c.wait("the migration to plan from the edit alone", func() (bool, string) {
		if err := c.get(configMapUnreadableMigration, migration); err != nil {
			return false, err.Error()
		}
		err := migrationReadTheRepair(migration, generation, failedSource)
		return err == nil, fmt.Sprint(err)
	})
	c.deleteAndWait(migration)
	c.logf("PASS a migration from a ConfigMap said why its file could not be loaded: %s", message)
}

// waitForUnreadableSchema returns the reading that satisfied holds, so a claim is
// asserted against the document that matched. holds says what is missing.
func (c *configMapRun) waitForUnreadableSchema(what string, holds func(*ptahv1alpha1.PtahSchema) error) *ptahv1alpha1.PtahSchema {
	c.t.Helper()
	var matched *ptahv1alpha1.PtahSchema
	c.wait(what, func() (bool, string) {
		schema := &ptahv1alpha1.PtahSchema{}
		if err := c.get(configMapUnreadableSchema, schema); err != nil {
			return false, err.Error()
		}
		if err := holds(schema); err != nil {
			return false, err.Error()
		}
		matched = schema
		return true, ""
	})
	return matched
}

// requestUnreadableSchemaRead writes a read request the way kubectl ptah refresh
// does, later than any the resource answered, and returns its value.
func (c *configMapRun) requestUnreadableSchemaRead(schema *ptahv1alpha1.PtahSchema) string {
	c.t.Helper()
	request := operate.NextReadRequest(time.Now(), schema.Annotations[ptahv1alpha1.ReadRequestAnnotation], schema.Status.LastReadRequest)
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, ptahv1alpha1.ReadRequestAnnotation, request)
	target := &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: configMapUnreadableSchema}}
	c.check(c.cluster.Client.Patch(c.ctx, target, client.RawPatch(types.MergePatchType, patch), client.FieldOwner(harness.FieldOwner)),
		"request a read of %s", target.Name)
	return request
}

// checkUnreadableText holds a reason to what the run that wrote it could know:
// the file, and neither the target's name nor its password.
func (c *configMapRun) checkUnreadableText(message string) {
	c.t.Helper()
	if strings.Contains(message, configMapUnreadableDatabase) || strings.Contains(message, c.password) {
		c.fatalf("the reason names the target: %s", message)
	}
}

// waitForUnreadableEvent waits for the Event that repeats a reason. Events are
// written after the status patch, so a reading of the condition does not yet
// promise one.
func (c *configMapRun) waitForUnreadableEvent(owner types.UID, reason, file, want string) {
	c.t.Helper()
	c.wait("the "+reason+" Event to say why", func() (bool, string) {
		events := &corev1.EventList{}
		if err := c.cluster.Client.List(c.ctx, events, client.InNamespace(c.in.TestNamespace)); err != nil {
			return false, err.Error()
		}
		err := unreadableSourceEvent(events.Items, owner, reason, file, want)
		return err == nil, fmt.Sprint(err)
	})
}

// refusedStatements makes a database refuse a statement of an approved plan, for
// a schema and for a migration, and reads the reason where a person reads it: the
// Pod log of a Job kept for 24 hours, pointed at from status and the Event. The
// resources are removed afterward, and their Jobs with them.
func (c *configMapRun) refusedStatements() {
	c.t.Helper()
	c.createDatabase(configMapRefusedDatabase, configMapRefusedSecret)
	c.refusedSchemaStatement()
	c.refusedMigrationStatement()
}

func (c *configMapRun) refusedTarget() map[string]any {
	return map[string]any{
		"engine": "PostgreSQL", "coordinationKey": "e2e/" + configMapRefusedDatabase,
		"urlFrom": map[string]any{"name": configMapRefusedSecret, "key": "url"},
	}
}

// waitForRefusedSchema waits for one reading that satisfies done and returns it.
func (c *configMapRun) waitForRefusedSchema(what string, done func(*ptahv1alpha1.PtahSchema) bool) *ptahv1alpha1.PtahSchema {
	c.t.Helper()
	var matched *ptahv1alpha1.PtahSchema
	c.wait(what, func() (bool, string) {
		schema := &ptahv1alpha1.PtahSchema{}
		if err := c.get(configMapRefusedSchema, schema); err != nil {
			return false, err.Error()
		}
		if done(schema) {
			matched = schema
			return true, ""
		}
		return false, fmt.Sprintf("%s, conditions %+v", schema.Status.Phase, schema.Status.Conditions)
	})
	return matched
}

func (c *configMapRun) approveRefusedPlan(approval string, schema *ptahv1alpha1.PtahSchema) {
	c.t.Helper()
	plan := &ptahv1alpha1.PtahSchemaPlan{}
	c.check(c.get(schema.Status.Plan.Name, plan), "read plan %s", schema.Status.Plan.Name)
	c.check(c.create(approvalDocument(c.in.TestNamespace, approval, configMapRefusedSchema, string(schema.UID),
		plan.Name, string(plan.UID), plan.Spec.Fingerprint)), "approve plan %s", plan.Name)
}

// applyJobs are the Apply Jobs of one resource.
func (c *configMapRun) applyJobs(label, name string) []batchv1.Job {
	c.t.Helper()
	jobs := &batchv1.JobList{}
	c.check(c.cluster.Client.List(c.ctx, jobs, client.InNamespace(c.in.TestNamespace),
		client.MatchingLabels{label: name, labelOperation: "apply"}), "list the Apply Jobs of %s", name)
	return jobs.Items
}

// refusedSchemaStatement is the reproduction of stokaro/ptah-operator#787: a
// schema in sync on a table whose rows share a customer, and a plan that adds a
// unique index on them.
func (c *configMapRun) refusedSchemaStatement() {
	c.t.Helper()
	c.check(c.apply(c.sourceDocument(configMapRefusedSchemaSource, map[string]string{"schema.sql": configMapRefusedTable})),
		"apply ConfigMap %s", configMapRefusedSchemaSource)
	c.check(c.create(map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahSchema",
		"metadata": map[string]any{"namespace": c.in.TestNamespace, "name": configMapRefusedSchema},
		"spec": map[string]any{
			"target": c.refusedTarget(),
			"desired": map[string]any{"configMap": map[string]any{
				"name":  configMapRefusedSchemaSource,
				"items": []any{map[string]any{"key": "schema.sql", "path": "schema.sql"}},
			}},
			"interval": configMapInterval, "policy": map[string]any{"apply": "OnApproval"},
			"execution": map[string]any{"activeDeadlineSeconds": int64(600)},
		},
	}), "create PtahSchema %s", configMapRefusedSchema)
	first := c.waitForRefusedSchema("the table's plan", awaitingSchemaPlan(""))
	c.approveRefusedPlan("e2e-configmap-refused-v1", first)
	c.waitForRefusedSchema("the table to apply", inSyncSchema)

	// A run that succeeded keeps the five minutes, read before they can pass.
	applied := c.applyJobs(labelSchema, configMapRefusedSchema)
	if len(applied) != 1 || applied[0].Spec.TTLSecondsAfterFinished == nil ||
		*applied[0].Spec.TTLSecondsAfterFinished != jobclaim.CleanupTTLSeconds {
		c.fatalf("the Apply that succeeded left %+v, want one Job with the short cleanup TTL", applied)
	}
	succeeded := applied[0].Name

	_, err := databaseSQL(c.ctx, c.cluster, c.in.TestNamespace, c.engine, configMapRefusedDatabase,
		"INSERT INTO e2e_refused_orders (id, customer_id) VALUES (1, 42), (2, 42)")
	c.check(err, "give two rows the same customer")
	version := c.editSource(configMapRefusedSchemaSource, "schema.sql", configMapRefusedTable+"\n"+configMapRefusedIndex)
	second := c.waitForRefusedSchema("the index's plan", func(schema *ptahv1alpha1.PtahSchema) bool {
		return sourceReadAt(schema.Status.Source.ConfigMap, schema.Status.Source.Snapshot, version) && awaitingSchemaPlan("")(schema)
	})
	c.approveRefusedPlan("e2e-configmap-refused-v2", second)
	refused := c.waitForRefusedSchema("the Apply's outcome to be unknown", func(schema *ptahv1alpha1.PtahSchema) bool {
		applying := meta.FindStatusCondition(schema.Status.Conditions, ptahv1alpha1.ConditionApplying)
		return applying != nil && applying.Reason == string(ptahv1alpha1.ReasonOutcomeUnknown)
	})
	applying := meta.FindStatusCondition(refused.Status.Conditions, ptahv1alpha1.ConditionApplying)
	var job *batchv1.Job
	for _, candidate := range c.applyJobs(labelSchema, configMapRefusedSchema) {
		if candidate.Name != succeeded {
			job = candidate.DeepCopy()
		}
	}
	if job == nil {
		c.fatalf("the refused Apply left no Job of its own")
	}
	c.check(failedApplyPointerSays(applying.Message, c.in.TestNamespace, job.Name), "Applying")
	c.assertFailedApplyEvidence(job, refused.UID, refused.Status, "ApplyOutcomeUnknown")
	c.deleteRetainedJob(job)
	c.deleteAndWait(refused)
	c.logf("PASS a statement the database refused left its answer in the Pod log of %s, and said where", job.Name)
}

// refusedMigrationStatement is the same refusal on a migration's second file. A
// run that failed is read back, not retried, and the run's record names it.
func (c *configMapRun) refusedMigrationStatement() {
	c.t.Helper()
	data := map[string]string{}
	items := []any{}
	for _, file := range configMapRefusedMigrationFiles {
		data[file.key] = file.body
		items = append(items, map[string]any{"key": file.key, "path": file.path})
	}
	c.check(c.apply(c.sourceDocument(configMapRefusedMigrationSource, data)), "apply ConfigMap %s", configMapRefusedMigrationSource)
	c.check(c.create(map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahMigration",
		"metadata": map[string]any{"namespace": c.in.TestNamespace, "name": configMapRefusedMigration},
		"spec": map[string]any{
			"target": c.refusedTarget(), "artifact": map[string]any{"configMap": map[string]any{"name": configMapRefusedMigrationSource, "items": items}},
			"interval": configMapInterval, "policy": map[string]any{"apply": "OnApproval"},
			"execution": map[string]any{"activeDeadlineSeconds": int64(600)},
		},
	}), "create PtahMigration %s", configMapRefusedMigration)
	var migration *ptahv1alpha1.PtahMigration
	wait := func(what string, done func(*ptahv1alpha1.PtahMigration) bool) {
		c.t.Helper()
		c.wait(what, func() (bool, string) {
			read := &ptahv1alpha1.PtahMigration{}
			if err := c.get(configMapRefusedMigration, read); err != nil {
				return false, err.Error()
			}
			migration = read
			return done(read), fmt.Sprintf("%s, last run %+v", read.Status.Phase, read.Status.LastRun)
		})
	}
	wait("the migration plan", func(read *ptahv1alpha1.PtahMigration) bool {
		return read.Status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval && read.Status.Plan != nil && settledMigration(read)
	})
	plan := &ptahv1alpha1.PtahMigrationPlan{}
	c.check(c.get(migration.Status.Plan.Name, plan), "read plan %s", migration.Status.Plan.Name)
	c.check(c.create(migrationApprovalDocument(c.in.TestNamespace, "e2e-configmap-refused-migration", configMapRefusedMigration,
		string(migration.UID), plan.Name, string(plan.UID), plan.Spec.Fingerprint)), "approve plan %s", plan.Name)
	wait("the run to be recorded as failed", func(read *ptahv1alpha1.PtahMigration) bool {
		return read.Status.LastRun != nil && read.Status.LastRun.Outcome == ptahv1alpha1.MigrationRunOutcomeFailed
	})
	run := migration.Status.LastRun
	if run.Failure == nil || run.Failure.SQLState != refusedStatementSQLState {
		c.fatalf("the run recorded failure %+v, want SQLSTATE %s", run.Failure, refusedStatementSQLState)
	}
	c.check(failedApplyPointerSays(run.Message, c.in.TestNamespace, run.JobName), "status.lastRun.message")
	job := &batchv1.Job{}
	c.check(c.get(run.JobName, job), "read Job %s", run.JobName)
	c.assertFailedApplyEvidence(job, migration.UID, migration.Status, "MigrationRunFinished")
	c.deleteRetainedJob(job)
	read := &ptahv1alpha1.PtahMigration{}
	c.check(c.get(migration.Name, read), "read PtahMigration %s", migration.Name)
	if read.Status.LastRun == nil || read.Status.LastRun.Outcome != ptahv1alpha1.MigrationRunOutcomeFailed {
		c.fatalf("deleting the Job moved the record of the run: %+v", read.Status.LastRun)
	}
	c.deleteAndWait(migration)
	c.logf("PASS a migration statement the database refused left its answer in the Pod log of %s", job.Name)
}

// assertFailedApplyEvidence reads what stays where: the Job kept, the answer in
// its Pod's log without the statement, and neither in status or in an Event, whose
// message says where the log is.
// deleteRetainedJob does what the security page tells a person who has read the log:
// `kubectl delete job`, which takes the Pod with it, and with the Pod what the node
// keeps of its containers.
func (c *configMapRun) deleteRetainedJob(job *batchv1.Job) {
	c.t.Helper()
	_, stderr, err := c.cluster.Kubectl(c.ctx, "-n", c.in.TestNamespace, "delete", "job", job.Name)
	if err != nil {
		c.fatalf("kubectl delete job %s: %v: %s", job.Name, err, stderr)
	}
	c.wait("the Pod of Job "+job.Name+" to go with it", func() (bool, string) {
		pods := &corev1.PodList{}
		if err := c.cluster.Client.List(c.ctx, pods, client.InNamespace(c.in.TestNamespace)); err != nil {
			return false, err.Error()
		}
		owned := ownedPods(pods.Items, job.UID)
		return len(owned) == 0, fmt.Sprintf("%d Pods remain", len(owned))
	})
}

func (c *configMapRun) assertFailedApplyEvidence(job *batchv1.Job, owner types.UID, status any, eventReason string) {
	c.t.Helper()
	c.check(failedApplyJobRetained(job), "the failed Apply's Job")
	pods := &corev1.PodList{}
	c.check(c.cluster.Client.List(c.ctx, pods, client.InNamespace(c.in.TestNamespace)), "list the Pods of Job %s", job.Name)
	owned := ownedPods(pods.Items, job.UID)
	if len(owned) != 1 {
		c.fatalf("Job %s has %d Pods, want the one that ran", job.Name, len(owned))
	}
	log, err := c.cluster.ContainerLog(c.ctx, c.in.TestNamespace, owned[0].Name, "ptah")
	c.check(err, "read the log of Pod %s", owned[0].Name)
	c.check(failedApplyLogSays(log), "the log of Pod %s", owned[0].Name)
	encoded, err := json.Marshal(status)
	c.check(err, "encode the status")
	c.check(statusKeepsTheAnswerOut("the status", string(encoded)), "the status")
	c.wait("the "+eventReason+" Event to say where the log is", func() (bool, string) {
		events := &corev1.EventList{}
		if err := c.cluster.Client.List(c.ctx, events, client.InNamespace(c.in.TestNamespace)); err != nil {
			return false, err.Error()
		}
		examined := 0
		for index := range events.Items {
			event := &events.Items[index]
			if event.InvolvedObject.UID != owner {
				continue
			}
			examined++
			c.check(statusKeepsTheAnswerOut("the "+event.Reason+" Event", event.Message), "an Event of the resource")
			if event.Reason == eventReason && failedApplyPointerSays(event.Message, c.in.TestNamespace, job.Name) == nil {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d Events of the resource, none says where the log is", examined)
	})
}

// finish suspends both resources, so no read of theirs lands inside a later
// phase.
func (c *configMapRun) finish() {
	c.t.Helper()
	for _, object := range []client.Object{
		&ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: configMapSchema}},
		&ptahv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: configMapMigration}},
		&ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: reachabilitySchema}},
		&ptahv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: reachabilityMigration}},
	} {
		patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"suspend":true}}`))
		c.check(c.cluster.Client.Patch(c.ctx, object, patch, client.FieldOwner(harness.FieldOwner)), "suspend %s", object.GetName())
	}
}

// cleanup prints what the phase's resources decided when it failed. The statuses
// carry no SQL and no credential by contract.
func (c *configMapRun) cleanup() {
	if c.cluster == nil || !c.parent.Failed() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, name := range []string{configMapSchema, reachabilitySchema} {
		schema := &ptahv1alpha1.PtahSchema{}
		if c.cluster.Client.Get(ctx, c.key(name), schema) == nil {
			_, _ = fmt.Fprintf(os.Stderr, "e2e ConfigMap sources: schema %s status %+v\n", name, schema.Status)
		}
	}
	for _, name := range []string{configMapMigration, reachabilityMigration} {
		migration := &ptahv1alpha1.PtahMigration{}
		if c.cluster.Client.Get(ctx, c.key(name), migration) == nil {
			_, _ = fmt.Fprintf(os.Stderr, "e2e ConfigMap sources: migration %s status %+v\n", name, migration.Status)
		}
	}
}
