package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controllerwrite"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// A finished Apply is recorded only after the controller schedules its Job for
// cleanup, and on a cluster that write passes the controller-write webhook,
// which holds the Job to the exact annotation set the builder writes. The fake
// API server runs no admission and the other settlement tests hand-build their
// Jobs, so they kept passing while every cluster refused the cleanup and left
// the migration in Applying. This one builds the Job with the real builder and
// sends the cleanup through the real webhook.
func TestAFinishedMigrationApplySettlesThroughTheControllerWriteWebhook(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	builder := workloadBuilderForMigrations()
	job, pod := builtTerminalMigrationWorkload(t, builder, migration, plan)
	frame := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
		OperationID: operation.ID, ChildExitCode: 0,
		CoordinationDigest:   operation.CoordinationDigest,
		TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
		MigrationRun: &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract,
			Direction:       "up",
			Outcome:         dataplane.MigrationOutcomeApplied,
			Planned:         []int64{3},
			Applied:         []int64{3},
		},
	})
	reconciler, api := fakeMigrationReconciler(
		t, staticLogs{content: frame}, migration, plan, job, pod, verificationPolicyConfigMap(),
	)
	reconciler.Jobs = builder
	reconciler.Client = admittedJobWrites(api, builder)
	holdMigrationApplyLease(t, reconciler, api, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.LastRun == nil || actual.Status.LastRun.Outcome != operatorv1alpha1.MigrationRunOutcomeApplied {
		t.Fatalf("the finished Apply was not recorded: phase=%q lastRun=%#v", actual.Status.Phase, actual.Status.LastRun)
	}
	if actual.Status.ActiveOperation != nil {
		t.Fatal("the finished Apply claim was retained")
	}
	stored := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.TTLSecondsAfterFinished == nil || *stored.Spec.TTLSecondsAfterFinished != jobCleanupTTLSeconds {
		t.Fatalf("the harvested Job's cleanup TTL = %v, want %d", stored.Spec.TTLSecondsAfterFinished, jobCleanupTTLSeconds)
	}
}

// builtTerminalMigrationWorkload is the Apply Job the real builder writes for
// the claim, with the admission snapshot digested from its own Pod template,
// and the Pod the Job controller made from it.
func builtTerminalMigrationWorkload(
	t *testing.T,
	builder workload.Builder,
	migration *operatorv1alpha1.PtahMigration,
	plan *operatorv1alpha1.PtahMigrationPlan,
) (*batchv1.Job, *corev1.Pod) {
	t.Helper()

	operation := migration.Status.ActiveOperation
	operation.AdmissionSnapshot = nil
	unadmitted, err := builder.BuildMigration(migration, *operation, plan)
	if err != nil {
		t.Fatal(err)
	}
	templateDigest, err := podintent.DigestTemplate(&unadmitted.Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	ensureMigrationAdmissionSnapshot(migration)
	snapshot := operation.AdmissionSnapshot
	snapshot.TemplateDigest = templateDigest
	snapshot.Digest = ""
	if snapshot.Digest, err = fingerprint.DigestCanonicalJSON(*snapshot); err != nil {
		t.Fatal(err)
	}
	job, err := builder.BuildMigration(migration, *operation, plan)
	if err != nil {
		t.Fatal(err)
	}

	job.UID = types.UID("job-uid")
	job.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{
		batchv1.ControllerUidLabel: string(job.UID),
	}}
	job.Spec.Template.Labels[batchv1.ControllerUidLabel] = string(job.UID)
	job.Spec.Template.Labels[batchv1.JobNameLabel] = job.Name
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}

	// The hand-built workload supplies a terminal Pod's spec and status; its
	// identity is replaced with the one the built Job gives it.
	_, reference := terminalMigrationWorkload(migration, batchv1.JobComplete)
	operation.JobUID = job.UID
	pod := reference.DeepCopy()
	pod.Labels = map[string]string{}
	for key, value := range job.Spec.Template.Labels {
		pod.Labels[key] = value
	}
	pod.Annotations = map[string]string{}
	for key, value := range job.Spec.Template.Annotations {
		pod.Annotations[key] = value
	}
	pod.OwnerReferences = []metav1.OwnerReference{jobControllerReference(job)}
	// The Pod is the template as admission left it: the priority and the
	// default tolerations the snapshot records are all it added.
	pod.Spec = *job.Spec.Template.Spec.DeepCopy()
	pod.Spec.Priority = reference.Spec.Priority
	pod.Spec.PreemptionPolicy = reference.Spec.PreemptionPolicy
	pod.Spec.Tolerations = append(pod.Spec.Tolerations, reference.Spec.Tolerations...)
	return job, pod
}

// admittedJobWrites puts the controller-write webhook in front of every Job
// update and patch, the way the API server does. The patch is judged by the
// object it produces, which for the merge patches the controller sends is the
// object it was computed from.
func admittedJobWrites(api client.WithWatch, jobs controllerwrite.JobBuilder) client.WithWatch {
	const managerUsername = "system:serviceaccount:ptah-system:ptah-operator"
	handler := &controllerwrite.ValidationHandler{Validator: &controllerwrite.Validator{
		Reader: api, Jobs: jobs, ManagerUsername: managerUsername,
	}}
	admit := func(ctx context.Context, reader client.Reader, job *batchv1.Job) error {
		stored := &batchv1.Job{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(job), stored); err != nil {
			return err
		}
		typeMeta := metav1.TypeMeta{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job"}
		stored.TypeMeta = typeMeta
		submitted := job.DeepCopy()
		submitted.TypeMeta = typeMeta
		oldObject, err := json.Marshal(stored)
		if err != nil {
			return err
		}
		object, err := json.Marshal(submitted)
		if err != nil {
			return err
		}
		response := handler.Handle(ctx, cradmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "request-uid",
			Name:      job.Name,
			Namespace: job.Namespace,
			Operation: admissionv1.Update,
			UserInfo:  authenticationv1.UserInfo{Username: managerUsername},
			Resource:  metav1.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"},
			Kind:      metav1.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"},
			Object:    runtime.RawExtension{Raw: object},
			OldObject: runtime.RawExtension{Raw: oldObject},
		}})
		if response.Allowed {
			return nil
		}
		message := "denied"
		if response.Result != nil {
			message = response.Result.Message
		}
		return apierrors.NewForbidden(batchv1.Resource("jobs"), job.Name, errors.New(message))
	}
	return interceptor.NewClient(api, interceptor.Funcs{
		Patch: func(
			ctx context.Context,
			writer client.WithWatch,
			object client.Object,
			patch client.Patch,
			options ...client.PatchOption,
		) error {
			if job, ok := object.(*batchv1.Job); ok {
				if err := admit(ctx, writer, job); err != nil {
					return err
				}
			}
			return writer.Patch(ctx, object, patch, options...)
		},
		Update: func(
			ctx context.Context,
			writer client.WithWatch,
			object client.Object,
			options ...client.UpdateOption,
		) error {
			if job, ok := object.(*batchv1.Job); ok {
				if err := admit(ctx, writer, job); err != nil {
					return err
				}
			}
			return writer.Update(ctx, object, options...)
		},
	})
}
