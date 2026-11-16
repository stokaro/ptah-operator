package crdupgrade

import (
	"fmt"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	jobOrphanDeletionGuardNamePrefix = "ptah-operator-job-orphan-deletion-guard-"
	jobOrphanDeletionGuardComponent  = "job-orphan-deletion-guard"

	// The identity the manager writes on every operation Job and on its Pod
	// template (internal/workload), spelled here the way the finalizers are so
	// the guards do not import the builder.
	operationComponentLabel     = "app.kubernetes.io/component"
	operationManager            = "ptah-operator"
	schemaOperationComponent    = "schema-operation"
	migrationOperationComponent = "migration-operation"
)

// JobOrphanDeletionGuardPolicyName returns the name of the release's guard on a
// deletion of an operation Job that would orphan the Job's Pods.
func JobOrphanDeletionGuardPolicyName(releaseNamespace, releaseName string) string {
	return jobOrphanDeletionGuardNamePrefix + releaseDigest(releaseNamespace, releaseName)
}

func jobOrphanDeletionDenialMessage() string {
	return "An operation Job's deletion cannot orphan its Pods: the garbage collector would have to take the Job's " +
		"owner reference off each Pod, which the Pod intent webhook refuses because the manager finds an Apply's " +
		"executor through the Pods its Job owns, so the Job would stay Terminating. Delete it with kubectl's " +
		"default propagation, Background, and save the Pod log with kubectl logs before you do if you need it"
}

func jobDefaultOrphanDenialMessage() string {
	return "A Job deletion that names no propagation orphans the Job's Pods, by the batch/v1 default or the orphan " +
		"finalizer the Job carries, and an operation Job's Pods cannot be orphaned, so the Job would stay " +
		"Terminating. Name propagationPolicy: Background, which kubectl delete sends by default"
}

// JobOrphanDeletionGuard builds the policy that refuses a deletion of an
// operation Job that would orphan the Job's Pods.
//
// Orphaning makes the garbage collector take the Job's owner reference off each
// Pod before it removes the orphan finalizer. The Pod-intent webhook refuses
// that write on an operation Pod, the garbage collector's included, and it has
// to: the manager finds a dispatched Apply's executor through the Pods the Job
// owns, so a Pod orphaned while its executor runs would let the database Lease
// go under it. So such a deletion never finishes, and the Job stays Terminating.
//
// It selects an operation Job by the two labels the manager writes on the Job
// and on its Pod template: app.kubernetes.io/managed-by ptah-operator, and
// app.kubernetes.io/component schema-operation or migration-operation. They
// are the pair the Pod-intent webhook reads on a Pod to decide it is managed,
// so the condition selects the Jobs whose Pods that webhook keeps owned.
// spec.execution.podMetadata cannot declare either key, and nothing else in the
// chart or the manager writes either component, so no other Job is judged. A
// Job keeps the labels when an orphaning deletion of its owner, before the
// orphan-deletion guard existed, took its owner reference off, and its Pods
// keep them from the template, so that Job is selected too.
//
// The verdict follows the registry (shouldOrphanDependents in k8s.io/apiserver
// pkg/registry/generic/registry/store.go): orphanDependents, when the request
// sets it, then propagationPolicy, then the finalizers the Job carries, and
// last the kind's default. The API server serves Jobs only as batch/v1, and
// its default there is to orphan, so a deletion that names no propagation
// orphans the Pods unless the Job carries the foregroundDeletion finalizer.
// kubectl delete names Background unless --cascade says otherwise, the garbage
// collector and the namespace controller name a propagation, and so does the
// TTL controller, so none of them is refused.
//
// It refuses no write that adds the orphan finalizer, for the reason
// OrphanDeletionGuard gives. It is a policy of its own rather than a rule in
// OrphanDeletionGuard because the two select different objects, decide a
// deletion that names no propagation differently and refuse for different
// reasons, so one policy would need every variable to branch on the resource.
//
// This is the only place the policy is written. hack/chartpolicies generates
// templates/job-orphan-deletion-guard.yaml from it, with the policy's name left
// as a Helm expression.
type JobOrphanDeletionGuard struct {
	ReleaseName      string
	ReleaseNamespace string
}

func (g *JobOrphanDeletionGuard) name() string {
	return JobOrphanDeletionGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
}

// Policy is the guard's ValidatingAdmissionPolicy.
func (g *JobOrphanDeletionGuard) Policy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	forbidden := metav1.StatusReasonForbidden
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: g.name(), Labels: componentLabels(jobOrphanDeletionGuardComponent)},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy:    &fail,
			MatchConstraints: g.matchResources(),
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name:       "operation-job",
				Expression: operationJobMatchExpression(),
			}},
			Variables: []admissionregistrationv1.Variable{
				orphanRequestedVariable(),
				propagationNamedVariable(),
				{
					Name: "foregroundFinalizer",
					Expression: fmt.Sprintf(`has(oldObject.metadata.finalizers) && %q in oldObject.metadata.finalizers`,
						metav1.FinalizerDeleteDependents),
				},
			},
			Validations: []admissionregistrationv1.Validation{
				{Expression: `!variables.orphanRequested`, Message: jobOrphanDeletionDenialMessage(), Reason: &forbidden},
				{
					Expression: `variables.propagationNamed || variables.foregroundFinalizer`,
					Message:    jobDefaultOrphanDenialMessage(),
					Reason:     &forbidden,
				},
			},
		},
	}
}

// operationJobMatchExpression selects a Job that carries the operation
// identity. A deletion has no object, so it reads the stored one.
func operationJobMatchExpression() string {
	return fmt.Sprintf(
		`has(oldObject.metadata.labels) && %[1]q in oldObject.metadata.labels && oldObject.metadata.labels[%[1]q] == %[2]q && %[3]q in oldObject.metadata.labels && oldObject.metadata.labels[%[3]q] in [%[4]q, %[5]q]`,
		managedByLabel, operationManager, operationComponentLabel, schemaOperationComponent, migrationOperationComponent)
}

// Binding is the binding that enforces Policy.
func (g *JobOrphanDeletionGuard) Binding() *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: g.name(), Labels: componentLabels(jobOrphanDeletionGuardComponent)},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        g.name(),
			MatchResources:    g.matchResources(),
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
}

// matchResources selects every deletion of a Job. The match condition narrows
// it to the operation Jobs.
func (g *JobOrphanDeletionGuard) matchResources() *admissionregistrationv1.MatchResources {
	equivalent := admissionregistrationv1.Equivalent
	return &admissionregistrationv1.MatchResources{
		MatchPolicy:       &equivalent,
		NamespaceSelector: &metav1.LabelSelector{},
		ObjectSelector:    &metav1.LabelSelector{},
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
			RuleWithOperations: admissionregistrationv1.RuleWithOperations{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Delete},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{"batch"},
					APIVersions: []string{"*"},
					Resources:   []string{"jobs"},
					Scope:       scopePtr(admissionregistrationv1.NamespacedScope),
				},
			},
		}},
	}
}
