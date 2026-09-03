package v1alpha1_test

import (
	"regexp"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestConditionReasonWireContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		reason operatorv1alpha1.ConditionReason
		wire   string
	}{
		{"active", operatorv1alpha1.ReasonActive, "Active"},
		{"apply disabled", operatorv1alpha1.ReasonApplyDisabled, "ApplyDisabled"},
		{"apply outcome unknown", operatorv1alpha1.ReasonApplyOutcomeUnknown, "ApplyOutcomeUnknown"},
		{"apply pending", operatorv1alpha1.ReasonApplyPending, "ApplyPending"},
		{"approval revoked", operatorv1alpha1.ReasonApprovalRevoked, "ApprovalRevoked"},
		{"approved plan", operatorv1alpha1.ReasonApprovedPlan, "ApprovedPlan"},
		{"artifact unverified", operatorv1alpha1.ReasonArtifactUnverified, "ArtifactUnverified"},
		{"awaiting approval", operatorv1alpha1.ReasonAwaitingApproval, "AwaitingApproval"},
		{"configuration error", operatorv1alpha1.ReasonConfigurationError, "ConfigurationError"},
		{"converged after unknown outcome", operatorv1alpha1.ReasonConvergedAfterUnknownOutcome, "ConvergedAfterUnknownOutcome"},
		{"current plan", operatorv1alpha1.ReasonCurrentPlan, "CurrentPlan"},
		{"desired state changed", operatorv1alpha1.ReasonDesiredStateChanged, "DesiredStateChanged"},
		{"destructive changes disabled", operatorv1alpha1.ReasonDestructiveChangesDisabled, "DestructiveChangesDisabled"},
		{"digest pinned", operatorv1alpha1.ReasonDigestPinned, "DigestPinned"},
		{"dispatch committed", operatorv1alpha1.ReasonDispatchCommitted, "DispatchCommitted"},
		{"execution binding changed", operatorv1alpha1.ReasonExecutionBindingChanged, "ExecutionBindingChanged"},
		{"inputs changed", operatorv1alpha1.ReasonInputsChanged, "InputsChanged"},
		{"in sync", operatorv1alpha1.ReasonInSync, "InSync"},
		{"job completed", operatorv1alpha1.ReasonJobCompleted, "JobCompleted"},
		{"lease continuity lost", operatorv1alpha1.ReasonLeaseContinuityLost, "LeaseContinuityLost"},
		{"no changes", operatorv1alpha1.ReasonNoChanges, "NoChanges"},
		{"not required", operatorv1alpha1.ReasonNotRequired, "NotRequired"},
		{"observed", operatorv1alpha1.ReasonObserved, "Observed"},
		{"operation failed", operatorv1alpha1.ReasonOperationFailed, "OperationFailed"},
		{"operation in progress", operatorv1alpha1.ReasonOperationInProgress, "OperationInProgress"},
		{"outcome unknown", operatorv1alpha1.ReasonOutcomeUnknown, "OutcomeUnknown"},
		{"pending", operatorv1alpha1.ReasonPending, "Pending"},
		{"plan no longer current", operatorv1alpha1.ReasonPlanNoLongerCurrent, "PlanNoLongerCurrent"},
		{"plan ready", operatorv1alpha1.ReasonPlanReady, "PlanReady"},
		{"policy blocked", operatorv1alpha1.ReasonPolicyBlocked, "PolicyBlocked"},
		{"policy changed", operatorv1alpha1.ReasonPolicyChanged, "PolicyChanged"},
		{"policy refused", operatorv1alpha1.ReasonPolicyRefused, "PolicyRefused"},
		{"policy satisfied", operatorv1alpha1.ReasonPolicySatisfied, "PolicySatisfied"},
		{"proof inputs changed", operatorv1alpha1.ReasonProofInputsChanged, "ProofInputsChanged"},
		{"published", operatorv1alpha1.ReasonPublished, "Published"},
		{"refresh failed", operatorv1alpha1.ReasonRefreshFailed, "RefreshFailed"},
		{"refreshing", operatorv1alpha1.ReasonRefreshing, "Refreshing"},
		{"refresh suspended", operatorv1alpha1.ReasonRefreshSuspended, "RefreshSuspended"},
		{"requested", operatorv1alpha1.ReasonRequested, "Requested"},
		{"resolve failed", operatorv1alpha1.ReasonResolveFailed, "ResolveFailed"},
		{"satisfied", operatorv1alpha1.ReasonSatisfied, "Satisfied"},
		{"scoped changes", operatorv1alpha1.ReasonScopedChanges, "ScopedChanges"},
		{"scoped converged", operatorv1alpha1.ReasonScopedConverged, "ScopedConverged"},
		{"scoped plan pending", operatorv1alpha1.ReasonScopedPlanPending, "ScopedPlanPending"},
		{"source freshness unknown", operatorv1alpha1.ReasonSourceFreshnessUnknown, "SourceFreshnessUnknown"},
		{"source refresh pending", operatorv1alpha1.ReasonSourceRefreshPending, "SourceRefreshPending"},
		{"source resolution unknown", operatorv1alpha1.ReasonSourceResolutionUnknown, "SourceResolutionUnknown"},
		{"source unresolved", operatorv1alpha1.ReasonSourceUnresolved, "SourceUnresolved"},
		{"stale", operatorv1alpha1.ReasonStale, "Stale"},
		{"stale observation", operatorv1alpha1.ReasonStaleObservation, "StaleObservation"},
		{"stale plan", operatorv1alpha1.ReasonStalePlan, "StalePlan"},
		{"succeeded", operatorv1alpha1.ReasonSucceeded, "Succeeded"},
		{"superseded approval", operatorv1alpha1.ReasonSupersededApproval, "SupersededApproval"},
		{"supported engine", operatorv1alpha1.ReasonSupportedEngine, "SupportedEngine"},
		{"suspended", operatorv1alpha1.ReasonSuspended, "Suspended"},
		{"unsupported engine", operatorv1alpha1.ReasonUnsupportedEngine, "UnsupportedEngine"},
		{"verifying convergence", operatorv1alpha1.ReasonVerifyingConvergence, "VerifyingConvergence"},
		{"waiting", operatorv1alpha1.ReasonWaiting, "Waiting"},
	}

	wirePattern := regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,62}$`)
	seen := make(map[string]string, len(tests))
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			wire := string(test.reason)
			if wire != test.wire {
				t.Fatalf("wire value = %q, want %q", wire, test.wire)
			}
			if !wirePattern.MatchString(wire) {
				t.Fatalf("wire value %q is not a bounded Kubernetes-style reason", wire)
			}
		})
		if previous, duplicate := seen[test.wire]; duplicate {
			t.Errorf("wire value %q is shared by %q and %q", test.wire, previous, test.name)
		}
		seen[test.wire] = test.name
	}
}
