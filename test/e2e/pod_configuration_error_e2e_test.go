//go:build e2e

package e2e

import (
	"net/url"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// podConfigurationNamedOnResource holds what a reader meets while a Secret has
// not arrived, which only a kubelet can produce (#821). The database URL is a
// file in a Secret volume (#843), so the kubelet cannot set the volume up and
// says why only in a FailedMount Event against the Pod. The resource names the
// missing Secret and then the missing key, in the kubelet's words, and the
// volume and Secret the key was looked for in. The Secret in between holds the
// URL and its password under another key, and neither reaches the resource,
// an Event or the Pod. Adding the key lets the same Job finish, with no action
// on the resource.
//
// The Job's deadline is far off, so the row ends with a Job that completed, which
// the Job audit can read: a Job the deadline stopped has no Pod left to audit.
// The read is followed by a plan, and the row waits for it to be stored, so no
// Job of the schema is running when the schema is deleted.
//
// The same hold then gives the OCI guard's refusal its row
// (ociAccessRefusedAtFetch), from the artifact this row published.
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
	digest := d.publishSchema("pod-configuration-postgresql", "v1", "postgres", reference, "")
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
	d.waitForSchemaEvent(name, "PodConfigurationError", missingSecret, func(events []corev1.Event) bool {
		return podConfigurationEventRecorded(events, stopped, missingSecret)
	})

	// The URL and its password, under a key the Job does not ask for.
	d.mustCreate(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"namespace": d.in.TestNamespace, "name": secret},
		"data":     secretData(map[string]string{"database": podConfigurationPGDatabase, "not-the-url": target}),
	})
	kubeletWords := `MountVolume.SetUp failed for volume "database-url" : references non-existent secret key: url`
	missingKey := kubeletWords + " (volume database-url reads Secret " + d.in.TestNamespace + "/" + secret + ")"
	keyless := d.waitForSchema(name, "the schema to name the key the Secret lacks", func(schema *ptahv1alpha1.PtahSchema) bool {
		return podConfigurationReported(schema, missingKey)
	})
	if keyless.Status.ActiveOperation.JobName != job {
		d.fatalf("%s moved from Job %s to %s before its Secret had the key", name, job, keyless.Status.ActiveOperation.JobName)
	}
	pods := &corev1.PodList{}
	d.mustList(pods, client.MatchingLabels{"job-name": job})
	mounts := &corev1.EventList{}
	d.mustList(mounts, client.MatchingFields{"involvedObject.kind": "Pod", "reason": "FailedMount"})
	if !kubeletReportedMountFailure(mounts.Items, pods.Items, job, kubeletWords) {
		d.fatalf("the kubelet recorded no FailedMount Event against a Pod of Job %s in the words the resource carries", job)
	}
	events := d.waitForSchemaEvent(name, "PodConfigurationError", missingKey, func(events []corev1.Event) bool {
		return podConfigurationEventRecorded(events, keyless, missingKey)
	})
	document := append(append(append(d.jsonBytes(keyless), d.jsonBytes(events)...), d.jsonBytes(pods)...), d.jsonBytes(mounts)...)
	d.check(holdsNoSecretValue(document, target, password),
		"read the resource, its Warnings, Job %s's Pods and the kubelet's mount Events for what the Secret holds", job)
	d.scan(document, "the resource, its Warnings, the Pods of a Job whose Secret lacks the key and their mount Events")

	object := &corev1.Secret{}
	object.Namespace, object.Name = d.in.TestNamespace, secret
	d.check(d.mergePatch(object, map[string]any{"data": secretData(map[string]string{"url": target})}), "add the key to Secret %s", secret)
	resumed := d.waitForSchema(name, "the read to finish and plan with nothing touched but the Secret", func(schema *ptahv1alpha1.PtahSchema) bool {
		return schema.Status.Target.LastObservedAt != nil && !podConfigurationReported(schema, "") &&
			awaitingApprovalOf(schema, digest)
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
	d.ociAccessRefusedAtFetch(reference)
}

// ociAccessRefusedAtFetch is the OCI row of what the Git and the HTTPS source
// phases prove for their own fetches (refusedAtFetch): the registry
// credential's owner grants it for another registry after a Resolve was
// allowed it, so the validate-source-authority step of the Observe that
// follows refuses it before the fetch that holds the credential, and the
// schema says so in the words the Resolve used for the same Secret.
//
// One Secret serves both readings. It grants another registry first, and a
// schema whose Resolve refuses it gives the words. Then it grants the registry
// the artifact is in, and a second schema resolves, verifies and dispatches an
// Observe whose Pod the kubelet holds for a database Secret that does not exist
// yet. The grant moves back while the Pod waits, the database Secret is
// created, and the guard reads the grant when it starts. The database the URL
// names is never created, so a guard that let the fetch run fails the wait on
// the Observe's own failure, and the Job audit reads the guard's log for the
// registry password before the schema is deleted.
func (d *dataPlane) ociAccessRefusedAtFetch(reference string) {
	d.t.Helper()
	const resolveName, fetchName = "e2e-oci-resolve-refused", "e2e-oci-fetch-refused"
	const credential, secret = "e2e-registry-auth-fetch-refused", "e2e-postgresql-oci-fetch-refused-db"
	const coordinationKey, elsewhere = "e2e/oci-fetch-refused/postgresql", "registry.example.com:5000"
	namespace := d.in.TestNamespace
	grant := func(registry string) map[string]any {
		return secretData(map[string]string{
			"username": d.registry.Username, "password": d.registry.Password, "registry": registry, "allowPlainHTTP": "true",
		})
	}
	d.mustCreate(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"namespace": namespace, "name": credential},
		"data":     grant(elsewhere),
	})
	resource := schemaResource{
		engine: "PostgreSQL", secret: secret, reference: reference, coordinationKey: coordinationKey,
		registryAuthSecret: credential, failureRetry: "30m", interval: "30m", activeDeadline: 900,
	}

	resource.name = resolveName
	d.createSchemaResource(resource)
	refusedAtResolve := d.waitForSchema(resolveName, "the Resolve to refuse a registry credential granted for another registry",
		failedBeforeResolveChild)
	resolveRefusal := meta.FindStatusCondition(refusedAtResolve.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed).Message
	d.scan([]byte(resolveRefusal), "the Resolve's refusal of a registry credential granted for another registry")
	if !strings.Contains(resolveRefusal, "registry credential is not granted for the OCI source authority") {
		d.fatalf("%s was refused for something else than its grant: %s", resolveName, resolveRefusal)
	}
	d.deleteAndWait(resolveName, 180*time.Second)

	access := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: credential}}
	d.check(d.mergePatch(access, map[string]any{"data": grant(d.registryHost)}), "grant Secret %s for the registry", credential)
	resource.name = fetchName
	d.createSchemaResource(resource)
	missing := `secret "` + secret + `" not found`
	held := d.waitForSchema(fetchName, "the schema to resolve and its Observe to wait for the database Secret",
		func(schema *ptahv1alpha1.PtahSchema) bool { return heldBeforeFetch(schema, missing) == nil })
	d.check(d.mergePatch(access, map[string]any{"data": grant(elsewhere)}), "grant Secret %s for another registry", credential)
	target, err := url.Parse(d.credentials.pgURL)
	d.check(err, "parse the data plane's database URL")
	target.Path = "/ptah_e2e_oci_fetch_refused"
	d.mustCreate(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"namespace": namespace, "name": secret},
		"data":     secretData(map[string]string{"url": target.String()}),
	})

	refused := d.waitForSchema(fetchName, "the Observe's guard to refuse the grant in the Resolve's words",
		func(schema *ptahv1alpha1.PtahSchema) bool {
			return refusedAtFetch(schema, held.Generation, sourceGuardStep, resolveRefusal) == nil
		})
	d.printReading("oci fetch-refused schema", map[string]any{"status": refused.Status})
	d.scan(d.jsonBytes(refused), "the schema whose Observe guard refused its registry credential")
	if refused.Status.Plan != nil {
		d.fatalf("%s planned %+v although its guard refused the source", fetchName, refused.Status.Plan)
	}
	message := meta.FindStatusCondition(refused.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed).Message
	d.deleteAndWait(fetchName, 180*time.Second)
	for _, name := range []string{credential, secret} {
		object := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
		d.check(d.cluster.Client.Delete(d.ctx, object), "delete Secret %s", name)
	}
	d.logf("PASS a registry credential granted for another registry after its Resolve was refused by the Observe's guard: %s", message)
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
