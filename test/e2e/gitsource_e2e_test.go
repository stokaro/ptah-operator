//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
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
	"github.com/stokaro/ptah-operator/test/e2e/gitrepo"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// TestGitSources is the git-sources phase.
func TestGitSources(t *testing.T) {
	run, inputs := harness.Begin(t, phases.GitSources)
	c := newGitRun(t, run, inputs)
	for _, scenario := range []struct {
		name string
		body func()
	}{
		{"git-schema", c.schemaFromGit},
		{"git-schema-moves", c.schemaFollowsItsBranch},
		{"git-schema-pinned", c.schemaAtAPinnedCommit},
		{"git-force-push", c.forcePushedBranch},
		{"git-migration-ssh", c.migrationOverSSH},
		{"git-credential-refused", c.credentialForAnotherHost},
		{"git-source-refused", c.refusedSources},
	} {
		if !run.Scenario(scenario.name, c.scenario(scenario.body)) {
			return
		}
	}
	c.finish()
	run.Logf("e2e Git sources: PASS a schema and a migration sequence from Git over HTTPS and SSH")
}

// gitRun is what the Git source proofs share. Each scenario runs as a
// subtest, and t is that subtest while it runs.
type gitRun struct {
	t      *testing.T
	parent *testing.T
	ctx    context.Context
	in     phases.GitSourceInputs

	cluster  *harness.Cluster
	engine   migrationEngine
	password string

	host  string
	keys  gitServerKeys
	https string
	ssh   string
}

func newGitRun(t *testing.T, run *harness.Run, in phases.GitSourceInputs) *gitRun {
	t.Helper()
	c := &gitRun{t: t, parent: t, ctx: run.Context(), in: in}
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
	c.host = gitServer + "." + in.TestNamespace + ".svc"
	c.https = "https://" + c.host + "/" + gitRepository
	c.ssh = "ssh://git@" + c.host + "/" + gitRepository
	c.standUp()
	return c
}

func (c *gitRun) scenario(body func()) func(*testing.T) {
	return func(t *testing.T) {
		c.t = t
		defer func() { c.t = c.parent }()
		body()
	}
}

func (c *gitRun) fatalf(format string, arguments ...any) {
	c.t.Helper()
	c.t.Fatalf("e2e Git sources: "+format, arguments...)
}

func (c *gitRun) logf(format string, arguments ...any) {
	c.t.Helper()
	c.t.Logf("e2e Git sources: "+format, arguments...)
}

func (c *gitRun) check(err error, format string, arguments ...any) {
	c.t.Helper()
	if err != nil {
		c.fatalf("%s: %v", fmt.Sprintf(format, arguments...), err)
	}
}

func (c *gitRun) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: c.in.TestNamespace, Name: name}
}

func (c *gitRun) get(name string, object client.Object) error {
	return c.cluster.Client.Get(c.ctx, c.key(name), object)
}

func (c *gitRun) create(document map[string]any) error {
	return c.cluster.Client.Create(c.ctx, &unstructured.Unstructured{Object: document},
		client.FieldOwner(harness.FieldOwner), client.FieldValidation("Strict"))
}

// put creates object, or replaces what an earlier run of the phase left.
func (c *gitRun) put(object client.Object) {
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

func (c *gitRun) wait(what string, check func() (bool, string)) {
	c.t.Helper()
	err := harness.Wait(c.ctx, what, waitTimeout, 3*time.Second, func(context.Context) (bool, string, error) {
		done, observed := check()
		return done, observed, nil
	})
	c.check(err, "wait for %s", what)
}

// standUp generates the server's keys and runs it from the fixture image,
// with the credential Secrets and the trust ConfigMaps the resources select.
func (c *gitRun) standUp() {
	c.t.Helper()
	var err error
	c.keys, err = newGitServerKeys(c.host, c.host, c.host+".cluster.local")
	c.check(err, "generate the Git server's keys")
	spec, err := gitSpecDocument(gitRepositorySpec(0))
	c.check(err, "encode the repository")
	namespace := c.in.TestNamespace
	object := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: namespace, Name: name} }
	c.put(&corev1.ConfigMap{ObjectMeta: object(gitSpecConfigMap), Data: map[string]string{"spec.json": spec}})
	c.put(&corev1.Secret{ObjectMeta: object(gitServerSecret), Data: map[string][]byte{
		"tls.crt": c.keys.certificatePEM, "tls.key": c.keys.keyPEM, "token": []byte(c.keys.token),
		"ssh_host_key": c.keys.hostKeyPEM, "authorized_key": c.keys.authorizedKey,
	}})
	c.put(&corev1.ConfigMap{ObjectMeta: object(gitCAConfigMap), Data: map[string]string{"ca.pem": string(c.keys.caPEM)}})
	c.put(&corev1.ConfigMap{ObjectMeta: object(gitKnownHostsConfigMap), Data: map[string]string{"known_hosts": string(c.keys.knownHosts)}})
	c.put(&corev1.ConfigMap{ObjectMeta: object(gitOtherHostsConfigMap), Data: map[string]string{"known_hosts": string(c.keys.otherKnownHosts)}})
	// The Secrets' owner grants each credential one host and the exact trust
	// it may be sent under. The last grants the token for another host.
	c.put(&corev1.Secret{ObjectMeta: object(gitTokenCredential), Data: map[string][]byte{
		"password": []byte(c.keys.token), "host": []byte(c.host), "caSHA256": []byte(grantDigest(c.keys.caPEM)),
	}})
	c.put(&corev1.Secret{ObjectMeta: object(gitDeployKeyCredential), Data: map[string][]byte{
		"sshPrivateKey": c.keys.clientKeyPEM, "host": []byte(c.host), "knownHostsSHA256": []byte(grantDigest(c.keys.knownHosts)),
	}})
	c.put(&corev1.Secret{ObjectMeta: object(gitOtherHostsCredential), Data: map[string][]byte{
		"sshPrivateKey": c.keys.clientKeyPEM, "host": []byte(c.host), "knownHostsSHA256": []byte(grantDigest(c.keys.otherKnownHosts)),
	}})
	c.put(&corev1.Secret{ObjectMeta: object(gitElsewhereCredential), Data: map[string][]byte{
		"password": []byte(c.keys.token), "host": []byte("git.example.com"), "caSHA256": []byte(grantDigest(c.keys.caPEM)),
	}})

	labels := map[string]string{"app.kubernetes.io/name": gitServer}
	mode := int32(0o440)
	c.put(&appsv1.Deployment{ObjectMeta: object(gitServer), Spec: appsv1.DeploymentSpec{
		Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
			AutomountServiceAccountToken: ptr.To(false), EnableServiceLinks: ptr.To(false),
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: registryPullSecret}},
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532)), FSGroup: ptr.To(int64(65532)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name: "git", Image: c.in.FixtureImage, ImagePullPolicy: corev1.PullIfNotPresent,
				Command: []string{"/e2e-git-server"},
				Args: []string{
					"-spec", "/spec/spec.json", "-repository", gitRepository,
					"-https-listen", ":8443", "-tls-certificate", "/keys/tls.crt", "-tls-key", "/keys/tls.key", "-token-file", "/keys/token",
					"-ssh-listen", ":2222", "-ssh-host-key", "/keys/ssh_host_key", "-ssh-authorized-key", "/keys/authorized_key",
				},
				Ports:          []corev1.ContainerPort{{Name: "https", ContainerPort: 8443}, {Name: "ssh", ContainerPort: 2222}},
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8443)}}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
					Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "spec", MountPath: "/spec", ReadOnly: true}, {Name: "keys", MountPath: "/keys", ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{
				{Name: "spec", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: gitSpecConfigMap}, DefaultMode: &mode,
				}}},
				{Name: "keys", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: gitServerSecret, DefaultMode: &mode}}},
			},
		}},
	}})
	c.put(&corev1.Service{ObjectMeta: object(gitServer), Spec: corev1.ServiceSpec{
		Selector: labels,
		Ports: []corev1.ServicePort{
			{Name: "https", Port: 443, TargetPort: intstr.FromString("https")},
			{Name: "ssh", Port: 22, TargetPort: intstr.FromString("ssh")},
		},
	}})
	if err := c.cluster.WaitForRollout(c.ctx, namespace, gitServer, waitTimeout); err != nil {
		c.fatalf("the Git server did not become ready: %v", err)
	}
	c.logf("the Git server serves %s and %s", c.https, c.ssh)
}

// moveMain points main at the commit at index.
func (c *gitRun) moveMain(index int) gitrepo.Spec {
	c.t.Helper()
	return c.serve(gitRepositorySpec(index), index)
}

// serve hands the server a repository whose main names the commit at index,
// and waits until the kubelet has handed the server the edit and the server
// reports it: the server logs every ref each time it builds the repository.
func (c *gitRun) serve(spec gitrepo.Spec, index int) gitrepo.Spec {
	c.t.Helper()
	document, err := gitSpecDocument(spec)
	c.check(err, "encode the repository")
	c.check(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		stored := &corev1.ConfigMap{}
		if err := c.get(gitSpecConfigMap, stored); err != nil {
			return err
		}
		stored.Data = map[string]string{"spec.json": document}
		return c.cluster.Client.Update(c.ctx, stored, client.FieldOwner(harness.FieldOwner))
	}), "move main to commit %d", index)
	built, err := gitrepo.Build(spec)
	c.check(err, "build the repository")
	want := "refs/heads/main is " + built.Commit(index).String()
	c.wait("the Git server to serve main at commit "+built.Commit(index).String(), func() (bool, string) {
		logs, err := deploymentLogs(c.ctx, c.cluster, c.in.TestNamespace, gitServer)
		if err != nil {
			return false, err.Error()
		}
		lines := strings.Split(strings.TrimSpace(logs), "\n")
		for index := len(lines) - 1; index >= 0; index-- {
			if strings.Contains(lines[index], "refs/heads/main is ") {
				return strings.Contains(lines[index], want), lines[index]
			}
		}
		return false, "no ref is logged yet"
	})
	return spec
}

// commit is the id of the commit at index of the phase's repository.
func (c *gitRun) commit(index int) string {
	c.t.Helper()
	built, err := gitrepo.Build(gitRepositorySpec(index))
	c.check(err, "build the repository")
	return built.Commit(index).String()
}

// rewrittenCommit is the id of the commit at index after the force-push.
func (c *gitRun) rewrittenCommit(index int) string {
	c.t.Helper()
	built, err := gitrepo.Build(gitRewrittenSpec())
	c.check(err, "build the rewritten repository")
	return built.Commit(index).String()
}

func (c *gitRun) schemaNamed(name string) *ptahv1alpha1.PtahSchema {
	c.t.Helper()
	schema := &ptahv1alpha1.PtahSchema{}
	c.check(c.get(name, schema), "read PtahSchema %s", name)
	return schema
}

func (c *gitRun) waitForSchema(what string, done func(*ptahv1alpha1.PtahSchema) bool) *ptahv1alpha1.PtahSchema {
	c.t.Helper()
	return c.waitForSchemaNamed(gitSchema, what, done)
}

func (c *gitRun) waitForSchemaNamed(name, what string, done func(*ptahv1alpha1.PtahSchema) bool) *ptahv1alpha1.PtahSchema {
	c.t.Helper()
	var matched *ptahv1alpha1.PtahSchema
	c.wait(what, func() (bool, string) {
		schema := &ptahv1alpha1.PtahSchema{}
		if err := c.get(name, schema); err != nil {
			return false, err.Error()
		}
		if schema.Status.Phase == ptahv1alpha1.PhaseFailed {
			c.fatalf("%s failed while waiting for %s: %v", name, what, schema.Status.Conditions)
		}
		if done(schema) {
			matched = schema
			return true, ""
		}
		return false, string(schema.Status.Phase)
	})
	return matched
}

func (c *gitRun) waitForMigration(what string, done func(*ptahv1alpha1.PtahMigration) bool) *ptahv1alpha1.PtahMigration {
	c.t.Helper()
	var matched *ptahv1alpha1.PtahMigration
	c.wait(what, func() (bool, string) {
		migration := &ptahv1alpha1.PtahMigration{}
		if err := c.get(gitMigration, migration); err != nil {
			return false, err.Error()
		}
		if migration.Status.Phase == ptahv1alpha1.MigrationPhaseFailed {
			c.fatalf("%s failed while waiting for %s: %v", gitMigration, what, migration.Status.Conditions)
		}
		if done(migration) {
			matched = migration
			return true, ""
		}
		return false, string(migration.Status.Phase)
	})
	return matched
}

func (c *gitRun) deleteAndWait(object client.Object) {
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
// phase alone uses, and the two databases.
func (c *gitRun) reset() {
	c.t.Helper()
	namespace := c.in.TestNamespace
	objects := []client.Object{
		&ptahv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: gitMigration}},
		&ptahv1alpha1.PtahMigrationApproval{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "e2e-git-migration-v1"}},
	}
	for _, schema := range []string{gitSchema, gitRefusedSchema, gitPinnedSchema, gitSubmoduleSchema, gitLFSSchema, gitHostKeySchema} {
		objects = append(objects, &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: schema}})
	}
	for _, approval := range []string{"e2e-git-schema-v1", "e2e-git-schema-v2", "e2e-git-schema-v3", "e2e-git-pinned-v1"} {
		objects = append(objects, &ptahv1alpha1.PtahSchemaApproval{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: approval}})
	}
	for _, object := range objects {
		c.deleteAndWait(object)
	}
	for _, database := range []string{gitSchemaDatabase, gitMigrationDatabase, gitPinnedDatabase} {
		_, err := serverSQL(c.ctx, c.cluster, namespace, c.engine, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)")
		c.check(err, "drop database %s", database)
	}
}

func (c *gitRun) createDatabase(database, secret string) {
	c.t.Helper()
	_, err := serverSQL(c.ctx, c.cluster, c.in.TestNamespace, c.engine, "CREATE DATABASE "+database)
	c.check(err, "create database %s", database)
	c.put(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: secret}, Data: map[string][]byte{
		"url": []byte(c.engine.databaseURL(c.in.TestNamespace, migrationDatabaseUser, c.password, database)),
	}})
}

func (c *gitRun) query(database, statement string) string {
	output, _ := databaseSQL(c.ctx, c.cluster, c.in.TestNamespace, c.engine, database, statement)
	return trimmedSQL(output)
}

func (c *gitRun) planCount(label, name string, list client.ObjectList) int {
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

// assertGitJobs holds every operation Job of a resource that still exists to
// its Git shape and refuses a Verify Job: a Git source has no verification
// policy. It needs a Job of each operation named in want, so a list that came
// back empty proves nothing.
func (c *gitRun) assertGitJobs(label, name string, commits []string, want ...string) {
	c.t.Helper()
	jobs := &batchv1.JobList{}
	c.check(c.cluster.Client.List(c.ctx, jobs, client.InNamespace(c.in.TestNamespace), client.MatchingLabels{label: name}),
		"list the Jobs of %s", name)
	seen := map[string]int{}
	for index := range jobs.Items {
		job := &jobs.Items[index]
		operation := job.Labels[labelOperation]
		switch operation {
		case "verify":
			c.fatalf("%s ran a Verify Job %s, and a Git source has none", name, job.Name)
		case "resolve":
			c.check(gitResolveJob(job), "Job %s", job.Name)
		case "observe", "plan", "history", "apply":
			if operation == "apply" && label == labelSchema {
				continue
			}
			c.check(gitFetchJob(job, commits...), "Job %s", job.Name)
		}
		seen[operation]++
	}
	for _, operation := range want {
		if seen[operation] == 0 {
			c.fatalf("%s has no %s Job to examine; it has %v", name, operation, seen)
		}
	}
}

// gitSchemaSource is what a schema of the phase reads: a repository, a ref
// and a path in it, the credential, and the transport's trust.
type gitSchemaSource struct {
	url, path, credential string
	ref, transport        map[string]any
}

// httpsSource is a path of main over HTTPS, under the phase's CA.
func (c *gitRun) httpsSource(path, credential string) gitSchemaSource {
	return gitSchemaSource{
		url: c.https, path: path, credential: credential, ref: map[string]any{"branch": "main"},
		transport: map[string]any{"caFrom": map[string]any{"name": gitCAConfigMap, "key": "ca.pem"}},
	}
}

func (c *gitRun) schemaDocument(name, credential string) map[string]any {
	return c.schemaDocumentFor(name, gitSchemaSecret, gitSchemaDatabase, c.httpsSource(gitSchemaPath, credential))
}

func (c *gitRun) schemaDocumentFor(name, secret, database string, source gitSchemaSource) map[string]any {
	return map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahSchema",
		"metadata": map[string]any{"namespace": c.in.TestNamespace, "name": name},
		"spec": map[string]any{
			"target": map[string]any{
				"engine": "PostgreSQL", "coordinationKey": "e2e/" + database,
				"urlFrom": map[string]any{"name": secret, "key": "url"},
			},
			"desired": map[string]any{"git": map[string]any{
				"url": source.url, "ref": source.ref, "path": source.path,
				"authFrom":  map[string]any{"name": source.credential},
				"transport": source.transport,
			}},
			"interval":  gitInterval,
			"policy":    map[string]any{"apply": "OnApproval"},
			"execution": map[string]any{"activeDeadlineSeconds": int64(600)},
		},
	}
}

// approve approves the schema's current plan after holding it to the Git
// source and content the phase expects.
func (c *gitRun) approve(approval string, schema *ptahv1alpha1.PtahSchema, revision int) {
	c.t.Helper()
	plan := &ptahv1alpha1.PtahSchemaPlan{}
	c.check(c.get(schema.Status.Plan.Name, plan), "read plan %s", schema.Status.Plan.Name)
	content, err := gitSchemaContent(revision)
	c.check(err, "digest schema revision %d", revision)
	c.check(gitPlanBound(plan.Spec.ContractVersion, plan.Spec.ArtifactDigest, string(plan.Spec.VerificationPolicyUID), plan.Spec.Source,
		c.https, gitSchemaPath, content), "plan %s", plan.Name)
	c.check(c.create(approvalDocument(c.in.TestNamespace, approval, schema.Name, string(schema.UID),
		plan.Name, string(plan.UID), plan.Spec.Fingerprint)), "approve plan %s", plan.Name)
}

func awaitingGitPlan(previous string) func(*ptahv1alpha1.PtahSchema) bool {
	return func(schema *ptahv1alpha1.PtahSchema) bool {
		return schema.Status.Phase == ptahv1alpha1.PhaseAwaitingApproval && schema.Status.Plan != nil &&
			schema.Status.Plan.Name != previous && settledSchema(schema)
	}
}

// schemaFromGit applies a schema whose desired state is one file of a branch,
// over HTTPS with a token and a CA. A Resolve Job reads the branch; every
// later Job fetches the commit it found; the plan binds the repository, path
// and content.
func (c *gitRun) schemaFromGit() {
	c.t.Helper()
	c.reset()
	c.moveMain(0)
	c.createDatabase(gitSchemaDatabase, gitSchemaSecret)
	c.check(c.create(c.schemaDocument(gitSchema, gitTokenCredential)), "create PtahSchema %s", gitSchema)
	content, err := gitSchemaContent(0)
	c.check(err, "digest the schema")
	planned := c.waitForSchema("the first plan", awaitingGitPlan(""))
	c.check(gitResolvedTo(planned.Status.Source.Git, c.https, gitSchemaPath, ptahv1alpha1.GitRef{Branch: "main"}, c.commit(0), content),
		"the schema's source")
	if resolved := meta.FindStatusCondition(planned.Status.Conditions, ptahv1alpha1.ConditionArtifactResolved); resolved == nil ||
		resolved.Reason != string(ptahv1alpha1.ReasonCommitResolved) {
		c.fatalf("ArtifactResolved is %+v, want CommitResolved", resolved)
	}
	c.approve("e2e-git-schema-v1", planned, 0)
	c.waitForSchema("the plan to apply", inSyncSchema)
	if table := c.query(gitSchemaDatabase, "SELECT count(*) FROM information_schema.tables WHERE table_name='e2e_git_items'"); table != "1" {
		c.fatalf("the applied plan left %q tables named e2e_git_items, want 1", table)
	}
	c.assertGitJobs(labelSchema, gitSchema, []string{c.commit(0)}, "resolve", "observe", "plan")
	c.logf("PASS a schema from a Git branch applied through commit %s", c.commit(0))
}

// schemaFollowsItsBranch moves the branch twice: to a commit that changes
// only a file nothing selects, which plans nothing, and then to one that
// changes the schema, which plans and applies. An edit to the schema's
// interval asks for the Resolve now rather than at the next interval.
func (c *gitRun) schemaFollowsItsBranch() {
	c.t.Helper()
	before := c.waitForSchema("the schema to be in sync", inSyncSchema)
	plans := c.planCount(labelSchema, gitSchema, &ptahv1alpha1.PtahSchemaPlanList{})
	content, err := gitSchemaContent(0)
	c.check(err, "digest the schema")

	c.moveMain(1)
	c.resolveNow("59m")
	moved := c.waitForSchema("the README commit to be read and settled", func(schema *ptahv1alpha1.PtahSchema) bool {
		return schema.Status.Source.Git != nil && schema.Status.Source.Git.Commit == c.commit(1) && inSyncSchema(schema)
	})
	c.check(gitResolvedTo(moved.Status.Source.Git, c.https, gitSchemaPath, ptahv1alpha1.GitRef{Branch: "main"}, c.commit(1), content),
		"the schema's source after the README commit")
	if after := c.planCount(labelSchema, gitSchema, &ptahv1alpha1.PtahSchemaPlanList{}); after != plans {
		c.fatalf("a commit that changes no selected file published %d plans", after-plans)
	}

	c.moveMain(2)
	c.resolveNow("58m")
	planned := c.waitForSchema("the column commit to plan", func(schema *ptahv1alpha1.PtahSchema) bool {
		return schema.Status.Source.Git != nil && schema.Status.Source.Git.Commit == c.commit(2) &&
			awaitingGitPlan(before.Status.Applied.PlanRef.Name)(schema)
	})
	c.approve("e2e-git-schema-v2", planned, 1)
	c.waitForSchema("the column plan to apply", inSyncSchema)
	if column := c.query(gitSchemaDatabase, "SELECT count(*) FROM information_schema.columns WHERE table_name='e2e_git_items' AND column_name='sku'"); column != "1" {
		c.fatalf("the applied plan left %q sku columns, want 1", column)
	}
	c.assertGitJobs(labelSchema, gitSchema, []string{c.commit(0), c.commit(1), c.commit(2)}, "resolve", "observe", "plan")
	c.logf("PASS the schema followed its branch: a README commit planned nothing, a schema commit applied")
}

// resolveNow edits the schema's interval, which the controller answers with a
// Resolve: the edit moves the generation.
func (c *gitRun) resolveNow(interval string) {
	c.t.Helper()
	c.resolveNowFor(gitSchema, interval)
}

func (c *gitRun) resolveNowFor(name, interval string) {
	c.t.Helper()
	patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"interval":"`+interval+`"}}`))
	c.check(c.cluster.Client.Patch(c.ctx, &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: name}},
		patch, client.FieldOwner(harness.FieldOwner)), "set the interval of %s", name)
}

// schemaAtAPinnedCommit applies a schema pinned to the commit main names now,
// into a database of its own. Its Resolve reads that commit by its id rather
// than a branch, and the plan binds it.
func (c *gitRun) schemaAtAPinnedCommit() {
	c.t.Helper()
	pinned := c.commit(2)
	c.createDatabase(gitPinnedDatabase, gitPinnedSecret)
	source := c.httpsSource(gitSchemaPath, gitTokenCredential)
	source.ref = map[string]any{"commit": pinned}
	c.check(c.create(c.schemaDocumentFor(gitPinnedSchema, gitPinnedSecret, gitPinnedDatabase, source)), "create PtahSchema %s", gitPinnedSchema)
	content, err := gitSchemaContent(1)
	c.check(err, "digest the schema")
	planned := c.waitForSchemaNamed(gitPinnedSchema, "the pinned commit to plan", awaitingGitPlan(""))
	c.check(gitResolvedTo(planned.Status.Source.Git, c.https, gitSchemaPath, ptahv1alpha1.GitRef{Commit: pinned}, pinned, content),
		"the pinned schema's source")
	c.approve("e2e-git-pinned-v1", planned, 1)
	c.waitForSchemaNamed(gitPinnedSchema, "the pinned plan to apply", inSyncSchema)
	if column := c.query(gitPinnedDatabase, "SELECT count(*) FROM information_schema.columns WHERE table_name='e2e_git_items' AND column_name='sku'"); column != "1" {
		c.fatalf("the pinned commit's plan left %q sku columns, want 1", column)
	}
	c.assertGitJobs(labelSchema, gitPinnedSchema, []string{pinned}, "resolve", "observe", "plan")
	c.logf("PASS a schema pinned to commit %s applied it", pinned)
}

// forcePushedBranch rewrites main: the commit both schemas read is replaced by
// one that adds a second column, and is gone from the server. The schema that
// follows main resolves the replacement, plans it and applies it. The schema
// pinned to the replaced commit is refused at its Resolve: the server, like
// most, gives a commit only at a branch or tag, and none names it now. It
// keeps the commit it applied and publishes no plan.
func (c *gitRun) forcePushedBranch() {
	c.t.Helper()
	following := c.waitForSchema("the schema to be in sync", inSyncSchema)
	pinned := c.waitForSchemaNamed(gitPinnedSchema, "the pinned schema to be in sync", inSyncSchema)
	pinnedPlans := c.planCount(labelSchema, gitPinnedSchema, &ptahv1alpha1.PtahSchemaPlanList{})
	gone, replacement := c.commit(2), c.rewrittenCommit(2)
	if gone == replacement || c.rewrittenCommit(1) != c.commit(1) {
		c.fatalf("the rewritten history does not replace commit 2 alone")
	}
	c.serve(gitRewrittenSpec(), 2)

	c.resolveNow("57m")
	content, err := gitSchemaContent(2)
	c.check(err, "digest the schema")
	planned := c.waitForSchema("the replacement commit to plan", func(schema *ptahv1alpha1.PtahSchema) bool {
		return schema.Status.Source.Git != nil && schema.Status.Source.Git.Commit == replacement &&
			awaitingGitPlan(following.Status.Applied.PlanRef.Name)(schema)
	})
	c.check(gitResolvedTo(planned.Status.Source.Git, c.https, gitSchemaPath, ptahv1alpha1.GitRef{Branch: "main"}, replacement, content),
		"the schema's source after the force-push")
	c.approve("e2e-git-schema-v3", planned, 2)
	c.waitForSchema("the replacement plan to apply", inSyncSchema)
	if column := c.query(gitSchemaDatabase, "SELECT count(*) FROM information_schema.columns WHERE table_name='e2e_git_items' AND column_name='code'"); column != "1" {
		c.fatalf("the replacement plan left %q code columns, want 1", column)
	}

	c.resolveNowFor(gitPinnedSchema, "59m")
	refusal := c.refusedAtResolve(gitPinnedSchema, "no branch or tag names commit "+gone)
	after := c.schemaNamed(gitPinnedSchema)
	if after.Status.Source.Git == nil || after.Status.Source.Git.Commit != gone {
		c.fatalf("the refused pinned schema reports the source %+v, want the commit it applied, %s", after.Status.Source.Git, gone)
	}
	if after.Status.Applied == nil || pinned.Status.Applied == nil || after.Status.Applied.PlanRef != pinned.Status.Applied.PlanRef {
		c.fatalf("the refused pinned schema's applied record moved from %+v to %+v", pinned.Status.Applied, after.Status.Applied)
	}
	if plans := c.planCount(labelSchema, gitPinnedSchema, &ptahv1alpha1.PtahSchemaPlanList{}); plans != pinnedPlans {
		c.fatalf("the refused pinned schema published %d plans", plans-pinnedPlans)
	}
	c.logf("PASS after a force-push the branch planned %s and the commit it replaced was refused: %s", replacement, refusal)
}

// migrationOverSSH applies a migration directory at the commit a tag names,
// over SSH with a deploy key and pinned known hosts.
func (c *gitRun) migrationOverSSH() {
	c.t.Helper()
	c.createDatabase(gitMigrationDatabase, gitMigrationSecret)
	c.check(c.create(map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahMigration",
		"metadata": map[string]any{"namespace": c.in.TestNamespace, "name": gitMigration},
		"spec": map[string]any{
			"target": map[string]any{
				"engine": "PostgreSQL", "coordinationKey": "e2e/" + gitMigrationDatabase,
				"urlFrom": map[string]any{"name": gitMigrationSecret, "key": "url"},
			},
			"artifact": map[string]any{"git": map[string]any{
				"url": c.ssh, "ref": map[string]any{"tag": "v1"}, "path": gitMigrationPath,
				"authFrom":  map[string]any{"name": gitDeployKeyCredential},
				"transport": map[string]any{"knownHostsFrom": map[string]any{"name": gitKnownHostsConfigMap, "key": "known_hosts"}},
			}},
			"interval":  gitInterval,
			"policy":    map[string]any{"apply": "OnApproval"},
			"execution": map[string]any{"activeDeadlineSeconds": int64(600)},
		},
	}), "create PtahMigration %s", gitMigration)
	content, err := gitMigrationContent()
	c.check(err, "digest the migration directory")
	planned := c.waitForMigration("the migration plan", func(migration *ptahv1alpha1.PtahMigration) bool {
		return migration.Status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval && migration.Status.Plan != nil &&
			settledMigration(migration)
	})
	git := planned.Status.Artifact.Git
	if git == nil || git.Commit != c.commit(0) || git.ContentDigest != content || git.URL != c.ssh || git.Path != gitMigrationPath {
		c.fatalf("the migration's source is %+v, want commit %s with content %s", git, c.commit(0), content)
	}
	plan := &ptahv1alpha1.PtahMigrationPlan{}
	c.check(c.get(planned.Status.Plan.Name, plan), "read plan %s", planned.Status.Plan.Name)
	c.check(gitPlanBound(plan.Spec.ContractVersion, plan.Spec.ArtifactDigest, string(plan.Spec.VerificationPolicyUID), plan.Spec.Source,
		c.ssh, gitMigrationPath, content), "plan %s", plan.Name)
	c.check(c.create(migrationApprovalDocument(c.in.TestNamespace, "e2e-git-migration-v1", gitMigration,
		string(planned.UID), plan.Name, string(plan.UID), plan.Spec.Fingerprint)), "approve plan %s", plan.Name)
	c.waitForMigration("the migration plan to apply", func(migration *ptahv1alpha1.PtahMigration) bool {
		return migration.Status.Phase == ptahv1alpha1.MigrationPhaseInSync && settledMigration(migration)
	})
	if table := c.query(gitMigrationDatabase, "SELECT count(*) FROM information_schema.tables WHERE table_name='e2e_git_parts'"); table != "1" {
		c.fatalf("the applied sequence left %q tables named e2e_git_parts, want 1", table)
	}
	c.assertGitJobs(labelMigration, gitMigration, []string{c.commit(0)}, "resolve", "history", "apply")
	c.logf("PASS a migration directory from a Git tag applied over SSH")
}

// credentialForAnotherHost names a credential whose owner granted it for
// another host. The Resolve Job refuses it before Ptah runs, and nothing
// reaches the database. The schema still in sync on the phase's database
// holds that realm, so the refused one takes a key of its own, as the
// refused sources do: sharing it would block both before any Resolve.
func (c *gitRun) credentialForAnotherHost() {
	c.t.Helper()
	c.check(c.create(c.schemaDocumentFor(gitRefusedSchema, gitSchemaSecret, gitRefusedKey,
		c.httpsSource(gitSchemaPath, gitElsewhereCredential))), "create PtahSchema %s", gitRefusedSchema)
	refusal := c.refusedAtResolve(gitRefusedSchema, "not granted for the source's host")
	schema := c.onlyResolved(gitRefusedSchema)
	c.deleteAndWait(schema)
	c.logf("PASS a credential granted for another host was refused before Ptah ran: %s", refusal)
}

// refusedAtResolve waits until a schema reports that its reconciliation
// failed with a message that names want, and returns the message.
func (c *gitRun) refusedAtResolve(name, want string) string {
	c.t.Helper()
	var refusal string
	c.wait(name+" to be refused for "+want, func() (bool, string) {
		schema := c.schemaNamed(name)
		failed := meta.FindStatusCondition(schema.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed)
		if failed == nil || failed.Status != metav1.ConditionTrue {
			return false, string(schema.Status.Phase)
		}
		refusal = failed.Message
		return strings.Contains(refusal, want), refusal
	})
	return refusal
}

// onlyResolved holds a schema whose source was refused to having resolved
// nothing, planned nothing and run nothing but Resolve Jobs, of which one at
// least is left to examine.
func (c *gitRun) onlyResolved(name string) *ptahv1alpha1.PtahSchema {
	c.t.Helper()
	schema := c.schemaNamed(name)
	if schema.Status.Source.Git != nil || schema.Status.Plan != nil {
		c.fatalf("refused %s still resolved %+v or planned %+v", name, schema.Status.Source.Git, schema.Status.Plan)
	}
	jobs := &batchv1.JobList{}
	c.check(c.cluster.Client.List(c.ctx, jobs, client.InNamespace(c.in.TestNamespace), client.MatchingLabels{labelSchema: name}),
		"list the Jobs of %s", name)
	resolves := 0
	for index := range jobs.Items {
		if operation := jobs.Items[index].Labels[labelOperation]; operation != "resolve" {
			c.fatalf("refused %s ran a %s Job", name, operation)
		}
		resolves++
	}
	if resolves == 0 {
		c.fatalf("no Resolve Job of %s is left to examine", name)
	}
	return schema
}

// refusedSources names three sources Ptah refuses to read: a selected path
// that is a submodule, one that is a Git LFS pointer, and a server whose host
// key is not the one the known hosts pin. Each is refused at its Resolve with
// Ptah's reason, and nothing else runs.
func (c *gitRun) refusedSources() {
	c.t.Helper()
	otherHosts := gitSchemaSource{
		url: c.ssh, path: gitSchemaPath, credential: gitOtherHostsCredential, ref: map[string]any{"branch": "main"},
		transport: map[string]any{"knownHostsFrom": map[string]any{"name": gitOtherHostsConfigMap, "key": "known_hosts"}},
	}
	for _, row := range []struct {
		name   string
		source gitSchemaSource
		want   string
	}{
		{gitSubmoduleSchema, c.httpsSource(gitSubmodulePath, gitTokenCredential), "selected path is a submodule: " + gitSubmodulePath},
		{gitLFSSchema, c.httpsSource(gitLFSPath, gitTokenCredential), "selected path is a Git LFS pointer: " + gitLFSPath},
		{gitHostKeySchema, otherHosts, "knownhosts: key mismatch"},
	} {
		// The Resolve refuses before anything reads the database; a key of
		// their own keeps them from sharing the schema's coordination.
		c.check(c.create(c.schemaDocumentFor(row.name, gitSchemaSecret, gitRefusedKey, row.source)), "create PtahSchema %s", row.name)
		refusal := c.refusedAtResolve(row.name, row.want)
		c.deleteAndWait(c.onlyResolved(row.name))
		c.logf("PASS %s was refused at its Resolve: %s", row.name, refusal)
	}
}

// deploymentLogs is the latest log of a Deployment's Pod.
func deploymentLogs(ctx context.Context, cluster *harness.Cluster, namespace, name string) (string, error) {
	stdout, stderr, err := cluster.Kubectl(ctx, "-n", namespace, "logs", "deployment/"+name, "--tail=400")
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(stderr)))
	}
	return string(stdout), nil
}

// finish suspends the resources, so no read of theirs lands inside a later
// phase.
func (c *gitRun) finish() {
	c.t.Helper()
	for _, object := range []client.Object{
		&ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: gitSchema}},
		&ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: gitPinnedSchema}},
		&ptahv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Namespace: c.in.TestNamespace, Name: gitMigration}},
	} {
		patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"suspend":true}}`))
		c.check(c.cluster.Client.Patch(c.ctx, object, patch, client.FieldOwner(harness.FieldOwner)), "suspend %s", object.GetName())
	}
}

// cleanup prints what the resources decided and what the server logged when
// the phase failed. The statuses carry no SQL and no credential by contract,
// and the server logs refs and refusals, never the token.
func (c *gitRun) cleanup() {
	if c.cluster == nil || !c.parent.Failed() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, name := range []string{gitSchema, gitRefusedSchema, gitPinnedSchema, gitSubmoduleSchema, gitLFSSchema, gitHostKeySchema} {
		schema := &ptahv1alpha1.PtahSchema{}
		if c.cluster.Client.Get(ctx, c.key(name), schema) == nil {
			_, _ = fmt.Fprintf(os.Stderr, "e2e Git sources: %s status %+v\n", name, schema.Status)
		}
	}
	migration := &ptahv1alpha1.PtahMigration{}
	if c.cluster.Client.Get(ctx, c.key(gitMigration), migration) == nil {
		_, _ = fmt.Fprintf(os.Stderr, "e2e Git sources: migration status %+v\n", migration.Status)
	}
	if logs, err := deploymentLogs(ctx, c.cluster, c.in.TestNamespace, gitServer); err == nil {
		_, _ = fmt.Fprintf(os.Stderr, "e2e Git sources: server log\n%s\n", logs)
	}
}
