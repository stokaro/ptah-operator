//go:build e2e

package e2e

import (
	"context"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// Publish an explicitly synthetic immutable image identity in this run's
// registry. The referenced executable and platform descriptors stay identical;
// the new manifest annotation changes the executor-image binding alone.
func (d *dataPlane) executorVariant(name string) string {
	d.t.Helper()
	repository, oldDigest, ok := strings.Cut(d.in.ExecutorImage, "@")
	host, _, hasPath := strings.Cut(repository, "/")
	if !ok || !hasPath || !sha256Pattern.MatchString(oldDigest) {
		d.fatalf("executor fixture needs the original digest-pinned image")
	}
	secret := &corev1.Secret{}
	d.check(d.get(registryAuthSecret, secret), "read the executor registry credential")
	copy := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: d.in.TestNamespace},
		Data: map[string][]byte{"registry": []byte(host), "username": secret.Data["username"], "password": secret.Data["password"]}}
	d.check(d.cluster.Client.Create(d.ctx, copy), "scope executor publication to its registry")
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: d.in.TestNamespace}, Spec: batchv1.JobSpec{
		BackoffLimit: ptr.To(int32(0)), ActiveDeadlineSeconds: ptr.To(int64(120)),
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: ptr.To(false),
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: registryPullSecret}},
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers: []corev1.Container{{Name: "publisher", Image: d.in.FixtureImage, ImagePullPolicy: corev1.PullIfNotPresent,
				Command: []string{"/e2e-handcraft-oci"}, Args: []string{"executor-variant", d.in.ExecutorImage},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
			}},
		}},
	}}
	for _, entry := range []struct{ name, key string }{{"PTAH_OCI_REGISTRY", "registry"}, {"PTAH_OCI_USERNAME", "username"}, {"PTAH_OCI_PASSWORD", "password"}} {
		job.Spec.Template.Spec.Containers[0].Env = append(job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: entry.name,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: entry.key}}})
	}
	d.check(d.cluster.Client.Create(d.ctx, job), "publish the executor identity fixture")
	published := d.waitForJob(name)
	if !publisherJobIsolation(published, d.in.FixtureImage, name) {
		d.fatalf("executor publication acquired an unexpected credential")
	}
	logs := d.jobLogs(published, "publisher")
	d.scan(logs, "executor fixture publication")
	updated, err := executorVariantReference(d.in.ExecutorImage, string(logs))
	d.check(err, "read the published executor identity")
	d.logf("executor identity fixture: original=%s replacement=%s", d.in.ExecutorImage, updated)
	return updated
}

// The installed Deployment uses Recreate. Holding status writes during this
// rollout prevents either manager from claiming the already admitted decision.
func setControllerExecutor(ctx context.Context, cluster *harness.Cluster, namespace, name, expected, replacement string) error {
	deployment := &appsv1.Deployment{}
	if err := cluster.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, deployment); err != nil {
		return err
	}
	before := deployment.DeepCopy()
	if err := replaceControllerExecutor(deployment, expected, replacement); err != nil {
		return err
	}
	if err := cluster.Client.Patch(ctx, deployment, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	return harness.Wait(ctx, "the installed manager to serve the replacement executor identity", 3*time.Minute, time.Second,
		func(ctx context.Context) (bool, string, error) {
			current := &appsv1.Deployment{}
			if err := cluster.Client.Get(ctx, client.ObjectKeyFromObject(deployment), current); err != nil {
				return false, "read manager Deployment", err
			}
			want := ptr.Deref(current.Spec.Replicas, 1)
			if current.Generation != deployment.Generation || current.Status.ObservedGeneration != current.Generation || want < 1 ||
				current.Status.UpdatedReplicas != want || current.Status.Replicas != want || current.Status.ReadyReplicas != want || current.Status.AvailableReplicas != want {
				return false, "waiting for all manager replicas at the patched generation", nil
			}
			return true, "all manager replicas ready", nil
		})
}

// Retain every old manager's complete log before Recreate removes its Pod.
// A successful rollout must replace every ready Pod and end each log stream
// naturally; a canceled stream cannot supply the credential-audit evidence.
func (f *faultRun) rolloutExecutor(expected, replacement string) {
	f.t.Helper()
	f.auditRuntime()
	rolloutExecutorManagers(f.t, f.ctx, f.cluster, f.operatorKey(f.controllerName), expected, replacement, f.scan)
	f.loadReadyManagerLeader("")
	f.auditRuntime()
}

func (f *faultRun) executorImageChanges() {
	f.t.Helper()
	replacement := f.executorVariant("e2e-executor-variant")
	original := f.in.ExecutorImage
	// Register recovery before changing the live Deployment. It also runs if
	// the rollout or a later proof fails; the task cluster remains inspectable.
	scenario := f.t
	scenario.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		if err := setControllerExecutor(ctx, f.cluster, f.in.OperatorNamespace, f.controllerName, replacement, original); err != nil {
			scenario.Errorf("restore the installed executor identity: %v", err)
		}
	})
	type proof struct {
		engine, name, database string
		window                 *schemaRefusalWindow
		resource               *ptahv1alpha1.PtahSchema
		plan                   *ptahv1alpha1.PtahSchemaPlan
		controls               []operationSQLClient
		before                 checkpoint
	}
	var cases []proof
	for _, engine := range []string{"postgresql", "mysql"} {
		name, database := "e2e-executor-change-"+engine, "e2e_executor_change"
		secret := name + "-db"
		kind, reference := "PostgreSQL", f.pgReference
		if engine == "mysql" {
			kind, reference = "MySQL", f.mysqlReference
		}
		f.createDatabase(engine, database, secret)
		f.query(engine, database, "INSERT INTO e2e_widgets (id, name, note) VALUES (701, 'executor-control', 'preserve-this-row')")
		window := f.startSchemaRefusalWindow(name, engine, database, secret)
		before := f.checkpointJobs(name, "")
		f.createSchema(faultSchema{name: name, engine: kind, reference: reference, secret: secret, coordinationKey: "e2e/executor-change/" + engine})
		resource := window.waitForSchema("the original executor's plan", planAwaitingApproval)
		plan := f.schemaPlan(resource.Status.Plan.Name)
		controls := []operationSQLClient{window.resultControl(resource, "observe", before), window.resultControl(resource, "plan", before)}
		cases = append(cases, proof{engine: engine, name: name, database: database, window: window, resource: resource, plan: plan, controls: controls, before: f.checkpointJobs(name, "")})
	}
	f.pauseStatusWrites()
	for _, row := range cases {
		f.createApproval(row.name, row.name+"-old")
		if f.schema(row.name).Status.ActiveOperation != nil {
			f.fatalf("executor transition lost the undispatched approval boundary")
		}
		f.assertNoNewJobs(row.name, "apply", row.before)
	}
	f.rolloutExecutor(original, replacement)
	f.mustResumeStatusWrites("allow reconciliation under the changed executor")
	for _, row := range cases {
		current := row.window.waitForSchema("a new executor-bound approval gate", func(resource *ptahv1alpha1.PtahSchema) bool {
			return planAwaitingApproval(resource) && resource.Status.Plan.UID != row.plan.UID
		})
		fresh := f.schemaPlan(current.Status.Plan.Name)
		f.check(changedSchemaExecutorDecision(row.resource, current, row.plan, fresh, original, replacement), "bind the replacement decision to the changed executor only")
		f.waitForApproval(row.name+"-old", "the old executor approval to become stale", func(approval *ptahv1alpha1.PtahSchemaApproval) bool {
			return conditionIs(approval.Status.Conditions, "Stale", "True", "ExecutionBindingChanged") && !conditionStatus(approval.Status.Conditions, "Consumed", "True")
		})
		f.assertNoNewJobs(row.name, "apply", row.before)
		observe := row.window.resultControl(current, "observe", row.before)
		plan := row.window.resultControl(current, "plan", row.before)
		row.window.assert(current, append(row.controls, observe, plan)...)
		f.assertColumn(row.engine, row.database, "fault_token", 0)
		beforeSQL, beforeApply := row.window.audit.snapshot(), f.checkpointJobs(row.name, "apply")
		f.createApproval(row.name, row.name+"-current")
		f.waitForApprovedPlanConverged(row.name, fresh.Spec.ArtifactDigest, fresh.Spec.Fingerprint, string(fresh.UID), "the newly authorized executor to converge")
		result := f.captureOneNewJobResult(row.name, "apply", beforeApply, nil)
		f.check(automaticApplyResult(result, fresh.Spec.ContentDigest, fresh.Spec.CoordinationDigest, fresh.Spec.TargetIdentityDigest), "verify the replacement executor's exact plan")
		job := &batchv1.Job{}
		f.check(f.get(f.captured.jobName, job), "read the replacement executor's Apply Job")
		if job.UID != types.UID(f.captured.jobUID) || !jobUsesExecutor(job, replacement) {
			f.fatalf("the fresh Apply did not run the replacement executor image")
		}
		row.window.audit.assertRecords(beforeSQL, row.window.audit.snapshot(), row.window.audit.terminalPod(map[string]string{"job-name": job.Name}, string(job.UID)), true)
		row.window.audit.close()
		f.dataPlane.assertApprovalConsumed(row.name+"-current", string(fresh.UID))
		f.assertOneNewJob(row.name, "apply", beforeApply)
		f.assertColumn(row.engine, row.database, "fault_token", 1)
		if f.query(row.engine, row.database, "SELECT count(*) FROM e2e_widgets WHERE id=701 AND name='executor-control' AND note='preserve-this-row'") != "1" ||
			f.query(row.engine, row.database, "SELECT count(*) FROM e2e_widgets") != "1" {
			f.fatalf("executor transition changed the preserved row")
		}
		f.logf("PASS %s executor identity changed after approval: old epoch refused, new approval applied", row.engine)
	}
	f.rolloutExecutor(replacement, original)
	f.auditRuntimeCredentials()
	f.assertObservedJobsAudited()
}
