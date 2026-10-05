//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Prepare the native inputs alerting consumes without running the migration
// and reference-data acceptance suites first. Those suites retain all of their
// cases. These producers stop at approval and never receive permission to Apply.
func (a *alertingRun) nativeProducers() {
	engine, err := migrationEngineFor("postgresql")
	a.check(err, "select the prepared producer database")
	registry := a.in.RegistryService + "." + a.in.TestNamespace + ".svc.cluster.local:5000"
	m := &migrationRun{
		t: a.t, parent: a.t, ctx: a.ctx, cluster: a.cluster, engine: engine,
		workDir: a.workDir, registryHost: registry, repository: "e2e-alert-producers",
		in: phases.MigrationsInputs{TestNamespace: a.in.TestNamespace, ExecutorImage: a.in.ExecutorImage},
	}
	credential := &corev1.Secret{}
	a.check(m.get(engine.sourceSecret, credential), "read the prepared producer credential")
	m.password = string(credential.Data["password"])
	if m.password == "" {
		a.fatalf("native producers have no database credential")
	}
	m.protect(m.password, a.credentials.Password)
	m.isolatedDatabase("ptah_alert_producer_migration", alMigrationProducer+"-db")
	schemaURL := m.isolatedDatabase("ptah_alert_producer_schema", alSchemaProducer+"-db")
	m.migrationPolicy()
	migrationDigest := m.publish("alerts-producer", m.fixtureDir(""), m.reference(""))

	r := &referenceRun{
		t: a.t, parent: a.t, ctx: a.ctx, cluster: a.cluster, engine: engine,
		workDir: a.workDir, scanner: m.scanner, password: m.password, url: schemaURL,
		in: phases.ReferenceDataInputs{TestNamespace: a.in.TestNamespace, ExecutorImage: a.in.ExecutorImage},
	}
	r.names = referenceNamesFor("postgresql", registry, "e2e-alert-producers")
	r.names.schema, r.names.secret = alSchemaProducer, alSchemaProducer+"-db"
	r.names.coordinationKey = "e2e/alert-producer/schema"
	r.names.configMapPrefix, r.names.jobPrefix = alSchemaProducer+"-", "e2e-push-alert-producer-schema-"
	r.declaredRowValues()
	schemaDigest := r.publish("v1")
	schemaDocument := referenceSchemaDocument(a.in.TestNamespace, engine.kind, r.names)
	schemaDocument["spec"].(map[string]any)["interval"] = alNegativeInterval.String()
	r.check(r.create(schemaDocument), "create the native schema producer")
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: alMigrationProducer, secret: alMigrationProducer + "-db", reference: m.reference(""),
		coordinationKey: "e2e/alert-producer/migration", apply: "OnApproval", interval: alNegativeInterval.String(),
	}))
	for _, producer := range []struct {
		name, digest string
		object       client.Object
	}{
		{alSchemaProducer, schemaDigest, &ptahv1.PtahSchema{}},
		{alMigrationProducer, migrationDigest, &ptahv1.PtahMigration{}},
	} {
		key := types.NamespacedName{Namespace: a.in.TestNamespace, Name: producer.name}
		a.check(a.cluster.Client.Get(a.ctx, key, producer.object), "retain the producer identity")
		uid := producer.object.GetUID()
		a.check(harness.Wait(a.ctx, "the native "+producer.name+" approval gate", alTimeout, time.Second,
			func(ctx context.Context) (bool, string, error) {
				if err := a.cluster.Client.Get(ctx, key, producer.object); err != nil {
					return false, "", err
				}
				if producer.object.GetUID() != uid {
					return false, "", fmt.Errorf("native producer %s was replaced", producer.name)
				}
				return alProducerReady(producer.object, producer.digest, a.in.ExecutorImage),
					"waiting for the exact artifact, executor, observed database, and unapproved plan", nil
			}), "prepare %s", producer.name)
	}
	// Keep the verified specs and execution bindings available to every case,
	// while preventing periodic reads from moving plan pins or emitting alerts.
	r.finishFixture()
	m.finishFixture(alMigrationProducer)
	a.logf("native schema and migration producers reached their exact approval gates and are suspended; neither received approval")
}
