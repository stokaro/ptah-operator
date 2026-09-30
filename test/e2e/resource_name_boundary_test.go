package e2e

import (
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fieldvalidation "k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
)

func TestResourceNameBoundaryRefusalIdentifiesItsRule(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"PtahSchema", "PtahMigration"} {
		name := boundaryResourceName(strings.ToLower(kind), "mysql") + "x"
		refused := apierrors.NewInvalid(schema.GroupKind{Group: "operator.ptah.run", Kind: kind}, name,
			fieldvalidation.ErrorList{fieldvalidation.Invalid(nil, name, "metadata.name must be at most 63 bytes, because it is carried whole in workload labels")})
		if err := resourceNameRefusal(refused, kind, name); err != nil {
			t.Fatal(err)
		}
		for label, edit := range map[string]func(*metav1.Status){
			"wrong name":  func(s *metav1.Status) { s.Details.Name = "another" },
			"wrong kind":  func(s *metav1.Status) { s.Details.Kind = "ConfigMap" },
			"wrong group": func(s *metav1.Status) { s.Details.Group = "another.test" },
			"no details":  func(s *metav1.Status) { s.Details = nil },
			"no cause":    func(s *metav1.Status) { s.Details.Causes = nil },
			"other field": func(s *metav1.Status) { s.Details.Causes[0].Field = "spec.interval" },
			"other rule":  func(s *metav1.Status) { s.Details.Causes[0].Message = "spec.interval must be between 10s and 24h" },
			"extra refusal": func(s *metav1.Status) {
				s.Details.Causes = append(s.Details.Causes, metav1.StatusCause{Field: "spec.interval", Message: "another invalid input"})
			},
		} {
			t.Run(kind+"/"+label, func(t *testing.T) {
				original := refused.Status()
				status := original.DeepCopy()
				edit(status)
				if resourceNameRefusal(&apierrors.StatusError{ErrStatus: *status}, kind, name) == nil {
					t.Fatal("another refusal counted as the name-boundary proof")
				}
			})
		}
		for _, err := range []error{nil, errors.New("unavailable"), apierrors.NewForbidden(schema.GroupResource{Resource: "ptahschemas"}, name, errors.New("denied"))} {
			if resourceNameRefusal(err, kind, name) == nil {
				t.Fatal("admission or another failure counted as Invalid")
			}
		}
		if resourceNameRefusal(refused, kind, name[:63]) == nil {
			t.Fatal("the allowed boundary counted as a 64-byte refusal")
		}
	}
}

func nameBoundaryWorkloadFixture(kind string) ([]batchv1.Job, []corev1.Pod) {
	label, family, operations := labelSchema, "schema", []string{"resolve", "verify", "observe", "plan", "apply"}
	if kind == "PtahMigration" {
		label, family, operations = labelMigration, "migration", []string{"resolve", "verify", "history", "apply"}
	}
	name := boundaryResourceName(family, "postgresql")
	var jobs []batchv1.Job
	var pods []corev1.Pod
	for _, operation := range operations {
		metadata := metav1.ObjectMeta{Name: "job-" + operation, Namespace: "team-a", UID: types.UID("job-" + operation),
			Labels: map[string]string{label: name, labelOperation: operation}, Annotations: map[string]string{annotationOperationID: "operation-" + operation}}
		job := batchv1.Job{ObjectMeta: metadata, Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{ObjectMeta: *metadata.DeepCopy()}},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}}
		job.OwnerReferences = []metav1.OwnerReference{{APIVersion: ptahSchemaAPIVersion, Kind: kind, Name: name, UID: "resource", Controller: ptr.To(true)}}
		pod := corev1.Pod{ObjectMeta: *metadata.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
		pod.Name, pod.UID = "pod-"+operation, types.UID("pod-"+operation)
		pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true)}}
		jobs, pods = append(jobs, job), append(pods, pod)
	}
	return jobs, pods
}

func TestResourceNameBoundaryNeedsEveryExecutedOperation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"PtahSchema", "PtahMigration"} {
		jobs, pods := nameBoundaryWorkloadFixture(kind)
		label := labelSchema
		if kind == "PtahMigration" {
			label = labelMigration
		}
		name := jobs[0].Labels[label]
		assert := func(jobs []batchv1.Job, pods []corev1.Pod) error {
			return resourceNameWorkloads("team-a", kind, name, "resource", "job-apply", jobs, pods)
		}
		if err := assert(jobs, pods); err != nil {
			t.Fatal(err)
		}
		// A failed read-only attempt does not erase the successful execution
		// that already proves this name works. Nor must a later read finish
		// before that proof can be assessed.
		for _, phase := range []corev1.PodPhase{corev1.PodFailed, corev1.PodRunning} {
			j, p := nameBoundaryWorkloadFixture(kind)
			job, pod := j[0].DeepCopy(), p[0].DeepCopy()
			job.Name, job.UID, pod.Name, pod.UID = "later-resolve", "later-job", "later-pod", "later-pod"
			job.Status.Conditions = nil
			pod.OwnerReferences[0].Name, pod.OwnerReferences[0].UID, pod.Status.Phase = job.Name, job.UID, phase
			job.Annotations[annotationOperationID], job.Spec.Template.Annotations[annotationOperationID], pod.Annotations[annotationOperationID] = "later-read", "later-read", "later-read"
			if err := assert(append(j, *job), append(p, *pod)); err != nil {
				t.Fatal("a later read-only attempt hid the complete maximum-name path:", err)
			}
		}
		for description, edit := range map[string]func(*[]batchv1.Job, *[]corev1.Pod){
			"empty Jobs":      func(j *[]batchv1.Job, _ *[]corev1.Pod) { *j = nil },
			"empty Pods":      func(_ *[]batchv1.Job, p *[]corev1.Pod) { *p = nil },
			"missing resolve": func(j *[]batchv1.Job, p *[]corev1.Pod) { *j, *p = (*j)[1:], (*p)[1:] },
			"truncated Job label": func(j *[]batchv1.Job, _ *[]corev1.Pod) {
				(*j)[0].Labels[label] = name[:62]
			},
			"truncated template label": func(j *[]batchv1.Job, _ *[]corev1.Pod) {
				(*j)[0].Spec.Template.Labels[label] = name[:62]
			},
			"truncated Pod label": func(_ *[]batchv1.Job, p *[]corev1.Pod) {
				(*p)[0].Labels[label] = name[:62]
			},
			"wrong resource UID": func(j *[]batchv1.Job, _ *[]corev1.Pod) { (*j)[0].OwnerReferences[0].UID = "replacement" },
			"wrong resource name": func(j *[]batchv1.Job, _ *[]corev1.Pod) {
				(*j)[0].OwnerReferences[0].Name = name[:62]
			},
			"wrong Pod owner":     func(_ *[]batchv1.Job, p *[]corev1.Pod) { (*p)[0].OwnerReferences[0].UID = "another-job" },
			"another namespace":   func(j *[]batchv1.Job, _ *[]corev1.Pod) { (*j)[0].Namespace = "another" },
			"Job name too long":   func(j *[]batchv1.Job, _ *[]corev1.Pod) { (*j)[0].Name = strings.Repeat("a", 64) },
			"no Job UID":          func(j *[]batchv1.Job, _ *[]corev1.Pod) { (*j)[0].UID = "" },
			"no operation":        func(j *[]batchv1.Job, _ *[]corev1.Pod) { (*j)[0].Labels[labelOperation] = "" },
			"another template op": func(j *[]batchv1.Job, _ *[]corev1.Pod) { (*j)[0].Spec.Template.Labels[labelOperation] = "another" },
			"another Pod op":      func(_ *[]batchv1.Job, p *[]corev1.Pod) { (*p)[0].Labels[labelOperation] = "another" },
			"another Pod attempt": func(_ *[]batchv1.Job, p *[]corev1.Pod) { (*p)[0].Annotations[annotationOperationID] = "another" },
			"unfinished Job":      func(j *[]batchv1.Job, _ *[]corev1.Pod) { (*j)[0].Status.Conditions = nil },
			"running Pod":         func(_ *[]batchv1.Job, p *[]corev1.Pod) { (*p)[0].Status.Phase = corev1.PodRunning },
			"failed Pod":          func(_ *[]batchv1.Job, p *[]corev1.Pod) { (*p)[0].Status.Phase = corev1.PodFailed },
			"duplicate Job":       func(j *[]batchv1.Job, _ *[]corev1.Pod) { *j = append(*j, (*j)[0]) },
			"duplicate Pod":       func(_ *[]batchv1.Job, p *[]corev1.Pod) { *p = append(*p, (*p)[0]) },
			"second Apply": func(j *[]batchv1.Job, p *[]corev1.Pod) {
				job, pod := (*j)[len(*j)-1].DeepCopy(), (*p)[len(*p)-1].DeepCopy()
				job.Name, job.UID, pod.Name, pod.UID = "second-apply", "second-job", "second-pod", "second-pod"
				pod.OwnerReferences[0].Name, pod.OwnerReferences[0].UID = job.Name, job.UID
				*j, *p = append(*j, *job), append(*p, *pod)
			},
		} {
			t.Run(kind+"/"+description, func(t *testing.T) {
				j, p := nameBoundaryWorkloadFixture(kind)
				edit(&j, &p)
				if assert(j, p) == nil {
					t.Fatal("incomplete or wrongly attributed boundary evidence passed")
				}
			})
		}
		if resourceNameWorkloads("team-a", kind, name[:62], "resource", "job-apply", jobs, pods) == nil ||
			resourceNameWorkloads("team-a", kind, name, "resource", "unrecorded-apply", jobs, pods) == nil {
			t.Fatal("a shorter resource name or unrecorded Apply passed")
		}
	}
}

func boundaryResourceName(family, engine string) string {
	prefix := "e2e-name-" + family + "-" + engine + "-"
	return prefix + strings.Repeat("x", 63-len(prefix))
}
