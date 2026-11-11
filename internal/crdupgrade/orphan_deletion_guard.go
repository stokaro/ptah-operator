package crdupgrade

import (
	"fmt"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	orphanDeletionGuardNamePrefix = "ptah-operator-orphan-deletion-guard-"
	orphanDeletionGuardComponent  = "orphan-deletion-guard"
)

// OrphanDeletionGuardPolicyName returns the name of the release's guard on a
// deletion of a PtahSchema or a PtahMigration that would orphan its
// dependents.
func OrphanDeletionGuardPolicyName(releaseNamespace, releaseName string) string {
	return orphanDeletionGuardNamePrefix + releaseDigest(releaseNamespace, releaseName)
}

func orphanDeletionDenialMessage() string {
	return "A PtahSchema or PtahMigration deletion cannot orphan its dependents: the garbage collector would " +
		"have to take the resource's owner reference off the PtahResultRecords it owns, which admission keeps " +
		"immutable, so the resource would stay Terminating. Delete it with the default propagation, Background, " +
		"which kubectl delete sends unless --cascade names another"
}

func orphanFinalizerDenialMessage() string {
	return "This resource carries the orphan finalizer, so a deletion that names no propagation would orphan " +
		"its dependents and stay Terminating on the PtahResultRecords it owns. Delete it with propagationPolicy: " +
		"Background, which kubectl delete sends by default and which takes the finalizer off"
}

// OrphanDeletionGuard builds the policy that refuses a deletion of a
// PtahSchema or a PtahMigration that would orphan its dependents.
//
// Orphaning makes the garbage collector take the owner reference off every
// dependent before it removes the orphan finalizer, and the controller-write
// webhook refuses any change to a PtahResultRecord's owner references, the
// garbage collector's included. So such a deletion never finishes: the
// resource stays Terminating until its records go.
//
// The policy decides the way the API server's registry does
// (shouldOrphanDependents in k8s.io/apiserver
// pkg/registry/generic/registry/store.go). orphanDependents, when the request
// sets it, decides; otherwise propagationPolicy does; a request that names
// neither follows the orphan finalizer the resource already carries. A
// deletion that names Background or Foreground takes that finalizer off in
// the same write, so it is admitted, and so is one that names nothing on a
// resource without the finalizer. The namespace controller always names
// Background.
//
// It refuses no write that adds the orphan finalizer. The finalizer does
// nothing until a deletion honors it, the API server refuses a new finalizer
// on a resource already being deleted, and the deletion that would honor it
// is refused here.
//
// A ValidatingAdmissionPolicy runs inside the API server, so a manager that is
// down blocks no deletion, as a webhook under failurePolicy Fail would.
//
// This is the only place the policy is written. hack/chartpolicies generates
// templates/orphan-deletion-guard.yaml from it, with the policy's name left
// as a Helm expression.
type OrphanDeletionGuard struct {
	ReleaseName      string
	ReleaseNamespace string
}

func (g *OrphanDeletionGuard) name() string {
	return OrphanDeletionGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
}

// Policy is the guard's ValidatingAdmissionPolicy.
func (g *OrphanDeletionGuard) Policy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	forbidden := metav1.StatusReasonForbidden
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: g.name(), Labels: componentLabels(orphanDeletionGuardComponent)},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy:    &fail,
			MatchConstraints: g.matchResources(),
			Variables: []admissionregistrationv1.Variable{
				// The legacy field wins over propagationPolicy, as it does in
				// the registry; the API server refuses a request that sets both.
				{
					Name: "orphanRequested",
					Expression: fmt.Sprintf(
						`has(request.options.orphanDependents) ? request.options.orphanDependents : has(request.options.propagationPolicy) && request.options.propagationPolicy == %q`,
						metav1.DeletePropagationOrphan),
				},
				{
					Name:       "propagationNamed",
					Expression: `has(request.options.orphanDependents) || has(request.options.propagationPolicy)`,
				},
				{
					Name: "orphanFinalizer",
					Expression: fmt.Sprintf(`has(oldObject.metadata.finalizers) && %q in oldObject.metadata.finalizers`,
						metav1.FinalizerOrphanDependents),
				},
			},
			Validations: []admissionregistrationv1.Validation{
				{Expression: `!variables.orphanRequested`, Message: orphanDeletionDenialMessage(), Reason: &forbidden},
				{
					Expression: `variables.propagationNamed || !variables.orphanFinalizer`,
					Message:    orphanFinalizerDenialMessage(),
					Reason:     &forbidden,
				},
			},
		},
	}
}

// Binding is the binding that enforces Policy.
func (g *OrphanDeletionGuard) Binding() *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: g.name(), Labels: componentLabels(orphanDeletionGuardComponent)},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        g.name(),
			MatchResources:    g.matchResources(),
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
}

// matchResources selects every deletion of the two kinds that own result
// records, in any version the API serves them under.
func (g *OrphanDeletionGuard) matchResources() *admissionregistrationv1.MatchResources {
	equivalent := admissionregistrationv1.Equivalent
	return &admissionregistrationv1.MatchResources{
		MatchPolicy:       &equivalent,
		NamespaceSelector: &metav1.LabelSelector{},
		ObjectSelector:    &metav1.LabelSelector{},
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
			RuleWithOperations: admissionregistrationv1.RuleWithOperations{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Delete},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{operatorAPIGroup},
					APIVersions: []string{"*"},
					Resources:   []string{schemaResource, migrationResource},
					Scope:       scopePtr(admissionregistrationv1.NamespacedScope),
				},
			},
		}},
	}
}
