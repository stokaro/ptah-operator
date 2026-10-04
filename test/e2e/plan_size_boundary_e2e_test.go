//go:build e2e

package e2e

import (
	"fmt"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/plancontract"
)

// Generate a real SQL artifact inside a credential-free init container. A
// ConfigMap cannot carry the boundary source, so the publisher shares only
// the generated file and obtains only its registry credential.
func (d *dataPlane) publishPlanSizeSchema(engine, revision, reference string, repeated, suffix int) string {
	d.t.Helper()
	job := planSizePublisherJob(d.in.TestNamespace, d.in.FixtureImage, d.in.ExecutorImage, engine, revision, reference, repeated, suffix)
	name := job.Name
	d.check(d.cluster.Client.Create(d.ctx, job), "publish the exact-size native schema fixture")
	published := d.waitForJob(name)
	init := published.Spec.Template.Spec.InitContainers
	copy := published.DeepCopy()
	copy.Spec.Template.Spec.InitContainers = nil
	if len(init) != 1 || init[0].Image != d.in.FixtureImage || len(init[0].Env) != 0 || len(init[0].EnvFrom) != 0 ||
		len(init[0].VolumeMounts) != 1 || init[0].VolumeMounts[0].Name != "schema" ||
		!publisherJobIsolation(copy, d.in.ExecutorImage, registryAuthSecret) {
		d.fatalf("the plan-size publisher acquired an unexpected credential")
	}
	logs := d.jobLogs(published, "publisher")
	d.scan(logs, "plan-size schema publication")
	digest := ""
	for line := range strings.SplitSeq(string(logs), "\n") {
		if match := publishedDigest.FindStringSubmatch(line); match != nil {
			digest = match[1]
		}
	}
	if !sha256Pattern.MatchString(digest) {
		d.fatalf("the native plan-size artifact has no published digest")
	}
	return digest
}

func planSizePublisherJob(namespace, fixtureImage, executorImage, engine, revision, reference string, repeated, suffix int) *batchv1.Job {
	name := "e2e-push-size-" + engine + "-" + revision
	security := &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
		Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	budget := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("256Mi")},
	}
	labels := map[string]string{"app.kubernetes.io/component": "e2e-schema-publisher"}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}, Spec: batchv1.JobSpec{
		BackoffLimit: ptr.To(int32(0)), ActiveDeadlineSeconds: ptr.To(int64(300)),
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: ptr.To(false),
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: registryPullSecret}},
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532)), FSGroup: ptr.To(int64(65532)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			InitContainers: []corev1.Container{{Name: "generate", Image: fixtureImage, ImagePullPolicy: corev1.PullIfNotPresent,
				Command: []string{"/e2e-handcraft-oci"}, Args: []string{"plan-size-schema", engine, strconv.Itoa(repeated), strconv.Itoa(suffix), "/schema/schema.sql"},
				Resources: budget, SecurityContext: security, VolumeMounts: []corev1.VolumeMount{{Name: "schema", MountPath: "/schema"}},
			}},
			Containers: []corev1.Container{{Name: "publisher", Image: executorImage, ImagePullPolicy: corev1.PullIfNotPresent,
				Command: []string{"/usr/local/bin/ptah"}, Args: []string{"schema", "push", reference, "--schema-file", "/schema/schema.sql", "--dialect", engine, "--version", revision, "--plain-http"},
				Resources: budget, SecurityContext: security, Env: []corev1.EnvVar{{Name: "HOME", Value: "/work"}, {Name: "TMPDIR", Value: "/work"}},
				VolumeMounts: []corev1.VolumeMount{{Name: "schema", MountPath: "/schema", ReadOnly: true}, {Name: "work", MountPath: "/work"}},
			}},
			Volumes: []corev1.Volume{{Name: "schema", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("8Mi"))}}},
				{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("64Mi"))}}}},
		}},
	}}
	for _, entry := range []struct{ name, key string }{{"PTAH_OCI_REGISTRY", "registry"}, {"PTAH_OCI_USERNAME", "username"}, {"PTAH_OCI_PASSWORD", "password"}} {
		job.Spec.Template.Spec.Containers[0].Env = append(job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: entry.name,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: registryAuthSecret}, Key: entry.key}}})
	}
	return job
}

func (d *dataPlane) planSizeBoundaries() {
	d.t.Helper()
	f := &faultRun{dataPlane: d}
	for _, engine := range []string{"postgresql", "mysql"} {
		dialect, kind := "postgres", "PostgreSQL"
		if engine == "mysql" {
			dialect, kind = "mysql", "MySQL"
		}
		type reading struct {
			schema, database string
			resource         *ptahv1alpha1.PtahSchema
			plan             *ptahv1alpha1.PtahSchemaPlan
			window           *schemaRefusalWindow
		}
		create := func(revision string, repeated, suffix int, oversized bool) reading {
			name := "e2e-size-" + engine + "-" + revision
			// Reach the accepted resource-name bound through actual Jobs,
			// immutable chunks, projections and Apply, not storage alone.
			name += strings.Repeat("s", 63-len(name))
			database := "e2e_size_" + engine + "_" + revision
			secret := "e2e-size-db-" + engine + "-" + revision
			if engine == "postgresql" {
				f.query(engine, "postgres", "CREATE DATABASE "+database)
			} else {
				f.query(engine, "mysql", "CREATE DATABASE "+database+"; GRANT ALL PRIVILEGES ON "+database+".* TO '"+mysqlUser+"'@'%'; FLUSH PRIVILEGES")
			}
			f.createURLSecret(engine, database, secret, false)
			reference := d.registryReference("size-" + engine + "-" + revision)
			digest := d.publishPlanSizeSchema(dialect, revision, reference, repeated, suffix)
			window := d.startSchemaRefusalWindow(name, engine, database, secret)
			before := d.checkpointJobs(name, "")
			budget := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
			}
			transactionMode := "file"
			if engine == "mysql" {
				transactionMode = "none"
			}
			d.createSchemaResource(schemaResource{name: name, engine: kind, secret: secret, reference: reference,
				coordinationKey: "e2e/plan-size/" + engine + "/" + revision, interval: "1h", failureRetry: "1h", resources: budget, transactionMode: transactionMode})
			if oversized {
				current := window.waitForSchema("the exact oversized native plan refusal", func(s *ptahv1alpha1.PtahSchema) bool {
					return s.Status.Source.Digest == digest && s.Status.Source.Verified && s.Status.Phase == ptahv1alpha1.PhaseFailed &&
						s.Status.Plan == nil && conditionIs(s.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed, metav1.ConditionTrue, "OperationFailed")
				})
				observe := window.resultControl(current, "observe", before)
				result := d.captureOneNewJobResult(name, "plan", before, nil)
				d.check(oversizedNativePlanRefused(current, result, digest), "refuse the exact native 8 MiB + 1 byte output")
				failed := operationSQLClient{resourceUID: string(current.UID), jobUID: d.captured.jobUID, podUID: d.captured.podUID, operation: "plan"}
				window.assert(current, observe, failed)
				d.assertNoNewJobs(name, "apply", before)
				plans := &ptahv1alpha1.PtahSchemaPlanList{}
				d.mustList(plans)
				for _, plan := range plans.Items {
					if plan.Spec.SchemaRef.UID == current.UID {
						d.fatalf("the oversized Plan published immutable executable storage")
					}
				}
				chunks := &ptahv1alpha1.PtahSchemaPlanChunkList{}
				d.mustList(chunks, client.MatchingLabels{labelSchema: name})
				if len(chunks.Items) != 0 || planSizeTableCount(f, engine, database) != "0" {
					d.fatalf("the oversized native Plan left chunks or database effects")
				}
				d.logf("PASS %s exact 8 MiB + 1 byte native Plan refused before publication or Apply", engine)
				return reading{}
			}
			current := window.waitForSchema("the real native approval gate", planAwaitingApproval)
			d.assertPlan(name, reference, digest, dialect, false, before, before, false)
			plan := d.schemaPlan(current.Status.Plan.Name)
			planClient := operationSQLClient{resourceUID: string(current.UID), jobUID: d.captured.jobUID, podUID: d.captured.podUID, operation: "plan"}
			observe := window.resultControl(current, "observe", before)
			window.assert(current, observe, planClient)
			if planSizeTableCount(f, engine, database) != "0" {
				d.fatalf("the exact-size plan executed SQL before approval")
			}
			return reading{schema: name, database: database, resource: current, plan: plan, window: window}
		}
		small := create("small", 32, 0, false)
		plus := create("plus", 33, 0, false)
		ascii := create("ascii", 32, 1, false)
		calibration, err := calibratePlanSize(int(small.plan.Spec.Size), int(plus.plan.Spec.Size), int(ascii.plan.Spec.Size))
		d.check(err, "measure the pinned native plan serializer on the operational path")
		repeated, suffix, err := calibration.lengths(int(plancontract.MaxExecutableBytes) + 1)
		d.check(err, "derive the exact native refusal boundary")
		create("oversize", repeated, suffix, true)
		repeated, suffix, err = calibration.lengths(int(plancontract.MaxExecutableBytes))
		d.check(err, "derive the exact native executable boundary")
		maximum := create("maximum", repeated, suffix, false)
		document, _ := d.rebuildPlanDocument(maximum.plan)
		d.check(executablePlanAtLimit(maximum.plan, document, dialect, repeated, suffix), "bind the complete 16-chunk maximum plan to its real native SQL")
		beforeApply := d.checkpointJobs(maximum.schema, "apply")
		beforeSQL := maximum.window.audit.snapshot()
		approval := "e2e-size-" + engine + "-approved"
		d.createExactApproval(maximum.schema, maximum.plan.Name, approval, "e2e/plan-size/"+engine+"/maximum", maximum.plan.Spec.CoordinationDigest)
		d.waitForApprovedPlanConverged(maximum.schema, maximum.plan.Spec.ArtifactDigest, maximum.plan.Spec.Fingerprint, string(maximum.plan.UID), "the exact 8 MiB approved native plan to converge")
		result := d.captureOneNewJobResult(maximum.schema, "apply", beforeApply, nil)
		d.check(automaticApplyResult(result, maximum.plan.Spec.ContentDigest, maximum.plan.Spec.CoordinationDigest, maximum.plan.Spec.TargetIdentityDigest), "read the exact maximum-plan Apply")
		maximum.window.audit.assertRecords(beforeSQL, maximum.window.audit.snapshot(),
			maximum.window.audit.terminalPod(map[string]string{"job-name": d.captured.jobName}, d.captured.jobUID), true)
		maximum.window.audit.close()
		d.assertPlanProjected(maximum.plan)
		d.assertApprovalConsumed(approval, string(maximum.plan.UID))
		d.assertOneNewJob(maximum.schema, "apply", beforeApply)
		verifyPlanSizeDefaults(f, engine, maximum.database, repeated, suffix)
		d.logf("PASS %s exact 8 MiB native plan: 16 full chunks, exact projections, one approved Apply and verified database defaults", engine)
	}
}

func planSizeTableCount(f *faultRun, engine, database string) string {
	query := "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name LIKE 'e2e_plan_size_limit%'"
	if engine == "mysql" {
		query = "SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name LIKE 'e2e_plan_size_limit%'"
	}
	return f.query(engine, database, query)
}

func verifyPlanSizeDefaults(f *faultRun, engine, database string, repeated, suffix int) {
	f.t.Helper()
	if engine == "mysql" {
		actual := f.query(engine, database, "SELECT CONCAT(COUNT(*), ':', SUM(CHAR_LENGTH(column_default)), ':', SUM(CHAR_LENGTH(column_default)-CHAR_LENGTH(REPLACE(column_default,'<','')))) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name LIKE 'e2e_plan_size_limit_%' AND column_name='payload'")
		if actual != fmt.Sprintf("100:%d:%d", repeated+suffix, repeated) || planSizeTableCount(f, engine, database) != "100" {
			f.fatalf("the maximum MySQL Apply did not persist every exact native default")
		}
		return
	}
	f.query(engine, database, "INSERT INTO e2e_plan_size_limit (id) VALUES (1)")
	actual := f.query(engine, database, "SELECT length(payload)::text || ':' || length(replace(payload,'x',''))::text || ':' || length(replace(payload,'<',''))::text FROM e2e_plan_size_limit WHERE id=1")
	if actual != fmt.Sprintf("%d:%d:%d", repeated+suffix, repeated, suffix) || planSizeTableCount(f, engine, database) != "1" {
		f.fatalf("the maximum PostgreSQL Apply did not create its exact executable default")
	}
}
