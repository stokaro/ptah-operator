package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// nrReadingFile names a document a native refusal row printed: the resource
// Ptah's own verification refused and, beside it, what the row matched it with.
// testdata/e2e/readings/README.md says in which run.
func nrReadingFile(name string) string { return "../../testdata/e2e/readings/" + name }

// nrMatched is what a native refusal row printed beside the resource: the
// Verify's result frame, where the row reads one, and the refusal's Warning
// Events.
type nrMatched struct {
	VerifyResult *runner.Result `json:"verifyResult"`
	Events       []corev1.Event `json:"events"`
}

// nrRead decodes one reading afresh, so a mutation changes one copy only. The
// file is read once, before any subtest, so a fixture called from a subtest
// cannot fail on it.
func nrRead[T any](t *testing.T, name string) func() (T, nrMatched) {
	t.Helper()
	content, err := os.ReadFile(nrReadingFile(name))
	if err != nil {
		t.Fatal(err)
	}
	decode := func() (T, nrMatched, error) {
		var resource T
		var matched nrMatched
		if err := json.Unmarshal(content, &resource); err != nil {
			return resource, matched, err
		}
		return resource, matched, json.Unmarshal(content, &matched)
	}
	if _, _, err := decode(); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return func() (T, nrMatched) {
		resource, matched, _ := decode()
		return resource, matched
	}
}

// nrCondition is the condition a mutation changes; the fixture was accepted
// with it, so it is there.
func nrCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	return meta.FindStatusCondition(conditions, conditionType)
}

func TestNativelyRefusedSchema(t *testing.T) {
	t.Parallel()
	read := nrRead[ptahv1alpha1.PtahSchema](t, "native-refusal-schema.json")
	verified := func(s *ptahv1alpha1.PtahSchema) *metav1.Condition {
		return nrCondition(s.Status.Conditions, ptahv1alpha1.ConditionArtifactVerified)
	}
	ready := func(s *ptahv1alpha1.PtahSchema) *metav1.Condition {
		return nrCondition(s.Status.Conditions, ptahv1alpha1.ConditionReady)
	}
	mtRefusesEach(t,
		func() *ptahv1alpha1.PtahSchema {
			schema, _ := read()
			return &schema
		},
		nativelyRefusedSchema,
		map[string]func(*ptahv1alpha1.PtahSchema){
			"the operation failure this row was written for": func(s *ptahv1alpha1.PtahSchema) {
				s.Status.Phase = ptahv1alpha1.PhaseFailed
				verified(s).Reason = "OperationFailed"
			},
			"an operation in flight": func(s *ptahv1alpha1.PtahSchema) {
				s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationVerify}
			},
			"a plan published":   func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{} },
			"a verified source":  func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.Verified = true },
			"no conditions":      func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions = nil },
			"verified as True":   func(s *ptahv1alpha1.PtahSchema) { verified(s).Status = metav1.ConditionTrue },
			"another reason":     func(s *ptahv1alpha1.PtahSchema) { verified(s).Reason = "OperationFailed" },
			"no requirement":     func(s *ptahv1alpha1.PtahSchema) { verified(s).Message = "Verification policy refused the artifact" },
			"Ready as a failure": func(s *ptahv1alpha1.PtahSchema) { ready(s).Reason = "OperationFailed" },
			"Ready as True":      func(s *ptahv1alpha1.PtahSchema) { ready(s).Status = metav1.ConditionTrue },
		})
}

func TestNativelyRefusedMigration(t *testing.T) {
	t.Parallel()
	read := nrRead[ptahv1alpha1.PtahMigration](t, "native-refusal-migration.json")
	verified := func(m *ptahv1alpha1.PtahMigration) *metav1.Condition {
		return nrCondition(m.Status.Conditions, ptahv1alpha1.ConditionMigrationArtifactVerified)
	}
	mtRefusesEach(t,
		func() *ptahv1alpha1.PtahMigration {
			migration, _ := read()
			return &migration
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
			"another reason": func(m *ptahv1alpha1.PtahMigration) { verified(m).Reason = "OperationFailed" },
			"no requirement": func(m *ptahv1alpha1.PtahMigration) { verified(m).Message = "refused" },
			"Ready as a failure": func(m *ptahv1alpha1.PtahMigration) {
				nrCondition(m.Status.Conditions, ptahv1alpha1.ConditionMigrationReady).Reason = "OperationFailed"
			},
		})
}

func TestNativeRefusalResult(t *testing.T) {
	t.Parallel()
	read := nrRead[ptahv1alpha1.PtahSchema](t, "native-refusal-schema.json")
	policy, err := os.ReadFile("../../testdata/e2e/" + otherTypePolicyFile)
	if err != nil {
		t.Fatal(err)
	}
	refused, matched := read()
	if matched.VerifyResult == nil {
		t.Fatal("the reading holds no Verify result")
	}
	// The row passes the digest it published and the policy's own digest; the
	// schema's source names both.
	digest, policyDigest := refused.Status.Source.Digest, sha256Digest(policy)
	if refused.Status.Source.VerificationPolicyDigest != policyDigest {
		t.Fatalf("the schema read policy %s, not %s, the digest of %s", refused.Status.Source.VerificationPolicyDigest, policyDigest, otherTypePolicyFile)
	}
	mtRefusesEach(t,
		func() *runner.Result {
			_, matched := read()
			return matched.VerifyResult
		},
		func(result *runner.Result) error {
			return nativeRefusalResult(*result, digest, policyDigest)
		},
		map[string]func(*runner.Result){
			"the digest-pin refusal's exit status": func(r *runner.Result) { r.ChildExitCode = 0 },
			"another digest":                       func(r *runner.Result) { r.ResolvedDigest = testGateDigest },
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
	for _, want := range []string{"ArtifactVerified=True/PolicySatisfied since 2026-11-12T18:35:01Z", "Blocked=False/HistoryMatched since 2026-11-12T18:35:01Z"} {
		if !strings.Contains(said, want) {
			t.Errorf("refusalTransitions says %q, want %q", said, want)
		}
	}
	if refusalTransitions(nil) == "" {
		t.Error("refusalTransitions says nothing of no document")
	}
}

// recoveredFromRefusalReading is what the refusal row printed for a resource
// past its refusal. TestTheRefusalRecoveryReadingIsTheReconcilers in
// internal/controller holds the reconciler to its phase and conditions.
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
	for _, name := range []string{"native-refusal-schema.json", "native-refusal-migration.json"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			read := nrRead[metav1.PartialObjectMetadata](t, name)
			refused, matched := read()
			if len(matched.Events) == 0 {
				t.Fatal("the reading holds no Event")
			}
			if err := nativeRefusalEvent(matched.Events, refused.UID); err != nil {
				t.Fatalf("the Event was refused: %v", err)
			}
			each := func(mutate func(*corev1.Event)) []corev1.Event {
				_, matched := read()
				for index := range matched.Events {
					mutate(&matched.Events[index])
				}
				return matched.Events
			}
			for name, events := range map[string][]corev1.Event{
				"no Event":          nil,
				"another resource":  each(func(e *corev1.Event) { e.InvolvedObject.UID = "other" }),
				"another reason":    each(func(e *corev1.Event) { e.Reason = "OperationRetried" }),
				"a Normal Event":    each(func(e *corev1.Event) { e.Type = corev1.EventTypeNormal }),
				"no requirement":    each(func(e *corev1.Event) { e.Message = "Verification policy refused the artifact" }),
				"a mismatched word": each(func(e *corev1.Event) { e.Message = "require_signature" }),
			} {
				if nativeRefusalEvent(events, refused.UID) == nil {
					t.Errorf("%s was accepted", name)
				}
			}
		})
	}
}
