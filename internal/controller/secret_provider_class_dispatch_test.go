package controller

import (
	"context"
	"reflect"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// classWorkloadBuilder is the Job builder the dispatch rows below build with,
// so a claim reaches the volumes the operator would mount.
func classWorkloadBuilder() workload.Builder {
	return workload.Builder{
		ExecutorImage:          "example.invalid/ptah@" + testDigest,
		RunnerImage:            testRunnerImage,
		PtahVersion:            "v0.3.0",
		ControllerImage:        testControllerImage,
		ControllerRevision:     testControllerRevision,
		ControllerStateVersion: testControllerStateVersion,
	}
}

// assertClassVolume requires the Job's volume name to be the read-only CSI
// volume of class, and the Ptah container to name the object's file in it.
func assertClassVolume(t *testing.T, job *batchv1.Job, volumeName, fileVariable, class, object string) {
	t.Helper()
	var volume *corev1.Volume
	for index := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[index].Name == volumeName {
			volume = &job.Spec.Template.Spec.Volumes[index]
		}
	}
	readOnly := true
	want := corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
		Driver: workload.SecretsStoreCSIDriver, ReadOnly: &readOnly,
		VolumeAttributes: map[string]string{workload.SecretProviderClassVolumeAttribute: class},
	}}
	if volume == nil || !reflect.DeepEqual(volume.VolumeSource, want) {
		t.Fatalf("volume %s = %#v, want the CSI volume of SecretProviderClass %s", volumeName, volume, class)
	}
	main := job.Spec.Template.Spec.Containers[0]
	var mountPath string
	for _, mount := range main.VolumeMounts {
		if mount.Name == volumeName {
			mountPath = mount.MountPath
		}
	}
	for _, variable := range main.Env {
		if variable.Name == fileVariable {
			if mountPath == "" || variable.Value != mountPath+"/"+object {
				t.Fatalf("%s = %q with %s mounted at %q, want the file %q in it", fileVariable, variable.Value, volumeName, mountPath, object)
			}
			return
		}
	}
	t.Fatalf("container %s does not name %s", main.Name, fileVariable)
}

// TestASchemaPlanClaimAndJobCarryItsClasses reconciles a schema whose target
// and development database come from SecretProviderClass objects from its
// Observe to its Plan's Job: the claim binds both objects, and the Job the
// builder makes from the claim mounts each class where the runner reads it.
// Nothing between the spec and the Job may drop a class: a claim that binds no
// URL builds no Job, and one that binds a Secret of no name mounts the wrong
// volume.
func TestASchemaPlanClaimAndJobCarryItsClasses(t *testing.T) {
	t.Parallel()

	target := &operatorv1alpha1.SecretProviderClassObjectSelector{Name: "orders-store", ObjectName: "url"}
	dev := &operatorv1alpha1.SecretProviderClassObjectSelector{Name: "orders-development-store", ObjectName: "dev-url"}
	policyBytes := "policy"
	schema := schemaFixture()
	schema.Spec.Target.URLFrom = nil
	schema.Spec.Target.URLFromSecretProviderClass = target.DeepCopy()
	schema.Spec.Dev = &operatorv1alpha1.DatabaseTargetRef{URLFromSecretProviderClass: dev.DeepCopy()}
	schema.Spec.Execution.ServiceAccountName = "orders-operations"
	schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseObserving
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
		ResolvedReference:        "oci://registry.example/team/schema@" + testDigest,
		Digest:                   testDigest,
		ArtifactType:             dataplane.SchemaArtifactType,
		Verified:                 true,
		VerificationPolicyUID:    testPolicyUID,
		VerificationPolicyDigest: fingerprint.DigestBytes([]byte(policyBytes)),
	}
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationObserve, ID: "observe-class",
		JobName: "observe-class-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
	}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            runner.OperationObserve,
		OperationID:          schema.Status.ActiveOperation.ID,
		ChildExitCode:        0,
		CoordinationDigest:   testCoordinationDigest,
		TargetIdentityDigest: testDigest,
		DriftReportDigest:    safetyOtherDigest,
		ObservedDialect:      "postgres",
		ObservedDrift:        true,
		HighestDriftSeverity: "safe",
	})
	policyConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: schema.Namespace, Name: schema.Spec.Desired.VerificationPolicyFrom.Name, UID: testPolicyUID,
		},
		Immutable: new(true),
		Data:      map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: policyBytes},
	}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Namespace: schema.Namespace, Name: "orders-operations", UID: "orders-operations-uid", ResourceVersion: "1",
	}}
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod, policyConfigMap, account)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() harvesting Observe error = %v", err)
	}
	reconciler.Jobs = classWorkloadBuilder()
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() claiming Plan error = %v", err)
	}
	claim := safetyGetSchema(t, api, schema).Status.ActiveOperation
	if claim == nil || claim.Type != operatorv1alpha1.OperationPlan {
		t.Fatalf("active operation = %#v, want a Plan claim", claim)
	}
	if claim.Target == nil || claim.Target.URLFrom != nil || claim.Target.URLFromSecretProviderClass == nil ||
		*claim.Target.URLFromSecretProviderClass != *target {
		t.Fatalf("the claim binds target %#v, want the class object %+v", claim.Target, *target)
	}
	if claim.ObservationDev == nil || claim.ObservationDev.URLFrom != nil || claim.ObservationDev.URLFromSecretProviderClass == nil ||
		*claim.ObservationDev.URLFromSecretProviderClass != *dev {
		t.Fatalf("the claim binds development database %#v, want the class object %+v", claim.ObservationDev, *dev)
	}

	for pass := 0; pass < 3 && !jobExists(t, api, schema.Namespace, claim.JobName); pass++ {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("Reconcile() dispatching Plan error = %v", err)
		}
	}
	created := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: schema.Namespace, Name: claim.JobName}, created); err != nil {
		t.Fatalf("the Plan claim's Job %s: %v; status %#v", claim.JobName, err, safetyGetSchema(t, api, schema).Status.Conditions)
	}
	assertClassVolume(t, created, "database-url", runner.EnvDatabaseURLFile, target.Name, target.ObjectName)
	assertClassVolume(t, created, "development-database-url", runner.EnvDevelopmentDatabaseURLFile, dev.Name, dev.ObjectName)
	if account := created.Spec.Template.Spec.ServiceAccountName; account != "orders-operations" {
		t.Fatalf("the Plan Job runs as %q, want the account the store trusts", account)
	}
}

func jobExists(t *testing.T, api client.Client, namespace, name string) bool {
	t.Helper()
	err := api.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &batchv1.Job{})
	return err == nil
}

// TestAMigrationHistoryClaimAndJobCarryItsClasses reconciles a migration
// whose target and drift-check shadow come from SecretProviderClass objects
// to its History Job: the claim binds the target's object, and the Job mounts
// both classes where the runner reads them.
func TestAMigrationHistoryClaimAndJobCarryItsClasses(t *testing.T) {
	t.Parallel()

	target := &operatorv1alpha1.SecretProviderClassObjectSelector{Name: "orders-store", ObjectName: "url"}
	shadow := &operatorv1alpha1.SecretProviderClassObjectSelector{Name: "orders-shadow-store", ObjectName: "shadow-url"}
	migration := migrationFixture()
	migration.Spec.Target.URLFrom = nil
	migration.Spec.Target.URLFromSecretProviderClass = target.DeepCopy()
	migration.Spec.Policy.DriftCheck = &operatorv1alpha1.MigrationDriftCheck{
		Shadow: operatorv1alpha1.DatabaseTargetRef{URLFromSecretProviderClass: shadow.DeepCopy()},
	}
	migration.Spec.Execution.ServiceAccountName = "orders-operations"
	// The manager reads a ConfigMap source itself, so the first claim it
	// makes is the history read.
	migration.Spec.Artifact = operatorv1alpha1.SourceSpec{ConfigMap: &operatorv1alpha1.ConfigMapSourceSpec{
		Name:  "app-schema",
		Items: []operatorv1alpha1.ConfigMapSourceItem{{Key: "0001.up.sql", Path: "0001_init.up.sql"}},
	}}
	source := sourceConfigMap("unused")
	source.Data = map[string]string{"0001.up.sql": "CREATE TABLE a (id int);\n"}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Namespace: migration.Namespace, Name: "orders-operations", UID: "orders-operations-uid", ResourceVersion: "1",
	}}
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, source, account)
	reconciler.Jobs = workloadBuilderForMigrations()

	var claim *operatorv1alpha1.MigrationOperationStatus
	for pass := 0; pass < 6; pass++ {
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		claim = readMigration(t, api, migration).Status.ActiveOperation
		if claim != nil && claim.JobName != "" && jobExists(t, api, migration.Namespace, claim.JobName) {
			break
		}
	}
	if claim == nil || claim.Type != operatorv1alpha1.MigrationOperationHistory {
		t.Fatalf("active operation = %#v, want a History claim", claim)
	}
	if claim.Target == nil || claim.Target.URLFrom != nil || claim.Target.URLFromSecretProviderClass == nil ||
		*claim.Target.URLFromSecretProviderClass != *target {
		t.Fatalf("the claim binds target %#v, want the class object %+v", claim.Target, *target)
	}
	created := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: migration.Namespace, Name: claim.JobName}, created); err != nil {
		t.Fatalf("the History claim's Job %s: %v; status %#v", claim.JobName, err, readMigration(t, api, migration).Status.Conditions)
	}
	assertClassVolume(t, created, "database-url", runner.EnvDatabaseURLFile, target.Name, target.ObjectName)
	assertClassVolume(t, created, "shadow-database-url", runner.EnvShadowDatabaseURLFile, shadow.Name, shadow.ObjectName)
	if account := created.Spec.Template.Spec.ServiceAccountName; account != "orders-operations" {
		t.Fatalf("the History Job runs as %q, want the account the store trusts", account)
	}
}
