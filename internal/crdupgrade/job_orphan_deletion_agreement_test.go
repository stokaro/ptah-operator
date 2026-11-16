package crdupgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// agreementJob is a Job the agreement is judged on, and what the test knows
// of it: whether the operator runs it, and whether its name is one the
// Pod-intent webhook's match condition routes by.
type agreementJob struct {
	job       *batchv1.Job
	operation bool
	// operationName is a name with an operation Job's prefix on a Job the
	// operator does not run.
	operationName bool
	// madeFrom is the template labels the Job's Pods were made with, where a
	// person changed the template after; nil means the template as it is.
	madeFrom map[string]string
	// known is the verdict a Pod of the Job is held to instead of the
	// agreement, by the Pod's name in podLabels.
	known map[string]knownVerdict
}

// knownVerdict is a verdict the test holds exactly, and why it is known.
type knownVerdict struct {
	verdict agreementVerdict
	why     string
}

// podLabels is the labels of the Pod the Job controller made from the Job, and
// the same with the operation identity taken off.
func (j agreementJob) podLabels() map[string]map[string]string {
	if j.madeFrom != nil {
		return agreementPodLabels(j.madeFrom)
	}
	return agreementPodLabels(j.job.Spec.Template.Labels)
}

// agreementVerdict is what each of the three decides about one Pod of one
// Job being orphaned.
type agreementVerdict struct {
	selected, routed, refused bool
}

func (v agreementVerdict) protected() bool { return v.routed && v.refused }

// An orphaning deletion of a Job finishes only once the garbage collector has
// taken the Job's owner reference off each of its Pods. The Job orphan-deletion
// guard refuses that deletion of the Jobs whose Pods the Pod-intent webhook
// keeps owned, and must refuse it of no other Job: a Job it admits whose Pods
// the webhook keeps stays Terminating, and a Job it refuses whose Pods the
// webhook lets go is refused for nothing.
//
// So, for every Job, the guard's match condition has to select it exactly when
// the webhook refuses the garbage collector's update to one of its Pods: the
// webhook's match condition routes the update to the handler, and the handler,
// with managedPodIdentity and the Job it reads, refuses it. The Jobs are the
// ones the builder produces, every golden Job of this release and of v0.2.10,
// which an upgrade can find running, the states a person can put such a Job
// in, and Jobs the operator does not run, a name-only one among them. Each Job
// is held to the Pod the Job controller made from it and to that Pod with the
// operation labels taken off, which is how a Pod looks once they were changed
// while the webhook did not see the write.
//
// One state is held to known verdicts instead: a started Job a person
// suspended and took the identity off in both maps, which the API server
// allows from 1.36. The guard no longer selects it while the webhook keeps its
// Pod by the Pod's own labels, so its orphaning deletion waits until that Pod
// has terminated; and a Pod of it that lost its labels too is let go. The test
// fails if either verdict moves, as it fails if any other Pod disagrees.
func TestJobOrphanDeletionGuardAgreesWithPodIntent(t *testing.T) {
	t.Parallel()

	jobs := agreementJobs(t)
	guard := testJobOrphanDeletionGuard().Policy()
	verdicts := map[string]agreementVerdict{}
	knowns := 0
	for name, candidate := range jobs {
		for pod, labels := range candidate.podLabels() {
			verdict := judgeOrphaning(t, guard, candidate.job, labels)
			if known, ok := candidate.known[pod]; ok {
				// A known verdict that moves is news either way: a disagreement
				// that went away, or a protection that changed shape.
				if verdict != known.verdict {
					t.Errorf("%s, %s: %+v, want the known %+v, because %s", name, pod, verdict, known.verdict, known.why)
				}
				knowns++
				continue
			}
			verdicts[name+", "+pod] = verdict
		}
	}
	if knowns != 2 {
		t.Fatalf("%d known verdicts were held, want 2", knowns)
	}
	if disagreements := agreementDisagreements(verdicts); len(disagreements) != 0 {
		t.Fatalf("the guard and the Pod-intent webhook disagree about which Pods keep their Job:\n%s", strings.Join(disagreements, "\n"))
	}

	// Agreement alone holds when both protect nothing. The operator's own Jobs
	// keep their Pods, as before; a Job named like one keeps none, and its Pods
	// do reach the handler, so it is the handler that lets them go.
	operations, nameOnly := 0, 0
	for name, candidate := range jobs {
		for pod := range candidate.podLabels() {
			verdict, judged := verdicts[name+", "+pod]
			switch {
			case !judged:
				continue
			case candidate.operation && !verdict.protected():
				t.Errorf("%s, %s: the operator's Job does not keep its Pod: %+v", name, pod, verdict)
			case !candidate.operation && verdict.protected():
				t.Errorf("%s, %s: a Job the operator does not run keeps its Pod: %+v", name, pod, verdict)
			case candidate.operationName && !verdict.routed:
				t.Errorf("%s, %s: a Pod of a Job named like an operation Job does not reach the handler, so the handler did not decide it", name, pod)
			}
		}
		if candidate.operation {
			operations++
		}
		if candidate.operationName {
			nameOnly++
		}
	}
	// Letting the Pods of a Job that names a PtahSchema its owner and carries
	// no identity go lets no operation's Pod go: the webhook holds a Pod of an
	// operator-owned Job to that Job's claim, and admits no Pod of this one.
	claimed := jobs["a Job named like a schema Plan Job that names a PtahSchema its owner"].job
	for pod, labels := range agreementPodLabels(claimed.Spec.Template.Labels) {
		if askPodIntent(t, claimed, admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			UserInfo:  authenticationv1.UserInfo{Username: "system:serviceaccount:kube-system:job-controller"},
		}, agreementPod(claimed, labels), nil) {
			t.Errorf("the webhook admitted the Job controller creating %s of %s", pod, claimed.Name)
		}
	}
	current, released := goldenJobFiles(t, currentJobDirectory), goldenJobFiles(t, releasedJobDirectory)
	if want := 3 + len(current) + len(released) + 4; operations != want || nameOnly != 3 {
		t.Fatalf("the agreement was held to %d operation Jobs and %d name-only Jobs, want %d and 3", operations, nameOnly, want)
	}
}

// The comparison reports a disagreement in either direction. A guard that also
// selected a Job by its name would refuse an orphaning deletion the webhook now
// lets finish, and one that read only the Job's labels would admit one the
// webhook still holds.
func TestJobOrphanDeletionAgreementReportsADisagreement(t *testing.T) {
	t.Parallel()

	jobs := agreementJobs(t)
	for _, mutation := range []struct {
		name, old, new string
		disagrees      string
	}{
		{
			name:      "the guard also reads the name",
			old:       `(has(oldObject.metadata.labels) &&`,
			new:       `oldObject.metadata.name.matches("^ptah-(m-)?(resolve|verify|observe|plan|history|apply)-") || (has(oldObject.metadata.labels) &&`,
			disagrees: "a Job only named like a schema Apply Job",
		},
		{
			name:      "the guard reads no Pod template",
			old:       `has(oldObject.spec.template.metadata.labels) &&`,
			new:       `false &&`,
			disagrees: "a schema Job a person took the component label off",
		},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			t.Parallel()
			guard := testJobOrphanDeletionGuard().Policy()
			expression := guard.Spec.MatchConditions[0].Expression
			if strings.Count(expression, mutation.old) != 1 {
				t.Fatalf("the guard's match condition holds %q %d times, want once", mutation.old, strings.Count(expression, mutation.old))
			}
			guard.Spec.MatchConditions[0].Expression = strings.Replace(expression, mutation.old, mutation.new, 1)
			verdicts := map[string]agreementVerdict{}
			candidate := jobs[mutation.disagrees]
			if candidate.job == nil {
				t.Fatalf("no Job is named %q", mutation.disagrees)
			}
			for pod, labels := range candidate.podLabels() {
				verdicts[mutation.disagrees+", "+pod] = judgeOrphaning(t, guard, candidate.job, labels)
			}
			if disagreements := agreementDisagreements(verdicts); len(disagreements) == 0 {
				t.Fatalf("with %s the comparison reports no disagreement about %s: %+v", mutation.name, mutation.disagrees, verdicts)
			}
		})
	}
}

// agreementDisagreements lists each Pod the guard and the webhook decide
// differently, in a stable order.
func agreementDisagreements(verdicts map[string]agreementVerdict) []string {
	var disagreements []string
	for _, name := range slices.Sorted(maps.Keys(verdicts)) {
		verdict := verdicts[name]
		if verdict.selected != verdict.protected() {
			disagreements = append(disagreements, fmt.Sprintf(
				"%s: the guard selects the Job = %t, the webhook's match condition routes the update = %t, the handler refuses it = %t",
				name, verdict.selected, verdict.routed, verdict.refused))
		}
	}
	return disagreements
}

// agreementPodLabels is the labels of the Pod the Job controller makes from a
// template with labels, and the same with the operation identity taken off.
func agreementPodLabels(template map[string]string) map[string]map[string]string {
	stripped := maps.Clone(template)
	delete(stripped, workload.LabelManagedBy)
	delete(stripped, workload.LabelComponent)
	return map[string]map[string]string{
		"its Pod":                    maps.Clone(template),
		"its Pod without the labels": stripped,
	}
}

// agreementPod is the Pod the Job controller makes from job, with labels.
func agreementPod(job *batchv1.Job, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: job.Namespace, Name: job.Name[:min(len(job.Name), 58)] + "abc12", GenerateName: job.Name + "-",
			UID: "pod-uid", Labels: labels, Annotations: maps.Clone(job.Spec.Template.Annotations),
			Finalizers: []string{batchv1.JobTrackingFinalizer},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID,
				Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: *job.Spec.Template.Spec.DeepCopy(),
	}
}

// askPodIntent sends the Pod-intent handler one request about pod, with job
// the one object its reader holds, and reports whether it was admitted.
func askPodIntent(t *testing.T, job *batchv1.Job, request admissionv1.AdmissionRequest, pod, oldPod *corev1.Pod) bool {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme, operatorv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	handler := &podintent.ValidationHandler{
		Reader:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(job.DeepCopy()).Build(),
		Decoder: cradmission.NewDecoder(scheme),
	}
	raw := func(pod *corev1.Pod) runtime.RawExtension {
		if pod == nil {
			return runtime.RawExtension{}
		}
		encoded, err := json.Marshal(pod)
		if err != nil {
			t.Fatal(err)
		}
		return runtime.RawExtension{Raw: encoded}
	}
	request.UID, request.Namespace, request.Name = "agreement-request", pod.Namespace, pod.Name
	request.Object, request.OldObject = raw(pod), raw(oldPod)
	return handler.Handle(context.Background(), cradmission.Request{AdmissionRequest: request}).Allowed
}

// judgeOrphaning asks the guard about an orphaning deletion of job, and the
// webhook's match condition and handler about the garbage collector's update
// that takes job's owner reference off its Pod.
func judgeOrphaning(
	t *testing.T,
	guard *admissionregistrationv1.ValidatingAdmissionPolicy,
	job *batchv1.Job,
	podLabels map[string]string,
) agreementVerdict {
	t.Helper()
	deletion := map[string]any{
		"operation": "DELETE",
		"resource":  map[string]any{"group": "batch", "version": "v1", "resource": "jobs"},
		"options":   map[string]any{"propagationPolicy": "Orphan"},
	}
	pod := agreementPod(job, podLabels)
	orphaned := pod.DeepCopy()
	orphaned.OwnerReferences = nil
	update := map[string]any{"operation": "UPDATE"}
	routed, ok := evaluatePolicyCEL(t, podIntentMatchExpression,
		map[string]any{"object": celObject(t, orphaned), "oldObject": celObject(t, pod), "request": update}, map[string]any{}).(bool)
	if !ok {
		t.Fatalf("the Pod-intent match condition did not answer a bool for %s", job.Name)
	}
	admitted := askPodIntent(t, job, admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		UserInfo:  authenticationv1.UserInfo{Username: "system:serviceaccount:kube-system:generic-garbage-collector"},
	}, orphaned, pod)
	return agreementVerdict{
		selected: evaluatePolicyMatchConditions(t, guard, nil, celObject(t, job), deletion),
		routed:   routed,
		refused:  !admitted,
	}
}

// agreementJobs is every Job the agreement is held to, by name.
func agreementJobs(t *testing.T) map[string]agreementJob {
	t.Helper()
	builder := workload.Builder{
		ExecutorImage:          "example.test/executor@sha256:" + strings.Repeat("2", 64),
		RunnerImage:            "example.test/runner@sha256:" + strings.Repeat("3", 64),
		PtahVersion:            "v0.3.0",
		ControllerImage:        "example.test/controller@sha256:" + strings.Repeat("1", 64),
		ControllerRevision:     "test-revision",
		ControllerStateVersion: ourStateVersion,
	}
	jobs := map[string]agreementJob{}
	add := func(name string, job *batchv1.Job, operation, operationName bool) {
		t.Helper()
		if _, exists := jobs[name]; exists {
			t.Fatalf("two agreement Jobs are named %q", name)
		}
		job = job.DeepCopy()
		if job.UID == "" {
			job.UID = types.UID("job-uid-" + job.Name)
		}
		jobs[name] = agreementJob{job: job, operation: operation, operationName: operationName}
	}

	schemaResource, schemaOperation := contractSchemaFixture(t, builder)
	schemaJob, err := builder.Build(schemaResource, schemaOperation, nil)
	if err != nil {
		t.Fatal(err)
	}
	add("the schema Resolve Job the builder produces", schemaJob, true, false)
	for _, operationType := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationResolve, operatorv1alpha1.MigrationOperationHistory,
	} {
		migration, operation := contractMigrationFixture(t, builder, operationType)
		job, err := builder.BuildMigration(migration, operation, nil)
		if err != nil {
			t.Fatal(err)
		}
		add("the migration "+string(operationType)+" Job the builder produces", job, true, false)
	}
	for _, directory := range []struct{ path, release string }{
		{currentJobDirectory, "this release"},
		{releasedJobDirectory, "v0.2.10"},
	} {
		for name, job := range goldenJobFiles(t, directory.path) {
			add("the "+name+" Job "+directory.release+" builds", job, true, false)
		}
	}

	// An orphaning deletion of its owner, before the resource guard existed,
	// left a Job with no owner reference; anyone who may update a Job can take
	// its own labels off; and on a suspended Job a person can take the
	// template's: on one that never started on every supported minor, and from
	// 1.36 on one that started and has no active Pod.
	orphaned := schemaJob.DeepCopy()
	orphaned.OwnerReferences = nil
	add("a schema Job left with no owner", orphaned, true, false)
	stripped := schemaJob.DeepCopy()
	stripped.Labels = maps.Clone(stripped.Labels)
	delete(stripped.Labels, workload.LabelComponent)
	add("a schema Job a person took the component label off", stripped, true, false)
	suspended := schemaJob.DeepCopy()
	suspended.Spec.Suspend = ptr.To(true)
	suspended.Spec.Template.Labels = maps.Clone(suspended.Spec.Template.Labels)
	delete(suspended.Spec.Template.Labels, workload.LabelComponent)
	add("a suspended schema Job whose template a person took the component label off", suspended, true, false)

	// From 1.36, where MutableSchedulingDirectivesForSuspendedJobs is on by
	// default, the Job controller clears startTime when it suspends a Job and
	// marks it JobSuspended, and the API server then lets the template change
	// once no Pod is active: the Pod the suspension deleted is terminating. It
	// was made from the template before the change, and keeps those labels.
	// Stripped in both maps, the Job is no longer the guard's, and only the
	// Pod's own labels say what it runs.
	strippedSuspended := schemaJob.DeepCopy()
	strippedSuspended.Spec.Suspend = ptr.To(true)
	strippedSuspended.Status = batchv1.JobStatus{
		Terminating: ptr.To(int32(1)),
		Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobSuspended, Status: corev1.ConditionTrue, Reason: "JobSuspended", Message: "Job suspended",
		}},
	}
	strippedSuspended.Labels = maps.Clone(strippedSuspended.Labels)
	strippedSuspended.Spec.Template.Labels = maps.Clone(strippedSuspended.Spec.Template.Labels)
	for _, labels := range []map[string]string{strippedSuspended.Labels, strippedSuspended.Spec.Template.Labels} {
		delete(labels, workload.LabelManagedBy)
		delete(labels, workload.LabelComponent)
	}
	const strippedSuspendedName = "a started schema Job a person suspended and took the identity off in both maps"
	add(strippedSuspendedName, strippedSuspended, true, false)
	entry := jobs[strippedSuspendedName]
	entry.madeFrom = maps.Clone(schemaJob.Spec.Template.Labels)
	entry.known = map[string]knownVerdict{
		"its Pod": {
			verdict: agreementVerdict{routed: true, refused: true},
			why: "the guard admits orphaning the Job while the webhook keeps the Pod by its own labels, so the deletion " +
				"waits until the Pod finishes the termination the suspension began, and no longer",
		},
		"its Pod without the labels": {
			verdict: agreementVerdict{routed: true},
			why: "a Pod whose labels also changed while the webhook did not see the write carries nothing that says it ran " +
				"an operation, so it is let go, as it was already being stopped",
		},
	}
	jobs[strippedSuspendedName] = entry

	// Jobs the operator does not run. A person can name a Job after the
	// command it runs, which is the name the webhook routes by, and can name a
	// PtahSchema its owner as well. The others carry neither the name nor the
	// identity, or part of the identity only.
	plain := func(name string, labels map[string]string) *batchv1.Job {
		return &batchv1.Job{
			TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
			ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: name, Labels: maps.Clone(labels)},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: maps.Clone(labels)},
				Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{
					Name: "ptah", Image: "registry.example/ptah@sha256:" + strings.Repeat("9", 64),
				}}},
			}},
		}
	}
	add("a Job only named like a schema Apply Job", plain("ptah-apply-nightly", nil), false, true)
	add("a Job only named like a migration History Job", plain("ptah-m-history-nightly", map[string]string{
		"app.kubernetes.io/name": "nightly",
	}), false, true)
	claimed := plain("ptah-plan-orders", nil)
	claimed.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(schemaResource, operatorv1alpha1.GroupVersion.WithKind("PtahSchema"))}
	add("a Job named like a schema Plan Job that names a PtahSchema its owner", claimed, false, true)
	add("a Job with neither the name nor the identity", plain("nightly-report", nil), false, false)
	add("a Job of another operator component", plain("operator-crd-reconcile", map[string]string{
		workload.LabelManagedBy: "ptah-operator", workload.LabelComponent: "crd-manager",
	}), false, false)
	add("a Job another manager labels as an operation", plain("helm-migration-hook", map[string]string{
		workload.LabelManagedBy: "Helm", workload.LabelComponent: workload.ComponentMigrationOperation,
	}), false, false)
	return jobs
}

// goldenJobFiles decodes every golden Job in directory, by file name.
func goldenJobFiles(t *testing.T, directory string) map[string]*batchv1.Job {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	jobs := map[string]*batchv1.Job{}
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		job := &batchv1.Job{}
		if err := json.Unmarshal(content, job); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		jobs[strings.TrimSuffix(entry.Name(), ".json")] = job
	}
	if len(jobs) == 0 {
		t.Fatalf("%s holds no Job", directory)
	}
	return jobs
}
