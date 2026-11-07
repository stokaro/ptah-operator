package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

func refusedConditions(verifiedType, readyType string) []metav1.Condition {
	return []metav1.Condition{
		{Type: verifiedType, Status: metav1.ConditionFalse, Reason: "PolicyRefused",
			Message: "Verification policy refused the artifact with 1 finding(s): artifact_types"},
		{Type: readyType, Status: metav1.ConditionFalse, Reason: "PolicyRefused",
			Message: "Artifact verification policy must be satisfied before database access"},
	}
}

func TestNativelyRefusedSchema(t *testing.T) {
	t.Parallel()
	mtRefusesEach(t,
		func() *ptahv1alpha1.PtahSchema {
			schema := &ptahv1alpha1.PtahSchema{}
			schema.Status.Phase = ptahv1alpha1.PhaseBlocked
			schema.Status.Conditions = refusedConditions(ptahv1alpha1.ConditionArtifactVerified, ptahv1alpha1.ConditionReady)
			return schema
		},
		nativelyRefusedSchema,
		map[string]func(*ptahv1alpha1.PtahSchema){
			"the operation failure this row was written for": func(s *ptahv1alpha1.PtahSchema) {
				s.Status.Phase = ptahv1alpha1.PhaseFailed
				s.Status.Conditions[0].Reason = "OperationFailed"
			},
			"an operation in flight": func(s *ptahv1alpha1.PtahSchema) {
				s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationVerify}
			},
			"a plan published":  func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{} },
			"a verified source": func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.Verified = true },
			"no conditions":     func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions = nil },
			"verified as True":  func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[0].Status = metav1.ConditionTrue },
			"another reason":    func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[0].Reason = "OperationFailed" },
			"no requirement": func(s *ptahv1alpha1.PtahSchema) {
				s.Status.Conditions[0].Message = "Verification policy refused the artifact"
			},
			"Ready as a failure": func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[1].Reason = "OperationFailed" },
			"Ready as True":      func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[1].Status = metav1.ConditionTrue },
		})
}

func TestNativelyRefusedMigration(t *testing.T) {
	t.Parallel()
	mtRefusesEach(t,
		func() *ptahv1alpha1.PtahMigration {
			migration := &ptahv1alpha1.PtahMigration{}
			migration.Status.Phase = ptahv1alpha1.MigrationPhaseBlocked
			migration.Status.Conditions = refusedConditions(ptahv1alpha1.ConditionMigrationArtifactVerified, ptahv1alpha1.ConditionMigrationReady)
			return migration
		},
		nativelyRefusedMigration,
		map[string]func(*ptahv1alpha1.PtahMigration){
			"still retrying the read": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.Phase = ptahv1alpha1.MigrationPhaseVerifying
				m.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{Type: ptahv1alpha1.MigrationOperationVerify}
			},
			"blocked with a claim": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{Type: ptahv1alpha1.MigrationOperationVerify}
			},
			"a plan published": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.Plan = &ptahv1alpha1.ImmutableObjectReference{Name: "plan"}
			},
			"a history read": func(m *ptahv1alpha1.PtahMigration) { m.Status.History = &ptahv1alpha1.MigrationHistoryStatus{} },
			"no conditions":  func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions = nil },
			"another reason": func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[0].Reason = "OperationFailed" },
			"no requirement": func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[0].Message = "refused" },
			"Ready as a failure": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.Conditions[1].Reason = "OperationFailed"
			},
		})
}

func TestNativeRefusalResult(t *testing.T) {
	t.Parallel()
	mtRefusesEach(t,
		func() *runner.Result {
			return &runner.Result{
				ChildExitCode: 2, ResolvedDigest: testGateDigest, VerificationPolicyDigest: testGateFingerprint,
				VerificationRequirements: []string{"artifact_types"},
				Error:                    &runner.ResultError{Code: "verification_refused", Message: refusalFrameMessage},
			}
		},
		func(result *runner.Result) error {
			return nativeRefusalResult(*result, testGateDigest, testGateFingerprint)
		},
		map[string]func(*runner.Result){
			"the digest-pin refusal's exit status": func(r *runner.Result) { r.ChildExitCode = 0 },
			"another digest":                       func(r *runner.Result) { r.ResolvedDigest = testGateFingerprint },
			"another policy":                       func(r *runner.Result) { r.VerificationPolicyDigest = testGateDigest },
			"another requirement":                  func(r *runner.Result) { r.VerificationRequirements = []string{"require_signature"} },
			"no requirement":                       func(r *runner.Result) { r.VerificationRequirements = nil },
			"not refused":                          func(r *runner.Result) { r.Error = nil },
			"the code a diagnostic used to give": func(r *runner.Result) {
				r.Error.Code = "invalid_verification_output"
			},
			"Ptah's words about the artifact": func(r *runner.Result) {
				r.Error.Message = `the artifact is "application/x", and the policy permits application/y`
			},
			"no message":                func(r *runner.Result) { r.Error.Message = "" },
			"what the executor printed": func(r *runner.Result) { r.Stdout = "{}" },
			"the artifact read":         func(r *runner.Result) { r.ObservedArtifactType = schemaArtifactType },
			"a mutation":                func(r *runner.Result) { r.MutationStarted = true },
			"an uncertain outcome":      func(r *runner.Result) { r.Uncertain = true },
			"lost output":               func(r *runner.Result) { r.Truncation = &runner.TruncationMetadata{Stdout: true} },
		})
}

func TestRefusedOnce(t *testing.T) {
	t.Parallel()
	const interval = 90 * time.Second
	created := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	job := func(name, operation string, at time.Time) batchv1.Job {
		return batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: map[string]string{labelOperation: operation}, CreationTimestamp: metav1.NewTime(at),
		}}
	}
	once := []batchv1.Job{job("resolve", "resolve", created), job("verify", "verify", created.Add(10*time.Second))}
	deadline := func(after time.Duration) *metav1.Time {
		at := metav1.NewTime(created.Add(after))
		return &at
	}
	// The claim sets the deadline a moment before the Job exists, and a refresh
	// slot can move it up to half an interval later.
	for _, next := range []time.Duration{interval - time.Second, interval + interval/2} {
		if err := refusedOnce(once, deadline(next), interval); err != nil {
			t.Fatalf("a refusal that reads again %s after its Resolve was refused: %v", next, err)
		}
	}
	for name, test := range map[string]struct {
		jobs []batchv1.Job
		next *metav1.Time
	}{
		"no deadline":      {once, nil},
		"nothing read":     {nil, deadline(interval)},
		"a second read":    {append(append([]batchv1.Job{}, once...), job("verify-2", "verify", created.Add(20*time.Second))), deadline(interval)},
		"no Verify":        {once[:1], deadline(interval)},
		"no Resolve":       {once[1:], deadline(interval)},
		"a history read":   {append(append([]batchv1.Job{}, once...), job("history", "history", created.Add(20*time.Second))), deadline(interval)},
		"an Apply":         {append(append([]batchv1.Job{}, once...), job("apply", "apply", created.Add(20*time.Second))), deadline(interval)},
		"no operation":     {append(append([]batchv1.Job{}, once...), batchv1.Job{}), deadline(interval)},
		"a failure retry":  {once, deadline(10 * time.Second)},
		"half an interval": {once, deadline(interval / 2)},
		"put off for good": {once, deadline(24 * time.Hour)},
		"two intervals":    {once, deadline(2 * interval)},
	} {
		if refusedOnce(test.jobs, test.next, interval) == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestRereadAtItsDeadlineAndReadAtOnce(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	job := func(name, operation string, after time.Duration) batchv1.Job {
		return batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: map[string]string{labelOperation: operation}, CreationTimestamp: metav1.NewTime(start.Add(after)),
		}}
	}
	at := func(after time.Duration) *metav1.Time {
		instant := metav1.NewTime(start.Add(after))
		return &instant
	}
	// The refusal at 0 persisted 90s; the interval's read at 91s persisted 181s.
	twice := []batchv1.Job{job("r1", "resolve", 0), job("v1", "verify", 8*time.Second), job("r2", "resolve", 91*time.Second), job("v2", "verify", 99*time.Second)}
	if err := rereadAtItsDeadline(twice, at(90*time.Second)); err != nil {
		t.Fatalf("the interval's read was refused: %v", err)
	}
	for name, jobs := range map[string][]batchv1.Job{
		"no second read":             twice[:2],
		"a read before the deadline": {twice[0], twice[1], job("r2", "resolve", 60*time.Second)},
		"a third read":               append(append([]batchv1.Job{}, twice...), job("r3", "resolve", 120*time.Second)),
	} {
		if rereadAtItsDeadline(jobs, at(90*time.Second)) == nil {
			t.Errorf("rereadAtItsDeadline accepted %s", name)
		}
	}
	if rereadAtItsDeadline(twice, nil) == nil {
		t.Error("rereadAtItsDeadline accepted no deadline")
	}

	// The edit at 110s; the next deadline is 181s, so a read at 170s is well ahead
	// of it and still no read the edit started.
	editedAt := start.Add(110 * time.Second)
	for _, after := range []time.Duration{110 * time.Second, 112 * time.Second, 130 * time.Second} {
		edited := append(append([]batchv1.Job{}, twice...), job("r3", "resolve", after))
		if err := readAtOnce(edited, editedAt); err != nil {
			t.Fatalf("a read %s after the start was refused: %v", after, err)
		}
	}
	edited := append(append([]batchv1.Job{}, twice...), job("r3", "resolve", 112*time.Second))
	for name, jobs := range map[string][]batchv1.Job{
		"no read after the edit":         twice,
		"the interval's own read":        append(append([]batchv1.Job{}, twice...), job("r3", "resolve", 181*time.Second)),
		"a read a minute after the edit": append(append([]batchv1.Job{}, twice...), job("r3", "resolve", 170*time.Second)),
		"a read before the edit":         append(append([]batchv1.Job{}, twice...), job("r3", "resolve", 100*time.Second)),
		"a fourth read":                  append(append([]batchv1.Job{}, edited...), job("r4", "resolve", 113*time.Second)),
	} {
		if readAtOnce(jobs, editedAt) == nil {
			t.Errorf("readAtOnce accepted %s", name)
		}
	}
	if readAtOnce(edited, time.Time{}) == nil {
		t.Error("readAtOnce accepted no edit")
	}
}

func TestRefusalTransitionsNamesBothTimes(t *testing.T) {
	t.Parallel()
	said := refusalTransitions(recoveredFromRefusalReading(t))
	for _, want := range []string{"ArtifactVerified=True/PolicySatisfied since 2026-11-07T10:31:12Z", "Blocked=False/HistoryMatched since 2026-11-07T10:31:12Z"} {
		if !strings.Contains(said, want) {
			t.Errorf("refusalTransitions says %q, want %q", said, want)
		}
	}
	if refusalTransitions(nil) == "" {
		t.Error("refusalTransitions says nothing of no document")
	}
}

// recoveredFromRefusalReading is what the reconciler wrote for a resource past
// its refusal. TestTheRefusalRecoveryReadingIsTheReconcilers in internal/controller
// holds it to that, because this row has not seen a cluster when it is written.
func recoveredFromRefusalReading(t *testing.T) *ptahv1alpha1.PtahMigration {
	t.Helper()
	content, err := os.ReadFile("../../testdata/e2e/readings/native-refusal-recovered-migration.json")
	if err != nil {
		t.Fatal(err)
	}
	migration := &ptahv1alpha1.PtahMigration{}
	if err := json.Unmarshal(content, migration); err != nil {
		t.Fatal(err)
	}
	return migration
}

func TestRecoveredFromRefusal(t *testing.T) {
	t.Parallel()
	condition := func(m *ptahv1alpha1.PtahMigration, conditionType string) *metav1.Condition {
		for index := range m.Status.Conditions {
			if m.Status.Conditions[index].Type == conditionType {
				return &m.Status.Conditions[index]
			}
		}
		t.Fatalf("the reading has no %s", conditionType)
		return nil
	}
	mtRefusesEach(t,
		func() *ptahv1alpha1.PtahMigration { return recoveredFromRefusalReading(t) },
		recoveredFromRefusal,
		map[string]func(*ptahv1alpha1.PtahMigration){
			"the generation before the edit": func(m *ptahv1alpha1.PtahMigration) { m.Generation++ },
			"still refused":                  func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseBlocked },
			"a plan still waiting": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.Phase = ptahv1alpha1.MigrationPhaseAwaitingApproval
				m.Status.Plan = &ptahv1alpha1.ImmutableObjectReference{Name: "plan", UID: "plan-uid"}
			},
			"a read in flight": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{Type: ptahv1alpha1.MigrationOperationResolve}
			},
			"a run nobody accounted for": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.UnresolvedRun = &ptahv1alpha1.UnresolvedMigrationRunStatus{OperationID: "op"}
			},
			"no conditions": func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions = nil },
			"an artifact not verified": func(m *ptahv1alpha1.PtahMigration) {
				condition(m, ptahv1alpha1.ConditionMigrationArtifactVerified).Status = metav1.ConditionUnknown
			},
			"Blocked still True": func(m *ptahv1alpha1.PtahMigration) {
				condition(m, ptahv1alpha1.ConditionMigrationBlocked).Status = metav1.ConditionTrue
			},
			"not Ready": func(m *ptahv1alpha1.PtahMigration) {
				condition(m, ptahv1alpha1.ConditionMigrationReady).Status = metav1.ConditionFalse
			},
			"a refusal left on ApprovalRequired": func(m *ptahv1alpha1.PtahMigration) {
				condition(m, ptahv1alpha1.ConditionMigrationApprovalRequired).Reason = string(ptahv1alpha1.ReasonPolicyRefused)
			},
			// What a Verify that ended nothing leaves: the reading rewrote Blocked,
			// a History Job's seconds after the artifact passed.
			"Blocked gone False with the reading": func(m *ptahv1alpha1.PtahMigration) {
				blocked := condition(m, ptahv1alpha1.ConditionMigrationBlocked)
				blocked.LastTransitionTime = metav1.NewTime(blocked.LastTransitionTime.Add(10 * time.Second))
			},
			"Blocked gone False before the artifact passed": func(m *ptahv1alpha1.PtahMigration) {
				blocked := condition(m, ptahv1alpha1.ConditionMigrationBlocked)
				blocked.LastTransitionTime = metav1.NewTime(blocked.LastTransitionTime.Add(-10 * time.Second))
			},
		})
}

func TestNativeRefusalEvent(t *testing.T) {
	t.Parallel()
	const owner = "owner-uid"
	event := func(mutate func(*corev1.Event)) []corev1.Event {
		each := corev1.Event{
			InvolvedObject: corev1.ObjectReference{UID: owner}, Type: corev1.EventTypeWarning, Reason: "ArtifactVerificationRefused",
			Message: "Verification policy refused the artifact with 1 finding(s): artifact_types", LastTimestamp: metav1.NewTime(time.Unix(0, 0)),
		}
		if mutate != nil {
			mutate(&each)
		}
		return []corev1.Event{each}
	}
	if err := nativeRefusalEvent(event(nil), owner); err != nil {
		t.Fatalf("the Event was refused: %v", err)
	}
	for name, events := range map[string][]corev1.Event{
		"no Event":          nil,
		"another resource":  event(func(e *corev1.Event) { e.InvolvedObject.UID = "other" }),
		"another reason":    event(func(e *corev1.Event) { e.Reason = "OperationRetried" }),
		"a Normal Event":    event(func(e *corev1.Event) { e.Type = corev1.EventTypeNormal }),
		"no requirement":    event(func(e *corev1.Event) { e.Message = "Verification policy refused the artifact" }),
		"a mismatched word": event(func(e *corev1.Event) { e.Message = "require_signature" }),
	} {
		if nativeRefusalEvent(events, owner) == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
