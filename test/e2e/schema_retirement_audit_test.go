package e2e

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func retiringSchemaAuditFixture() (*ptahv1alpha1.PtahSchema, []batchv1.Job, []corev1.Pod, map[string]bool, map[string]bool) {
	jobs, pods := nameBoundaryWorkloadFixture("PtahSchema")
	jobs, pods = jobs[:4], pods[:4]
	resource := &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: jobs[0].Labels[labelSchema], UID: "resource"}}
	auditedJobs, auditedPods := map[string]bool{}, map[string]bool{}
	for i := range jobs {
		auditedJobs[string(jobs[i].UID)], auditedPods[string(pods[i].UID)] = true, true
	}
	return resource, jobs, pods, auditedJobs, auditedPods
}

func TestSchemaRetirementWaitsForTheLastPlanAudit(t *testing.T) {
	t.Parallel()
	resource, jobs, pods, auditedJobs, auditedPods := retiringSchemaAuditFixture()
	check := func(want bool) {
		t.Helper()
		complete, err := schemaRetirementAudited(resource, jobs, pods, func(uid string) bool { return auditedJobs[uid] }, auditedPods)
		if err != nil || complete != want {
			t.Fatalf("retirement audit complete = %t, %v; want %t", complete, err, want)
		}
	}
	check(true)
	// Run 020-running-schema-bb9a6d49 lost both engines' predecessor Plan
	// Job/Pod logs during same-name replacement. Their SQL evidence cannot
	// substitute for the complete credential audit.
	delete(auditedJobs, "job-plan")
	delete(auditedPods, "pod-plan")
	auditedJobs["replacement-plan"], auditedPods["replacement-plan-pod"] = true, true
	check(false)
	auditedPods["pod-plan"] = true
	check(false)
	auditedJobs["job-plan"] = true
	check(true)
	auditedPods["pod-plan"] = false
	check(false)
}

func TestSchemaRetirementAuditRejectsMissingOrDifferentObjects(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*ptahv1alpha1.PtahSchema, *[]batchv1.Job, *[]corev1.Pod){
		"empty Jobs":           func(_ *ptahv1alpha1.PtahSchema, j *[]batchv1.Job, _ *[]corev1.Pod) { *j = nil },
		"empty Pods":           func(_ *ptahv1alpha1.PtahSchema, _ *[]batchv1.Job, p *[]corev1.Pod) { *p = nil },
		"replacement resource": func(s *ptahv1alpha1.PtahSchema, _ *[]batchv1.Job, _ *[]corev1.Pod) { s.UID = "replacement" },
		"another Job owner": func(_ *ptahv1alpha1.PtahSchema, j *[]batchv1.Job, _ *[]corev1.Pod) {
			(*j)[0].OwnerReferences[0].UID = "replacement"
		},
		"another Job label": func(_ *ptahv1alpha1.PtahSchema, j *[]batchv1.Job, _ *[]corev1.Pod) {
			(*j)[0].Labels[labelSchema] = "another"
		},
		"another namespace": func(_ *ptahv1alpha1.PtahSchema, j *[]batchv1.Job, _ *[]corev1.Pod) { (*j)[0].Namespace = "another" },
		"no Job UID":        func(_ *ptahv1alpha1.PtahSchema, j *[]batchv1.Job, _ *[]corev1.Pod) { (*j)[0].UID = "" },
		"no Pod UID":        func(_ *ptahv1alpha1.PtahSchema, _ *[]batchv1.Job, p *[]corev1.Pod) { (*p)[0].UID = "" },
		"another Pod owner": func(_ *ptahv1alpha1.PtahSchema, _ *[]batchv1.Job, p *[]corev1.Pod) {
			(*p)[0].OwnerReferences[0].UID = "replacement"
		},
		"another Pod label": func(_ *ptahv1alpha1.PtahSchema, _ *[]batchv1.Job, p *[]corev1.Pod) {
			(*p)[0].Labels[labelSchema] = "another"
		},
		"duplicate Job":    func(_ *ptahv1alpha1.PtahSchema, j *[]batchv1.Job, _ *[]corev1.Pod) { *j = append(*j, (*j)[0]) },
		"duplicate Pod":    func(_ *ptahv1alpha1.PtahSchema, _ *[]batchv1.Job, p *[]corev1.Pod) { *p = append(*p, (*p)[0]) },
		"missing Plan Pod": func(_ *ptahv1alpha1.PtahSchema, _ *[]batchv1.Job, p *[]corev1.Pod) { *p = (*p)[:3] },
	} {
		t.Run(name, func(t *testing.T) {
			resource, jobs, pods, auditedJobs, auditedPods := retiringSchemaAuditFixture()
			edit(resource, &jobs, &pods)
			if complete, err := schemaRetirementAudited(resource, jobs, pods, func(uid string) bool { return auditedJobs[uid] }, auditedPods); err == nil || complete {
				t.Fatal("missing or different predecessor evidence permitted retirement")
			}
		})
	}
}
