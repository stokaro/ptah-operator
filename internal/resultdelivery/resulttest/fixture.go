// Package resulttest shares production Job fixtures for delivery component tests.
package resulttest

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/workload"
)

type Fixture struct {
	Identity resultdelivery.Identity
	Subject  client.Object
	Job      *batchv1.Job
	Pod      *corev1.Pod
}

// These inputs are independently held to the production builders by workload's
// TestOperationJobsMatchTheirGoldenFiles. Only API-assigned identity and modeled
// admission defaults are added here, not a second executable Job definition.
func New(t *testing.T, name string) *Fixture {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var data []byte
	for {
		data, err = os.ReadFile(filepath.Join(directory, "internal", "workload", "testdata", "jobs", name+".json"))
		if err == nil {
			break
		}
		parent := filepath.Dir(directory)
		if !os.IsNotExist(err) || parent == directory {
			t.Fatal(err)
		}
		directory = parent
	}
	job := &batchv1.Job{}
	if err := json.Unmarshal(data, job); err != nil {
		t.Fatal(err)
	}
	if err := jobconfig.Attach(job, job.OwnerReferences[0].UID, 2, job.Annotations[workload.AnnotationOperationID], "https://receiver.operator.svc:9444"); err != nil {
		t.Fatal(err)
	}
	job.UID = "job-uid"
	job.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{batchv1.ControllerUidLabel: string(job.UID)}}
	for key, value := range map[string]string{batchv1.ControllerUidLabel: string(job.UID), "controller-uid": string(job.UID), batchv1.JobNameLabel: job.Name, "job-name": job.Name} {
		job.Spec.Template.Labels[key] = value
	}
	digest, err := podintent.DigestTemplate(&job.Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &api.PodAdmissionSnapshot{Version: podintent.SnapshotVersion, TemplateDigest: digest,
		ServiceAccount: api.ServiceAccountAdmissionSnapshot{Object: api.AdmissionObjectBinding{Name: job.Spec.Template.Spec.ServiceAccountName, UID: "sa-uid", ResourceVersion: "1"}},
		PriorityClass:  api.PriorityClassAdmissionSnapshot{PreemptionPolicy: ptr.To(corev1.PreemptLowerPriority)},
	}
	if runtimeClass := job.Spec.Template.Spec.RuntimeClassName; runtimeClass != nil {
		snapshot.RuntimeClass = &api.RuntimeClassAdmissionSnapshot{Object: api.AdmissionObjectBinding{Name: *runtimeClass, UID: "runtime-uid", ResourceVersion: "1"}, Handler: "runsc"}
	}
	if priority := job.Spec.Template.Spec.PriorityClassName; priority != "" {
		snapshot.PriorityClass.Name = priority
		snapshot.PriorityClass.Value = 100
		snapshot.PriorityClass.Object = &api.AdmissionObjectBinding{Name: priority, UID: "priority-uid", ResourceVersion: "1"}
	}
	snapshot.Digest, err = fingerprint.DigestCanonicalJSON(*snapshot)
	if err != nil {
		t.Fatal(err)
	}
	job.Annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshot.Digest
	job.Spec.Template.Annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshot.Digest
	prefix := job.Name + "-"
	if len(prefix) > 58 {
		prefix = prefix[:58]
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: prefix + "abcde", GenerateName: job.Name + "-", UID: "pod-uid", Labels: maps.Clone(job.Spec.Template.Labels), Annotations: maps.Clone(job.Spec.Template.Annotations), OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}}, Spec: *job.Spec.Template.Spec.DeepCopy()}
	pod.Spec.Priority = ptr.To(snapshot.PriorityClass.Value)
	pod.Spec.PreemptionPolicy = snapshot.PriorityClass.PreemptionPolicy
	owner := job.OwnerReferences[0]
	b := resultstore.Binding{Namespace: job.Namespace, Kind: owner.Kind, Name: owner.Name, UID: owner.UID, Generation: 2, ExecutionBindingID: job.Annotations[workload.AnnotationExecutionBindingID], InputFingerprint: job.Annotations[workload.AnnotationInputFingerprint], OperationID: job.Annotations[workload.AnnotationOperationID], JobName: job.Name, JobUID: job.UID, PodName: pod.Name, PodUID: pod.UID}
	binding := &api.ExecutionBindingStatus{Epoch: b.ExecutionBindingID, ControllerStateVersion: 1, PtahVersion: job.Annotations[workload.AnnotationPtahVersion]}
	var target *api.DatabaseTargetBinding
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "PTAH_EXPECTED_DATABASE_ENGINE" {
			target = &api.DatabaseTargetBinding{Engine: api.DatabaseEngine(env.Value)}
		}
	}
	meta := metav1.ObjectMeta{Namespace: b.Namespace, Name: b.Name, UID: b.UID, Generation: b.Generation}
	operation := job.Labels[workload.LabelOperation]
	operation = strings.ToUpper(operation[:1]) + operation[1:]
	f := &Fixture{Job: job, Pod: pod}
	var claim jobclaim.Claim
	if owner.Kind == "PtahSchema" {
		op := &api.ActiveOperationStatus{Type: api.OperationType(operation), ID: b.OperationID, InputFingerprint: b.InputFingerprint, JobName: b.JobName, JobUID: b.JobUID, ExecutionBindingID: b.ExecutionBindingID, AdmissionSnapshot: snapshot, Target: target}
		semantics := mutationlifecycle.SchemaOperation(op.Type)
		op.DispatchStarted = semantics.Mutating
		b.Operation = string(semantics.Runner)
		subject := &api.PtahSchema{ObjectMeta: meta, Status: api.PtahSchemaStatus{ActiveOperation: op, ExecutionBinding: binding}}
		if op.Type == api.OperationApply {
			subject.Status.Plan = &api.CurrentPlanStatus{Name: "plan", UID: "plan-uid", Fingerprint: job.Annotations[workload.AnnotationPlanFingerprint], ContentDigest: job.Annotations[workload.AnnotationPlanContentDigest], ExecutionBindingID: b.ExecutionBindingID, PtahVersion: binding.PtahVersion, ControllerStateVersion: 1}
		}
		f.Subject = subject
		claim = jobclaim.SchemaOperation(subject, op)
	} else {
		op := &api.MigrationOperationStatus{Type: api.MigrationOperationType(operation), ID: b.OperationID, InputFingerprint: b.InputFingerprint, JobName: b.JobName, JobUID: b.JobUID, ExecutionBindingID: b.ExecutionBindingID, AdmissionSnapshot: snapshot, Target: target}
		semantics := mutationlifecycle.MigrationOperation(op.Type)
		op.DispatchStarted = semantics.Mutating
		b.Operation = string(semantics.Runner)
		subject := &api.PtahMigration{ObjectMeta: meta, Status: api.PtahMigrationStatus{ActiveOperation: op, ExecutionBinding: binding}}
		f.Subject = subject
		claim = jobclaim.MigrationOperation(subject, op)
	}
	f.Identity = resultdelivery.Identity{Binding: b}
	if target != nil {
		f.Identity.Engine = strings.ToLower(string(target.Engine))
	}
	claim.Binding = binding
	claim.Stored = true
	if err := jobclaim.Match(job, claim); err != nil {
		t.Fatalf("golden Job Fixture: %v", err)
	}
	if err := podintent.ValidateActiveJob(job); err != nil {
		t.Fatalf("Job envelope Fixture: %v", err)
	}
	if err := podintent.ValidateStoredPod(pod, job, snapshot); err != nil {
		t.Fatalf("admitted Pod Fixture: %v", err)
	}
	return f
}

func (f *Fixture) Client(t *testing.T, extra ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, batchv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(append([]client.Object{f.Subject, f.Job, f.Pod}, extra...)...).Build()
}
