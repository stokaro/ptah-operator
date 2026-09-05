package crdupgrade

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
)

const (
	admissionConvergenceDependencyContractVersion = "1"
	admissionConvergenceProbeFieldManagerPrefix   = "ptah-admission-convergence-v1-"
	admissionConvergenceProbePersistenceMessage   = "Ptah admission convergence probe field manager is reserved for dry-run verification"
)

type admissionConvergenceDependencyProbe struct {
	PolicyName   string
	FieldManager string
	Message      string
}

func newAdmissionConvergenceDependencyProbe(policyName, attempt string) admissionConvergenceDependencyProbe {
	digest := sha256.Sum256([]byte(admissionConvergenceDependencyContractVersion + "\n" + policyName + "\n" + attempt))
	fieldManager := admissionConvergenceProbeFieldManagerPrefix + fmt.Sprintf("%x", digest)
	return admissionConvergenceDependencyProbe{
		PolicyName:   policyName,
		FieldManager: fieldManager,
		Message:      "Ptah admission convergence confirmed exact workload guard " + fieldManager,
	}
}

func admissionConvergenceProbeRequestExpression(releaseNamespace, markerName, fieldManager string) string {
	return fmt.Sprintf(
		`request.operation == "UPDATE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "configmaps" && (!has(request.subResource) || request.subResource == "") && request.namespace == %q && request.name == %q && has(request.options) && has(request.options.fieldManager) && request.options.fieldManager == %q`,
		releaseNamespace,
		markerName,
		fieldManager,
	)
}

func admissionConvergenceAnyProbeRequestExpression(releaseNamespace, markerName string) string {
	return fmt.Sprintf(
		`request.operation == "UPDATE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "configmaps" && (!has(request.subResource) || request.subResource == "") && request.namespace == %q && request.name == %q && has(request.options) && has(request.options.fieldManager) && request.options.fieldManager.matches(%q)`,
		releaseNamespace,
		markerName,
		"^"+admissionConvergenceProbeFieldManagerPrefix+`[0-9a-f]{64}$`,
	)
}

func admissionConvergenceProbeResourceRule() admissionregistrationv1.NamedRuleWithOperations {
	return admissionregistrationv1.NamedRuleWithOperations{
		RuleWithOperations: admissionregistrationv1.RuleWithOperations{
			Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
			Rule: admissionregistrationv1.Rule{
				APIGroups:   []string{""},
				APIVersions: []string{"v1"},
				Resources:   []string{"configmaps"},
				Scope:       scopePtr(admissionregistrationv1.NamespacedScope),
			},
		},
	}
}

func addAdmissionConvergenceProbeMatchResource(match *admissionregistrationv1.MatchResources) {
	if match == nil {
		return
	}
	match.ResourceRules = append(match.ResourceRules, admissionConvergenceProbeResourceRule())
}

// addAdmissionConvergenceDependencyProbe makes one immutable workload policy
// directly observable through an unchanged marker update. Normal validations
// are bypassed only for the exact marker and content-versioned field manager;
// the final validation then emits one unique denial. A non-dry-run use of the
// reserved field manager is also denied, so the proof can never mutate state.
func addAdmissionConvergenceDependencyProbe(
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
	releaseNamespace string,
	markerName string,
	attempt string,
) admissionConvergenceDependencyProbe {
	probe := newAdmissionConvergenceDependencyProbe(policy.Name, attempt)
	expression := admissionConvergenceProbeRequestExpression(releaseNamespace, markerName, probe.FieldManager)
	anyProbeExpression := admissionConvergenceAnyProbeRequestExpression(releaseNamespace, markerName)
	addAdmissionConvergenceProbeMatchResource(policy.Spec.MatchConstraints)
	for index := range policy.Spec.MatchConditions {
		policy.Spec.MatchConditions[index].Expression = "(" + anyProbeExpression + ") || (" + policy.Spec.MatchConditions[index].Expression + ")"
	}
	policy.Spec.Variables = append([]admissionregistrationv1.Variable{
		{Name: "isAnyAdmissionConvergenceProbe", Expression: anyProbeExpression},
		{Name: "isAdmissionConvergenceProbe", Expression: expression},
	}, policy.Spec.Variables...)
	for index := range policy.Spec.Validations {
		policy.Spec.Validations[index].Expression = "variables.isAnyAdmissionConvergenceProbe || (" + policy.Spec.Validations[index].Expression + ")"
	}
	policy.Spec.Validations = append(policy.Spec.Validations,
		admissionregistrationv1.Validation{
			Expression: `!variables.isAnyAdmissionConvergenceProbe || request.dryRun == true`,
			Message:    admissionConvergenceProbePersistenceMessage,
		},
		admissionregistrationv1.Validation{
			Expression: `!variables.isAdmissionConvergenceProbe`,
			Message:    probe.Message,
		},
	)
	return probe
}

// addStableAdmissionConvergenceDependencyProbe makes a release-stable policy
// observable without embedding a candidate sequence or image in its retained
// spec. The current attempt still supplies a content-versioned field manager;
// the policy echoes it in the denial so the caller can distinguish this exact
// policy from every other admission dependency.
func addStableAdmissionConvergenceDependencyProbe(
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
	releaseNamespace string,
	markerNamePattern string,
) {
	expression := fmt.Sprintf(
		`request.operation == "UPDATE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "configmaps" && (!has(request.subResource) || request.subResource == "") && request.namespace == %q && request.name.matches(%q) && has(request.options) && has(request.options.fieldManager) && request.options.fieldManager.matches(%q)`,
		releaseNamespace,
		markerNamePattern,
		"^"+admissionConvergenceProbeFieldManagerPrefix+`[0-9a-f]{64}$`,
	)
	policy.Spec.MatchConstraints.ResourceRules = append(
		policy.Spec.MatchConstraints.ResourceRules,
		admissionregistrationv1.NamedRuleWithOperations{
			RuleWithOperations: admissionregistrationv1.RuleWithOperations{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"configmaps"},
					Scope:       scopePtr(admissionregistrationv1.NamespacedScope),
				},
			},
		},
	)
	for index := range policy.Spec.MatchConditions {
		policy.Spec.MatchConditions[index].Expression = "(" + expression + ") || (" + policy.Spec.MatchConditions[index].Expression + ")"
	}
	policy.Spec.Variables = append([]admissionregistrationv1.Variable{
		{Name: "isAnyAdmissionConvergenceProbe", Expression: expression},
		{Name: "isAdmissionConvergenceProbe", Expression: expression},
	}, policy.Spec.Variables...)
	for index := range policy.Spec.Validations {
		policy.Spec.Validations[index].Expression = "variables.isAnyAdmissionConvergenceProbe || (" + policy.Spec.Validations[index].Expression + ")"
	}
	policy.Spec.Validations = append(policy.Spec.Validations,
		admissionregistrationv1.Validation{
			Expression: `!variables.isAnyAdmissionConvergenceProbe || request.dryRun == true`,
			Message:    admissionConvergenceProbePersistenceMessage,
		},
		admissionregistrationv1.Validation{
			Expression:        `!variables.isAdmissionConvergenceProbe`,
			MessageExpression: `"Ptah admission convergence confirmed exact workload guard " + request.options.fieldManager`,
		},
	)
}

// removeAdmissionConvergenceDependencyProbe restores the native policy shape
// for immutable predecessor contracts that predate this publication probe.
// It rejects any unfamiliar wrapper instead of silently redefining history.
func removeAdmissionConvergenceDependencyProbe(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
	if policy == nil || policy.Spec.MatchConstraints == nil {
		return fmt.Errorf("admission convergence dependency policy or match constraints are nil")
	}
	if len(policy.Spec.Variables) < 2 ||
		policy.Spec.Variables[0].Name != "isAnyAdmissionConvergenceProbe" ||
		policy.Spec.Variables[1].Name != "isAdmissionConvergenceProbe" {
		return fmt.Errorf("admission convergence dependency variables do not have the exact wrapper")
	}
	anyExpression := policy.Spec.Variables[0].Expression
	exactExpression := policy.Spec.Variables[1].Expression
	fieldManagerMarker := ` && request.options.fieldManager == "`
	markerIndex := strings.LastIndex(exactExpression, fieldManagerMarker)
	if markerIndex < 0 || !strings.HasSuffix(exactExpression, `"`) {
		return fmt.Errorf("admission convergence dependency exact selector is malformed")
	}
	wantAnyExpression := exactExpression[:markerIndex] +
		` && request.options.fieldManager.matches("^` + admissionConvergenceProbeFieldManagerPrefix + `[0-9a-f]{64}$")`
	if anyExpression != wantAnyExpression {
		return fmt.Errorf("admission convergence dependency union selector differs from the exact selector")
	}

	wantRule := admissionregistrationv1.NamedRuleWithOperations{
		RuleWithOperations: admissionregistrationv1.RuleWithOperations{
			Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
			Rule: admissionregistrationv1.Rule{
				APIGroups:   []string{""},
				APIVersions: []string{"v1"},
				Resources:   []string{"configmaps"},
				Scope:       scopePtr(admissionregistrationv1.NamespacedScope),
			},
		},
	}
	resourceRules := policy.Spec.MatchConstraints.ResourceRules
	if len(resourceRules) == 0 || !reflect.DeepEqual(resourceRules[len(resourceRules)-1], wantRule) {
		return fmt.Errorf("admission convergence dependency marker rule differs from the exact wrapper")
	}
	if len(policy.Spec.Validations) < 2 {
		return fmt.Errorf("admission convergence dependency validations omit the exact wrapper")
	}
	probeValidations := policy.Spec.Validations[len(policy.Spec.Validations)-2:]
	if probeValidations[0].Expression != `!variables.isAnyAdmissionConvergenceProbe || request.dryRun == true` ||
		probeValidations[0].Message != admissionConvergenceProbePersistenceMessage ||
		probeValidations[1].Expression != `!variables.isAdmissionConvergenceProbe` ||
		!strings.HasPrefix(probeValidations[1].Message, "Ptah admission convergence confirmed exact workload guard "+admissionConvergenceProbeFieldManagerPrefix) {
		return fmt.Errorf("admission convergence dependency proof validations differ from the exact wrapper")
	}

	matchPrefix := "(" + anyExpression + ") || ("
	for index := range policy.Spec.MatchConditions {
		expression := policy.Spec.MatchConditions[index].Expression
		if !strings.HasPrefix(expression, matchPrefix) || !strings.HasSuffix(expression, ")") {
			return fmt.Errorf("admission convergence dependency match condition %d differs from the exact wrapper", index)
		}
		policy.Spec.MatchConditions[index].Expression = strings.TrimSuffix(strings.TrimPrefix(expression, matchPrefix), ")")
	}
	validationPrefix := "variables.isAnyAdmissionConvergenceProbe || ("
	for index := range policy.Spec.Validations[:len(policy.Spec.Validations)-2] {
		expression := policy.Spec.Validations[index].Expression
		if !strings.HasPrefix(expression, validationPrefix) || !strings.HasSuffix(expression, ")") {
			return fmt.Errorf("admission convergence dependency validation %d differs from the exact wrapper", index)
		}
		policy.Spec.Validations[index].Expression = strings.TrimSuffix(strings.TrimPrefix(expression, validationPrefix), ")")
	}
	policy.Spec.MatchConstraints.ResourceRules = resourceRules[:len(resourceRules)-1]
	policy.Spec.Variables = policy.Spec.Variables[2:]
	policy.Spec.Validations = policy.Spec.Validations[:len(policy.Spec.Validations)-2]
	return nil
}
