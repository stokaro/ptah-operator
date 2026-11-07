//go:build e2e

package e2e

import (
	"net/url"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// podConfigurationNamedOnResource holds what a reader meets while a Secret has
// not arrived, which only a kubelet can produce (#821). The resource names the
// missing Secret and then the missing key, in the kubelet's words. The Secret in
// between holds the URL and its password under another key, and neither reaches
// the resource, an Event or the Pod. Adding the key lets the same Job finish,
// with no action on the resource.
//
// The Job's deadline is far off, so the row ends with a Job that completed, which
// the Job audit can read: a Job the deadline stopped has no Pod left to audit.
func (d *dataPlane) podConfigurationNamedOnResource() {
	d.t.Helper()
	const name, coordinationKey = "e2e-pod-configuration", "e2e/pod-configuration/postgresql"
	const secret = podConfigurationPGSecret
	target := d.credentials.podConfigurationPGURL
	parsed, err := url.Parse(target)
	d.check(err, "parse the row's database URL")
	password, _ := parsed.User.Password()

	count, err := d.psqlDefault("SELECT count(*) FROM pg_database WHERE datname='" + podConfigurationPGDatabase + "'")
	d.check(err, "look PostgreSQL database %s up", podConfigurationPGDatabase)
	if removeWhitespace(count) != "0" {
		d.fatalf("PostgreSQL database %s already exists, and the row needs one that holds nothing", podConfigurationPGDatabase)
	}
	_, err = d.psqlDefault("CREATE DATABASE " + podConfigurationPGDatabase)
	d.check(err, "create PostgreSQL database %s", podConfigurationPGDatabase)

	reference := d.registryReference("pod-configuration-postgresql")
	d.publishSchema("pod-configuration-postgresql", "v1", "postgres", reference, "")
	d.createSchemaResource(schemaResource{
		name: name, engine: "PostgreSQL", secret: secret, reference: reference,
		coordinationKey: coordinationKey, activeDeadline: 900,
	})
	missingSecret := `secret "` + secret + `" not found`
	stopped := d.waitForSchema(name, "the schema to name the Secret that does not exist", func(schema *ptahv1alpha1.PtahSchema) bool {
		return podConfigurationReported(schema, missingSecret)
	})
	job := stopped.Status.ActiveOperation.JobName
	if !readyConditionNamesJob(stopped, job) {
		d.fatalf("%s does not name Job %s in its Ready condition", name, job)
	}
	warnings := func() *corev1.EventList {
		d.t.Helper()
		events := &corev1.EventList{}
		d.mustList(events, client.MatchingFields{"involvedObject.name": name, "reason": "PodConfigurationError"})
		return events
	}
	if !podConfigurationEventRecorded(warnings().Items, stopped, missingSecret) {
		d.fatalf("%s carries no PodConfigurationError Warning saying %s", name, missingSecret)
	}

	// The URL and its password, under a key the Job does not ask for.
	d.mustCreate(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"namespace": d.in.TestNamespace, "name": secret},
		"data":     secretData(map[string]string{"database": podConfigurationPGDatabase, "not-the-url": target}),
	})
	missingKey := "couldn't find key url in Secret " + d.in.TestNamespace + "/" + secret
	keyless := d.waitForSchema(name, "the schema to name the key the Secret lacks", func(schema *ptahv1alpha1.PtahSchema) bool {
		return podConfigurationReported(schema, missingKey)
	})
	if keyless.Status.ActiveOperation.JobName != job {
		d.fatalf("%s moved from Job %s to %s before its Secret had the key", name, job, keyless.Status.ActiveOperation.JobName)
	}
	pods := &corev1.PodList{}
	d.mustList(pods, client.MatchingLabels{"job-name": job})
	if !kubeletReportedMissingKey(pods.Items, job, missingKey) {
		d.fatalf("no Pod of Job %s waits in the words the resource carries", job)
	}
	events := warnings()
	if !podConfigurationEventRecorded(events.Items, keyless, missingKey) {
		d.fatalf("%s carries no PodConfigurationError Warning saying %s", name, missingKey)
	}
	document := append(append(d.jsonBytes(keyless), d.jsonBytes(events)...), d.jsonBytes(pods)...)
	d.check(holdsNoSecretValue(document, target, password), "read the resource, its Warnings and Job %s's Pods for what the Secret holds", job)
	d.scan(document, "the resource, its Warnings and the Pods of a Job whose Secret lacks the key")

	object := &corev1.Secret{}
	object.Namespace, object.Name = d.in.TestNamespace, secret
	d.check(d.mergePatch(object, map[string]any{"data": secretData(map[string]string{"url": target})}), "add the key to Secret %s", secret)
	resumed := d.waitForSchema(name, "the read to finish and plan with nothing touched but the Secret", func(schema *ptahv1alpha1.PtahSchema) bool {
		return schema.Status.Target.LastObservedAt != nil && !podConfigurationReported(schema, "")
	})
	finished := &batchv1.Job{}
	d.mustGet(job, finished)
	if finished.Status.Succeeded != 1 {
		d.fatalf("Job %s did not finish once its Secret had the key: %+v", job, finished.Status)
	}
	if resumed.Status.ObservedGeneration != stopped.Status.ObservedGeneration {
		d.fatalf("%s moved generation while nothing but the Secret changed", name)
	}
	d.deleteAndWait(name, 180*time.Second)
	d.logf("PASS a missing Secret and a missing key are named on the resource in the kubelet's words, and neither reading holds what the Secret holds")
}

// podConfigurationPastDeadline holds what a reader meets once a missing Secret
// has outlasted the Job: the kubelet's words must survive the Job's deadline,
// which deletes the Pod that held them before the Job turns terminal. Only a
// cluster has that gap. It runs here and not beside the row above because the
// schema phases audit every terminal Job's Pod, and this Job has none left.
func (m *migrationRun) podConfigurationPastDeadline() {
	m.t.Helper()
	name := "e2e-pod-configuration-" + m.engine.name
	secret := name + "-absent"
	words := `secret "` + secret + `" not found`
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: m.reference(""),
		coordinationKey: "e2e/pod-configuration/" + m.engine.name,
		apply:           "OnApproval", interval: "1h",
		execution: map[string]any{"activeDeadlineSeconds": int64(60), "failureRetryInterval": "1h", "connectTimeout": "30s"},
	}))
	m.waitForMigration(name, "the migration to name the Secret that does not exist", time.Second, func(resource *ptahv1alpha1.PtahMigration) bool {
		return migrationPodConfigurationReported(resource, words)
	})
	failed := m.waitForMigration(name, "the failure past the Job's deadline to name the Secret as well", time.Second, func(resource *ptahv1alpha1.PtahMigration) bool {
		return migrationFailureNamesUnstartedPod(resource, words)
	})
	m.scanObject(failed, name+" past its Job's deadline")
	m.deleteAndWait(failed, name+" past its Job's deadline")
	m.logf("PASS %s: a Secret missing past the Job's deadline is still named on the resource", m.engine.name)
}
