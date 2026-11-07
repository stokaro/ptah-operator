package jobclaim_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	retentionJobUID    = types.UID("job-uid")
	retentionOperation = "sha256:operation"
)

// TestOnlyARunThatStopsItsResourceKeepsItsLogWithoutAnApproval is the rule, for every
// outcome the API has. A run that repeats under `apply: Always` writes a fresh log
// each cycle, and a long TTL would hold one Job per cycle.
func TestOnlyARunThatStopsItsResourceKeepsItsLogWithoutAnApproval(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		outcome operatorv1alpha1.MigrationRunOutcome
		blocks  bool
	}{
		{outcome: operatorv1alpha1.MigrationRunOutcomeUpToDate},
		{outcome: operatorv1alpha1.MigrationRunOutcomeApplied},
		{outcome: operatorv1alpha1.MigrationRunOutcomeFailed},
		{outcome: operatorv1alpha1.MigrationRunOutcomePartial, blocks: true},
		{outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, blocks: true},
	} {
		t.Run(string(test.outcome), func(t *testing.T) {
			t.Parallel()
			if got := jobclaim.MigrationRunBlocks(test.outcome); got != test.blocks {
				t.Errorf("MigrationRunBlocks() = %v, want %v", got, test.blocks)
			}
			if got := jobclaim.MigrationRunKeepsLog(false, test.outcome); got != test.blocks {
				t.Errorf("MigrationRunKeepsLog() with no approval = %v, want %v", got, test.blocks)
			}
			if !jobclaim.MigrationRunKeepsLog(true, test.outcome) {
				t.Error("MigrationRunKeepsLog() with an approval = false")
			}
		})
	}
}

// TestEveryReportedOutcomeHasItsStatusTerm holds the one mapping both readers use to
// the set the runner's decoder admits, and to what an unknown word means.
func TestEveryReportedOutcomeHasItsStatusTerm(t *testing.T) {
	t.Parallel()

	want := map[string]operatorv1alpha1.MigrationRunOutcome{
		dataplane.MigrationOutcomeUpToDate: operatorv1alpha1.MigrationRunOutcomeUpToDate,
		dataplane.MigrationOutcomeApplied:  operatorv1alpha1.MigrationRunOutcomeApplied,
		dataplane.MigrationOutcomeDryRun:   operatorv1alpha1.MigrationRunOutcomeApplied,
		dataplane.MigrationOutcomeFailed:   operatorv1alpha1.MigrationRunOutcomeFailed,
		dataplane.MigrationOutcomePartial:  operatorv1alpha1.MigrationRunOutcomePartial,
		dataplane.MigrationOutcomeUnknown:  operatorv1alpha1.MigrationRunOutcomeUnknown,
		"a word this build does not know":  operatorv1alpha1.MigrationRunOutcomeUnknown,
	}
	for reported, outcome := range want {
		if got := jobclaim.MigrationRunOutcome(reported); got != outcome {
			t.Errorf("MigrationRunOutcome(%q) = %q, want %q", reported, got, outcome)
		}
		if reported != "a word this build does not know" && !dataplane.IsKnownMigrationOutcome(reported) {
			t.Errorf("%q is not an outcome the runner's decoder admits", reported)
		}
	}
}

// runSummary is what the runner leaves for a run that reported this outcome.
func runSummary(t *testing.T, operationID, outcome string) string {
	t.Helper()
	encoded, err := runner.EncodeSummary(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply, OperationID: operationID,
		CoordinationDigest: digest('c'), TargetIdentityDigest: digest('d'),
		MutationStarted: outcome != dataplane.MigrationOutcomeUpToDate,
		Uncertain:       outcome == dataplane.MigrationOutcomePartial || outcome == dataplane.MigrationOutcomeUnknown,
		MigrationRun: &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up", Outcome: outcome,
		},
	})
	if err != nil {
		t.Fatalf("EncodeSummary() error = %v", err)
	}
	return string(encoded)
}

// jobPod is a Pod its Job controls, with this termination message on its executor.
func jobPod(name string, owner types.UID, message string) corev1.Pod {
	controller := true
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, OwnerReferences: []metav1.OwnerReference{{Kind: "Job", UID: owner, Controller: &controller}},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "ptah", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}},
		}}},
	}
}

// TestAPodsReportIsReadOnlyFromTheOnePodItsJobControls reads what a Pod reported,
// and holds every way of reporting nothing to the unknown outcome, which is what the
// manager records for each of them.
func TestAPodsReportIsReadOnlyFromTheOnePodItsJobControls(t *testing.T) {
	t.Parallel()

	failed := runSummary(t, retentionOperation, dataplane.MigrationOutcomeFailed)
	notController := jobPod("a", retentionJobUID, failed)
	notController.OwnerReferences[0].Controller = nil
	running := jobPod("a", retentionJobUID, "")
	running.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	anotherContainer := jobPod("a", retentionJobUID, failed)
	anotherContainer.Status.ContainerStatuses[0].Name = "fetch-migrations"

	for _, test := range []struct {
		name string
		pods []corev1.Pod
		want operatorv1alpha1.MigrationRunOutcome
	}{
		{
			name: "a run that reported Failed",
			pods: []corev1.Pod{jobPod("a", retentionJobUID, failed)},
			want: operatorv1alpha1.MigrationRunOutcomeFailed,
		},
		{
			name: "a run that reported Partial",
			pods: []corev1.Pod{jobPod("a", retentionJobUID, runSummary(t, retentionOperation, dataplane.MigrationOutcomePartial))},
			want: operatorv1alpha1.MigrationRunOutcomePartial,
		},
		{
			name: "a run that reported it was up to date",
			pods: []corev1.Pod{jobPod("a", retentionJobUID, runSummary(t, retentionOperation, dataplane.MigrationOutcomeUpToDate))},
			want: operatorv1alpha1.MigrationRunOutcomeUpToDate,
		},
		{
			// The runtime's own text may stand in front, joined by a colon.
			name: "a report behind the runtime's message",
			pods: []corev1.Pod{jobPod("a", retentionJobUID, "OOMKilled: "+failed)},
			want: operatorv1alpha1.MigrationRunOutcomeFailed,
		},
		{
			name: "another Job's Pod beside this Job's",
			pods: []corev1.Pod{
				jobPod("a", "another-job", runSummary(t, retentionOperation, dataplane.MigrationOutcomePartial)),
				jobPod("b", retentionJobUID, failed),
			},
			want: operatorv1alpha1.MigrationRunOutcomeFailed,
		},
		{name: "no Pod", want: operatorv1alpha1.MigrationRunOutcomeUnknown},
		{
			name: "only another Job's Pod",
			pods: []corev1.Pod{jobPod("a", "another-job", failed)},
			want: operatorv1alpha1.MigrationRunOutcomeUnknown,
		},
		{
			name: "a Pod this Job owns and does not control",
			pods: []corev1.Pod{notController},
			want: operatorv1alpha1.MigrationRunOutcomeUnknown,
		},
		{
			// No one Pod's report speaks for another.
			name: "two Pods",
			pods: []corev1.Pod{jobPod("a", retentionJobUID, failed), jobPod("b", retentionJobUID, failed)},
			want: operatorv1alpha1.MigrationRunOutcomeUnknown,
		},
		{name: "a Pod still running", pods: []corev1.Pod{running}, want: operatorv1alpha1.MigrationRunOutcomeUnknown},
		{
			name: "only another container's message",
			pods: []corev1.Pod{anotherContainer},
			want: operatorv1alpha1.MigrationRunOutcomeUnknown,
		},
		{
			name: "no summary",
			pods: []corev1.Pod{jobPod("a", retentionJobUID, "")},
			want: operatorv1alpha1.MigrationRunOutcomeUnknown,
		},
		{
			name: "a summary cut short",
			pods: []corev1.Pod{jobPod("a", retentionJobUID, failed[:len(failed)/2])},
			want: operatorv1alpha1.MigrationRunOutcomeUnknown,
		},
		{
			// The label's hash is not the ID the runner wrote.
			name: "a summary of another attempt",
			pods: []corev1.Pod{jobPod("a", retentionJobUID, runSummary(t, "sha256:another", dataplane.MigrationOutcomeFailed))},
			want: operatorv1alpha1.MigrationRunOutcomeUnknown,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := jobclaim.ReportedMigrationRun(test.pods, retentionJobUID, retentionOperation); got != test.want {
				t.Fatalf("ReportedMigrationRun() = %q, want %q", got, test.want)
			}
		})
	}

	t.Run("a summary with no report in it", func(t *testing.T) {
		t.Parallel()
		encoded, err := runner.EncodeSummary(runner.Result{
			ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply, OperationID: retentionOperation,
			CoordinationDigest: digest('c'), TargetIdentityDigest: digest('d'), ChildExitCode: 2,
			Error: &runner.ResultError{Code: "invalid_migration_run_output", Message: "failed strict validation"},
		})
		if err != nil {
			t.Fatalf("EncodeSummary() error = %v", err)
		}
		pods := []corev1.Pod{jobPod("a", retentionJobUID, string(encoded))}
		if got := jobclaim.ReportedMigrationRun(pods, retentionJobUID, retentionOperation); got != operatorv1alpha1.MigrationRunOutcomeUnknown {
			t.Fatalf("ReportedMigrationRun() = %q, want Unknown", got)
		}
	})
}

// TestASummaryIsReadForItsOwnOperation holds the reader both webhook checks use to
// the operation it was asked about.
func TestASummaryIsReadForItsOwnOperation(t *testing.T) {
	t.Parallel()

	failed := runSummary(t, retentionOperation, dataplane.MigrationOutcomeFailed)
	pods := []corev1.Pod{jobPod("a", retentionJobUID, failed)}
	summary, read := jobclaim.ApplySummary(pods, retentionJobUID, runner.OperationMigrationApply, retentionOperation)
	if !read || summary.Migration == nil || summary.Migration.Outcome != dataplane.MigrationOutcomeFailed {
		t.Fatalf("ApplySummary() = %+v, %v", summary, read)
	}
	if _, read := jobclaim.ApplySummary(pods, retentionJobUID, runner.OperationApply, retentionOperation); read {
		t.Fatal("a migration run's summary read as a schema Apply's")
	}
	if _, read := jobclaim.ApplySummary(pods, retentionJobUID, runner.OperationMigrationApply, "sha256:another"); read {
		t.Fatal("a summary read for another attempt")
	}
}
