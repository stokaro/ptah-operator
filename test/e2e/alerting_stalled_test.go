package e2e

import (
	"encoding/json"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

func alStalledFixture() (alStalledClaim, *batchv1.Job, *corev1.Pod) {
	started := time.Unix(1800000000, 0).UTC()
	claim := alStalledClaim{family: "schema", namespace: "held", name: "held-resolve", uid: "schema-uid", generation: 1,
		id: "operation", jobName: "resolve-job", jobUID: "job-uid", started: started, operation: "Resolve", phase: "Resolving"}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: claim.jobName, Namespace: claim.namespace, UID: claim.jobUID,
		Annotations: map[string]string{workload.AnnotationOperationID: claim.id}, Labels: map[string]string{workload.LabelOperation: "resolve"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: ptahv1.GroupVersion.String(), Kind: "PtahSchema", Name: claim.name, UID: claim.uid, Controller: ptr.To(true)}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "executor", Namespace: claim.namespace, UID: "pod-uid",
		CreationTimestamp: metav1.NewTime(started.Add(time.Second)),
		Annotations:       map[string]string{workload.AnnotationOperationID: claim.id}, Labels: map[string]string{workload.LabelOperation: "resolve"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: claim.jobName, UID: claim.jobUID, Controller: ptr.To(true)}}},
		Spec: corev1.PodSpec{NodeSelector: map[string]string{alGateLabel: "open"}, Containers: []corev1.Container{{Name: "runner"}}},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{Name: "runner", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, StartedAt: metav1.NewTime(started.Add(70 * time.Second)), FinishedAt: metav1.NewTime(started.Add(75 * time.Second))}}}}}}
	return claim, job, pod
}

func TestAlStalledResourceFamilies(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"schema", "migration"} {
		body, err := json.Marshal(alHeldResource("held", family))
		if err != nil {
			t.Fatal(err)
		}
		var object client.Object = &ptahv1.PtahSchema{}
		if family == "migration" {
			object = &ptahv1.PtahMigration{}
		}
		if err := json.Unmarshal(body, object); err != nil {
			t.Fatal(err)
		}
		object.SetUID("subject")
		object.SetGeneration(1)
		started := metav1.NewTime(time.Unix(1800000000, 0).UTC())
		switch r := object.(type) {
		case *ptahv1.PtahSchema:
			r.Status.Phase = ptahv1.PhaseResolving
			r.Status.ActiveOperation = &ptahv1.ActiveOperationStatus{Type: ptahv1.OperationResolve, ID: "op", JobName: "job", JobUID: "job-uid", StartedAt: started}
			if r.Spec.Desired.OCIRef == "" || r.Spec.Execution.NodeSelector[alGateLabel] != "open" {
				t.Fatal("schema cannot reach its held Resolve")
			}
		case *ptahv1.PtahMigration:
			r.Status.Phase = ptahv1.MigrationPhaseResolving
			r.Status.ActiveOperation = &ptahv1.MigrationOperationStatus{Type: ptahv1.MigrationOperationResolve, ID: "op", JobName: "job", JobUID: "job-uid", StartedAt: started}
			if r.Spec.Artifact.OCIRef == "" || r.Spec.Execution.NodeSelector[alGateLabel] != "open" {
				t.Fatal("migration cannot reach its held Resolve")
			}
		}
		claim := alStalledReading(object)
		if claim.family != family || !claim.ready() {
			t.Fatalf("%s claim is not usable: %+v", family, claim)
		}
		for _, change := range []func(*alStalledClaim){func(c *alStalledClaim) { c.uid = "" }, func(c *alStalledClaim) { c.jobUID = "" },
			func(c *alStalledClaim) { c.id = "" }, func(c *alStalledClaim) { c.started = time.Time{} },
			func(c *alStalledClaim) { c.phase = "Failed" }, func(c *alStalledClaim) { c.operation = "Apply" }} {
			bad := claim
			change(&bad)
			if bad.ready() {
				t.Fatalf("incomplete or ended %s claim accepted: %+v", family, bad)
			}
		}
	}
	if alHeldResource("held", "unknown") != nil {
		t.Fatal("unknown family silently selected a schema")
	}
}

func TestAlStalledWorkloadIdentityAndTerminalEvidence(t *testing.T) {
	t.Parallel()
	claim, job, pod := alStalledFixture()
	if !alStalledJobMatches(job, claim) || !alStalledPodMatches(pod, claim, pod.UID) {
		t.Fatal("bound workload rejected")
	}
	for _, change := range []func(*batchv1.Job){func(j *batchv1.Job) { j.UID = "replacement" },
		func(j *batchv1.Job) { j.OwnerReferences[0].UID = "other-resource" },
		func(j *batchv1.Job) { j.Annotations[workload.AnnotationOperationID] = "other-claim" },
		func(j *batchv1.Job) { j.OwnerReferences[0].Kind = "PtahMigration" },
		func(j *batchv1.Job) { j.OwnerReferences[0].Controller = ptr.To(false) }} {
		bad := job.DeepCopy()
		change(bad)
		if alStalledJobMatches(bad, claim) {
			t.Fatal("changed Job or resource identity passed")
		}
	}
	migration := claim
	migration.family = "migration"
	migrationJob := job.DeepCopy()
	migrationJob.OwnerReferences[0].Kind = "PtahMigration"
	if !alStalledJobMatches(migrationJob, migration) {
		t.Fatal("migration Job ownership rejected")
	}
	finished, err := alStalledFinished(pod, claim, pod.UID)
	if err != nil || !finished.Equal(claim.started.Add(75*time.Second)) {
		t.Fatalf("native terminal timestamp = %v, %v", finished, err)
	}
	withInit := pod.DeepCopy()
	withInit.Spec.InitContainers = []corev1.Container{{Name: "prepare"}}
	withInit.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "prepare", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, StartedAt: metav1.NewTime(claim.started.Add(65 * time.Second)), FinishedAt: metav1.NewTime(claim.started.Add(69 * time.Second))}}}}
	if at, err := alStalledFinished(withInit, claim, pod.UID); err != nil || !at.Equal(finished) {
		t.Fatalf("complete initializer evidence rejected: %s %v", at, err)
	}
	withInit.Status.InitContainerStatuses = nil
	if _, err := alStalledFinished(withInit, claim, pod.UID); err == nil {
		t.Fatal("missing initializer outcome passed")
	}
	for name, change := range map[string]func(*corev1.Pod){
		"replacement":       func(p *corev1.Pod) { p.UID = "other" },
		"wrong Job":         func(p *corev1.Pod) { p.OwnerReferences[0].UID = "other" },
		"wrong claim":       func(p *corev1.Pod) { p.Annotations[workload.AnnotationOperationID] = "other" },
		"wrong operation":   func(p *corev1.Pod) { p.Labels[workload.LabelOperation] = "verify" },
		"wrong owner kind":  func(p *corev1.Pod) { p.OwnerReferences[0].Kind = "Deployment" },
		"running":           func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning },
		"missing status":    func(p *corev1.Pod) { p.Status.ContainerStatuses = nil },
		"missing container": func(p *corev1.Pod) { p.Spec.Containers = nil },
		"unknown container": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Name = "other" },
		"restart":           func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 },
		"no termination":    func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated = nil },
		"missing finish":    func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.Time{} },
		"before claim": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.StartedAt = metav1.NewTime(claim.started.Add(-time.Second))
		},
		"failed transport": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := pod.DeepCopy()
			change(bad)
			if at, err := alStalledFinished(bad, claim, pod.UID); err == nil {
				t.Fatalf("invalid terminal evidence accepted: %s", at)
			}
		})
	}
	held := pod.DeepCopy()
	held.Status.Phase, held.Status.ContainerStatuses = corev1.PodPending, nil
	if !alStalledPodHeld(held) {
		t.Fatal("unscheduled executor rejected")
	}
	held.Spec.NodeName = "ran-before-alert"
	if alStalledPodHeld(held) {
		t.Fatal("scheduled executor accepted as held")
	}
}

func TestAlStalledDeliveryBoundsKeepSubsecondAndIncidentIdentity(t *testing.T) {
	t.Parallel()
	started := time.Unix(1800000000, 500000000).UTC()
	firing := alDelivery{StartsAt: started.Add(alStalledAfter), ReceivedAt: started.Add(alStalledAfter + alDetectionSlack)}
	if !alStalledDelivered(firing, started) {
		t.Fatal("delivery exactly at the bound rejected")
	}
	late := firing
	late.ReceivedAt = late.ReceivedAt.Add(time.Nanosecond)
	if alStalledDelivered(late, started) {
		t.Fatal("fractional late delivery was rounded into the bound")
	}
	early := firing
	early.StartsAt = early.StartsAt.Add(-time.Nanosecond)
	if alStalledDelivered(early, started) {
		t.Fatal("an early firing hidden by delayed delivery passed")
	}
	finished := started.Add(2 * time.Minute)
	resolved := alDelivery{StartsAt: firing.StartsAt, EndsAt: finished.Add(time.Second), ReceivedAt: finished.Add(105 * time.Second)}
	if !alStalledCleared(firing, resolved, finished) {
		t.Fatal("matching native resolution rejected")
	}
	for _, change := range []func(*alDelivery){func(d *alDelivery) { d.ReceivedAt = d.ReceivedAt.Add(time.Nanosecond) },
		func(d *alDelivery) { d.StartsAt = d.StartsAt.Add(time.Second) }, func(d *alDelivery) { d.EndsAt = finished.Add(-time.Nanosecond) }} {
		bad := resolved
		change(&bad)
		if alStalledCleared(firing, bad, finished) {
			t.Fatal("late, different-incident or premature resolution passed")
		}
	}
}

func TestAlStalledAccountedRequiresTheOriginalAttemptAndDeferredRetry(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"schema", "migration"} {
		t.Run(family, func(t *testing.T) {
			claim, _, _ := alStalledFixture()
			claim.family = family
			finished := claim.started.Add(75 * time.Second)
			body, err := json.Marshal(alHeldResource(claim.namespace, family))
			if err != nil {
				t.Fatal(err)
			}
			var object client.Object = &ptahv1.PtahSchema{}
			if family == "migration" {
				object = &ptahv1.PtahMigration{}
			}
			if err := json.Unmarshal(body, object); err != nil {
				t.Fatal(err)
			}
			object.SetName(claim.name)
			object.SetUID(claim.uid)
			object.SetGeneration(claim.generation)
			deadline := metav1.NewTime(finished.Add(time.Hour + time.Second))
			switch r := object.(type) {
			case *ptahv1.PtahSchema:
				r.Status.Phase = ptahv1.PhaseFailed
				r.Status.NextReconciliationTime = &deadline
				r.Status.ActiveOperation = &ptahv1.ActiveOperationStatus{ID: claim.id, Type: ptahv1.OperationResolve, JobName: "retry-job", StartedAt: metav1.NewTime(claim.started)}
			case *ptahv1.PtahMigration:
				r.Status.Phase = ptahv1.MigrationPhaseResolving
				r.Status.ActiveOperation = &ptahv1.MigrationOperationStatus{ID: claim.id, Type: ptahv1.MigrationOperationResolve, JobName: "retry-job", StartedAt: metav1.NewTime(finished.Add(time.Second)), RetryNotBefore: &deadline}
			}
			if !alStalledAccounted(object, claim, finished) {
				t.Fatal("accounted failed operation with deferred retry rejected")
			}
			for _, field := range []string{"uid", "generation", "id", "jobName", "jobUID", "deadline", "phase", "interval", "active"} {
				t.Run(field, func(t *testing.T) {
					bad := object.DeepCopyObject().(client.Object)
					switch field {
					case "uid":
						bad.SetUID("replacement")
					case "generation":
						bad.SetGeneration(claim.generation + 1)
					}
					early := metav1.NewTime(finished.Add(time.Hour - time.Second))
					switch r := bad.(type) {
					case *ptahv1.PtahSchema:
						switch field {
						case "id":
							r.Status.ActiveOperation.ID = "different"
						case "jobName":
							r.Status.ActiveOperation.JobName = claim.jobName
						case "jobUID":
							r.Status.ActiveOperation.JobUID = "dispatched"
						case "deadline":
							r.Status.NextReconciliationTime = &early
						case "phase":
							r.Status.Phase = ptahv1.PhaseResolving
						case "interval":
							r.Spec.Execution.FailureRetryInterval.Duration = time.Minute
						case "active":
							r.Status.ActiveOperation = nil
						}
					case *ptahv1.PtahMigration:
						switch field {
						case "id":
							r.Status.ActiveOperation.ID = "different"
						case "jobName":
							r.Status.ActiveOperation.JobName = claim.jobName
						case "jobUID":
							r.Status.ActiveOperation.JobUID = "dispatched"
						case "deadline":
							r.Status.ActiveOperation.RetryNotBefore = &early
						case "phase":
							r.Status.Phase = ptahv1.MigrationPhaseFailed
						case "interval":
							r.Spec.Execution.FailureRetryInterval.Duration = time.Minute
						case "active":
							r.Status.ActiveOperation = nil
						}
					}
					if alStalledAccounted(bad, claim, finished) {
						t.Fatal("changed identity or missing retry evidence accepted")
					}
				})
			}
		})
	}
}
