package crdupgrade

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
)

// Kubernetes rejects a CEL expression above 100,000 code points. Keep ten percent
// of that ceiling unused so ordinary contract growth cannot turn an upgrade
// into an API-server compilation failure without first failing this test.
const admissionExpressionCodePointLimitWithHeadroom = 90_000

// This is intentionally a white-box test: the immutable admission policies are
// internal upgrade contracts, and their generated CEL is not an exported API.
func TestHookExecutableArgumentsUseBoundedCELExpressions(t *testing.T) {
	t.Parallel()

	rollout := runtimePodGuardFixture()
	// Make the shared contract large enough that combining all five executable
	// argument alternatives would cross the Kubernetes expression-size limit.
	rollout.RuntimeAdmissionContractB64 = strings.Repeat("Y", 24_000)
	parentGuard := NewParentWorkloadGuard(rollout)
	identityJob := HookIdentityProbeJobName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
	preflightJob := rollout.hookJobName("preflight")
	reconcileJob := rollout.hookJobName("reconcile")
	quiesceJob := rollout.hookJobName("teardown-quiesce")
	teardownJob := rollout.hookJobName("teardown")

	tests := []struct {
		name                    string
		policy                  *admissionregistrationv1.ValidatingAdmissionPolicy
		validationPrefix        string
		argumentValidationExprs []string
	}{
		{
			name:             "compiled parent hook Job contract",
			policy:           parentGuard.hookJobContractPolicy(),
			validationPrefix: "!variables.isMainWrite ||",
			argumentValidationExprs: []string{
				fmt.Sprintf(`!variables.isMainWrite || (!variables.isImageCheck || object.spec.template.spec.containers[0].args == ["image-check", %q, %q])`, "--release-sequence="+fmt.Sprint(rollout.ReleaseSequence), "--manager-image="+rollout.ManagerImage),
				fmt.Sprintf(`!variables.isMainWrite || (!variables.isIdentity || %s)`, rollout.hookArgsValidationExpression("object.spec.template.spec.containers[0]", "identity-probe")),
				fmt.Sprintf(`!variables.isMainWrite || (!variables.isPreflight || %s)`, rollout.hookArgsValidationExpression("object.spec.template.spec.containers[0]", "preflight")),
				fmt.Sprintf(`!variables.isMainWrite || (variables.effectiveName != %q || %s)`, reconcileJob, rollout.hookArgsValidationExpression("object.spec.template.spec.containers[0]", "reconcile")),
				fmt.Sprintf(`!variables.isMainWrite || (!variables.isQuiesce || %s)`, rollout.hookArgsValidationExpression("object.spec.template.spec.containers[0]", "teardown-quiesce")),
				fmt.Sprintf(`!variables.isMainWrite || (!variables.isTeardown || %s)`, rollout.hookArgsValidationExpression("object.spec.template.spec.containers[0]", "teardown")),
			},
		},
		{
			name:   "compiled hook Pod identity contract",
			policy: rollout.hookIdentityPolicy(),
			argumentValidationExprs: []string{
				hookPodArgumentValidationExpression(rollout, identityJob, "identity-probe"),
				hookPodArgumentValidationExpression(rollout, preflightJob, "preflight"),
				hookPodArgumentValidationExpression(rollout, reconcileJob, "reconcile"),
				hookPodArgumentValidationExpression(rollout, quiesceJob, "teardown-quiesce"),
				hookPodArgumentValidationExpression(rollout, teardownJob, "teardown"),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assertAdmissionPolicyCELHeadroom(t, test.name, test.policy)
			policy := stripAdmissionConvergenceDependencyProbe(t, test.policy)

			var got []string
			for _, validation := range policy.Spec.Validations {
				if strings.Contains(validation.Expression, ".args ==") && (test.validationPrefix == "" || strings.HasPrefix(validation.Expression, test.validationPrefix)) {
					got = append(got, validation.Expression)
				}
			}
			if !reflect.DeepEqual(got, test.argumentValidationExprs) {
				t.Fatalf("executable argument validations\n got: %#v\nwant: %#v", got, test.argumentValidationExprs)
			}
			for index, expression := range got {
				if count := strings.Count(expression, ".args =="); count != 1 {
					t.Fatalf("executable argument validation %d contains %d argument contracts, want one", index, count)
				}
			}
		})
	}
}

func hookPodArgumentValidationExpression(rollout *RolloutGuard, jobName, mode string) string {
	const jobLabel = `object.metadata.labels["batch.kubernetes.io/job-name"]`
	return fmt.Sprintf(`%s != %q || %s`, jobLabel, jobName, rollout.hookArgsValidationExpression("object.spec.containers[0]", mode))
}

func assertAdmissionPolicyCELHeadroom(t *testing.T, description string, policy *admissionregistrationv1.ValidatingAdmissionPolicy) {
	t.Helper()
	if policy == nil {
		t.Fatalf("%s policy is missing", description)
	}
	for index, condition := range policy.Spec.MatchConditions {
		assertCELExpressionHeadroom(t, fmt.Sprintf("%s match condition %d", description, index), condition.Expression)
	}
	for index, variable := range policy.Spec.Variables {
		assertCELExpressionHeadroom(t, fmt.Sprintf("%s variable %d", description, index), variable.Expression)
	}
	for index, validation := range policy.Spec.Validations {
		assertCELExpressionHeadroom(t, fmt.Sprintf("%s validation %d", description, index), validation.Expression)
		if validation.MessageExpression != "" {
			assertCELExpressionHeadroom(t, fmt.Sprintf("%s validation %d message expression", description, index), validation.MessageExpression)
		}
	}
	for index, annotation := range policy.Spec.AuditAnnotations {
		assertCELExpressionHeadroom(t, fmt.Sprintf("%s audit annotation %d", description, index), annotation.ValueExpression)
	}
}

func assertCELExpressionHeadroom(t *testing.T, description, expression string) {
	t.Helper()
	codePoints := utf8.RuneCountInString(expression)
	if codePoints >= admissionExpressionCodePointLimitWithHeadroom {
		t.Fatalf("%s has %d CEL code points, want fewer than %d", description, codePoints, admissionExpressionCodePointLimitWithHeadroom)
	}
}
