package e2e

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// mrReading is the reading a failing lifecycle printed for the partial row,
// with that cluster's digests, times and names scrubbed out: the document
// that broke two proofs at once, whose phase is Resolving while the refusal
// holds and whose pending count is one.
const mrReading = "../../testdata/e2e/readings/partial-run-left-a-dirty-revision.json"

func mrMigrationStatus(t *testing.T, document string) ptahv1alpha1.PtahMigrationStatus {
	t.Helper()
	migration := &ptahv1alpha1.PtahMigration{}
	if err := json.Unmarshal([]byte(document), migration); err != nil {
		t.Fatalf("decode %s: %v", document, err)
	}
	return migration.Status
}

func mrSchemaStatus(t *testing.T, document string) ptahv1alpha1.PtahSchemaStatus {
	t.Helper()
	schema := &ptahv1alpha1.PtahSchema{}
	if err := json.Unmarshal([]byte(document), schema); err != nil {
		t.Fatalf("decode %s: %v", document, err)
	}
	return schema.Status
}

func mrRecordedReading(t *testing.T) ptahv1alpha1.PtahMigrationStatus {
	t.Helper()
	content, err := os.ReadFile(mrReading)
	if err != nil {
		t.Fatal(err)
	}
	return mrMigrationStatus(t, string(content))
}

// mrMutation changes one clause of a reading its predicate accepts.
type mrMutation[S any] struct {
	name   string
	mutate func(*S)
}

// mrRefusesEach holds a predicate to accepting its fixture and to refusing
// every mutation of it, each of which breaks one clause.
func mrRefusesEach[S any](t *testing.T, fixture func() S, accepts func(S) bool, mutations []mrMutation[S]) {
	t.Helper()
	if !accepts(fixture()) {
		t.Fatal("the fixture was refused")
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			t.Parallel()
			reading := fixture()
			mutation.mutate(&reading)
			if accepts(reading) {
				t.Errorf("a reading with %s was accepted", mutation.name)
			}
		})
	}
}

func mrCondition(kind string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: kind, Status: status, Reason: reason, Message: message}
}

func TestPartialRunRecordedReadsTheRecord(t *testing.T) {
	t.Parallel()
	if !partialRunRecorded(mrRecordedReading(t)) {
		t.Error("the reading the failing run printed was refused")
	}
	for name, document := range map[string]string{
		"a partial run with no version list":   `{"status":{"lastRun":{"outcome":"Partial"}}}`,
		"a partial run with an empty list":     `{"status":{"lastRun":{"outcome":"Partial","appliedVersions":[]}}}`,
		"a partial run, whatever the phase is": `{"status":{"phase":"Resolving","lastRun":{"outcome":"Partial"}}}`,
	} {
		if !partialRunRecorded(mrMigrationStatus(t, document)) {
			t.Errorf("%s was refused", name)
		}
	}
	for name, document := range map[string]string{
		"a run that finished":              `{"status":{"lastRun":{"outcome":"Applied","appliedVersions":[4]}}}`,
		"a partial that claimed a version": `{"status":{"lastRun":{"outcome":"Partial","appliedVersions":[4]}}}`,
		"no run at all":                    `{"status":{"phase":"Blocked"}}`,
	} {
		if partialRunRecorded(mrMigrationStatus(t, document)) {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestDirtyReadingHoldsTheRefusalNotThePhase(t *testing.T) {
	t.Parallel()
	if !dirtyReading(mrRecordedReading(t), 3) {
		t.Error("the reading that broke the dirty wait was refused")
	}
	for name, document := range map[string]string{
		"dirty at 3, mid-cycle, Ptah counting it pending": `{"status":{"phase":"Resolving","history":{"dirty":true,"currentVersion":3,"pendingCount":1},
 "conditions":[{"type":"Blocked","status":"True","reason":"HistoryDirty"}]}}`,
		"dirty at 3, counted pending by nobody": `{"status":{"phase":"Blocked","history":{"dirty":true,"currentVersion":3,"pendingCount":0},
 "conditions":[{"type":"Blocked","status":"True","reason":"HistoryDirty"}]}}`,
	} {
		if !dirtyReading(mrMigrationStatus(t, document), 3) {
			t.Errorf("%s was refused", name)
		}
	}
	for name, document := range map[string]string{
		"no dirty revision": `{"status":{"phase":"Blocked","history":{"dirty":false,"currentVersion":3},
 "conditions":[{"type":"Blocked","status":"True","reason":"HistoryDirty"}]}}`,
		"stopped at another version": `{"status":{"phase":"Blocked","history":{"dirty":true,"currentVersion":4},
 "conditions":[{"type":"Blocked","status":"True","reason":"HistoryDirty"}]}}`,
		"blocked for another reason": `{"status":{"phase":"Blocked","history":{"dirty":true,"currentVersion":3},
 "conditions":[{"type":"Blocked","status":"True","reason":"HistoryModified"}]}}`,
		"the refusal lapsed": `{"status":{"history":{"dirty":true,"currentVersion":3},
 "conditions":[{"type":"Blocked","status":"False","reason":"HistoryDirty"}]}}`,
		"no history read yet":  `{"status":{"conditions":[{"type":"Blocked","status":"True","reason":"HistoryDirty"}]}}`,
		"no conditions at all": `{"status":{"history":{"dirty":true,"currentVersion":3}}}`,
	} {
		if dirtyReading(mrMigrationStatus(t, document), 3) {
			t.Errorf("%s was accepted", name)
		}
	}
}

func mrHistoryAheadFixture(t *testing.T) func() ptahv1alpha1.PtahMigrationStatus {
	return func() ptahv1alpha1.PtahMigrationStatus {
		return mrMigrationStatus(t, `{"status":{"artifact":{"digest":"sha256:ff"},
 "history":{"currentVersion":3,"appliedCount":2,"pendingCount":0},
 "conditions":[{"type":"Blocked","status":"True","reason":"HistoryAhead"},
               {"type":"Ready","status":"False","reason":"HistoryAhead"}]}}`)
	}
}

func TestHistoryAheadNeedsBothNumbers(t *testing.T) {
	t.Parallel()
	accepts := func(status ptahv1alpha1.PtahMigrationStatus) bool { return historyAhead(status, "sha256:ff", 3, 2) }
	mrRefusesEach(t, mrHistoryAheadFixture(t), accepts, []mrMutation[ptahv1alpha1.PtahMigrationStatus]{
		{"the tag the operator read is another one", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Artifact.Digest = "sha256:ee" }},
		{"no artifact resolved", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Artifact = nil }},
		{"no history read", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History = nil }},
		{"the database at another version", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.CurrentVersion = 2 }},
		{"an ordinary settled database", func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.History.AppliedCount = 3
			s.Conditions = []metav1.Condition{mrCondition("Ready", metav1.ConditionTrue, "HistoryMatched", "")}
		}},
		{"work still pending", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.PendingCount = 1 }},
		{"a dirty revision", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.Dirty = true }},
		{"a modified version", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.ModifiedVersions = []int64{1} }},
		{"a plan published anyway", func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.Plan = &ptahv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-0", UID: "u"}
		}},
		{"an operation running", func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{Type: ptahv1alpha1.MigrationOperationApply}
		}},
		{"called ready", func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.Conditions = append(s.Conditions, mrCondition("Ready", metav1.ConditionTrue, "HistoryMatched", ""))
		}},
		{"no conditions for jq to iterate", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions = nil }},
	})
	for name, document := range map[string]string{
		"an ordinary settled database": `{"status":{"artifact":{"digest":"sha256:ff"},
 "history":{"currentVersion":3,"appliedCount":3,"pendingCount":0},
 "conditions":[{"type":"Ready","status":"True","reason":"HistoryMatched"}]}}`,
		"work still pending": `{"status":{"artifact":{"digest":"sha256:ff"},
 "history":{"currentVersion":3,"appliedCount":2,"pendingCount":1},
 "conditions":[{"type":"Ready","status":"False","reason":"MigrationsPending"}]}}`,
		"a plan published anyway": `{"status":{"artifact":{"digest":"sha256:ff"},"plan":{"name":"ptah-mplan-0","uid":"u"},
 "history":{"currentVersion":3,"appliedCount":2,"pendingCount":0},
 "conditions":[{"type":"Ready","status":"False","reason":"HistoryAhead"}]}}`,
		"the tag the operator read is another one": `{"status":{"artifact":{"digest":"sha256:ee"},
 "history":{"currentVersion":3,"appliedCount":2,"pendingCount":0},
 "conditions":[{"type":"Ready","status":"False","reason":"HistoryAhead"}]}}`,
	} {
		if accepts(mrMigrationStatus(t, document)) {
			t.Errorf("the self-test's %q was accepted", name)
		}
	}
}

func mrRivalFixture(t *testing.T) func() ptahv1alpha1.PtahSchemaStatus {
	return func() ptahv1alpha1.PtahSchemaStatus {
		return mrSchemaStatus(t, `{"status":{"phase":"Blocked",
 "conditions":[{"type":"Ready","status":"False","reason":"RealmNotAuthorized","message":"realm e2e-realm-postgresql does not list this namespace"},
               {"type":"ApprovalRequired","status":"False","reason":"RealmNotAuthorized","message":"nothing to approve"}]}}`)
	}
}

func TestRivalRefusedWhole(t *testing.T) {
	t.Parallel()
	accepts := func(status ptahv1alpha1.PtahSchemaStatus) bool {
		return realmNotAuthorized(status) && rivalRefusedWhole(status, "e2e-test", "e2e-migrations-postgresql")
	}
	mrRefusesEach(t, mrRivalFixture(t), accepts, []mrMutation[ptahv1alpha1.PtahSchemaStatus]{
		{"a phase other than Blocked", func(s *ptahv1alpha1.PtahSchemaStatus) { s.Phase = ptahv1alpha1.PhaseResolving }},
		{"an operation claimed", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationResolve}
		}},
		{"approval still required", func(s *ptahv1alpha1.PtahSchemaStatus) { s.Conditions[1].Status = metav1.ConditionTrue }},
		{"not ready for another reason", func(s *ptahv1alpha1.PtahSchemaStatus) { s.Conditions[0].Reason = "RealmConflict" }},
		{"no approval refusal", func(s *ptahv1alpha1.PtahSchemaStatus) { s.Conditions = s.Conditions[:1] }},
		{"the admitted namespace named", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Conditions[1].Message = "the realm admits e2e-test"
		}},
		{"the admitted migration named", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Conditions[0].Message = "e2e-migrations-postgresql holds the realm"
		}},
	})
}

func TestReadyTransitionDatesTheRefusal(t *testing.T) {
	t.Parallel()
	at := metav1.NewTime(time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC))
	later := metav1.NewTime(at.Add(time.Hour))
	conditions := []metav1.Condition{
		{Type: "ApprovalRequired", LastTransitionTime: later},
		{Type: "Ready", LastTransitionTime: at},
		{Type: "Ready", LastTransitionTime: later},
	}
	if got, ok := readyTransition(conditions); !ok || !got.Equal(at.Time) {
		t.Errorf("readyTransition = %v, %t; want the first Ready's %v", got, ok, at)
	}
	if _, ok := readyTransition([]metav1.Condition{{Type: "Ready"}}); ok {
		t.Error("a Ready with no transition time dated the refusal")
	}
	if _, ok := readyTransition(conditions[:1]); ok {
		t.Error("a status with no Ready dated the refusal")
	}
}

func TestMigrationKeptRunning(t *testing.T) {
	t.Parallel()
	refusedAt := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	fixture := func() ptahv1alpha1.PtahMigrationStatus {
		return ptahv1alpha1.PtahMigrationStatus{
			History: &ptahv1alpha1.MigrationHistoryStatus{
				ObservedAt: metav1.NewTime(refusedAt), CurrentVersion: 3, AppliedCount: 3,
			},
			Conditions: []metav1.Condition{mrCondition("Ready", metav1.ConditionTrue, "HistoryMatched", "")},
		}
	}
	accepts := func(status ptahv1alpha1.PtahMigrationStatus) bool { return migrationKeptRunning(status, refusedAt) }
	mrRefusesEach(t, fixture, accepts, []mrMutation[ptahv1alpha1.PtahMigrationStatus]{
		{"a reading from before the refusal", func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.History.ObservedAt = metav1.NewTime(refusedAt.Add(-time.Second))
		}},
		{"no history read", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History = nil }},
		{"another version", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.CurrentVersion = 2 }},
		{"work pending", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.PendingCount = 1 }},
		{"not ready", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Status = metav1.ConditionFalse }},
		{"blocked", func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.Conditions = append(s.Conditions, mrCondition("Blocked", metav1.ConditionTrue, "RealmConflict", ""))
		}},
	})
	later := fixture()
	later.History.ObservedAt = metav1.NewTime(refusedAt.Add(time.Minute))
	if !accepts(later) {
		t.Error("a reading after the refusal was refused")
	}
}

func TestRivalStillRefused(t *testing.T) {
	t.Parallel()
	mrRefusesEach(t, mrRivalFixture(t), rivalStillRefused, []mrMutation[ptahv1alpha1.PtahSchemaStatus]{
		{"an operation claimed", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationResolve}
		}},
		{"ready for another reason", func(s *ptahv1alpha1.PtahSchemaStatus) { s.Conditions[0].Reason = "InSync" }},
		{"the reason on another condition", func(s *ptahv1alpha1.PtahSchemaStatus) { s.Conditions = s.Conditions[1:] }},
	})
	whatever := mrRivalFixture(t)()
	whatever.Conditions[0].Status = metav1.ConditionUnknown
	if !rivalStillRefused(whatever) {
		t.Error("the refusal was read by its status, which the filter never asked for")
	}
}

func mrConflictFixture() ptahv1alpha1.PtahMigrationStatus {
	return ptahv1alpha1.PtahMigrationStatus{
		Phase: ptahv1alpha1.MigrationPhaseBlocked,
		Conditions: []metav1.Condition{
			mrCondition("Blocked", metav1.ConditionTrue, "RealmConflict", "2 claimants name realm e2e-realm-postgresql"),
			mrCondition("Ready", metav1.ConditionFalse, "RealmConflict", "2 claimants"),
		},
	}
}

func TestMigrationConflictHeld(t *testing.T) {
	t.Parallel()
	accepts := func(status ptahv1alpha1.PtahMigrationStatus) bool {
		return migrationConflictHeld(status) && conditionMessagesAvoid(status.Conditions, "e2e-realm-rival-postgresql", "e2e-migrations-postgresql-rival")
	}
	mrRefusesEach(t, mrConflictFixture, accepts, []mrMutation[ptahv1alpha1.PtahMigrationStatus]{
		{"a phase other than Blocked", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Phase = ptahv1alpha1.MigrationPhaseReading }},
		{"an operation running", func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{Type: ptahv1alpha1.MigrationOperationHistory}
		}},
		{"blocked for another reason", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Reason = "HistoryDirty" }},
		{"the refusal lapsed", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Status = metav1.ConditionFalse }},
		{"called ready", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[1].Status = metav1.ConditionTrue }},
		{"the rival's namespace named", func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.Conditions[1].Message = "e2e-realm-rival-postgresql claims it too"
		}},
		{"the rival named", func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.Conditions[0].Message = "e2e-migrations-postgresql-rival claims it too"
		}},
	})
	if migrationRealmConflict(ptahv1alpha1.PtahMigrationStatus{}) || migrationConflictHeld(ptahv1alpha1.PtahMigrationStatus{Phase: ptahv1alpha1.MigrationPhaseBlocked}) {
		t.Error("a status with no conditions read as a conflict")
	}
}

func TestRivalConflictHeld(t *testing.T) {
	t.Parallel()
	fixture := func() ptahv1alpha1.PtahSchemaStatus {
		return ptahv1alpha1.PtahSchemaStatus{Conditions: []metav1.Condition{
			mrCondition("Ready", metav1.ConditionFalse, "RealmConflict", "2 claimants name the realm"),
		}}
	}
	accepts := func(status ptahv1alpha1.PtahSchemaStatus) bool {
		return rivalRealmConflict(status) && rivalConflictHeld(status, "e2e-test", "e2e-migrations-postgresql")
	}
	mrRefusesEach(t, fixture, accepts, []mrMutation[ptahv1alpha1.PtahSchemaStatus]{
		{"ready for another reason", func(s *ptahv1alpha1.PtahSchemaStatus) { s.Conditions[0].Reason = "RealmNotAuthorized" }},
		{"an operation claimed", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationResolve}
		}},
		{"the migration's namespace named", func(s *ptahv1alpha1.PtahSchemaStatus) { s.Conditions[0].Message = "e2e-test holds it" }},
		{"the migration named", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Conditions[0].Message = "e2e-migrations-postgresql holds it"
		}},
		{"no conditions", func(s *ptahv1alpha1.PtahSchemaStatus) { s.Conditions = nil }},
	})
	onBlocked := ptahv1alpha1.PtahSchemaStatus{Conditions: []metav1.Condition{
		mrCondition("Blocked", metav1.ConditionTrue, "RealmConflict", ""),
	}}
	if rivalRealmConflict(onBlocked) {
		t.Error("the wait's reading took the reason on a condition other than Ready")
	}
	if !rivalConflictHeld(onBlocked, "e2e-test", "e2e-migrations-postgresql") {
		t.Error("the held reading refused the reason on another condition, which it never asked about")
	}
}

func TestPartialRowReadings(t *testing.T) {
	t.Parallel()
	planned := func() ptahv1alpha1.PtahMigrationStatus {
		return ptahv1alpha1.PtahMigrationStatus{History: &ptahv1alpha1.MigrationHistoryStatus{CurrentVersion: 3, PendingCount: 1}}
	}
	mrRefusesEach(t, planned, partialPlanned, []mrMutation[ptahv1alpha1.PtahMigrationStatus]{
		{"two pending", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.PendingCount = 2 }},
		{"another version", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.CurrentVersion = 4 }},
		{"no history", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History = nil }},
	})
	recovered := func() ptahv1alpha1.PtahMigrationStatus {
		return ptahv1alpha1.PtahMigrationStatus{
			Phase:   ptahv1alpha1.MigrationPhaseInSync,
			History: &ptahv1alpha1.MigrationHistoryStatus{CurrentVersion: 3, AppliedCount: 3},
			LastRun: &ptahv1alpha1.MigrationRunStatus{Outcome: ptahv1alpha1.MigrationRunOutcomePartial},
			Conditions: []metav1.Condition{
				mrCondition("Ready", metav1.ConditionTrue, "HistoryMatched", ""),
				mrCondition("Blocked", metav1.ConditionFalse, "HistoryMatched", ""),
			},
		}
	}
	mrRefusesEach(t, recovered, recoveredAfterPartial, []mrMutation[ptahv1alpha1.PtahMigrationStatus]{
		{"not InSync", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Phase = ptahv1alpha1.MigrationPhaseBlocked }},
		{"another version", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.CurrentVersion = 4 }},
		{"two applied", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.AppliedCount = 2 }},
		{"one pending", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.PendingCount = 1 }},
		{"still dirty", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.Dirty = true }},
		{"no history", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History = nil }},
		{"a plan published", func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.Plan = &ptahv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-0"}
		}},
		{"another last run", func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeApplied }},
		{"no last run", func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun = nil }},
		{"ready for another reason", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Reason = "InSync" }},
		{"still blocked", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[1].Status = metav1.ConditionTrue }},
	})
}

func TestBlockedMessagesNameWhatTheyRefuse(t *testing.T) {
	t.Parallel()
	blocked := func(message string) []metav1.Condition {
		return []metav1.Condition{
			mrCondition("Ready", metav1.ConditionFalse, "HistoryDirty", "version 3 is dirty"),
			mrCondition("Blocked", metav1.ConditionTrue, "HistoryDirty", message),
		}
	}
	if !blockedMessageMatches(blocked("revision 4 did not finish"), namesARevision) {
		t.Error("a refusal naming the revision was refused")
	}
	if blockedMessageMatches(blocked("a revision did not finish"), namesARevision) {
		t.Error("a refusal naming no revision was accepted on another condition's number")
	}
	if !blockedMessageMatches(blocked("the database is at 3 and the artifact ends at 2"), namesBothVersions) {
		t.Error("a refusal naming both versions was refused")
	}
	for _, message := range []string{"the database is at 3", "the artifact ends at 2"} {
		if blockedMessageMatches(blocked(message), namesBothVersions) {
			t.Errorf("%q, naming one version, was accepted", message)
		}
	}
}

func TestOlderArtifactReadings(t *testing.T) {
	t.Parallel()
	blocked := func() ptahv1alpha1.PtahMigrationStatus {
		return ptahv1alpha1.PtahMigrationStatus{
			Phase:      ptahv1alpha1.MigrationPhaseBlocked,
			Conditions: []metav1.Condition{mrCondition("Blocked", metav1.ConditionTrue, "HistoryAhead", "")},
		}
	}
	mrRefusesEach(t, blocked, historyAheadBlocked, []mrMutation[ptahv1alpha1.PtahMigrationStatus]{
		{"not Blocked", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Phase = ptahv1alpha1.MigrationPhaseResolving }},
		{"blocked for another reason", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Reason = "HistoryModified" }},
	})
	settled := func() ptahv1alpha1.PtahMigrationStatus {
		return ptahv1alpha1.PtahMigrationStatus{
			History:    &ptahv1alpha1.MigrationHistoryStatus{CurrentVersion: 3, AppliedCount: 3},
			Conditions: []metav1.Condition{mrCondition("Ready", metav1.ConditionTrue, "HistoryMatched", "")},
		}
	}
	mrRefusesEach(t, settled, settledOnTheThree, []mrMutation[ptahv1alpha1.PtahMigrationStatus]{
		{"another version", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.CurrentVersion = 2 }},
		{"two applied", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.AppliedCount = 2 }},
		{"one pending", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.PendingCount = 1 }},
		{"no history", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History = nil }},
		{"not ready", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Status = metav1.ConditionFalse }},
		{"ready for another reason", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Reason = "InSync" }},
	})
}

func TestModifiedRefusal(t *testing.T) {
	t.Parallel()
	fixture := func() ptahv1alpha1.PtahMigrationStatus {
		return ptahv1alpha1.PtahMigrationStatus{
			Artifact: &ptahv1alpha1.OCIArtifactAccessBinding{Digest: "sha256:ee"},
			History:  &ptahv1alpha1.MigrationHistoryStatus{CurrentVersion: 3, ModifiedVersions: []int64{1}},
			Conditions: []metav1.Condition{
				mrCondition("Blocked", metav1.ConditionTrue, "HistoryModified", "migration 1 changed"),
				mrCondition("Ready", metav1.ConditionFalse, "HistoryModified", ""),
			},
		}
	}
	accepts := func(status ptahv1alpha1.PtahMigrationStatus) bool { return modifiedRefusal(status, "sha256:ee") }
	mrRefusesEach(t, fixture, accepts, []mrMutation[ptahv1alpha1.PtahMigrationStatus]{
		{"the unedited artifact", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Artifact.Digest = "sha256:ff" }},
		{"no artifact", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Artifact = nil }},
		{"no modified version", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.ModifiedVersions = nil }},
		{"another modified version", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.ModifiedVersions = []int64{2} }},
		{"two modified versions", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.ModifiedVersions = []int64{1, 2} }},
		{"another version", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.CurrentVersion = 2 }},
		{"no history", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History = nil }},
		{"a plan published", func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.Plan = &ptahv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-0"}
		}},
		{"an operation running", func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{Type: ptahv1alpha1.MigrationOperationApply}
		}},
		{"blocked for another reason", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Reason = "HistoryAhead" }},
		{"called ready", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[1].Status = metav1.ConditionTrue }},
	})
}

func TestOutOfOrderRefusal(t *testing.T) {
	t.Parallel()
	fixture := func() ptahv1alpha1.PtahMigrationStatus {
		return ptahv1alpha1.PtahMigrationStatus{
			Phase:   ptahv1alpha1.MigrationPhaseBlocked,
			History: &ptahv1alpha1.MigrationHistoryStatus{CurrentVersion: 30, OutOfOrderVersions: []int64{20}},
			Conditions: []metav1.Condition{
				mrCondition("Blocked", metav1.ConditionTrue, "HistoryOutOfOrder", ""),
				mrCondition("Ready", metav1.ConditionFalse, "HistoryOutOfOrder", ""),
			},
		}
	}
	accepts := func(status ptahv1alpha1.PtahMigrationStatus) bool { return outOfOrderRefusal(status, false) }
	mrRefusesEach(t, fixture, accepts, []mrMutation[ptahv1alpha1.PtahMigrationStatus]{
		{"not Blocked", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Phase = ptahv1alpha1.MigrationPhaseReading }},
		{"an operation running", func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{Type: ptahv1alpha1.MigrationOperationHistory}
		}},
		{"no late version named", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.OutOfOrderVersions = nil }},
		{"another late version", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.OutOfOrderVersions = []int64{20, 21} }},
		{"the database elsewhere", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.CurrentVersion = 10 }},
		{"no history", func(s *ptahv1alpha1.PtahMigrationStatus) { s.History = nil }},
		{"blocked for another reason", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Reason = "HistoryAhead" }},
		{"not ready for another reason", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[1].Reason = "MigrationsPending" }},
		{"called ready", func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[1].Status = metav1.ConditionTrue }},
	})
	if outOfOrderRefusal(fixture(), true) {
		t.Error("a status that stores a plan key was accepted")
	}
	if !outOfOrderBlocked(fixture()) {
		t.Error("the wait's reading refused the refusal")
	}
}

func TestRivalAuthorGrantAdaptsTheExample(t *testing.T) {
	t.Parallel()
	example, err := os.ReadFile("../../examples/desired-state-author-role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	objects, err := rivalAuthorGrant(example, "e2e-realm-rival-postgresql", "e2e:realm-rival-authors-postgresql")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, object := range objects {
		kinds = append(kinds, object["kind"].(string))
		metadata := object["metadata"].(map[string]any)
		if metadata["namespace"] != "e2e-realm-rival-postgresql" {
			t.Errorf("%s is in %v", object["kind"], metadata["namespace"])
		}
		if object["kind"] == "RoleBinding" {
			subjects := object["subjects"].([]any)
			subject := subjects[0].(map[string]any)
			if len(subjects) != 1 || subject["kind"] != "Group" || subject["name"] != "e2e:realm-rival-authors-postgresql" ||
				subject["apiGroup"] != "rbac.authorization.k8s.io" {
				t.Errorf("the binding's subjects are %v", subjects)
			}
		}
		if object["kind"] == "Role" && !strings.Contains(mrJSON(t, object), "ptahschemas") {
			t.Error("the Role lost the rules the example grants")
		}
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"Role", "RoleBinding"}) {
		t.Errorf("kinds = %v", kinds)
	}
	if strings.Contains(string(example), "e2e-realm-rival") {
		t.Error("the example itself was edited")
	}

	list := `{"apiVersion":"v1","kind":"List","items":[
 {"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"r"}},
 {"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding","metadata":{"name":"b"}}]}`
	if objects, err := rivalAuthorGrant([]byte(list), "n", "g"); err != nil || len(objects) != 2 {
		t.Errorf("a List of the two was refused: %v", err)
	}
	for name, document := range map[string]string{
		"a Role alone":          "apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata: {name: r}\n",
		"a ClusterRole instead": "kind: ClusterRole\nmetadata: {name: r}\n---\nkind: RoleBinding\nmetadata: {name: b}\n",
		"a third object":        "kind: Role\n---\nkind: RoleBinding\n---\nkind: ConfigMap\n",
		"not a document at all": "kind: [Role\n",
	} {
		if _, err := rivalAuthorGrant([]byte(document), "n", "g"); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func mrJSON(t *testing.T, value any) string {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
