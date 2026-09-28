package e2e

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// mfMigration decodes a stored PtahMigration document, so a fixture reads
// the way the jq it replaces read it.
func mfMigration(t *testing.T, document string) *ptahv1alpha1.PtahMigration {
	t.Helper()
	migration := &ptahv1alpha1.PtahMigration{}
	if err := json.Unmarshal([]byte(document), migration); err != nil {
		t.Fatalf("decode %s: %v", document, err)
	}
	return migration
}

// mfMutation changes one clause of a reading its predicate accepts.
type mfMutation struct {
	name   string
	mutate func(*ptahv1alpha1.PtahMigration)
}

// mfRefusesEach holds a predicate to accepting its fixture and to refusing
// every mutation of it, each of which breaks one clause.
func mfRefusesEach(t *testing.T, document string, accepts func(*ptahv1alpha1.PtahMigration) bool, mutations []mfMutation) {
	t.Helper()
	if !accepts(mfMigration(t, document)) {
		t.Fatal("the fixture was refused")
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			t.Parallel()
			reading := mfMigration(t, document)
			mutation.mutate(reading)
			if accepts(reading) {
				t.Errorf("a reading with %s was accepted", mutation.name)
			}
		})
	}
}

// mfReading is one document a filter's self-test judged, and its verdict.
type mfReading struct {
	name     string
	accepted bool
	document string
}

// The readings the migration filter self-test held
// migration-stopped-run-recorded.jq to, judged with stoppedAt=3 and the job
// u-isolated-apply.
var mfStoppedReadings = []mfReading{
	{"the report the stopped run wrote", true, `{"status":{"phase":"VerifyingHistory",
 "lastRun":{"outcome":"Failed","jobUID":"u-isolated-apply","appliedVersions":[1,2],
  "message":"A migration failed and committed nothing; 2 migrations before it are recorded applied"}}}`},
	{"the same record once the history read moved the resource on", true, `{"status":{"phase":"Blocked",
 "lastRun":{"outcome":"Failed","jobUID":"u-isolated-apply","appliedVersions":[1,2]}}}`},
	{"no report was read, as before the runner passed SIGTERM on", false, `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply",
  "message":"read the Apply result: ptah runner result frame not found"}}}`},
	{"the outcome without the versions it applied", false, `{"status":{"lastRun":{"outcome":"Failed","jobUID":"u-isolated-apply"}}}`},
	{"fewer versions than the run applied", false, `{"status":{"lastRun":{"outcome":"Failed","jobUID":"u-isolated-apply","appliedVersions":[1]}}}`},
	{"the stopped migration named as applied", false, `{"status":{"lastRun":{"outcome":"Failed","jobUID":"u-isolated-apply","appliedVersions":[1,2,3]}}}`},
	{"a run that was never stopped", false, `{"status":{"lastRun":{"outcome":"Applied","jobUID":"u-isolated-apply","appliedVersions":[1,2,3]}}}`},
	{"a record of another run", false, `{"status":{"lastRun":{"outcome":"Failed","jobUID":"u-replacement","appliedVersions":[1,2]}}}`},
	{"no run recorded yet", false, `{"status":{"activeOperation":{"type":"Apply","jobUID":"u-isolated-apply"}}}`},
}

func TestStoppedRunRecordedJudgesTheSelfTestReadings(t *testing.T) {
	t.Parallel()
	for _, reading := range mfStoppedReadings {
		t.Run(reading.name, func(t *testing.T) {
			t.Parallel()
			if got := stoppedRunRecorded(mfMigration(t, reading.document).Status, "u-isolated-apply", 3); got != reading.accepted {
				t.Errorf("stoppedRunRecorded = %t, want %t", got, reading.accepted)
			}
		})
	}
}

func TestStoppedRunRecordedRefusesEachClause(t *testing.T) {
	t.Parallel()
	const stopped = `{"status":{"lastRun":{"outcome":"Failed","jobUID":"u-apply","appliedVersions":[1,2]}}}`
	accepts := func(migration *ptahv1alpha1.PtahMigration) bool {
		return stoppedRunRecorded(migration.Status, "u-apply", 3)
	}
	mfRefusesEach(t, stopped, accepts, []mfMutation{
		{"no run recorded", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun = nil }},
		{"another run", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.JobUID = "u-other" }},
		{"no Job UID", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.JobUID = "" }},
		{"a Partial outcome", func(m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomePartial
		}},
		{"the versions out of order", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.AppliedVersions = []int64{2, 1} }},
		{"a version skipped", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.AppliedVersions = []int64{1, 3} }},
		{"no versions", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.AppliedVersions = nil }},
	})
	// An empty job never matches a record with no UID, as jq's null never
	// equals a string.
	if stoppedRunRecorded(mfMigration(t, `{"status":{"lastRun":{"outcome":"Failed","appliedVersions":[1,2]}}}`).Status, "", 3) {
		t.Error("a record with no Job UID matched an empty job")
	}
	// Stopped in the first migration, nothing is applied, and an absent list
	// is the empty one, as ($run.appliedVersions // []) reads it.
	if !stoppedRunRecorded(mfMigration(t, `{"status":{"lastRun":{"outcome":"Failed","jobUID":"u-apply"}}}`).Status, "u-apply", 1) {
		t.Error("a run stopped in its first migration was refused")
	}
}

// The readings the self-test held migration-lost-log-run-recorded.jq to,
// judged with stoppedAt=3, digest=sha256:ff and the job u-isolated-apply.
var mfLostLogReadings = []mfReading{
	{"the record the summary produced", true, `{"status":{"lastRun":{"outcome":"Applied","jobUID":"u-isolated-apply",
  "message":"The Apply's frame could not be read from its log; its termination message, bound to frame sha256:ff, reports outcome applied with 3 migrations recorded applied, from version 1 to version 3"}}}`},
	{"the same run settled from its frame", false, `{"status":{"lastRun":{"outcome":"Applied","jobUID":"u-isolated-apply","appliedVersions":[1,2,3],
  "message":"3 migrations are recorded applied"}}}`},
	{"the run left unknown", false, `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply",
  "message":"read the Apply result: the result log is gone: nodes \"kind-worker2\" not found"}}}`},
	{"a summary bound to another frame", false, `{"status":{"lastRun":{"outcome":"Applied","jobUID":"u-isolated-apply",
  "message":"The Apply's frame could not be read from its log; its termination message, bound to frame sha256:ffff, reports outcome applied with 3 migrations recorded applied, from version 1 to version 3"}}}`},
	{"a summary that counts fewer versions", false, `{"status":{"lastRun":{"outcome":"Applied","jobUID":"u-isolated-apply",
  "message":"The Apply's frame could not be read from its log; its termination message, bound to frame sha256:ff, reports outcome applied with 2 migrations recorded applied, from version 1 to version 2"}}}`},
	{"a record of another run", false, `{"status":{"lastRun":{"outcome":"Applied","jobUID":"u-replacement",
  "message":"The Apply's frame could not be read from its log; its termination message, bound to frame sha256:ff, reports outcome applied with 3 migrations recorded applied, from version 1 to version 3"}}}`},
}

func TestLostLogRunRecordedJudgesTheSelfTestReadings(t *testing.T) {
	t.Parallel()
	for _, reading := range mfLostLogReadings {
		t.Run(reading.name, func(t *testing.T) {
			t.Parallel()
			if got := lostLogRunRecorded(mfMigration(t, reading.document).Status, "u-isolated-apply", "sha256:ff", 3); got != reading.accepted {
				t.Errorf("lostLogRunRecorded = %t, want %t", got, reading.accepted)
			}
		})
	}
}

func TestLostLogRunRecordedRefusesEachClause(t *testing.T) {
	t.Parallel()
	accepts := func(migration *ptahv1alpha1.PtahMigration) bool {
		return lostLogRunRecorded(migration.Status, "u-isolated-apply", "sha256:ff", 3)
	}
	mfRefusesEach(t, mfLostLogReadings[0].document, accepts, []mfMutation{
		{"no run recorded", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun = nil }},
		{"no Job UID", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.JobUID = "" }},
		{"a Failed outcome", func(m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeFailed
		}},
		{"no message", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.Message = "" }},
		{"the frame named without its closing comma", func(m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun.Message = strings.Replace(m.Status.LastRun.Message, "sha256:ff,", "sha256:ff ", 1)
		}},
		{"a range that stops short", func(m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun.Message = strings.Replace(m.Status.LastRun.Message, "to version 3", "to version 2", 1)
		}},
	})
}

func TestDispatchedApplyNeedsTheJobItNamed(t *testing.T) {
	t.Parallel()
	const dispatched = `{"status":{"activeOperation":{"type":"Apply","jobName":"j-apply","jobUID":"u-apply"}}}`
	mfRefusesEach(t, dispatched, func(m *ptahv1alpha1.PtahMigration) bool {
		_, _, ok := mfDispatchedApply(m)
		return ok
	}, []mfMutation{
		{"no claim", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation = nil }},
		{"a History claim", func(m *ptahv1alpha1.PtahMigration) {
			m.Status.ActiveOperation.Type = ptahv1alpha1.MigrationOperationHistory
		}},
		{"no Job name", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation.JobName = "" }},
		{"no Job UID", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation.JobUID = "" }},
	})
	job, uid, _ := mfDispatchedApply(mfMigration(t, dispatched))
	if job != "j-apply" || uid != "u-apply" {
		t.Errorf("mfDispatchedApply = %q, %q", job, uid)
	}
}

func TestStopRowApplyCarriesTheExecutionDeadline(t *testing.T) {
	t.Parallel()
	const claimed = `{"status":{"activeOperation":{"type":"Apply","jobName":"j-apply","jobUID":"u-apply",
  "executionNotAfter":"2026-09-26T10:05:00Z"}}}`
	mfRefusesEach(t, claimed, func(m *ptahv1alpha1.PtahMigration) bool {
		_, _, ok := stopRowApply(m)
		return ok
	}, []mfMutation{
		{"no claim", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation = nil }},
		{"a Verify claim", func(m *ptahv1alpha1.PtahMigration) {
			m.Status.ActiveOperation.Type = ptahv1alpha1.MigrationOperationVerify
		}},
		{"no Job name", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation.JobName = "" }},
		{"no Job UID", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation.JobUID = "" }},
		{"no execution deadline", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation.ExecutionNotAfter = nil }},
	})
	uid, notAfter, _ := stopRowApply(mfMigration(t, claimed))
	if uid != "u-apply" || notAfter.Unix() != time.Date(2026, 9, 26, 10, 5, 0, 0, time.UTC).Unix() {
		t.Errorf("stopRowApply = %q, %s", uid, notAfter)
	}
}

func TestRunRecordedForNamesTheJob(t *testing.T) {
	t.Parallel()
	mfRefusesEach(t, `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-apply"}}}`,
		func(m *ptahv1alpha1.PtahMigration) bool { return mfRunRecordedFor(m, "u-apply") },
		[]mfMutation{
			{"no run recorded", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun = nil }},
			{"another run", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.JobUID = "u-other" }},
		})
	if mfRunRecordedFor(mfMigration(t, `{"status":{"lastRun":{"outcome":"Unknown"}}}`), "") {
		t.Error("a run with no Job UID matched an empty one")
	}
}

// mfRunnerPod is an Apply Pod whose ptah container finished at the instant
// given, with the grace given.
func mfRunnerPod(finished time.Time, grace *int64) *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{TerminationGracePeriodSeconds: grace},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "runner-sidecar", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				FinishedAt: metav1.NewTime(finished.Add(-time.Hour)),
			}}},
			{Name: "ptah", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				FinishedAt: metav1.NewTime(finished),
			}}},
		}},
	}
}

func TestRunnerEndedInGraceBoundsTheStop(t *testing.T) {
	t.Parallel()
	notAfter := time.Date(2026, 9, 26, 10, 5, 0, 0, time.UTC)
	grace := int64(30)
	for _, row := range []struct {
		name     string
		pod      *corev1.Pod
		accepted bool
	}{
		{"ended at the deadline", mfRunnerPod(notAfter, &grace), true},
		{"ended in the last second of the grace", mfRunnerPod(notAfter.Add(29*time.Second), &grace), true},
		{"ended before the deadline", mfRunnerPod(notAfter.Add(-time.Second), &grace), false},
		{"ended when the grace ran out", mfRunnerPod(notAfter.Add(30*time.Second), &grace), false},
		{"no grace", mfRunnerPod(notAfter, nil), false},
		{"still running", &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "ptah", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		}}, Spec: corev1.PodSpec{TerminationGracePeriodSeconds: &grace}}, false},
		{"no ptah container", &corev1.Pod{Spec: corev1.PodSpec{TerminationGracePeriodSeconds: &grace}}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := runnerEndedInGrace(row.pod, notAfter); got != row.accepted {
				t.Errorf("runnerEndedInGrace = %t, want %t", got, row.accepted)
			}
		})
	}
}

func TestTerminationMessageIsThePtahContainers(t *testing.T) {
	t.Parallel()
	pod := mfRunnerPod(time.Now(), nil)
	pod.Status.ContainerStatuses[0].State.Terminated.Message = "the sidecar's"
	if _, ok := mfTerminationMessage(pod); ok {
		t.Error("an empty ptah message read as one")
	}
	pod.Status.ContainerStatuses[1].State.Terminated.Message = "PTAH_RUNNER_SUMMARY_V1 {}"
	if message, ok := mfTerminationMessage(pod); !ok || message != "PTAH_RUNNER_SUMMARY_V1 {}" {
		t.Errorf("mfTerminationMessage = %q, %t", message, ok)
	}
	pod.Status.ContainerStatuses[1].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	if _, ok := mfTerminationMessage(pod); ok {
		t.Error("a running container's message was read")
	}
}

func TestSummaryFrameDigestReadsTheSummaryLine(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, message, digest string
	}{
		{"the summary alone", `PTAH_RUNNER_SUMMARY_V1 {"frameDigest":"sha256:ff","outcome":"applied"}`, "sha256:ff"},
		{"after other lines", "starting\nPTAH_RUNNER_SUMMARY_V1 {\"frameDigest\":\"sha256:aa\"}\n", "sha256:aa"},
		{"after text on its line", `tail PTAH_RUNNER_SUMMARY_V1 {"frameDigest":"sha256:bb"}`, "sha256:bb"},
		{"the last marker on its line", `PTAH_RUNNER_SUMMARY_V1 x PTAH_RUNNER_SUMMARY_V1 {"frameDigest":"sha256:cc"}`, "sha256:cc"},
		{"the first summary line", "PTAH_RUNNER_SUMMARY_V1 {\"frameDigest\":\"sha256:01\"}\nPTAH_RUNNER_SUMMARY_V1 {\"frameDigest\":\"sha256:02\"}", "sha256:01"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			digest, err := summaryFrameDigest(row.message)
			if err != nil || digest != row.digest {
				t.Errorf("summaryFrameDigest = %q, %v, want %q", digest, err, row.digest)
			}
		})
	}
	for _, row := range []struct{ name, message string }{
		{"no summary", "PTAH_RUNNER_RESULT_V1 12 abc"},
		{"no marker space", `PTAH_RUNNER_SUMMARY_V1{"frameDigest":"sha256:ff"}`},
		{"not JSON", "PTAH_RUNNER_SUMMARY_V1 frameDigest=sha256:ff"},
		{"no frame digest", `PTAH_RUNNER_SUMMARY_V1 {"outcome":"applied"}`},
		{"a null frame digest", `PTAH_RUNNER_SUMMARY_V1 {"frameDigest":null}`},
		{"empty", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if digest, err := summaryFrameDigest(row.message); err == nil {
				t.Errorf("summaryFrameDigest(%q) = %q, want a refusal", row.message, digest)
			}
		})
	}
}

func TestSessionCountReadsOnlyANumber(t *testing.T) {
	t.Parallel()
	for output, want := range map[string]int{"0": 0, "2": 2, "": -1, "ERROR 1045": -1, "1 2": -1} {
		if got := mfSessionCount(output); got != want {
			t.Errorf("mfSessionCount(%q) = %d, want %d", output, got, want)
		}
	}
}

func TestDeletionRetainsClaimHoldsTheRunningApply(t *testing.T) {
	t.Parallel()
	const deleting = `{"metadata":{"deletionTimestamp":"2026-09-26T10:00:00Z",
  "finalizers":["example.com/other","operator.ptah.run/migration-operation"]},
 "status":{"activeOperation":{"type":"Apply","jobName":"j-apply","jobUID":"u-apply"}}}`
	mfRefusesEach(t, deleting, deletionRetainsClaim, []mfMutation{
		{"no deletion", func(m *ptahv1alpha1.PtahMigration) { m.DeletionTimestamp = nil }},
		{"the finalizer released", func(m *ptahv1alpha1.PtahMigration) { m.Finalizers = []string{"example.com/other"} }},
		{"no claim", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation = nil }},
		{"a History claim", func(m *ptahv1alpha1.PtahMigration) {
			m.Status.ActiveOperation.Type = ptahv1alpha1.MigrationOperationHistory
		}},
	})
}

func TestOnePodRunningIsExactlyOne(t *testing.T) {
	t.Parallel()
	running := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	pending := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}
	for _, row := range []struct {
		name     string
		pods     []corev1.Pod
		accepted bool
	}{
		{"one running", []corev1.Pod{running}, true},
		{"none", nil, false},
		{"two running", []corev1.Pod{running, running}, false},
		{"one pending", []corev1.Pod{pending}, false},
		{"one running and one pending", []corev1.Pod{running, pending}, false},
	} {
		if got := mfOnePodRunning(row.pods); got != row.accepted {
			t.Errorf("%s: mfOnePodRunning = %t, want %t", row.name, got, row.accepted)
		}
	}
}

func TestSuspensionRetainsClaimHoldsTheDispatchedApply(t *testing.T) {
	t.Parallel()
	const suspended = `{"metadata":{"finalizers":["operator.ptah.run/migration-operation"]},"spec":{"suspend":true},
 "status":{"activeOperation":{"type":"Apply","jobName":"j-apply","jobUID":"u-apply"}}}`
	mfRefusesEach(t, suspended, func(m *ptahv1alpha1.PtahMigration) bool { return suspensionRetainsClaim(m, "u-apply") },
		[]mfMutation{
			{"not suspended", func(m *ptahv1alpha1.PtahMigration) { m.Spec.Suspend = false }},
			{"the finalizer released", func(m *ptahv1alpha1.PtahMigration) { m.Finalizers = nil }},
			{"no claim", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation = nil }},
			{"a Resolve claim", func(m *ptahv1alpha1.PtahMigration) {
				m.Status.ActiveOperation.Type = ptahv1alpha1.MigrationOperationResolve
			}},
			{"a replacement Job", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation.JobUID = "u-other" }},
		})
}

// mfSuspendedReading is a migration that settled Suspended with the
// dispatched run held unresolved.
const mfSuspendedReading = `{"metadata":{"generation":2},"spec":{"suspend":true},
 "status":{"phase":"Suspended","observedGeneration":2,
  "lastRun":{"outcome":"Unknown","jobName":"j-apply","jobUID":"u-apply"},
  "unresolvedRun":{"jobUID":"u-apply"},
  "conditions":[
   {"type":"Ready","status":"False","reason":"Suspended","message":"suspended","lastTransitionTime":"2026-09-26T10:00:00Z"},
   {"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown",
    "message":"the Apply's outcome is unknown: its inputs changed while it ran","lastTransitionTime":"2026-09-26T10:01:00Z"}]}}`

func mfSuspendedMutations() []mfMutation {
	return []mfMutation{
		{"another phase", func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseBlocked }},
		{"a new claim", func(m *ptahv1alpha1.PtahMigration) {
			m.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{Type: ptahv1alpha1.MigrationOperationResolve}
		}},
		{"no run recorded", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun = nil }},
		{"another run recorded", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.JobUID = "u-other" }},
		{"nothing unresolved", func(m *ptahv1alpha1.PtahMigration) { m.Status.UnresolvedRun = nil }},
		{"another run unresolved", func(m *ptahv1alpha1.PtahMigration) { m.Status.UnresolvedRun.JobUID = "u-other" }},
	}
}

func TestSuspendedAfterApplyRefusesEachClause(t *testing.T) {
	t.Parallel()
	blocked := func(m *ptahv1alpha1.PtahMigration) *metav1.Condition { return &m.Status.Conditions[1] }
	mutations := append(mfSuspendedMutations(),
		mfMutation{"an Applied outcome", func(m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeApplied
		}},
		mfMutation{"no Blocked condition", func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions = m.Status.Conditions[:1] }},
		mfMutation{"Blocked False", func(m *ptahv1alpha1.PtahMigration) { blocked(m).Status = metav1.ConditionFalse }},
		mfMutation{"another reason", func(m *ptahv1alpha1.PtahMigration) { blocked(m).Reason = "RealmConflict" }},
		mfMutation{"a message about something else", func(m *ptahv1alpha1.PtahMigration) {
			blocked(m).Message = "the Apply's outcome is unknown: its Pod is gone"
		}},
		mfMutation{"the reason on another condition", func(m *ptahv1alpha1.PtahMigration) {
			blocked(m).Type = "Degraded"
		}},
	)
	mfRefusesEach(t, mfSuspendedReading, func(m *ptahv1alpha1.PtahMigration) bool { return suspendedAfterApply(m, "u-apply") },
		mutations)
}

func TestSuspendedQuietRefusesEachClause(t *testing.T) {
	t.Parallel()
	mfRefusesEach(t, mfSuspendedReading, func(m *ptahv1alpha1.PtahMigration) bool { return suspendedQuiet(m, "u-apply") },
		mfSuspendedMutations())
	// The quiet window asks about claims, not about the outcome or the
	// condition, which the controller may rewrite while it stays suspended.
	reading := mfMigration(t, mfSuspendedReading)
	reading.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeApplied
	reading.Status.Conditions = nil
	if !suspendedQuiet(reading, "u-apply") {
		t.Error("the quiet window required more than a quiet claim")
	}
}

func TestResumedSettledRefusesEachClause(t *testing.T) {
	t.Parallel()
	mfRefusesEach(t, `{"metadata":{"generation":3},"status":{"phase":"InSync","observedGeneration":3,
  "lastRun":{"outcome":"Unknown","jobUID":"u-apply"}}}`, resumedSettled, []mfMutation{
		{"another phase", func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseReading }},
		{"a run still unresolved", func(m *ptahv1alpha1.PtahMigration) {
			m.Status.UnresolvedRun = &ptahv1alpha1.UnresolvedMigrationRunStatus{JobUID: "u-apply"}
		}},
		{"an older generation observed", func(m *ptahv1alpha1.PtahMigration) { m.Status.ObservedGeneration = 2 }},
	})
}

func TestRetryScheduledNeedsASecondAttemptAndItsDeadline(t *testing.T) {
	t.Parallel()
	const scheduled = `{"status":{"activeOperation":{"type":"History","attempt":2,"retryNotBefore":"2026-09-26T10:02:00Z"}}}`
	mfRefusesEach(t, scheduled, retryScheduled, []mfMutation{
		{"no claim", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation = nil }},
		{"a first attempt", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation.Attempt = 1 }},
		{"no deadline", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation.RetryNotBefore = nil }},
	})
	mfRefusesEach(t, scheduled, func(m *ptahv1alpha1.PtahMigration) bool {
		_, ok := retryDeadline(m)
		return ok
	}, []mfMutation{
		{"no claim", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation = nil }},
		{"no deadline", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation.RetryNotBefore = nil }},
	})
	if deadline, _ := retryDeadline(mfMigration(t, scheduled)); !deadline.Equal(time.Date(2026, 9, 26, 10, 2, 0, 0, time.UTC)) {
		t.Errorf("retryDeadline = %s", deadline)
	}
}

func TestPlanPublishedForApprovalNamesThePlan(t *testing.T) {
	t.Parallel()
	const awaiting = `{"status":{"phase":"AwaitingApproval","plan":{"name":"e2e-release-fault-postgresql-abc"}}}`
	mfRefusesEach(t, awaiting, func(m *ptahv1alpha1.PtahMigration) bool {
		_, ok := planPublishedForApproval(m)
		return ok
	}, []mfMutation{
		{"another phase", func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhasePlanning }},
		{"no plan", func(m *ptahv1alpha1.PtahMigration) { m.Status.Plan = nil }},
		{"a plan with no name", func(m *ptahv1alpha1.PtahMigration) { m.Status.Plan.Name = "" }},
	})
	if plan, _ := planPublishedForApproval(mfMigration(t, awaiting)); plan != "e2e-release-fault-postgresql-abc" {
		t.Errorf("planPublishedForApproval = %q", plan)
	}
}

func TestApplyClaimUnderLeaseNeedsTheEpoch(t *testing.T) {
	t.Parallel()
	const claimed = `{"status":{"activeOperation":{"type":"Apply","jobName":"j-apply","jobUID":"u-apply","leaseEpoch":"v1-epoch"}}}`
	mfRefusesEach(t, claimed, func(m *ptahv1alpha1.PtahMigration) bool {
		_, _, _, ok := applyClaimUnderLease(m)
		return ok
	}, []mfMutation{
		{"no claim", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation = nil }},
		{"a Verify claim", func(m *ptahv1alpha1.PtahMigration) {
			m.Status.ActiveOperation.Type = ptahv1alpha1.MigrationOperationVerify
		}},
		{"no Job name", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation.JobName = "" }},
		{"no Job UID", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation.JobUID = "" }},
		{"no epoch", func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation.LeaseEpoch = "" }},
	})
	job, uid, epoch, _ := applyClaimUnderLease(mfMigration(t, claimed))
	if job != "j-apply" || uid != "u-apply" || epoch != "v1-epoch" {
		t.Errorf("applyClaimUnderLease = %q, %q, %q", job, uid, epoch)
	}
}

func TestReleaseOwedRefusesEachClause(t *testing.T) {
	t.Parallel()
	const owed = `{"status":{"lastRun":{"outcome":"Applied","jobUID":"u-apply"},"pendingLockRelease":{"leaseEpoch":"v1-epoch"}}}`
	mfRefusesEach(t, owed, func(m *ptahv1alpha1.PtahMigration) bool { return releaseOwed(m, "u-apply", "v1-epoch") },
		[]mfMutation{
			{"a claim still active", func(m *ptahv1alpha1.PtahMigration) {
				m.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{Type: ptahv1alpha1.MigrationOperationApply}
			}},
			{"no run recorded", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun = nil }},
			{"another run", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.JobUID = "u-other" }},
			{"a Failed outcome", func(m *ptahv1alpha1.PtahMigration) {
				m.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeFailed
			}},
			{"nothing owed", func(m *ptahv1alpha1.PtahMigration) { m.Status.PendingLockRelease = nil }},
			{"another epoch owed", func(m *ptahv1alpha1.PtahMigration) { m.Status.PendingLockRelease.LeaseEpoch = "v1-other" }},
		})
	mfRefusesEach(t, owed, func(m *ptahv1alpha1.PtahMigration) bool { return releaseStillOwed(m, "v1-epoch") },
		[]mfMutation{
			{"a new claim", func(m *ptahv1alpha1.PtahMigration) {
				m.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{Type: ptahv1alpha1.MigrationOperationResolve}
			}},
			{"nothing owed", func(m *ptahv1alpha1.PtahMigration) { m.Status.PendingLockRelease = nil }},
			{"another epoch owed", func(m *ptahv1alpha1.PtahMigration) { m.Status.PendingLockRelease.LeaseEpoch = "v1-other" }},
		})
	// An absent epoch is jq's null, which equals no string, the empty one
	// included.
	if releaseStillOwed(mfMigration(t, `{"status":{"pendingLockRelease":{}}}`), "") {
		t.Error("a release with no epoch matched an empty one")
	}
}

// mfLease is a Lease in a namespace, annotated with an epoch when one is
// given.
func mfLease(namespace, name, epoch string, holder *string) coordinationv1.Lease {
	lease := coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: holder},
	}
	if epoch != "" {
		lease.Annotations = map[string]string{annotationLeaseEpoch: epoch}
	}
	return lease
}

func TestLeaseAtEpochFindsExactlyOne(t *testing.T) {
	t.Parallel()
	holder := "ptah-operator-7c9/u-apply"
	leases := []coordinationv1.Lease{
		mfLease("kube-system", "kube-scheduler", "", &holder),
		mfLease("ptah-system", "ptah-realm-a", "v1-other", &holder),
		mfLease("ptah-system", "ptah-realm-b", "v1-epoch", &holder),
	}
	namespace, name, found, err := leaseAtEpoch(leases, "v1-epoch")
	if err != nil || namespace != "ptah-system" || name != "ptah-realm-b" || found != holder {
		t.Errorf("leaseAtEpoch = %q, %q, %q, %v", namespace, name, found, err)
	}
	if _, _, found, err := leaseAtEpoch([]coordinationv1.Lease{mfLease("ns", "l", "v1-epoch", nil)}, "v1-epoch"); err != nil || found != "" {
		t.Errorf("a Lease with no holder read %q, %v", found, err)
	}
	if _, _, _, err := leaseAtEpoch(leases, "v1-missing"); err == nil {
		t.Error("an epoch no Lease carries was found")
	}
	if _, _, _, err := leaseAtEpoch(append(leases, mfLease("other", "ptah-realm-b", "v1-epoch", &holder)), "v1-epoch"); err == nil {
		t.Error("an epoch two Leases carry was taken as one")
	}
	if _, _, _, err := leaseAtEpoch(leases, ""); err == nil {
		t.Error("an empty epoch matched a Lease with no epoch")
	}
}

// mfManagerPod is a manager Pod running as an account in a phase.
func mfManagerPod(name, account string, phase corev1.PodPhase, deleting bool) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec:       corev1.PodSpec{ServiceAccountName: account},
		Status:     corev1.PodStatus{Phase: phase},
	}
	if deleting {
		now := metav1.Now()
		pod.DeletionTimestamp = &now
	}
	return pod
}

func TestSharedManagerAccountIsOne(t *testing.T) {
	t.Parallel()
	account, err := sharedManagerAccount([]corev1.Pod{
		mfManagerPod("a", "ptah-operator", corev1.PodRunning, false),
		mfManagerPod("b", "ptah-operator", corev1.PodPending, false),
	})
	if err != nil || account != "ptah-operator" {
		t.Errorf("sharedManagerAccount = %q, %v", account, err)
	}
	if _, err := sharedManagerAccount([]corev1.Pod{
		mfManagerPod("a", "ptah-operator", corev1.PodRunning, false),
		mfManagerPod("b", "ptah-operator-next", corev1.PodRunning, false),
	}); err == nil {
		t.Error("two accounts were taken as one")
	}
	if _, err := sharedManagerAccount(nil); err == nil {
		t.Error("no manager Pod gave an account")
	}
}

func TestRunningManagerPodSkipsPodsThatCannotWrite(t *testing.T) {
	t.Parallel()
	name, uid, ok := runningManagerPod([]corev1.Pod{
		mfManagerPod("pending", "ptah-operator", corev1.PodPending, false),
		mfManagerPod("leaving", "ptah-operator", corev1.PodRunning, true),
		mfManagerPod("serving", "ptah-operator", corev1.PodRunning, false),
		mfManagerPod("second", "ptah-operator", corev1.PodRunning, false),
	})
	if !ok || name != "serving" || uid != "uid-serving" {
		t.Errorf("runningManagerPod = %q, %q, %t", name, uid, ok)
	}
	if _, _, ok := runningManagerPod([]corev1.Pod{mfManagerPod("leaving", "ptah-operator", corev1.PodRunning, true)}); ok {
		t.Error("a Pod being deleted was taken as running")
	}
}

func TestReleaseFaultPolicyRefusesOnlyTheRelease(t *testing.T) {
	t.Parallel()
	documents := releaseFaultPolicyDocuments("ptah-e2e-release-fault-postgresql", "ptah-realm-b", "ptah-system",
		"system:serviceaccount:ptah-system:ptah-operator")
	if len(documents) != 2 {
		t.Fatalf("the fault is %d documents, want a policy and its binding", len(documents))
	}
	want := []map[string]any{
		{
			"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicy",
			"metadata": map[string]any{"name": "ptah-e2e-release-fault-postgresql"},
			"spec": map[string]any{
				"failurePolicy": "Fail",
				"matchConstraints": map[string]any{"resourceRules": []any{map[string]any{
					"apiGroups": []any{"coordination.k8s.io"}, "apiVersions": []any{"v1"},
					"operations": []any{"UPDATE"}, "resources": []any{"leases"},
				}}},
				"matchConditions": []any{
					map[string]any{
						"name":       "this-realm-lease",
						"expression": "object.metadata.name == 'ptah-realm-b' && object.metadata.namespace == 'ptah-system'",
					},
					map[string]any{
						"name":       "written-by-the-manager",
						"expression": "request.userInfo.username == 'system:serviceaccount:ptah-system:ptah-operator'",
					},
				},
				"validations": []any{map[string]any{
					"expression": "!(has(oldObject.spec.holderIdentity) && oldObject.spec.holderIdentity != '' && " +
						"(!has(object.spec.holderIdentity) || object.spec.holderIdentity == ''))",
					"message": "e2e fault: this realm Lease may not be released",
					"reason":  "Forbidden",
				}},
			},
		},
		{
			"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicyBinding",
			"metadata": map[string]any{"name": "ptah-e2e-release-fault-postgresql"},
			"spec": map[string]any{
				"policyName": "ptah-e2e-release-fault-postgresql", "validationActions": []any{"Deny"},
			},
		},
	}
	if !reflect.DeepEqual(documents, want) {
		got, _ := json.MarshalIndent(documents, "", "  ")
		t.Errorf("the release fault is\n%s", got)
	}
	// The row waits for this text in the refusal, so the message it installs
	// has to carry it.
	if !strings.Contains(releaseFaultMessage, "may not be released") {
		t.Errorf("the fault answers %q, which the row does not wait for", releaseFaultMessage)
	}
}

func TestAddedUIDsCountsOnlyAdditions(t *testing.T) {
	t.Parallel()
	before := []string{"u-1", "u-2"}
	if added := mfAddedUIDs([]string{"u-2"}, before); len(added) != 0 {
		t.Errorf("a removal counted as %v", added)
	}
	if added := mfAddedUIDs([]string{"u-1", "u-2", "u-3"}, before); !slices.Equal(added, []string{"u-3"}) {
		t.Errorf("mfAddedUIDs = %v", added)
	}
	if added := mfAddedUIDs([]string{"u-3"}, nil); !slices.Equal(added, []string{"u-3"}) {
		t.Errorf("against nothing, mfAddedUIDs = %v", added)
	}
}

func TestStateLinesNameTheStateAndNothingElse(t *testing.T) {
	t.Parallel()
	lines := mfStateLines(mfMigration(t, `{"metadata":{"name":"e2e-suspend-postgresql"},"spec":{"suspend":true},
 "status":{"phase":"Suspended","activeOperation":{"type":"Apply","jobName":"j-apply","leaseEpoch":"v1-epoch"},
  "pendingLockRelease":{"leaseEpoch":"v1-epoch"},
  "lastRun":{"outcome":"Unknown","jobName":"j-apply","jobUID":"u-apply","message":"postgres://ptah_e2e:secret@db/x"},
  "unresolvedRun":{"jobUID":"u-apply"},
  "conditions":[{"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown","message":"postgres://ptah_e2e:secret@db/x",
   "lastTransitionTime":"2026-09-26T10:01:00Z"}]}}`))
	text := strings.Join(lines, "\n")
	for _, want := range []string{
		"suspend=true phase=Suspended activeOperation=Apply/j-apply epoch=v1-epoch",
		"pendingLockRelease=v1-epoch lastRun=Unknown/j-apply/u-apply unresolvedRun=u-apply",
		"condition Blocked=True reason=ApplyOutcomeUnknown at=2026-09-26T10:01:00Z",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the state lines do not say %q:\n%s", want, text)
		}
	}
	// Messages are free text a controller writes, so the lines leave them out.
	if strings.Contains(text, "secret") {
		t.Errorf("the state lines print a message:\n%s", text)
	}
	empty := strings.Join(mfStateLines(&ptahv1alpha1.PtahMigration{}), "\n")
	if !strings.Contains(empty, "phase=<none> activeOperation=<none>") ||
		!strings.Contains(empty, "pendingLockRelease=<none> lastRun=<none> unresolvedRun=<none>") {
		t.Errorf("an empty status reads:\n%s", empty)
	}
}
