//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/httpsfixture"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// TestHTTPSSources is the https-sources phase.
func TestHTTPSSources(t *testing.T) {
	run, inputs := harness.Begin(t, phases.HTTPSSources)
	c := newHTTPSRun(t, run, inputs)
	for _, scenario := range []struct {
		name string
		body func()
	}{
		{"https-schema", c.schemaFromAFile},
		{"https-migration", c.migrationFromAnArchive},
		{"https-schema-zip", c.schemaFromAZip},
		{"https-republished", c.republishedZip},
		{"https-redirect-refused", c.redirectToAnotherOrigin},
		{"https-credential-refused", c.credentialForAnotherHost},
		{"https-source-refused", c.refusedArchives},
		{"https-source-policy", c.sourcePolicy},
	} {
		if !run.Scenario(scenario.name, c.scenario(scenario.body)) {
			return
		}
	}
	c.finish()
	run.Logf("e2e HTTPS sources: PASS a schema and a migration sequence from HTTPS downloads pinned by digest")
}

// httpsRun is what the HTTPS source proofs share. Each scenario runs as a
// subtest, and t is that subtest while it runs.
type httpsRun struct {
	t      *testing.T
	parent *testing.T
	ctx    context.Context
	in     phases.HTTPSSourceInputs

	cluster  *harness.Cluster
	engine   migrationEngine
	password string
	// scanner looks for the database password and the server's token in what
	// a failed wait prints.
	scanner credentialScanner

	host, other string
	keys        gitServerKeys
	files       map[string][]byte
	digests     map[string]string
}

func newHTTPSRun(t *testing.T, run *harness.Run, in phases.HTTPSSourceInputs) *httpsRun {
	t.Helper()
	c := &httpsRun{t: t, parent: t, ctx: run.Context(), in: in}
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
	if c.files, c.digests, err = httpsDownloads(); err != nil {
		c.fatalf("build the downloads: %v", err)
	}
	c.host = httpsServer + "." + in.TestNamespace + ".svc"
	c.other = httpsOtherServer + "." + in.TestNamespace + ".svc"
	c.standUp()
	if c.scanner, err = newCredentialScanner(c.password, c.keys.Token); err != nil {
		c.fatalf("build the credential scanner: %v", err)
	}
	return c
}

func (c *httpsRun) scenario(body func()) func(*testing.T) {
	return func(t *testing.T) {
		c.t = t
		defer func() { c.t = c.parent }()
		body()
	}
}

func (c *httpsRun) fatalf(format string, arguments ...any) {
	c.t.Helper()
	c.t.Fatalf("e2e HTTPS sources: "+format, arguments...)
}

func (c *httpsRun) logf(format string, arguments ...any) {
	c.t.Helper()
	c.t.Logf("e2e HTTPS sources: "+format, arguments...)
}

func (c *httpsRun) check(err error, format string, arguments ...any) {
	c.t.Helper()
	if err != nil {
		c.fatalf("%s: %v", fmt.Sprintf(format, arguments...), err)
	}
}

func (c *httpsRun) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: c.in.TestNamespace, Name: name}
}

func (c *httpsRun) meta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: name}
}

func (c *httpsRun) get(name string, object client.Object) error {
	return c.cluster.Client.Get(c.ctx, c.key(name), object)
}

func (c *httpsRun) create(document map[string]any) error {
	return c.cluster.Client.Create(c.ctx, &unstructured.Unstructured{Object: document},
		client.FieldOwner(harness.FieldOwner), client.FieldValidation("Strict"))
}

// put creates object, or replaces what an earlier run of the phase left.
func (c *httpsRun) put(object client.Object) {
	c.t.Helper()
	err := c.cluster.Client.Create(c.ctx, object, client.FieldOwner(harness.FieldOwner))
	if apierrors.IsAlreadyExists(err) {
		current := object.DeepCopyObject().(client.Object)
		c.check(c.cluster.Client.Get(c.ctx, client.ObjectKeyFromObject(object), current), "read %s", object.GetName())
		object.SetResourceVersion(current.GetResourceVersion())
		err = c.cluster.Client.Update(c.ctx, object, client.FieldOwner(harness.FieldOwner))
	}
	c.check(err, "store %s", object.GetName())
}

func (c *httpsRun) wait(what string, check func() (bool, string)) {
	c.t.Helper()
	err := harness.Wait(c.ctx, what, waitTimeout, 3*time.Second, func(context.Context) (bool, string, error) {
		done, observed := check()
		return done, observed, nil
	})
	c.check(err, "wait for %s", what)
}

// url is a download of the server: file, reached through access.
func (c *httpsRun) url(access, file string) string {
	return "https://" + c.host + "/" + access + "/" + file
}

// standUp generates the server's certificate and token and runs it from the
// fixture image under two Service names, with the credential Secrets and the
// trust ConfigMap the resources select. It waits until the server reports
// the digest of the zip the republished row replaces, so every row reads the
// bytes httpsfixture builds.
func (c *httpsRun) standUp() {
	c.t.Helper()
	var err error
	c.keys, err = newGitServerKeys(c.host, c.host, c.other, c.host+".cluster.local", c.other+".cluster.local")
	c.check(err, "generate the download server's keys")
	served := map[string][]byte{}
	for name, data := range c.files {
		if name != httpsfixture.SchemaArchiveRepublished {
			served[name] = data
		}
	}
	c.put(&corev1.ConfigMap{ObjectMeta: c.meta(httpsFiles), BinaryData: served})
	c.put(&corev1.Secret{ObjectMeta: c.meta(httpsServerSecret), Data: map[string][]byte{
		"tls.crt": c.keys.CertificatePEM, "tls.key": c.keys.KeyPEM, "token": []byte(c.keys.Token),
	}})
	c.put(&corev1.ConfigMap{ObjectMeta: c.meta(httpsCAConfigMap), Data: map[string]string{"ca.pem": string(c.keys.CAPEM)}})
	// The Secrets' owner grants each credential one host and the exact CA it
	// may be sent under. The last grants the token for another host.
	ca := []byte(grantDigest(c.keys.CAPEM))
	c.put(&corev1.Secret{ObjectMeta: c.meta(httpsBasicCredential), Data: map[string][]byte{
		"host": []byte(c.host), "username": []byte(httpsUser), "password": []byte(c.keys.Token), "caSHA256": ca,
	}})
	c.put(&corev1.Secret{ObjectMeta: c.meta(httpsBearerCredential), Data: map[string][]byte{
		"host": []byte(c.host), "token": []byte(c.keys.Token), "caSHA256": ca,
	}})
	c.put(&corev1.Secret{ObjectMeta: c.meta(httpsElsewhereGrant), Data: map[string][]byte{
		"host": []byte(httpsElsewhereHost), "token": []byte(c.keys.Token), "caSHA256": ca,
	}})

	labels := map[string]string{"app.kubernetes.io/name": httpsServer}
	mode := int32(0o440)
	c.put(&appsv1.Deployment{ObjectMeta: c.meta(httpsServer), Spec: appsv1.DeploymentSpec{
		Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
			AutomountServiceAccountToken: ptr.To(false), EnableServiceLinks: ptr.To(false),
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: registryPullSecret}},
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532)), FSGroup: ptr.To(int64(65532)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name: "https", Image: c.in.FixtureImage, ImagePullPolicy: corev1.PullIfNotPresent,
				Command: []string{"/e2e-https-server"},
				Args: []string{
					"-root", "/files", "-listen", ":8443", "-tls-certificate", "/keys/tls.crt", "-tls-key", "/keys/tls.key",
					"-basic-user", httpsUser, "-basic-password-file", "/keys/token", "-token-file", "/keys/token",
					"-other-origin", "https://" + c.other,
				},
				Ports:          []corev1.ContainerPort{{Name: "https", ContainerPort: 8443}},
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8443)}}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
					Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "files", MountPath: "/files", ReadOnly: true}, {Name: "keys", MountPath: "/keys", ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{
				{Name: "files", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: httpsFiles}, DefaultMode: &mode,
				}}},
				{Name: "keys", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: httpsServerSecret, DefaultMode: &mode}}},
			},
		}},
	}})
	for _, name := range []string{httpsServer, httpsOtherServer} {
		c.put(&corev1.Service{ObjectMeta: c.meta(name), Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Name: "https", Port: 443, TargetPort: intstr.FromString("https")}},
		}})
	}
	if err := c.cluster.WaitForRollout(c.ctx, c.in.TestNamespace, httpsServer, waitTimeout); err != nil {
		c.fatalf("the download server did not become ready: %v", err)
	}
	c.waitServing(httpsfixture.SchemaArchive, c.digests[httpsfixture.SchemaArchive])
	c.logf("the download server serves https://%s and, as another origin, https://%s", c.host, c.other)
}

// waitServing waits until the server has logged digest as the bytes of file.
func (c *httpsRun) waitServing(file, digest string) {
	c.t.Helper()
	c.wait("the download server to serve "+file+" as "+digest, func() (bool, string) {
		logs, err := deploymentLogs(c.ctx, c.cluster, c.in.TestNamespace, httpsServer)
		if err != nil {
			return false, err.Error()
		}
		return httpsServing(logs, file, digest), "the server has not logged that digest yet"
	})
}

// requests reads every request the server logged.
func (c *httpsRun) requests() []httpsServed {
	c.t.Helper()
	logs, err := deploymentLogs(c.ctx, c.cluster, c.in.TestNamespace, httpsServer)
	c.check(err, "read the download server's log")
	return httpsServerRequests(logs)
}

// httpsSourceDocument is what one resource of the phase reads: a download,
// its digest, format and selected path, the credential if any, and the
// source policy if any. Every source trusts the phase's CA.
type httpsSourceDocument struct {
	url, digest, path, credential, policy string
	format                                ptahv1alpha1.HTTPSFormat
}

func (source httpsSourceDocument) spec() map[string]any {
	https := map[string]any{
		"url": source.url, "digest": source.digest, "format": string(source.format), "path": source.path,
		"transport": map[string]any{"caFrom": map[string]any{"name": httpsCAConfigMap, "key": "ca.pem"}},
	}
	if source.credential != "" {
		https["authFrom"] = map[string]any{"name": source.credential}
	}
	spec := map[string]any{"https": https}
	if source.policy != "" {
		spec["sourcePolicyFrom"] = map[string]any{"name": source.policy, "key": "policy.yaml"}
	}
	return spec
}

func (c *httpsRun) target(secret, key string) map[string]any {
	return map[string]any{
		"engine": "PostgreSQL", "coordinationKey": "e2e/" + key,
		"urlFrom": map[string]any{"name": secret, "key": "url"},
	}
}

func (c *httpsRun) schemaDocument(name, secret, key string, source httpsSourceDocument) map[string]any {
	return map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahSchema",
		"metadata": map[string]any{"namespace": c.in.TestNamespace, "name": name},
		"spec": map[string]any{
			"target": c.target(secret, key), "desired": source.spec(), "interval": httpsInterval,
			"policy":    map[string]any{"apply": "OnApproval"},
			"execution": map[string]any{"activeDeadlineSeconds": int64(600)},
		},
	}
}

func (c *httpsRun) migrationDocument(name, secret, key string, source httpsSourceDocument) map[string]any {
	return map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahMigration",
		"metadata": map[string]any{"namespace": c.in.TestNamespace, "name": name},
		"spec": map[string]any{
			"target": c.target(secret, key), "artifact": source.spec(), "interval": httpsInterval,
			"policy":    map[string]any{"apply": "OnApproval"},
			"execution": map[string]any{"activeDeadlineSeconds": int64(600)},
		},
	}
}

func (c *httpsRun) schemaNamed(name string) *ptahv1alpha1.PtahSchema {
	c.t.Helper()
	schema := &ptahv1alpha1.PtahSchema{}
	c.check(c.get(name, schema), "read PtahSchema %s", name)
	return schema
}

// waitForSchema waits for done, and fails at once if the schema fails at or
// after generation: a failure the phase caused before it is not one.
func (c *httpsRun) waitForSchema(name string, generation int64, what string, done func(*ptahv1alpha1.PtahSchema) bool) *ptahv1alpha1.PtahSchema {
	c.t.Helper()
	var matched *ptahv1alpha1.PtahSchema
	c.wait(what, func() (bool, string) {
		schema := &ptahv1alpha1.PtahSchema{}
		if err := c.get(name, schema); err != nil {
			return false, err.Error()
		}
		if schema.Status.Phase == ptahv1alpha1.PhaseFailed && schema.Status.ObservedGeneration >= generation {
			c.fatalf("%s failed while waiting for %s; %s", name, what, failedConditions(c.scanner, schema.Status.Conditions))
		}
		if done(schema) {
			matched = schema
			return true, ""
		}
		return false, string(schema.Status.Phase)
	})
	return matched
}

func (c *httpsRun) waitForMigration(name, what string, done func(*ptahv1alpha1.PtahMigration) bool) *ptahv1alpha1.PtahMigration {
	c.t.Helper()
	var matched *ptahv1alpha1.PtahMigration
	c.wait(what, func() (bool, string) {
		migration := &ptahv1alpha1.PtahMigration{}
		if err := c.get(name, migration); err != nil {
			return false, err.Error()
		}
		if migration.Status.Phase == ptahv1alpha1.MigrationPhaseFailed {
			c.fatalf("%s failed while waiting for %s; %s", name, what, failedConditions(c.scanner, migration.Status.Conditions))
		}
		if done(migration) {
			matched = migration
			return true, ""
		}
		return false, string(migration.Status.Phase)
	})
	return matched
}

func awaitingHTTPSPlan(schema *ptahv1alpha1.PtahSchema) bool {
	return schema.Status.Phase == ptahv1alpha1.PhaseAwaitingApproval && schema.Status.Plan != nil && settledSchema(schema)
}

func (c *httpsRun) deleteAndWait(object client.Object) {
	c.t.Helper()
	if err := c.cluster.Client.Delete(c.ctx, object, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil &&
		!apierrors.IsNotFound(err) {
		c.fatalf("%s was not removed: %v", object.GetName(), err)
	}
	c.wait(object.GetName()+" to be removed", func() (bool, string) {
		err := c.cluster.Client.Get(c.ctx, client.ObjectKeyFromObject(object), object.DeepCopyObject().(client.Object))
		return apierrors.IsNotFound(err), "still present"
	})
}

// reset removes what an earlier run of this phase left, by the names this
// phase alone uses, and its databases.
func (c *httpsRun) reset() {
	c.t.Helper()
	var objects []client.Object
	for _, name := range []string{httpsMigration, httpsSymlinkMigration, httpsDotDotMigration} {
		objects = append(objects, &ptahv1alpha1.PtahMigration{ObjectMeta: c.meta(name)})
	}
	for _, name := range []string{
		httpsSchema, httpsZipSchema, httpsRedirectSchema, httpsElsewhereSchema, httpsHostPolicySchema, httpsSizePolicySchema,
	} {
		objects = append(objects, &ptahv1alpha1.PtahSchema{ObjectMeta: c.meta(name)})
	}
	for _, name := range []string{"e2e-https-schema-v1", "e2e-https-zip-v1"} {
		objects = append(objects, &ptahv1alpha1.PtahSchemaApproval{ObjectMeta: c.meta(name)})
	}
	objects = append(objects, &ptahv1alpha1.PtahMigrationApproval{ObjectMeta: c.meta("e2e-https-migration-v1")})
	for _, name := range []string{httpsHostPolicy, httpsSizePolicy} {
		objects = append(objects, &corev1.ConfigMap{ObjectMeta: c.meta(name)})
	}
	for _, object := range objects {
		c.deleteAndWait(object)
	}
	for _, database := range []string{httpsSchemaDatabase, httpsZipDatabase, httpsMigrationDatabase} {
		_, err := serverSQL(c.ctx, c.cluster, c.in.TestNamespace, c.engine, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)")
		c.check(err, "drop database %s", database)
	}
}

func (c *httpsRun) createDatabase(database, secret string) {
	c.t.Helper()
	_, err := serverSQL(c.ctx, c.cluster, c.in.TestNamespace, c.engine, "CREATE DATABASE "+database)
	c.check(err, "create database %s", database)
	c.put(&corev1.Secret{ObjectMeta: c.meta(secret), Data: map[string][]byte{
		"url": []byte(c.engine.databaseURL(c.in.TestNamespace, migrationDatabaseUser, c.password, database)),
	}})
}

func (c *httpsRun) query(database, statement string) string {
	output, _ := databaseSQL(c.ctx, c.cluster, c.in.TestNamespace, c.engine, database, statement)
	return trimmedSQL(output)
}

func (c *httpsRun) jobsOf(label, name string) []batchv1.Job {
	c.t.Helper()
	jobs := &batchv1.JobList{}
	c.check(c.cluster.Client.List(c.ctx, jobs, client.InNamespace(c.in.TestNamespace), client.MatchingLabels{label: name}),
		"list the Jobs of %s", name)
	return jobs.Items
}

// assertHTTPSJobs holds every operation Job of a resource that still exists
// to its HTTPS shape and refuses a Verify Job: an HTTPS source has no
// verification policy. It needs a Job of each operation named in want, so a
// list that came back empty proves nothing.
func (c *httpsRun) assertHTTPSJobs(label, name string, credentialed bool, downloads []string, want ...string) {
	c.t.Helper()
	seen := map[string]int{}
	jobs := c.jobsOf(label, name)
	for index := range jobs {
		job := &jobs[index]
		operation := job.Labels[labelOperation]
		switch operation {
		case "verify":
			c.fatalf("%s ran a Verify Job %s, and an HTTPS source has none", name, job.Name)
		case "resolve":
			c.check(httpsResolveJob(job), "Job %s", job.Name)
		case "observe", "plan", "history", "apply":
			if operation == "apply" && label == labelSchema {
				continue
			}
			c.check(httpsFetchJob(job, credentialed, downloads...), "Job %s", job.Name)
		}
		seen[operation]++
	}
	for _, operation := range want {
		if seen[operation] == 0 {
			c.fatalf("%s has no %s Job to examine; it has %v", name, operation, seen)
		}
	}
}

// approve approves the schema's current plan after holding it to the HTTPS
// source and content the phase expects.
func (c *httpsRun) approve(approval string, schema *ptahv1alpha1.PtahSchema, url, selected, content string) {
	c.t.Helper()
	plan := &ptahv1alpha1.PtahSchemaPlan{}
	c.check(c.get(schema.Status.Plan.Name, plan), "read plan %s", schema.Status.Plan.Name)
	c.check(httpsPlanBound(plan.Spec.ContractVersion, plan.Spec.ArtifactDigest, string(plan.Spec.VerificationPolicyUID), plan.Spec.Source,
		url, selected, content), "plan %s", plan.Name)
	c.check(c.create(approvalDocument(c.in.TestNamespace, approval, schema.Name, string(schema.UID),
		plan.Name, string(plan.UID), plan.Spec.Fingerprint)), "approve plan %s", plan.Name)
}

// applySchema creates a schema, waits for its first plan, holds the
// resolution and the plan to the download and the content, approves it and
// waits until it is in sync.
func (c *httpsRun) applySchema(name, approval, secret, database string, source httpsSourceDocument, content string) *ptahv1alpha1.PtahSchema {
	c.t.Helper()
	c.createDatabase(database, secret)
	c.check(c.create(c.schemaDocument(name, secret, database, source)), "create PtahSchema %s", name)
	planned := c.waitForSchema(name, 1, "the first plan of "+name, awaitingHTTPSPlan)
	c.check(httpsResolvedTo(planned.Status.Source.HTTPS, source.url, source.format, source.path, source.digest, content), "the source of %s", name)
	c.check(downloadVerified(planned.Status.Conditions, source.digest, content), "%s", name)
	if planned.Status.Source.Git != nil || planned.Status.Source.Snapshot != nil || planned.Status.Source.Digest != "" {
		c.fatalf("%s records another kind of source beside the download: %+v", name, planned.Status.Source)
	}
	c.approve(approval, planned, source.url, source.path, content)
	return c.waitForSchema(name, 1, "the plan of "+name+" to apply", inSyncSchema)
}

// schemaFromAFile applies a schema served as one file behind Basic and the
// phase's CA, reached through a redirect on the same origin. A Resolve Job
// downloads it held to its digest; every later Job fetches it held to both
// digests; the plan binds the URL, path and content.
func (c *httpsRun) schemaFromAFile() {
	c.t.Helper()
	c.reset()
	source := httpsSourceDocument{
		url: c.url("same/basic", httpsfixture.SchemaFile), digest: c.digests[httpsfixture.SchemaFile],
		format: ptahv1alpha1.HTTPSFormatFile, path: httpsfixture.SchemaFile, credential: httpsBasicCredential,
	}
	content, err := httpsfixture.SchemaContent(httpsfixture.SchemaFile)
	c.check(err, "digest the schema")
	c.applySchema(httpsSchema, "e2e-https-schema-v1", httpsSchemaSecret, httpsSchemaDatabase, source, content)
	if table := c.query(httpsSchemaDatabase, "SELECT count(*) FROM information_schema.tables WHERE table_name='e2e_https_items'"); table != "1" {
		c.fatalf("the applied plan left %q tables named e2e_https_items, want 1", table)
	}
	c.assertHTTPSJobs(labelSchema, httpsSchema, true, []string{source.digest}, "resolve", "observe", "plan")
	c.check(httpsRedirectFollowed(c.requests(), c.host, "/same/basic/"+httpsfixture.SchemaFile, "/basic/"+httpsfixture.SchemaFile, "Basic"),
		"the server's log")
	c.logf("PASS a schema file behind Basic and a private CA, reached through a same-origin redirect, applied")
}

// migrationFromAnArchive applies a migration directory from a gzip tar behind
// a Bearer token, and reads every migration in the history.
func (c *httpsRun) migrationFromAnArchive() {
	c.t.Helper()
	c.createDatabase(httpsMigrationDatabase, httpsMigrationSecret)
	source := httpsSourceDocument{
		url: c.url("bearer", httpsfixture.MigrationArchive), digest: c.digests[httpsfixture.MigrationArchive],
		format: ptahv1alpha1.HTTPSFormatTarGzip, path: httpsfixture.MigrationPath, credential: httpsBearerCredential,
	}
	c.check(c.create(c.migrationDocument(httpsMigration, httpsMigrationSecret, httpsMigrationDatabase, source)),
		"create PtahMigration %s", httpsMigration)
	content, err := httpsfixture.MigrationContent()
	c.check(err, "digest the migration directory")
	planned := c.waitForMigration(httpsMigration, "the migration plan", func(migration *ptahv1alpha1.PtahMigration) bool {
		return migration.Status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval && migration.Status.Plan != nil &&
			settledMigration(migration)
	})
	var resolved *ptahv1alpha1.HTTPSSourceRevision
	if artifact := planned.Status.Artifact; artifact != nil && artifact.HTTPS != nil {
		download := artifact.HTTPS
		resolved = &ptahv1alpha1.HTTPSSourceRevision{
			URL: download.URL, Format: download.Format, Path: download.Path, Digest: download.Digest, ContentDigest: download.ContentDigest,
		}
		if artifact.Git != nil || artifact.Snapshot != nil || artifact.Digest != "" {
			c.fatalf("the migration records another kind of source beside the download: %+v", artifact)
		}
	}
	c.check(httpsResolvedTo(resolved, source.url, source.format, source.path, source.digest, content), "the migration's source")
	plan := &ptahv1alpha1.PtahMigrationPlan{}
	c.check(c.get(planned.Status.Plan.Name, plan), "read plan %s", planned.Status.Plan.Name)
	c.check(httpsPlanBound(plan.Spec.ContractVersion, plan.Spec.ArtifactDigest, string(plan.Spec.VerificationPolicyUID), plan.Spec.Source,
		source.url, source.path, content), "plan %s", plan.Name)
	c.check(c.create(migrationApprovalDocument(c.in.TestNamespace, "e2e-https-migration-v1", httpsMigration,
		string(planned.UID), plan.Name, string(plan.UID), plan.Spec.Fingerprint)), "approve plan %s", plan.Name)
	c.waitForMigration(httpsMigration, "the migration plan to apply", func(migration *ptahv1alpha1.PtahMigration) bool {
		return migration.Status.Phase == ptahv1alpha1.MigrationPhaseInSync && settledMigration(migration)
	})
	if applied := c.query(httpsMigrationDatabase, "SELECT count(*) FROM schema_migrations WHERE state='applied'"); applied != "2" {
		c.fatalf("the history holds %q applied migrations, want both", applied)
	}
	if column := c.query(httpsMigrationDatabase,
		"SELECT count(*) FROM information_schema.columns WHERE table_name='e2e_https_parts' AND column_name='sku'"); column != "1" {
		c.fatalf("the applied sequence left %q sku columns on e2e_https_parts, want 1", column)
	}
	c.assertHTTPSJobs(labelMigration, httpsMigration, true, []string{source.digest}, "resolve", "history", "apply")
	c.logf("PASS a migration directory from a gzip tar behind a Bearer token applied both migrations")
}

// zipSource is the public zip the next two rows read, pinned to digest.
func (c *httpsRun) zipSource(digest string) httpsSourceDocument {
	return httpsSourceDocument{
		url: c.url("public", httpsfixture.SchemaArchive), digest: digest,
		format: ptahv1alpha1.HTTPSFormatZip, path: httpsfixture.SchemaInArchive,
	}
}

// schemaFromAZip applies a schema file selected from a public zip, beside a
// file nothing selects.
func (c *httpsRun) schemaFromAZip() {
	c.t.Helper()
	source := c.zipSource(c.digests[httpsfixture.SchemaArchive])
	content, err := httpsfixture.SchemaContent(path.Base(httpsfixture.SchemaInArchive))
	c.check(err, "digest the schema")
	c.applySchema(httpsZipSchema, "e2e-https-zip-v1", httpsZipSecret, httpsZipDatabase, source, content)
	if table := c.query(httpsZipDatabase, "SELECT count(*) FROM information_schema.tables WHERE table_name='e2e_https_items'"); table != "1" {
		c.fatalf("the applied plan left %q tables named e2e_https_items, want 1", table)
	}
	c.assertHTTPSJobs(labelSchema, httpsZipSchema, false, []string{source.digest}, "resolve", "observe", "plan")
	c.logf("PASS a schema file selected from a public zip applied")
}

// republishedZip has the server answer the zip's URL with other bytes that
// hold the same selected file. The Resolve the next edit causes is refused,
// naming the digest the server served and the one the spec pins, and the
// schema keeps what it applied. Once the spec names the new digest, the
// Resolve records the same content and the schema settles on the plan it
// had applied: the plan binds the content, not the download.
//
// The applied record is the proof. A Resolve that read the new digest as new
// content would clear it, and the schema would still reach InSync, through an
// Observe that finds the database as declared and a Plan with nothing to
// publish, with no record of what it applied. So a count of published plans
// reads the same whichever way the Resolve went; the plan reference does not.
func (c *httpsRun) republishedZip() {
	c.t.Helper()
	before := c.waitForSchema(httpsZipSchema, 1, "the zip schema to be in sync", inSyncSchema)
	first, republished := c.digests[httpsfixture.SchemaArchive], c.digests[httpsfixture.SchemaArchiveRepublished]
	content, err := httpsfixture.SchemaContent(path.Base(httpsfixture.SchemaInArchive))
	c.check(err, "digest the schema")

	c.check(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		stored := &corev1.ConfigMap{}
		if err := c.get(httpsFiles, stored); err != nil {
			return err
		}
		stored.BinaryData[httpsfixture.SchemaArchive] = c.files[httpsfixture.SchemaArchiveRepublished]
		return c.cluster.Client.Update(c.ctx, stored, client.FieldOwner(harness.FieldOwner))
	}), "republish %s", httpsfixture.SchemaArchive)
	c.waitServing(httpsfixture.SchemaArchive, republished)
	c.resolveNow(httpsZipSchema, "59m")
	refusal := c.refusedAtResolve(httpsZipSchema, func(message string) error { return downloadRefused(message, republished, first) })
	after := c.schemaNamed(httpsZipSchema)
	if after.Status.Applied == nil || before.Status.Applied == nil || after.Status.Applied.PlanRef != before.Status.Applied.PlanRef {
		c.fatalf("the refused schema's applied record moved from %+v to %+v", before.Status.Applied, after.Status.Applied)
	}
	c.check(httpsResolvedTo(after.Status.Source.HTTPS, c.zipSource(first).url, ptahv1alpha1.HTTPSFormatZip, httpsfixture.SchemaInArchive, first, content),
		"the refused schema's source")

	generation := c.pinDigest(httpsZipSchema, republished)
	kept := c.waitForSchema(httpsZipSchema, generation, "the republished zip to be resolved and the schema to settle",
		func(schema *ptahv1alpha1.PtahSchema) bool {
			return schema.Status.ObservedGeneration >= generation && schema.Status.Source.HTTPS != nil &&
				schema.Status.Source.HTTPS.Digest == republished && inSyncSchema(schema)
		})
	c.check(httpsResolvedTo(kept.Status.Source.HTTPS, c.zipSource(republished).url, ptahv1alpha1.HTTPSFormatZip, httpsfixture.SchemaInArchive,
		republished, content), "the schema's source after the new digest")
	c.check(downloadVerified(kept.Status.Conditions, republished, content), "%s", httpsZipSchema)
	if kept.Status.Applied == nil || kept.Status.Applied.PlanRef != before.Status.Applied.PlanRef {
		c.fatalf("the new digest moved the applied record from %+v to %+v", before.Status.Applied, kept.Status.Applied)
	}
	c.logf("PASS a republished zip was refused naming both digests, and once pinned still records %s as applied: %s",
		kept.Status.Applied.PlanRef.Name, refusal)
}

// resolveNow edits a schema's interval, which the controller answers with a
// Resolve: the edit moves the generation.
func (c *httpsRun) resolveNow(name, interval string) {
	c.t.Helper()
	patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"interval":"`+interval+`"}}`))
	c.check(c.cluster.Client.Patch(c.ctx, &ptahv1alpha1.PtahSchema{ObjectMeta: c.meta(name)}, patch, client.FieldOwner(harness.FieldOwner)),
		"set the interval of %s", name)
}

// pinDigest names digest as the schema's download, and returns the generation
// the edit made.
func (c *httpsRun) pinDigest(name, digest string) int64 {
	c.t.Helper()
	patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"desired":{"https":{"digest":"`+digest+`"}}}}`))
	schema := &ptahv1alpha1.PtahSchema{ObjectMeta: c.meta(name)}
	c.check(c.cluster.Client.Patch(c.ctx, schema, patch, client.FieldOwner(harness.FieldOwner)), "pin %s on %s", digest, name)
	return schema.Generation
}

// refusedAtResolve waits until a schema reports that its reconciliation
// failed with a message refused accepts, and returns the message.
func (c *httpsRun) refusedAtResolve(name string, refused func(string) error) string {
	c.t.Helper()
	var refusal string
	c.wait(name+" to be refused", func() (bool, string) {
		schema := c.schemaNamed(name)
		failed := meta.FindStatusCondition(schema.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed)
		if failed == nil || failed.Status != metav1.ConditionTrue {
			return false, string(schema.Status.Phase)
		}
		refusal = failed.Message
		err := refused(refusal)
		return err == nil, fmt.Sprint(err)
	})
	return refusal
}

// says is a refusal that names want.
func says(want string) func(string) error {
	return func(message string) error {
		if !strings.Contains(message, want) {
			return fmt.Errorf("%q does not say %q", message, want)
		}
		return nil
	}
}

// onlyResolved holds a schema whose source was refused to having resolved
// nothing, planned nothing and run nothing but Resolve Jobs, of which one at
// least is left to examine.
func (c *httpsRun) onlyResolved(name string) *ptahv1alpha1.PtahSchema {
	c.t.Helper()
	schema := c.schemaNamed(name)
	if schema.Status.Source.HTTPS != nil || schema.Status.Plan != nil {
		c.fatalf("refused %s still resolved %+v or planned %+v", name, schema.Status.Source.HTTPS, schema.Status.Plan)
	}
	c.onlyResolveJobs(labelSchema, name)
	return schema
}

func (c *httpsRun) onlyResolveJobs(label, name string) {
	c.t.Helper()
	resolves := 0
	for _, job := range c.jobsOf(label, name) {
		if operation := job.Labels[labelOperation]; operation != "resolve" {
			c.fatalf("refused %s ran a %s Job", name, operation)
		}
		resolves++
	}
	if resolves == 0 {
		c.fatalf("no Resolve Job of %s is left to examine", name)
	}
}

// redirectToAnotherOrigin names a credentialed download whose URL redirects
// to the other Service name. Ptah refuses the redirect at the Resolve, so
// the credential goes nowhere else: the other origin received no request.
func (c *httpsRun) redirectToAnotherOrigin() {
	c.t.Helper()
	source := httpsSourceDocument{
		url: c.url("away/basic", httpsfixture.SchemaFile), digest: c.digests[httpsfixture.SchemaFile],
		format: ptahv1alpha1.HTTPSFormatFile, path: httpsfixture.SchemaFile, credential: httpsBasicCredential,
	}
	c.check(c.create(c.schemaDocument(httpsRedirectSchema, httpsSchemaSecret, httpsRefusedKey, source)), "create PtahSchema %s", httpsRedirectSchema)
	refusal := c.refusedAtResolve(httpsRedirectSchema, says(httpsRedirectRefusal))
	c.deleteAndWait(c.onlyResolved(httpsRedirectSchema))
	c.check(httpsRedirectStoppedAtTheOrigin(c.requests(), c.host, "/away/basic/"+httpsfixture.SchemaFile, c.other), "the server's log")
	c.logf("PASS a redirect to another origin was refused with the credential set, and that origin received nothing: %s", refusal)
}

// credentialForAnotherHost names a credential whose owner granted it for
// another host. The Resolve Job refuses it before Ptah runs, so the server
// never sees a request for the path only this row names.
func (c *httpsRun) credentialForAnotherHost() {
	c.t.Helper()
	source := httpsSourceDocument{
		url: c.url("bearer", httpsfixture.SchemaFile), digest: c.digests[httpsfixture.SchemaFile],
		format: ptahv1alpha1.HTTPSFormatFile, path: httpsfixture.SchemaFile, credential: httpsElsewhereGrant,
	}
	c.check(c.create(c.schemaDocument(httpsElsewhereSchema, httpsSchemaSecret, httpsRefusedKey, source)), "create PtahSchema %s", httpsElsewhereSchema)
	refusal := c.refusedAtResolve(httpsElsewhereSchema, says(httpsElsewhereRefusal))
	c.deleteAndWait(c.onlyResolved(httpsElsewhereSchema))
	c.check(httpsNoRequestFor(c.requests(), "/bearer/"+httpsfixture.SchemaFile), "the server's log")
	c.logf("PASS a credential granted for another host was refused before Ptah ran: %s", refusal)
}

// refusedArchives names two migration archives Ptah refuses to unpack: one
// with a symbolic link in the selected directory and one with an entry that
// climbs out of the archive. Each is refused at its Resolve with Ptah's
// reason, and nothing else runs.
func (c *httpsRun) refusedArchives() {
	c.t.Helper()
	for _, row := range []struct{ name, file, want string }{
		{httpsSymlinkMigration, httpsfixture.SymlinkArchive, httpsSymlinkRefusal},
		{httpsDotDotMigration, httpsfixture.DotDotArchive, httpsDotDotRefusal},
	} {
		source := httpsSourceDocument{
			url: c.url("bearer", row.file), digest: c.digests[row.file],
			format: ptahv1alpha1.HTTPSFormatTarGzip, path: httpsfixture.MigrationPath, credential: httpsBearerCredential,
		}
		c.check(c.create(c.migrationDocument(row.name, httpsMigrationSecret, httpsRefusedKey, source)), "create PtahMigration %s", row.name)
		migration := &ptahv1alpha1.PtahMigration{}
		var refusal string
		c.wait(row.name+" to be refused for "+row.want, func() (bool, string) {
			if err := c.get(row.name, migration); err != nil {
				return false, err.Error()
			}
			progressing := meta.FindStatusCondition(migration.Status.Conditions, ptahv1alpha1.ConditionMigrationProgressing)
			if progressing == nil || progressing.Reason != string(ptahv1alpha1.ReasonOperationFailed) {
				return false, fmt.Sprintf("%s, Progressing %+v", migration.Status.Phase, progressing)
			}
			refusal = progressing.Message
			return strings.Contains(refusal, row.want), refusal
		})
		if artifact := migration.Status.Artifact; (artifact != nil && artifact.HTTPS != nil) || migration.Status.Plan != nil {
			c.fatalf("refused %s still resolved %+v or planned %+v", row.name, artifact, migration.Status.Plan)
		}
		c.onlyResolveJobs(labelMigration, row.name)
		c.deleteAndWait(migration)
		c.logf("PASS %s was refused at its Resolve: %s", row.name, refusal)
	}
}

// sourcePolicy names a source policy on two schemas of their own. One allows
// only another host, and refuses the source before any Job is made, so the
// server never sees its path; the other bounds the content below the
// selected file, and refuses the source after the Resolve Job read it. That
// second schema's Job is what shows the listing that found none for the
// first could see one.
func (c *httpsRun) sourcePolicy() {
	c.t.Helper()
	immutable := true
	for name, document := range map[string]string{
		httpsHostPolicy: "allowedHTTPSHosts:\n  - " + httpsElsewhereHost + "\n",
		httpsSizePolicy: fmt.Sprintf("maxContentBytes: %d\n", httpsPolicyMaxBytes),
	} {
		c.check(c.cluster.Client.Create(c.ctx, &corev1.ConfigMap{
			ObjectMeta: c.meta(name), Immutable: &immutable, Data: map[string]string{"policy.yaml": document},
		}, client.FieldOwner(harness.FieldOwner)), "create source policy %s", name)
	}

	host := httpsSourceDocument{
		url: c.url("public", httpsfixture.SchemaFile), digest: c.digests[httpsfixture.SchemaFile],
		format: ptahv1alpha1.HTTPSFormatFile, path: httpsfixture.SchemaFile, policy: httpsHostPolicy,
	}
	c.check(c.create(c.schemaDocument(httpsHostPolicySchema, httpsSchemaSecret, httpsRefusedKey, host)), "create PtahSchema %s", httpsHostPolicySchema)
	hostRefusal := c.refusedAtResolve(httpsHostPolicySchema, says("allowedHTTPSHosts does not name "+c.host))
	schema := c.schemaNamed(httpsHostPolicySchema)
	if schema.Status.Source.HTTPS != nil || schema.Status.Plan != nil {
		c.fatalf("the refused %s resolved %+v or planned %+v", httpsHostPolicySchema, schema.Status.Source.HTTPS, schema.Status.Plan)
	}
	if jobs := c.jobsOf(labelSchema, httpsHostPolicySchema); len(jobs) != 0 {
		c.fatalf("a host the policy refuses got %d Jobs, the first %s", len(jobs), jobs[0].Name)
	}
	c.check(httpsNoRequestFor(c.requests(), "/public/"+httpsfixture.SchemaFile), "the server's log")
	c.deleteAndWait(schema)

	// The republished row left the server answering the zip with its second
	// bytes, which this schema pins, so the pull succeeds and the bound is
	// what refuses it.
	size := c.zipSource(c.digests[httpsfixture.SchemaArchiveRepublished])
	size.policy = httpsSizePolicy
	c.check(c.create(c.schemaDocument(httpsSizePolicySchema, httpsSchemaSecret, httpsRefusedKey, size)), "create PtahSchema %s", httpsSizePolicySchema)
	sizeRefusal := c.refusedAtResolve(httpsSizePolicySchema, says(fmt.Sprintf("more than maxContentBytes %d", httpsPolicyMaxBytes)))
	c.deleteAndWait(c.onlyResolved(httpsSizePolicySchema))
	c.logf("PASS a source policy refused a host it does not name before any Job (%s) and content past its bound (%s)", hostRefusal, sizeRefusal)
}

// finish suspends the resources, so no read of theirs lands inside a later
// phase, and prints the server's request log: the requests every row read.
func (c *httpsRun) finish() {
	c.t.Helper()
	for _, object := range []client.Object{
		&ptahv1alpha1.PtahSchema{ObjectMeta: c.meta(httpsSchema)},
		&ptahv1alpha1.PtahSchema{ObjectMeta: c.meta(httpsZipSchema)},
		&ptahv1alpha1.PtahMigration{ObjectMeta: c.meta(httpsMigration)},
	} {
		patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"suspend":true}}`))
		c.check(c.cluster.Client.Patch(c.ctx, object, patch, client.FieldOwner(harness.FieldOwner)), "suspend %s", object.GetName())
	}
	for _, request := range c.requests() {
		c.logf("served %s %s %s %d", request.host, request.path, request.credential, request.status)
	}
}

// cleanup prints what the resources decided and what the server logged when
// the phase failed. The statuses carry no SQL and no credential by contract,
// and the server logs the scheme of a credential, never its value.
func (c *httpsRun) cleanup() {
	if c.cluster == nil || !c.parent.Failed() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, name := range []string{
		httpsSchema, httpsZipSchema, httpsRedirectSchema, httpsElsewhereSchema, httpsHostPolicySchema, httpsSizePolicySchema,
	} {
		schema := &ptahv1alpha1.PtahSchema{}
		if c.cluster.Client.Get(ctx, c.key(name), schema) == nil {
			_, _ = fmt.Fprintf(os.Stderr, "e2e HTTPS sources: %s status %+v\n", name, schema.Status)
		}
	}
	for _, name := range []string{httpsMigration, httpsSymlinkMigration, httpsDotDotMigration} {
		migration := &ptahv1alpha1.PtahMigration{}
		if c.cluster.Client.Get(ctx, c.key(name), migration) == nil {
			_, _ = fmt.Fprintf(os.Stderr, "e2e HTTPS sources: %s status %+v\n", name, migration.Status)
		}
	}
	if logs, err := deploymentLogs(ctx, c.cluster, c.in.TestNamespace, httpsServer); err == nil {
		_, _ = fmt.Fprintf(os.Stderr, "e2e HTTPS sources: server log\n%s\n", logs)
	}
}
