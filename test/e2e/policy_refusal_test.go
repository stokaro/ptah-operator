package e2e

import (
	"testing"
	"time"

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
				Error:                    &runner.ResultError{Code: "verification_refused"},
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
			"what the executor printed": func(r *runner.Result) { r.Stdout = "{}" },
			"the artifact read":         func(r *runner.Result) { r.ObservedArtifactType = schemaArtifactType },
			"a mutation":                func(r *runner.Result) { r.MutationStarted = true },
			"an uncertain outcome":      func(r *runner.Result) { r.Uncertain = true },
			"lost output":               func(r *runner.Result) { r.Truncation = &runner.TruncationMetadata{Stdout: true} },
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
