package certrotation

import (
	"fmt"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	secretCreateGuardDenialMessage = "certificate rotator Secret CREATE is outside its exact recovery contract"
	secretCreateGuardComponent     = "certificate-rotation"
)

// SecretCreateGuard is the admission policy that narrows the certificate
// rotator's Secret CREATE grant to the one generated TLS Secret it may
// recreate. RBAC cannot scope CREATE by resource name, so when recreation is
// opted into the chart grants create on every Secret in the release
// namespace, and this policy refuses every CREATE the rotator's
// ServiceAccount makes that is not exactly that Secret: its name, its
// namespace, the labels and Helm ownership annotations the rotator writes,
// the TLS type, and the four keys with material in each.
//
// This is the only place the policy is written. hack/chartpolicies generates
// templates/certificate-secret-guard.yaml from it, with the release
// namespace, the release name, the Secret name and the rotator's
// ServiceAccount left as Helm expressions, and verify-source refuses a
// template the generator did not write.
type SecretCreateGuard struct {
	Namespace          string
	ReleaseName        string
	SecretName         string
	ServiceAccountName string
}

// Policy is the guard's ValidatingAdmissionPolicy. It is named after the
// rotator's ServiceAccount, as the binding is.
func (g SecretCreateGuard) Policy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   g.ServiceAccountName,
			Labels: map[string]string{"app.kubernetes.io/component": secretCreateGuardComponent},
		},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
						Rule: admissionregistrationv1.Rule{
							APIGroups:   []string{""},
							APIVersions: []string{"v1"},
							Resources:   []string{"secrets"},
							Scope:       scopePointer(admissionregistrationv1.NamespacedScope),
						},
					},
				}},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name:       "exact-certificate-rotator-service-account",
				Expression: g.matchExpression(),
			}},
			Validations: []admissionregistrationv1.Validation{{
				Expression: g.validationExpression(),
				Message:    secretCreateGuardDenialMessage,
			}},
		},
	}
}

// Binding enforces Policy in the release namespace alone: the rotator's
// grant is a Role there, so a CREATE anywhere else is refused by RBAC before
// admission sees it.
func (g SecretCreateGuard) Binding() *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   g.ServiceAccountName,
			Labels: map[string]string{"app.kubernetes.io/component": secretCreateGuardComponent},
		},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        g.ServiceAccountName,
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
			MatchResources: &admissionregistrationv1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": g.Namespace},
				},
			},
		},
	}
}

func (g SecretCreateGuard) matchExpression() string {
	return fmt.Sprintf("request.userInfo.username == 'system:serviceaccount:%s:%s'", g.Namespace, g.ServiceAccountName)
}

// validationExpression is the exact recovery contract, on one line: the
// policy carries no whitespace a reader of the installed object has to see
// past, and CEL reads it the same either way.
func (g SecretCreateGuard) validationExpression() string {
	return compactCEL(fmt.Sprintf(`
		object.metadata.name == '%s' &&
		object.metadata.namespace == '%s' &&
		(!has(object.metadata.generateName) || object.metadata.generateName == '') &&
		has(object.metadata.labels) &&
		object.metadata.labels == {'%s': '%s', '%s': '%s'} &&
		has(object.metadata.annotations) &&
		object.metadata.annotations == {'%s': '%s', '%s': '%s'} &&
		(!has(object.metadata.ownerReferences) || object.metadata.ownerReferences.size() == 0) &&
		(!has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0) &&
		object.type == 'kubernetes.io/tls' &&
		!has(object.immutable) &&
		(!has(object.stringData) || object.stringData.size() == 0) &&
		object.data.size() == 4 &&
		'ca.crt' in object.data && object.data['ca.crt'].size() > 0 &&
		'ca.key' in object.data && object.data['ca.key'].size() > 0 &&
		'tls.crt' in object.data && object.data['tls.crt'].size() > 0 &&
		'tls.key' in object.data && object.data['tls.key'].size() > 0
	`,
		g.SecretName,
		g.Namespace,
		GeneratedSecretLabel,
		GeneratedSecretLabelValue,
		HelmManagedByLabel,
		HelmManagedByLabelValue,
		HelmReleaseNameAnnotation,
		g.ReleaseName,
		HelmReleaseNamespaceAnnotation,
		g.Namespace,
	))
}

func compactCEL(expression string) string {
	return strings.Join(strings.Fields(expression), " ")
}

func scopePointer(scope admissionregistrationv1.ScopeType) *admissionregistrationv1.ScopeType {
	return &scope
}
