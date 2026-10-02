package resultauthority

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/workload"
)

type fixture struct {
	identity resultdelivery.Identity
	subject  client.Object
	job      *batchv1.Job
	pod      *corev1.Pod
}

// These inputs are independently held to the production builders by workload's
// TestOperationJobsMatchTheirGoldenFiles. Only API-assigned identity and modeled
// admission defaults are added here, not a second executable Job definition.
func newFixture(t *testing.T, name string) *fixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "workload", "testdata", "jobs", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	if err := json.Unmarshal(data, job); err != nil {
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
	f := &fixture{job: job, pod: pod}
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
		f.subject = subject
		claim = jobclaim.SchemaOperation(subject, op)
	} else {
		op := &api.MigrationOperationStatus{Type: api.MigrationOperationType(operation), ID: b.OperationID, InputFingerprint: b.InputFingerprint, JobName: b.JobName, JobUID: b.JobUID, ExecutionBindingID: b.ExecutionBindingID, AdmissionSnapshot: snapshot, Target: target}
		semantics := mutationlifecycle.MigrationOperation(op.Type)
		op.DispatchStarted = semantics.Mutating
		b.Operation = string(semantics.Runner)
		subject := &api.PtahMigration{ObjectMeta: meta, Status: api.PtahMigrationStatus{ActiveOperation: op, ExecutionBinding: binding}}
		f.subject = subject
		claim = jobclaim.MigrationOperation(subject, op)
	}
	f.identity = resultdelivery.Identity{Binding: b}
	if target != nil {
		f.identity.Engine = strings.ToLower(string(target.Engine))
	}
	claim.Binding = binding
	claim.Stored = true
	if err := jobclaim.Match(job, claim); err != nil {
		t.Fatalf("golden Job fixture: %v", err)
	}
	if err := podintent.ValidateActiveJob(job); err != nil {
		t.Fatalf("Job envelope fixture: %v", err)
	}
	if err := podintent.ValidateStoredPod(pod, job, snapshot); err != nil {
		t.Fatalf("admitted Pod fixture: %v", err)
	}
	return f
}

func (f *fixture) reader(t *testing.T, extra ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, batchv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(append([]client.Object{f.subject, f.job, f.pod}, extra...)...).Build()
}

func TestAllOperationJobs(t *testing.T) {
	for _, name := range []string{"schema-resolve", "schema-verify-admitted", "schema-observe", "schema-plan-dev-fence-scheduling", "schema-apply-admitted-scheduling", "migration-resolve", "migration-verify-admitted", "migration-history", "migration-apply-admitted-scheduling"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, name)
			if err := (Authorizer{Reader: f.reader(t)}).Check(t.Context(), f.identity); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRefusals(t *testing.T) {
	cases := map[string]func(*fixture){
		"subject UID":     func(f *fixture) { f.subject.SetUID("replacement") },
		"generation":      func(f *fixture) { f.subject.SetGeneration(3) },
		"retired claim":   func(f *fixture) { f.subject.(*api.PtahSchema).Status.ActiveOperation = nil },
		"binding removed": func(f *fixture) { f.subject.(*api.PtahSchema).Status.ExecutionBinding = nil },
		"binding rotated": func(f *fixture) {
			f.subject.(*api.PtahSchema).Status.ExecutionBinding.Epoch = "v1-44444444444444444444444444444444"
		},
		"operation ID": func(f *fixture) { f.subject.(*api.PtahSchema).Status.ActiveOperation.ID = "replacement" },
		"input fingerprint": func(f *fixture) {
			f.subject.(*api.PtahSchema).Status.ActiveOperation.InputFingerprint = "sha256:" + strings.Repeat("a", 64)
		},
		"operation kind":        func(f *fixture) { f.subject.(*api.PtahSchema).Status.ActiveOperation.Type = api.OperationObserve },
		"claim Job UID":         func(f *fixture) { f.subject.(*api.PtahSchema).Status.ActiveOperation.JobUID = "replacement" },
		"claim Job name":        func(f *fixture) { f.subject.(*api.PtahSchema).Status.ActiveOperation.JobName = "replacement" },
		"engine":                func(f *fixture) { f.identity.Engine = "mysql" },
		"dispatch not recorded": func(f *fixture) { f.subject.(*api.PtahSchema).Status.ActiveOperation.DispatchStarted = false },
		"lease continuity lost": func(f *fixture) { f.subject.(*api.PtahSchema).Status.ActiveOperation.LeaseContinuityLost = true },
		"Job UID":               func(f *fixture) { f.job.UID = "replacement" },
		"Job owner":             func(f *fixture) { f.job.OwnerReferences[0].UID = "replacement" },
		"Job cleanup":           func(f *fixture) { f.job.Spec.TTLSecondsAfterFinished = ptr.To(int32(300)) },
		"Job retries":           func(f *fixture) { f.job.Spec.BackoffLimit = ptr.To(int32(1)) },
		"Job template":          func(f *fixture) { f.job.Spec.Template.Spec.Containers[0].Image = "other" },
		"Pod UID":               func(f *fixture) { f.pod.UID = "replacement" },
		"Pod owner":             func(f *fixture) { f.pod.OwnerReferences[0].UID = "replacement" },
		"Pod name chain":        func(f *fixture) { f.pod.GenerateName = "other-" },
		"Pod command":           func(f *fixture) { f.pod.Spec.Containers[0].Command = []string{"other"} },
		"Pod service account":   func(f *fixture) { f.pod.Spec.ServiceAccountName = "other" },
		"Pod metadata":          func(f *fixture) { f.pod.Annotations[workload.AnnotationOperationID] = "other" },
		"Pod snapshot": func(f *fixture) {
			f.subject.(*api.PtahSchema).Status.ActiveOperation.AdmissionSnapshot.Digest = "invalid"
		},
		"certificate generation": func(f *fixture) { f.identity.Binding.Generation = 0 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "schema-apply-admitted-scheduling")
			change(f)
			if err := (Authorizer{Reader: f.reader(t)}).Check(t.Context(), f.identity); !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatalf("got %v, want authority refusal", err)
			}
		})
	}
}

func TestAdoptionAndLateOutcome(t *testing.T) {
	for _, name := range []string{"schema-apply-admitted-scheduling", "migration-apply-admitted-scheduling"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, name)
			expired := metav1.NewTime(time.Now().Add(-time.Hour))
			switch subject := f.subject.(type) {
			case *api.PtahSchema:
				subject.Status.ActiveOperation.ExecutionNotAfter = &expired
			case *api.PtahMigration:
				subject.Status.ActiveOperation.ExecutionNotAfter = &expired
			}
			f.pod.DeletionTimestamp = &expired
			f.pod.Finalizers = []string{"test.ptah.run/hold"}
			if err := (Authorizer{Reader: f.reader(t)}).Check(t.Context(), f.identity); err != nil {
				t.Fatalf("late outcome from original terminating Pod: %v", err)
			}
			switch subject := f.subject.(type) {
			case *api.PtahSchema:
				subject.Status.ActiveOperation.JobUID = ""
			case *api.PtahMigration:
				subject.Status.ActiveOperation.JobUID = ""
			}
			if err := (Authorizer{Reader: f.reader(t)}).Check(t.Context(), f.identity); !errors.Is(err, ErrNotReady) {
				t.Fatalf("unadopted Job: %v", err)
			}
		})
	}
}

func TestSecondPodRefused(t *testing.T) {
	f := newFixture(t, "migration-history")
	other := f.pod.DeepCopy()
	other.Name = f.job.Name + "-fghij"
	other.UID = "other-pod"
	if err := (Authorizer{Reader: f.reader(t, other)}).Check(t.Context(), f.identity); !errors.Is(err, resultdelivery.ErrAuthority) {
		t.Fatal(err)
	}
}

type readerHook struct {
	client.Reader
	get  func(client.Object) error
	list func(*corev1.PodList) error
}

func (r readerHook) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := r.Reader.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if r.get != nil {
		return r.get(obj)
	}
	return nil
}
func (r readerHook) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := r.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	if r.list != nil {
		return r.list(list.(*corev1.PodList))
	}
	return nil
}

func TestAuthorityReadRacesAndFailures(t *testing.T) {
	for _, stage := range []string{"subject", "job", "pod", "list", "second subject"} {
		t.Run("API failure "+stage, func(t *testing.T) {
			f := newFixture(t, "schema-observe")
			unavailable := errors.New("API unavailable")
			reads := 0
			reader := readerHook{Reader: f.reader(t), get: func(obj client.Object) error {
				reads++
				matches := stage == "subject" && reads == 1 || stage == "job" && reads == 2 || stage == "pod" && reads == 3 || stage == "second subject" && reads == 4
				if matches {
					return unavailable
				}
				return nil
			}, list: func(*corev1.PodList) error {
				if stage == "list" {
					return unavailable
				}
				return nil
			}}
			if err := (Authorizer{Reader: reader}).Check(t.Context(), f.identity); !errors.Is(err, unavailable) {
				t.Fatalf("want retryable API error, got %v", err)
			}
		})
	}
	t.Run("retirement during Pod reads", func(t *testing.T) {
		f := newFixture(t, "migration-history")
		reads := 0
		reader := readerHook{Reader: f.reader(t), get: func(obj client.Object) error {
			if m, ok := obj.(*api.PtahMigration); ok {
				reads++
				if reads == 2 {
					m.Status.ActiveOperation = nil
				}
			}
			return nil
		}}
		if err := (Authorizer{Reader: reader}).Check(t.Context(), f.identity); !errors.Is(err, resultdelivery.ErrAuthority) {
			t.Fatal(err)
		}
	})
	t.Run("claim changes without retirement", func(t *testing.T) {
		f := newFixture(t, "schema-observe")
		reads := 0
		reader := readerHook{Reader: f.reader(t), get: func(obj client.Object) error {
			if s, ok := obj.(*api.PtahSchema); ok {
				reads++
				if reads == 2 {
					s.Status.ExecutionBinding.PtahVersion = "v9.0.0"
				}
			}
			return nil
		}}
		if err := (Authorizer{Reader: reader}).Check(t.Context(), f.identity); !errors.Is(err, resultdelivery.ErrAuthority) {
			t.Fatal(err)
		}
	})
	for _, mode := range []string{"empty", "continued", "replaced"} {
		t.Run("Pod list "+mode, func(t *testing.T) {
			f := newFixture(t, "schema-observe")
			reader := readerHook{Reader: f.reader(t), list: func(list *corev1.PodList) error {
				switch mode {
				case "empty":
					list.Items = nil
				case "continued":
					list.Continue = "more"
				case "replaced":
					list.Items[0].UID = "replacement"
				}
				return nil
			}}
			if err := (Authorizer{Reader: reader}).Check(t.Context(), f.identity); !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatal(err)
			}
		})
	}
}

func TestMigrationClaimRefusals(t *testing.T) {
	changes := map[string]func(*api.PtahMigration){
		"retired":                func(m *api.PtahMigration) { m.Status.ActiveOperation = nil },
		"no binding":             func(m *api.PtahMigration) { m.Status.ExecutionBinding = nil },
		"epoch rotated":          func(m *api.PtahMigration) { m.Status.ExecutionBinding.Epoch = "v1-44444444444444444444444444444444" },
		"operation changed":      func(m *api.PtahMigration) { m.Status.ActiveOperation.Type = api.MigrationOperationHistory },
		"claim identity changed": func(m *api.PtahMigration) { m.Status.ActiveOperation.ID = "other" },
		"engine changed":         func(m *api.PtahMigration) { m.Status.ActiveOperation.Target.Engine = api.DatabaseEngine("MySQL") },
		"not dispatched":         func(m *api.PtahMigration) { m.Status.ActiveOperation.DispatchStarted = false },
		"continuity lost":        func(m *api.PtahMigration) { m.Status.ActiveOperation.LeaseContinuityLost = true },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "migration-apply-admitted-scheduling")
			change(f.subject.(*api.PtahMigration))
			if err := (Authorizer{Reader: f.reader(t)}).Check(t.Context(), f.identity); !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatal(err)
			}
		})
	}
}

func TestMissingObjectsAreDefinitive(t *testing.T) {
	for _, kind := range []string{"subject", "job", "pod"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, "schema-observe")
			reader := f.reader(t).(client.Client)
			var obj client.Object
			switch kind {
			case "subject":
				obj = f.subject
			case "job":
				obj = f.job
			case "pod":
				obj = f.pod
			}
			if err := reader.Delete(t.Context(), obj); err != nil {
				t.Fatal(err)
			}
			if err := (Authorizer{Reader: reader}).Check(t.Context(), f.identity); !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatalf("want definitive refusal, got %v", err)
			}
		})
	}
}
