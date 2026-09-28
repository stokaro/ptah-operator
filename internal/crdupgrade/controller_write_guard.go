package crdupgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	controllerWriteGuardNamePrefix = "ptah-operator-controller-write-guard-"
	controllerWriteGuardComponent  = "controller-write-guard"

	activeOperationFinalizer    = "operator.ptah.run/active-operation"
	migrationOperationFinalizer = "operator.ptah.run/migration-operation"

	schemaResource    = "ptahschemas"
	migrationResource = "ptahmigrations"
)

// releaseDigest names a release's cluster-scoped objects apart from another
// release's. It depends on nothing a release changes from one version to the
// next, so an upgrade updates those objects in place. The chart computes the
// same value in ptah-operator.releaseDigest.
func releaseDigest(releaseNamespace, releaseName string) string {
	sum := sha256.Sum256([]byte(releaseNamespace + "\n" + releaseName))
	return hex.EncodeToString(sum[:])[:12]
}

// ControllerWriteGuardPolicyName returns the name of the release's
// desired-state boundary for the controller ServiceAccount.
func ControllerWriteGuardPolicyName(releaseNamespace, releaseName string) string {
	return controllerWriteGuardNamePrefix + releaseDigest(releaseNamespace, releaseName)
}

func controllerWriteGuardDenialMessage() string {
	return "Ptah controller write guard rejected a desired-state mutation"
}

// controllerPrincipalMatchExpression selects the requests the controller's
// ServiceAccount makes.
func controllerPrincipalMatchExpression(releaseNamespace, serviceAccount string) string {
	return fmt.Sprintf(`request.userInfo.username == %q`, "system:serviceaccount:"+releaseNamespace+":"+serviceAccount)
}

// ControllerWriteGuard builds the policy that confines the main-resource
// PtahSchema and PtahMigration patches the controller's ServiceAccount makes
// to the one finalizer it owns on the kind being written. Status writes use
// the status subresource and therefore do not match it.
//
// This is the only place the policy is written. hack/chartpolicies generates
// templates/controller-write-guard.yaml from it, with the release namespace
// and the controller ServiceAccount left as Helm expressions, and
// verify-source refuses a template the generator did not write.
type ControllerWriteGuard struct {
	ReleaseName                  string
	ReleaseNamespace             string
	ControllerServiceAccountName string
}

func (g *ControllerWriteGuard) name() string {
	return ControllerWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
}

// Policy is the guard's ValidatingAdmissionPolicy.
func (g *ControllerWriteGuard) Policy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	message := controllerWriteGuardDenialMessage()
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: g.name(), Labels: componentLabels(controllerWriteGuardComponent)},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy:    &fail,
			MatchConstraints: g.matchResources(),
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name:       "controller-service-account",
				Expression: controllerPrincipalMatchExpression(g.ReleaseNamespace, g.ControllerServiceAccountName),
			}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "oldFinalizers", Expression: `has(oldObject.metadata.finalizers) ? oldObject.metadata.finalizers : []`},
				{Name: "newFinalizers", Expression: `has(object.metadata.finalizers) ? object.metadata.finalizers : []`},
				// Each family has its own finalizer, so the one the
				// controller may add and remove follows the resource being
				// written rather than the schema path's name.
				{Name: "activeFinalizer", Expression: controllerWriteFinalizerExpression()},
				{Name: "oldActiveCount", Expression: `variables.oldFinalizers.filter(value, value == variables.activeFinalizer).size()`},
				{Name: "newActiveCount", Expression: `variables.newFinalizers.filter(value, value == variables.activeFinalizer).size()`},
			},
			Validations: []admissionregistrationv1.Validation{
				{Expression: `dyn(object).spec == dyn(oldObject).spec`, Message: message},
				{Expression: `has(dyn(object).status) == has(dyn(oldObject).status) && (!has(dyn(object).status) || dyn(object).status == dyn(oldObject).status)`, Message: message},
				{
					Expression: `has(object.metadata.labels) == has(oldObject.metadata.labels) && (!has(object.metadata.labels) || object.metadata.labels == oldObject.metadata.labels) && has(object.metadata.annotations) == has(oldObject.metadata.annotations) && (!has(object.metadata.annotations) || object.metadata.annotations == oldObject.metadata.annotations) && has(object.metadata.ownerReferences) == has(oldObject.metadata.ownerReferences) && (!has(object.metadata.ownerReferences) || object.metadata.ownerReferences == oldObject.metadata.ownerReferences)`,
					Message:    message,
				},
				{
					Expression: `(variables.oldActiveCount <= 1 && variables.newActiveCount <= 1) && ((variables.oldActiveCount == variables.newActiveCount && variables.oldFinalizers == variables.newFinalizers) || (variables.oldActiveCount == 0 && variables.newActiveCount == 1 && variables.newFinalizers.filter(value, value != variables.activeFinalizer) == variables.oldFinalizers) || (variables.oldActiveCount == 1 && variables.newActiveCount == 0 && variables.newFinalizers == variables.oldFinalizers.filter(value, value != variables.activeFinalizer)))`,
					Message:    message,
				},
			},
		},
	}
}

// Binding is the binding that enforces Policy.
func (g *ControllerWriteGuard) Binding() *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: g.name(), Labels: componentLabels(controllerWriteGuardComponent)},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        g.name(),
			MatchResources:    g.matchResources(),
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
}

// controllerWriteFinalizerExpression names the finalizer the controller owns
// on the resource this request writes.
func controllerWriteFinalizerExpression() string {
	return fmt.Sprintf(
		`request.resource.resource == %q ? %q : %q`,
		migrationResource,
		migrationOperationFinalizer,
		activeOperationFinalizer,
	)
}

func (g *ControllerWriteGuard) matchResources() *admissionregistrationv1.MatchResources {
	exact := admissionregistrationv1.Exact
	return &admissionregistrationv1.MatchResources{
		MatchPolicy:       &exact,
		NamespaceSelector: &metav1.LabelSelector{},
		ObjectSelector:    &metav1.LabelSelector{},
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
			RuleWithOperations: admissionregistrationv1.RuleWithOperations{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{"operator.ptah.run"},
					APIVersions: []string{"v1alpha1"},
					Resources:   []string{schemaResource, migrationResource},
					Scope:       scopePtr(admissionregistrationv1.NamespacedScope),
				},
			},
		}},
	}
}

func scopePtr(scope admissionregistrationv1.ScopeType) *admissionregistrationv1.ScopeType {
	return &scope
}
