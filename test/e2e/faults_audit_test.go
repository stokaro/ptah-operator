package e2e

import (
	"slices"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestMissingWatchedAuditsLateRead(t *testing.T) {
	// Master CI 36524481016 closed the fault watches while the ordinary
	// e2e-postgresql schema's periodic Verify was still running. The history
	// stays closed while the audit catches up with that Job and its Pod.
	resolve := ftComplete(ftJob("resolve", "e2e-postgresql", "resolve"))
	verify := ftJob("verify", "e2e-postgresql", "verify")
	pod := ftPod("verify-pod", "e2e-postgresql", "verify", corev1.PodRunning)
	jobs := []watchEvent[*batchv1.Job]{ftEvent(watch.Added, resolve), ftEvent(watch.Added, verify)}
	pods := []watchEvent[*corev1.Pod]{ftEvent(watch.Added, pod)}
	auditedJobs := map[string]bool{"resolve": true}
	auditedPods := map[string]bool{}
	check := func(wantJobs, wantPods []string) {
		t.Helper()
		gotJobs, err := missingWatchedAudits(jobs, auditedJobs)
		if err != nil || !slices.Equal(gotJobs, wantJobs) {
			t.Fatalf("missing Job audits = %v, %v; want %v", gotJobs, err, wantJobs)
		}
		gotPods, err := missingWatchedAudits(pods, auditedPods)
		if err != nil || !slices.Equal(gotPods, wantPods) {
			t.Fatalf("missing Pod audits = %v, %v; want %v", gotPods, err, wantPods)
		}
	}
	check([]string{"verify"}, []string{"verify-pod"})
	auditedPods["verify-pod"] = true
	check([]string{"verify"}, nil)
	auditedJobs["verify"] = true
	check(nil, nil)
	// Coverage requires both ledgers even if the Job's audit arrived first.
	delete(auditedPods, "verify-pod")
	check(nil, []string{"verify-pod"})
}

func TestMissingWatchedAuditsRetainsDeletedAndTerminalObjects(t *testing.T) {
	job := ftJob("late", ftSchema, "verify")
	for _, event := range []watchEvent[*batchv1.Job]{
		ftEvent(watch.Modified, ftComplete(job)),
		ftEvent(watch.Deleted, job),
	} {
		events := []watchEvent[*batchv1.Job]{ftEvent(watch.Added, job), event}
		missing, err := missingWatchedAudits(events, map[string]bool{"late": false, "another-job": true})
		if err != nil || !slices.Equal(missing, []string{"late"}) {
			t.Fatalf("%s discharged an unaudited UID: %v, %v", event.Type, missing, err)
		}
	}
}

func TestMissingWatchedAuditsRejectsInvalidHistory(t *testing.T) {
	for name, events := range map[string][]watchEvent[*batchv1.Job]{
		"empty":         nil,
		"only bookmark": {ftJobBookmark()},
		"no added Job":  {ftEvent(watch.Modified, ftComplete(ftJob("late", ftSchema, "verify")))},
		"empty UID":     {ftEvent(watch.Added, ftJob("", ftSchema, "verify"))},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := missingWatchedAudits(events, map[string]bool{"": true, "late": true}); err == nil {
				t.Fatal("accepted an empty or invalid audit history")
			}
		})
	}
}
