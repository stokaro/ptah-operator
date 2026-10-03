package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func admissionRefusalAuditFixture(t *testing.T) (*ptahv1alpha1.PtahSchema, *batchv1.Job, []corev1.Event) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", "pod-admission-refused-job-event.json"))
	if err != nil {
		t.Fatal(err)
	}
	var event corev1.Event
	if err := json.Unmarshal(body, &event); err != nil {
		t.Fatal(err)
	}
	ref := event.InvolvedObject
	schema := refusedReport(ref.Name)
	schema.ObjectMeta = metav1.ObjectMeta{Name: "refused-schema", Namespace: ref.Namespace, UID: types.UID("schema-original")}
	schema.Status.ActiveOperation.Type = ptahv1alpha1.OperationResolve
	schema.Status.ActiveOperation.ID = "exact-resolve-operation"
	schema.Status.ActiveOperation.JobUID = ref.UID
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: ref.Name, Namespace: ref.Namespace, UID: ref.UID,
		Labels:      map[string]string{labelSchema: schema.Name, labelOperation: "resolve"},
		Annotations: map[string]string{annotationOperationID: schema.Status.ActiveOperation.ID},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: ptahSchemaAPIVersion, Kind: "PtahSchema", Name: schema.Name,
			UID: schema.UID, Controller: ptr.To(true)}},
	}}
	return schema, job, []corev1.Event{event}
}

func TestPodAdmissionRefusalAuditUsesTheExactNativeEvent(t *testing.T) {
	t.Parallel()
	schema, job, events := admissionRefusalAuditFixture(t)
	unrelated := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "another-operation", Namespace: schema.Namespace,
		Labels: map[string]string{labelSchema: "another-schema"}}}
	if err := podAdmissionRefusalAudit(schema, job, []corev1.Pod{unrelated}, events, podMetadataPolicyName); err != nil {
		t.Fatal("the exact native FailedCreate and no matching Pods were refused:", err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema, *batchv1.Job, *[]corev1.Pod, *[]corev1.Event){
		"missing schema UID": func(s *ptahv1alpha1.PtahSchema, _ *batchv1.Job, _ *[]corev1.Pod, _ *[]corev1.Event) { s.UID = "" },
		"replacement schema": func(s *ptahv1alpha1.PtahSchema, _ *batchv1.Job, _ *[]corev1.Pod, _ *[]corev1.Event) {
			s.UID = "replacement"
		},
		"wrong Job label": func(_ *ptahv1alpha1.PtahSchema, j *batchv1.Job, _ *[]corev1.Pod, _ *[]corev1.Event) {
			j.Labels[labelSchema] = "another"
		},
		"another operation": func(_ *ptahv1alpha1.PtahSchema, j *batchv1.Job, _ *[]corev1.Pod, _ *[]corev1.Event) {
			j.Labels[labelOperation] = "apply"
		},
		"no exact claim": func(s *ptahv1alpha1.PtahSchema, _ *batchv1.Job, _ *[]corev1.Pod, _ *[]corev1.Event) {
			s.Status.ActiveOperation = nil
		},
		"changed operation ID": func(s *ptahv1alpha1.PtahSchema, _ *batchv1.Job, _ *[]corev1.Pod, _ *[]corev1.Event) {
			s.Status.ActiveOperation.ID = "replacement"
		},
		"changed claim Job UID": func(s *ptahv1alpha1.PtahSchema, _ *batchv1.Job, _ *[]corev1.Pod, _ *[]corev1.Event) {
			s.Status.ActiveOperation.JobUID = "replacement"
		},
		"active Pod count": func(_ *ptahv1alpha1.PtahSchema, j *batchv1.Job, _ *[]corev1.Pod, _ *[]corev1.Event) {
			j.Status.Active = 1
		},
		"terminal Job": func(_ *ptahv1alpha1.PtahSchema, j *batchv1.Job, _ *[]corev1.Pod, _ *[]corev1.Event) {
			j.Status.Failed = 1
		},
		"same-name event from another UID": func(_ *ptahv1alpha1.PtahSchema, _ *batchv1.Job, _ *[]corev1.Pod, e *[]corev1.Event) {
			(*e)[0].InvolvedObject.UID = "another"
		},
		"another event namespace": func(_ *ptahv1alpha1.PtahSchema, _ *batchv1.Job, _ *[]corev1.Pod, e *[]corev1.Event) {
			(*e)[0].Namespace = "another"
		},
		"another reference namespace": func(_ *ptahv1alpha1.PtahSchema, _ *batchv1.Job, _ *[]corev1.Pod, e *[]corev1.Event) {
			(*e)[0].InvolvedObject.Namespace = "another"
		},
		"another object kind": func(_ *ptahv1alpha1.PtahSchema, _ *batchv1.Job, _ *[]corev1.Pod, e *[]corev1.Event) {
			(*e)[0].InvolvedObject.Kind = "Pod"
		},
		"another event producer": func(_ *ptahv1alpha1.PtahSchema, _ *batchv1.Job, _ *[]corev1.Pod, e *[]corev1.Event) {
			(*e)[0].Source.Component = "some-controller"
		},
		"no admission evidence": func(_ *ptahv1alpha1.PtahSchema, _ *batchv1.Job, _ *[]corev1.Pod, e *[]corev1.Event) { *e = nil },
		"another policy": func(_ *ptahv1alpha1.PtahSchema, _ *batchv1.Job, _ *[]corev1.Pod, e *[]corev1.Event) {
			(*e)[0].Message = "ValidatingAdmissionPolicy 'other' denied request"
		},
		"same schema has a Pod": func(s *ptahv1alpha1.PtahSchema, _ *batchv1.Job, p *[]corev1.Pod, _ *[]corev1.Event) {
			*p = []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Labels: map[string]string{labelSchema: s.Name}}}}
		},
		"Job Pod with changed labels": func(s *ptahv1alpha1.PtahSchema, j *batchv1.Job, p *[]corev1.Pod, _ *[]corev1.Event) {
			*p = []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, OwnerReferences: []metav1.OwnerReference{{UID: j.UID, Kind: "Job", APIVersion: "batch/v1", Controller: ptr.To(true)}}}}}
		},
		"Job reference without controller bit": func(s *ptahv1alpha1.PtahSchema, j *batchv1.Job, p *[]corev1.Pod, _ *[]corev1.Event) {
			*p = []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, OwnerReferences: []metav1.OwnerReference{{UID: j.UID}}}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, j, e := admissionRefusalAuditFixture(t)
			var pods []corev1.Pod
			mutate(s, j, &pods, &e)
			if err := podAdmissionRefusalAudit(s, j, pods, e, podMetadataPolicyName); err == nil {
				t.Fatal("incomplete or changed no-Pod evidence passed")
			}
		})
	}
	if podAdmissionRefusalAudit(nil, job, nil, events, podMetadataPolicyName) == nil ||
		podAdmissionRefusalAudit(schema, nil, nil, events, podMetadataPolicyName) == nil ||
		podAdmissionRefusalAudit(schema, job, nil, events, "") == nil {
		t.Fatal("absent workload or policy identity supplied a no-Pod audit")
	}
}
