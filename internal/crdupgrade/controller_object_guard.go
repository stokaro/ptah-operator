package crdupgrade

import (
	"fmt"
	"strconv"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	controllerJobWriteGuardNamePrefix  = "ptah-operator-job-write-guard-"
	controllerChunkWriteGuardPrefix    = "ptah-operator-chunk-write-guard-"
	controllerPlanWriteGuardNamePrefix = "ptah-operator-plan-write-guard-"
	// The migration plan is a separate kind with a separate shape, so it gets
	// its own boundary rather than a widened one: the schema plan contract
	// stays exactly as strict as it was.
	controllerMigrationPlanWriteGuardNamePrefix = "ptah-operator-migration-plan-write-guard-"

	controllerJobWriteGuardComponent   = "controller-job-write-guard"
	controllerChunkWriteGuardComponent = "controller-chunk-write-guard"
	controllerPlanWriteGuardComponent  = "controller-plan-write-guard"

	controllerMigrationPlanWriteGuardComponent = "controller-migration-plan-write-guard"
)

// ControllerJobWriteGuardPolicyName returns the stable release-owned name of
// the manager's structural Job write boundary.
func ControllerJobWriteGuardPolicyName(releaseNamespace, releaseName string) string {
	return controllerObjectGuardPolicyName(controllerJobWriteGuardNamePrefix, releaseNamespace, releaseName)
}

// ControllerChunkWriteGuardPolicyName returns the stable release-owned name
// of the manager's structural plan-chunk ConfigMap write boundary.
func ControllerChunkWriteGuardPolicyName(releaseNamespace, releaseName string) string {
	return controllerObjectGuardPolicyName(controllerChunkWriteGuardPrefix, releaseNamespace, releaseName)
}

// ControllerPlanWriteGuardPolicyName returns the stable release-owned name of
// the manager's structural PtahSchemaPlan write boundary.
func ControllerPlanWriteGuardPolicyName(releaseNamespace, releaseName string) string {
	return controllerObjectGuardPolicyName(controllerPlanWriteGuardNamePrefix, releaseNamespace, releaseName)
}

// ControllerMigrationPlanWriteGuardPolicyName returns the stable release-owned
// name of the manager's structural migration plan write boundary.
func ControllerMigrationPlanWriteGuardPolicyName(releaseNamespace, releaseName string) string {
	return controllerObjectGuardPolicyName(controllerMigrationPlanWriteGuardNamePrefix, releaseNamespace, releaseName)
}

func controllerObjectGuardPolicyName(prefix, releaseNamespace, releaseName string) string {
	return prefix + releaseDigest(releaseNamespace, releaseName)
}

type controllerObjectGuardEntry struct {
	name          string
	component     string
	apiGroups     []string
	apiVersions   []string
	resource      string
	operations    []admissionregistrationv1.OperationType
	denialMessage string
	validations   []admissionregistrationv1.Validation
	// releaseValues is whether the kind names the manager that built it. A Job
	// and both plans do; a chunk does not, because it is bound to its plan by
	// name and owner, and the plan carries the manager's identity.
	releaseValues bool
}

// ControllerObjectGuard builds the typed structural boundaries around every
// main-resource object the manager may create or update. The validating
// webhook remains the authoritative reconstruction boundary; these policies
// independently reject broad or privileged shapes, and refuse a Job or a plan
// that names a manager other than this release's.
//
// This is the only place the policies are written. hack/chartpolicies
// generates templates/controller-object-guard.yaml from it, with the release
// values -- the namespace, the controller ServiceAccount, the manager image
// and the controller-state version -- left as Helm expressions, and
// verify-source refuses a template the generator did not write.
type ControllerObjectGuard struct {
	ReleaseName                  string
	ReleaseNamespace             string
	ControllerServiceAccountName string
	ManagerImage                 string
	ControllerStateVersion       int32
}

func (g *ControllerObjectGuard) entries() []controllerObjectGuardEntry {
	entries := []controllerObjectGuardEntry{
		{
			name:          ControllerJobWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName),
			component:     controllerJobWriteGuardComponent,
			apiGroups:     []string{"batch"},
			apiVersions:   []string{"v1"},
			resource:      "jobs",
			operations:    []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
			denialMessage: "Ptah controller Job write guard rejected an unsafe workload shape",
			releaseValues: true,
		},
		{
			name:          ControllerChunkWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName),
			component:     controllerChunkWriteGuardComponent,
			apiGroups:     []string{""},
			apiVersions:   []string{"v1"},
			resource:      "configmaps",
			operations:    []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
			denialMessage: "Ptah controller chunk write guard rejected an unsafe ConfigMap shape",
		},
		{
			name:          ControllerPlanWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName),
			component:     controllerPlanWriteGuardComponent,
			apiGroups:     []string{"operator.ptah.run"},
			apiVersions:   []string{"v1alpha1"},
			resource:      "ptahschemaplans",
			operations:    []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
			denialMessage: "Ptah controller plan write guard rejected an unsafe manifest shape",
			releaseValues: true,
		},
		{
			name:          ControllerMigrationPlanWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName),
			component:     controllerMigrationPlanWriteGuardComponent,
			apiGroups:     []string{"operator.ptah.run"},
			apiVersions:   []string{"v1alpha1"},
			resource:      "ptahmigrationplans",
			operations:    []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
			denialMessage: "Ptah controller migration plan write guard rejected an unsafe manifest shape",
			releaseValues: true,
		},
	}
	entries[0].validations = controllerJobWriteValidations(entries[0].denialMessage)
	entries[1].validations = controllerChunkWriteValidations(entries[1].denialMessage)
	entries[2].validations = controllerPlanWriteValidations(entries[2].denialMessage)
	entries[3].validations = controllerMigrationPlanWriteValidations(entries[3].denialMessage)
	return entries
}

// AdmissionPolicy is one ValidatingAdmissionPolicy with the binding that
// enforces it.
type AdmissionPolicy struct {
	Policy  *admissionregistrationv1.ValidatingAdmissionPolicy
	Binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
}

// Policies returns the four object guards, each with its binding, in the
// order the chart renders them.
func (g *ControllerObjectGuard) Policies() []AdmissionPolicy {
	entries := g.entries()
	policies := make([]AdmissionPolicy, len(entries))
	for index, entry := range entries {
		policies[index] = AdmissionPolicy{Policy: g.policy(entry), Binding: g.binding(entry)}
	}
	return policies
}

// componentLabels is the one label the chart sets on a guard beside the
// release labels every object carries.
func componentLabels(component string) map[string]string {
	return map[string]string{"app.kubernetes.io/component": component}
}

func (g *ControllerObjectGuard) policy(entry controllerObjectGuardEntry) *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	var variables []admissionregistrationv1.Variable
	if entry.releaseValues {
		variables = controllerObjectReleaseVariables(g.ManagerImage, g.ControllerStateVersion)
	}
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: entry.name, Labels: componentLabels(entry.component)},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy:    &fail,
			MatchConstraints: g.matchResources(entry),
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name:       "controller-service-account",
				Expression: controllerPrincipalMatchExpression(g.ReleaseNamespace, g.ControllerServiceAccountName),
			}},
			Variables:   variables,
			Validations: entry.validations,
		},
	}
}

func (g *ControllerObjectGuard) binding(entry controllerObjectGuardEntry) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: entry.name, Labels: componentLabels(entry.component)},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        entry.name,
			MatchResources:    g.matchResources(entry),
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
}

// controllerObjectReleaseVariables carry the manager image and the
// controller-state version this release runs. A Job or a plan the controller
// creates names both, and one that names another manager is refused, so a
// manager left over from another release cannot create either. The values are
// CEL literals, written into the policy when the chart renders it; the
// generated template computes them with printf "%q" the way strconv.Quote
// does here.
func controllerObjectReleaseVariables(managerImage string, controllerStateVersion int32) []admissionregistrationv1.Variable {
	state := strconv.FormatInt(int64(controllerStateVersion), 10)
	return []admissionregistrationv1.Variable{
		{Name: "releaseControllerStateString", Expression: strconv.Quote(state)},
		{Name: "releaseControllerState", Expression: state},
		{Name: "releaseControllerImage", Expression: strconv.Quote(managerImage)},
	}
}

func (g *ControllerObjectGuard) matchResources(entry controllerObjectGuardEntry) *admissionregistrationv1.MatchResources {
	exact := admissionregistrationv1.Exact
	return &admissionregistrationv1.MatchResources{
		MatchPolicy:       &exact,
		NamespaceSelector: &metav1.LabelSelector{},
		ObjectSelector:    &metav1.LabelSelector{},
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
			RuleWithOperations: admissionregistrationv1.RuleWithOperations{
				Operations: entry.operations,
				Rule: admissionregistrationv1.Rule{
					APIGroups:   entry.apiGroups,
					APIVersions: entry.apiVersions,
					Resources:   []string{entry.resource},
					Scope:       scopePtr(admissionregistrationv1.NamespacedScope),
				},
			},
		}},
	}
}

// migrationJobShape derives the migration Job's sealed contract from the
// schema Job's by the exact substitutions that distinguish the two subjects.
//
// The two shapes are the same shape. Deriving one from the other is what keeps
// a change to the schema contract from silently leaving the migration contract
// weaker, which a second hand-written copy would do the first time someone
// edited only one of them. The generated template carries the derived
// expression, so the chart performs no substitution of its own.
var migrationJobShape = strings.NewReplacer(
	`"operator.ptah.run/schema"`, `"operator.ptah.run/migration"`,
	`"schema-operation"`, `"migration-operation"`,
	`["resolve", "verify", "observe", "plan", "apply"]`, `["resolve", "verify", "history", "apply"]`,
	`["observe", "plan"]`, `["history", "apply"]`,
	`"ptah-" + object.metadata.labels`, `"ptah-m-" + object.metadata.labels`,
	`^ptah-(resolve|verify|observe|plan|apply)-`, `^ptah-m-(resolve|verify|history|apply)-`,
	`"PtahSchema"`, `"PtahMigration"`,
	`"fetch-schema"`, `"fetch-migrations"`,
)

// eitherSubject admits a Job that satisfies the schema shape or the migration
// shape, and nothing else. Neither branch is weakened by the other: a Job that
// matches neither subject exactly is refused by both.
func eitherSubject(schemaExpression string) string {
	return "(" + schemaExpression + ") || (" + migrationJobShape.Replace(schemaExpression) + ")"
}

func controllerJobWriteValidations(message string) []admissionregistrationv1.Validation {
	validations := controllerObjectValidations(message,
		eitherSubject(`has(object.metadata.labels) && object.metadata.labels.size() >= 5 && object.metadata.labels.size() <= 21 && ["app.kubernetes.io/managed-by", "app.kubernetes.io/component", "operator.ptah.run/schema", "operator.ptah.run/operation", "operator.ptah.run/operation-id"].all(key, key in object.metadata.labels) && object.metadata.labels.all(key, key in ["app.kubernetes.io/managed-by", "app.kubernetes.io/component", "operator.ptah.run/schema", "operator.ptah.run/operation", "operator.ptah.run/operation-id"] || !(key.startsWith("ptah.run/") || key.contains(".ptah.run/") || key.startsWith("kubernetes.io/") || key.contains(".kubernetes.io/") || key.startsWith("k8s.io/") || key.contains(".k8s.io/") || key == "controller-uid" || key == "job-name")) && object.metadata.labels["app.kubernetes.io/managed-by"] == "ptah-operator" && object.metadata.labels["app.kubernetes.io/component"] == "schema-operation" && object.metadata.labels["operator.ptah.run/schema"] != "" && object.metadata.labels["operator.ptah.run/operation"] in ["resolve", "verify", "observe", "plan", "apply"] && object.metadata.labels["operator.ptah.run/operation-id"].matches("^[0-9a-f]{16}$") && object.metadata.name.startsWith("ptah-" + object.metadata.labels["operator.ptah.run/operation"] + "-") && object.metadata.name.matches("^ptah-(resolve|verify|observe|plan|apply)-[a-z0-9]([-a-z0-9.]*[a-z0-9])?-[0-9a-f]{16}$") && (!has(object.metadata.generateName) || object.metadata.generateName == "") && (!has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0) && !has(object.metadata.deletionTimestamp) && has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1 && object.metadata.ownerReferences[0].apiVersion == "operator.ptah.run/v1alpha1" && object.metadata.ownerReferences[0].kind == "PtahSchema" && object.metadata.ownerReferences[0].name == object.metadata.labels["operator.ptah.run/schema"] && object.metadata.ownerReferences[0].uid != "" && has(object.metadata.ownerReferences[0].controller) && object.metadata.ownerReferences[0].controller && has(object.metadata.ownerReferences[0].blockOwnerDeletion) && object.metadata.ownerReferences[0].blockOwnerDeletion`),
		controllerJobAnnotationContractExpression(),
		`has(dyn(object).spec.parallelism) && dyn(object).spec.parallelism == 1 && has(dyn(object).spec.completions) && dyn(object).spec.completions == 1 && has(dyn(object).spec.activeDeadlineSeconds) && dyn(object).spec.activeDeadlineSeconds >= 30 && dyn(object).spec.activeDeadlineSeconds <= 86400 && has(dyn(object).spec.backoffLimit) && dyn(object).spec.backoffLimit == 0 && !has(dyn(object).spec.backoffLimitPerIndex) && !has(dyn(object).spec.maxFailedIndexes) && has(dyn(object).spec.manualSelector) && !dyn(object).spec.manualSelector && has(dyn(object).spec.completionMode) && dyn(object).spec.completionMode == "NonIndexed" && has(dyn(object).spec.suspend) && !dyn(object).spec.suspend && has(dyn(object).spec.podReplacementPolicy) && dyn(object).spec.podReplacementPolicy == "Failed" && !has(dyn(object).spec.podFailurePolicy) && !has(dyn(object).spec.successPolicy) && !has(dyn(object).spec.managedBy) && ((request.operation == "CREATE" && !has(dyn(object).spec.ttlSecondsAfterFinished)) || (request.operation == "UPDATE" && has(dyn(object).spec.ttlSecondsAfterFinished) && dyn(object).spec.ttlSecondsAfterFinished == 300))`,
		eitherSubject(`has(dyn(object).spec.template.metadata.labels) && ["app.kubernetes.io/managed-by", "app.kubernetes.io/component", "operator.ptah.run/schema", "operator.ptah.run/operation", "operator.ptah.run/operation-id"].all(key, key in dyn(object).spec.template.metadata.labels && dyn(object).spec.template.metadata.labels[key] == object.metadata.labels[key]) && dyn(object).spec.template.metadata.labels.all(key, key in ["batch.kubernetes.io/controller-uid", "batch.kubernetes.io/job-name", "controller-uid", "job-name"] || (key in object.metadata.labels && dyn(object).spec.template.metadata.labels[key] == object.metadata.labels[key])) && object.metadata.labels.all(key, key in dyn(object).spec.template.metadata.labels) && has(dyn(object).spec.template.metadata.annotations) && dyn(object).spec.template.metadata.annotations == object.metadata.annotations && has(dyn(object).spec.template.spec.activeDeadlineSeconds) && dyn(object).spec.template.spec.activeDeadlineSeconds == dyn(object).spec.activeDeadlineSeconds && has(dyn(object).spec.template.spec.automountServiceAccountToken) && !dyn(object).spec.template.spec.automountServiceAccountToken && has(dyn(object).spec.template.spec.enableServiceLinks) && !dyn(object).spec.template.spec.enableServiceLinks && has(dyn(object).spec.template.spec.serviceAccountName) && dyn(object).spec.template.spec.serviceAccountName != "" && dyn(object).spec.template.spec.restartPolicy == "Never" && (!has(dyn(object).spec.template.spec.hostNetwork) || !dyn(object).spec.template.spec.hostNetwork) && (!has(dyn(object).spec.template.spec.hostPID) || !dyn(object).spec.template.spec.hostPID) && (!has(dyn(object).spec.template.spec.hostIPC) || !dyn(object).spec.template.spec.hostIPC) && (!has(dyn(object).spec.template.spec.shareProcessNamespace) || !dyn(object).spec.template.spec.shareProcessNamespace) && dyn(object).spec.template.spec.containers.size() == 1 && dyn(object).spec.template.spec.containers[0].name == "ptah" && ((object.metadata.labels["operator.ptah.run/operation"] in ["observe", "plan"] && dyn(object).spec.template.spec.initContainers.size() == 3) || (!(object.metadata.labels["operator.ptah.run/operation"] in ["observe", "plan"]) && dyn(object).spec.template.spec.initContainers.size() == 1)) && dyn(object).spec.template.spec.containers.all(container, has(container.securityContext) && has(container.securityContext.allowPrivilegeEscalation) && !container.securityContext.allowPrivilegeEscalation && (!has(container.securityContext.privileged) || !container.securityContext.privileged) && has(container.securityContext.readOnlyRootFilesystem) && container.securityContext.readOnlyRootFilesystem && has(container.securityContext.runAsNonRoot) && container.securityContext.runAsNonRoot && has(container.securityContext.capabilities) && container.securityContext.capabilities.drop == ["ALL"]) && dyn(object).spec.template.spec.initContainers.all(container, has(container.securityContext) && has(container.securityContext.allowPrivilegeEscalation) && !container.securityContext.allowPrivilegeEscalation && (!has(container.securityContext.privileged) || !container.securityContext.privileged) && has(container.securityContext.readOnlyRootFilesystem) && container.securityContext.readOnlyRootFilesystem && has(container.securityContext.runAsNonRoot) && container.securityContext.runAsNonRoot && has(container.securityContext.capabilities) && container.securityContext.capabilities.drop == ["ALL"])`),
		eitherSubject(controllerJobPodBoundaryExpression),
		controllerJobSupportedWindowTopLevelExpression("object"),
		controllerJobSupportedWindowVolumeExpression("object"),
		controllerJobSupportedWindowProjectionExpression("object"),
		controllerJobSupportedWindowMountExpression("object"),
		controllerJobSupportedWindowContainerExpression("object"),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowTopLevelExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowVolumeExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowProjectionExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowMountExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowContainerExpression("oldObject")),
		`request.operation != "CREATE" || !has(dyn(object).status) || dyn(dyn(object).status).size() == 0`,
		// The cleanup TTL is the one field the manager may add to a Job it
		// already created, and normally only once that Job is terminal.
		//
		// A schema Apply is the exception, because the manager has one case
		// where it must abandon a Job that is still running: losing database
		// lock continuity during an Apply retires the operation immediately,
		// and the Job it leaves behind would otherwise keep its whole deadline
		// with nothing left to schedule its collection. Setting the TTL then
		// changes no timing -- Kubernetes starts that timer when the Job
		// finishes either way -- so the field is permitted early for that
		// shape and for no other. The validating webhook is what decides that
		// this Apply is the one the schema claimed, by name, UID, annotations
		// and the pending-observation fence; this layer only says which shape
		// may ask.
		`request.operation != "UPDATE" || (oldObject != null && !has(dyn(oldObject).spec.ttlSecondsAfterFinished) && has(dyn(object).spec.ttlSecondsAfterFinished) && dyn(object).spec.ttlSecondsAfterFinished == 300 && ((has(dyn(oldObject).status.conditions) && dyn(oldObject).status.conditions.exists(condition, condition.status == "True" && condition.type in ["Complete", "Failed"])) || (has(object.metadata.labels) && object.metadata.labels["operator.ptah.run/operation"] == "apply" && object.metadata.labels["app.kubernetes.io/component"] == "schema-operation")) && object.metadata.name == oldObject.metadata.name && object.metadata.namespace == oldObject.metadata.namespace && object.metadata.uid == oldObject.metadata.uid && object.metadata.labels == oldObject.metadata.labels && object.metadata.annotations == oldObject.metadata.annotations && object.metadata.ownerReferences == oldObject.metadata.ownerReferences && has(object.metadata.finalizers) == has(oldObject.metadata.finalizers) && (!has(object.metadata.finalizers) || object.metadata.finalizers == oldObject.metadata.finalizers) && has(object.metadata.generateName) == has(oldObject.metadata.generateName) && (!has(object.metadata.generateName) || object.metadata.generateName == oldObject.metadata.generateName) && has(object.metadata.deletionTimestamp) == has(oldObject.metadata.deletionTimestamp) && (!has(object.metadata.deletionTimestamp) || object.metadata.deletionTimestamp == oldObject.metadata.deletionTimestamp) && has(dyn(object).status) == has(dyn(oldObject).status) && (!has(dyn(object).status) || dyn(object).status == dyn(oldObject).status) && dyn(object).spec.parallelism == dyn(oldObject).spec.parallelism && dyn(object).spec.completions == dyn(oldObject).spec.completions && dyn(object).spec.activeDeadlineSeconds == dyn(oldObject).spec.activeDeadlineSeconds && has(dyn(object).spec.podFailurePolicy) == has(dyn(oldObject).spec.podFailurePolicy) && (!has(dyn(object).spec.podFailurePolicy) || dyn(object).spec.podFailurePolicy == dyn(oldObject).spec.podFailurePolicy) && has(dyn(object).spec.successPolicy) == has(dyn(oldObject).spec.successPolicy) && (!has(dyn(object).spec.successPolicy) || dyn(object).spec.successPolicy == dyn(oldObject).spec.successPolicy) && dyn(object).spec.backoffLimit == dyn(oldObject).spec.backoffLimit && has(dyn(object).spec.backoffLimitPerIndex) == has(dyn(oldObject).spec.backoffLimitPerIndex) && (!has(dyn(object).spec.backoffLimitPerIndex) || dyn(object).spec.backoffLimitPerIndex == dyn(oldObject).spec.backoffLimitPerIndex) && has(dyn(object).spec.maxFailedIndexes) == has(dyn(oldObject).spec.maxFailedIndexes) && (!has(dyn(object).spec.maxFailedIndexes) || dyn(object).spec.maxFailedIndexes == dyn(oldObject).spec.maxFailedIndexes) && has(dyn(object).spec.selector) == has(dyn(oldObject).spec.selector) && (!has(dyn(object).spec.selector) || dyn(object).spec.selector == dyn(oldObject).spec.selector) && dyn(object).spec.manualSelector == dyn(oldObject).spec.manualSelector && dyn(object).spec.template == dyn(oldObject).spec.template && dyn(object).spec.completionMode == dyn(oldObject).spec.completionMode && dyn(object).spec.suspend == dyn(oldObject).spec.suspend && dyn(object).spec.podReplacementPolicy == dyn(oldObject).spec.podReplacementPolicy && has(dyn(object).spec.managedBy) == has(dyn(oldObject).spec.managedBy) && (!has(dyn(object).spec.managedBy) || dyn(object).spec.managedBy == dyn(oldObject).spec.managedBy))`,
	)
	return validations
}

// controllerJobAnnotationContractExpression admits the one Job annotation
// envelope the workload builder writes. A create must carry this release's
// controller identity. An update, which can only be the cleanup
// TTL, may carry any valid identity, because a Job can outlive the release
// that created it.
func controllerJobAnnotationContractExpression() string {
	current := `has(object.metadata.annotations) && object.metadata.annotations.size() <= 27 && ["operator.ptah.run/operation-id", "operator.ptah.run/input-fingerprint", "operator.ptah.run/ptah-version", "operator.ptah.run/execution-binding-id", "operator.ptah.run/controller-image", "operator.ptah.run/controller-revision", "operator.ptah.run/controller-state-version", "operator.ptah.run/admission-snapshot-digest"].all(key, key in object.metadata.annotations) && object.metadata.annotations.all(key, key in ["operator.ptah.run/operation-id", "operator.ptah.run/input-fingerprint", "operator.ptah.run/ptah-version", "operator.ptah.run/execution-binding-id", "operator.ptah.run/controller-image", "operator.ptah.run/controller-revision", "operator.ptah.run/controller-state-version", "operator.ptah.run/admission-snapshot-digest", "operator.ptah.run/plan-fingerprint", "operator.ptah.run/plan-content-digest", "cluster-autoscaler.kubernetes.io/safe-to-evict"] || !(key.startsWith("ptah.run/") || key.contains(".ptah.run/") || key.startsWith("kubernetes.io/") || key.contains(".kubernetes.io/") || key.startsWith("k8s.io/") || key.contains(".k8s.io/") || key == "controller-uid" || key == "job-name")) && object.metadata.annotations["operator.ptah.run/operation-id"] != "" && object.metadata.annotations["operator.ptah.run/input-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.run/ptah-version"] != "" && object.metadata.annotations["operator.ptah.run/execution-binding-id"].matches("^v1-[0-9a-f]{32}$") && object.metadata.annotations["operator.ptah.run/controller-image"].matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.run/controller-revision"] != "" && object.metadata.annotations["operator.ptah.run/controller-state-version"].matches("^[1-9][0-9]*$") && object.metadata.annotations["operator.ptah.run/admission-snapshot-digest"].matches("^sha256:[0-9a-f]{64}$") && ((object.metadata.labels["operator.ptah.run/operation"] == "apply" && object.metadata.labels["app.kubernetes.io/component"] == "schema-operation" && "operator.ptah.run/plan-fingerprint" in object.metadata.annotations && object.metadata.annotations["operator.ptah.run/plan-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && "operator.ptah.run/plan-content-digest" in object.metadata.annotations && object.metadata.annotations["operator.ptah.run/plan-content-digest"].matches("^sha256:[0-9a-f]{64}$")) || ((object.metadata.labels["operator.ptah.run/operation"] != "apply" || object.metadata.labels["app.kubernetes.io/component"] == "migration-operation") && !("operator.ptah.run/plan-fingerprint" in object.metadata.annotations) && !("operator.ptah.run/plan-content-digest" in object.metadata.annotations))) && ((object.metadata.labels["operator.ptah.run/operation"] == "apply" && "cluster-autoscaler.kubernetes.io/safe-to-evict" in object.metadata.annotations && object.metadata.annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"] == "false") || (object.metadata.labels["operator.ptah.run/operation"] != "apply" && !("cluster-autoscaler.kubernetes.io/safe-to-evict" in object.metadata.annotations)))`
	activeIdentity := `object.metadata.annotations["operator.ptah.run/controller-image"] == variables.releaseControllerImage && object.metadata.annotations["operator.ptah.run/controller-state-version"] == variables.releaseControllerStateString`
	return fmt.Sprintf(
		`(%s) && (request.operation == "UPDATE" || (request.operation == "CREATE" && (%s)))`,
		current,
		activeIdentity,
	)
}

const controllerJobPodBoundaryExpression = `has(dyn(object).spec.template.spec.securityContext) && has(dyn(object).spec.template.spec.securityContext.runAsNonRoot) && dyn(object).spec.template.spec.securityContext.runAsNonRoot && has(dyn(object).spec.template.spec.securityContext.runAsUser) && dyn(object).spec.template.spec.securityContext.runAsUser == 65532 && has(dyn(object).spec.template.spec.securityContext.runAsGroup) && dyn(object).spec.template.spec.securityContext.runAsGroup == 65532 && has(dyn(object).spec.template.spec.securityContext.fsGroup) && dyn(object).spec.template.spec.securityContext.fsGroup == 65532 && has(dyn(object).spec.template.spec.securityContext.fsGroupChangePolicy) && dyn(object).spec.template.spec.securityContext.fsGroupChangePolicy == "OnRootMismatch" && has(dyn(object).spec.template.spec.securityContext.seccompProfile) && dyn(object).spec.template.spec.securityContext.seccompProfile.type == "RuntimeDefault" && (!has(dyn(object).spec.template.spec.securityContext.sysctls) || dyn(object).spec.template.spec.securityContext.sysctls.size() == 0) && dyn(object).spec.template.spec.volumes.size() >= 2 && dyn(object).spec.template.spec.volumes.size() <= 7 && dyn(object).spec.template.spec.volumes.all(volume, (volume.name in ["runner", "work", "schema-source", "fetch-work", "registry-ca-snapshot"] && has(volume.emptyDir)) || (volume.name in ["verification-policy", "registry-ca"] && has(volume.configMap)) || (volume.name == "registry-docker-config" && has(volume.secret)) || (volume.name == "plan" && has(volume.projected) && volume.projected.sources.size() >= 1 && volume.projected.sources.all(source, has(source.configMap)))) && dyn(object).spec.template.spec.containers.all(container, container.image.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && container.imagePullPolicy == "IfNotPresent" && (!has(container.envFrom) || container.envFrom.size() == 0) && (!has(container.volumeDevices) || container.volumeDevices.size() == 0) && has(container.securityContext.runAsUser) && container.securityContext.runAsUser == 65532 && has(container.securityContext.runAsGroup) && container.securityContext.runAsGroup == 65532 && has(container.securityContext.seccompProfile) && container.securityContext.seccompProfile.type == "RuntimeDefault" && (!has(container.securityContext.capabilities.add) || container.securityContext.capabilities.add.size() == 0)) && dyn(object).spec.template.spec.initContainers.all(container, container.image.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && container.imagePullPolicy == "IfNotPresent" && (!has(container.envFrom) || container.envFrom.size() == 0) && (!has(container.volumeDevices) || container.volumeDevices.size() == 0) && has(container.securityContext.runAsUser) && container.securityContext.runAsUser == 65532 && has(container.securityContext.runAsGroup) && container.securityContext.runAsGroup == 65532 && has(container.securityContext.seccompProfile) && container.securityContext.seccompProfile.type == "RuntimeDefault" && (!has(container.securityContext.capabilities.add) || container.securityContext.capabilities.add.size() == 0)) && ((object.metadata.labels["operator.ptah.run/operation"] in ["observe", "plan"] && dyn(object).spec.template.spec.initContainers.map(container, container.name) == ["install-runner", "validate-source-authority", "fetch-schema"]) || (!(object.metadata.labels["operator.ptah.run/operation"] in ["observe", "plan"]) && dyn(object).spec.template.spec.initContainers.map(container, container.name) == ["install-runner"]))`

func controllerJobSupportedWindowTopLevelExpression(root string) string {
	return fmt.Sprintf(
		`!has(dyn(dyn(%[1]s).spec).scheduling) && !has(dyn(dyn(%[1]s).spec.template.spec).evictionResponders) && !has(dyn(dyn(%[1]s).spec.template.spec).workloadRef)`,
		root,
	)
}

func controllerJobSupportedWindowVolumeExpression(root string) string {
	return fmt.Sprintf(
		`dyn(%[1]s).spec.template.spec.volumes.all(volume, (!has(volume.emptyDir) || !has(dyn(volume.emptyDir).mode)) && (!has(volume.configMap) || (!has(dyn(volume.configMap).defaultUser) && (!has(volume.configMap.items) || volume.configMap.items.all(item, !has(dyn(item).user))))) && (!has(volume.secret) || (!has(dyn(volume.secret).defaultUser) && (!has(volume.secret.items) || volume.secret.items.all(item, !has(dyn(item).user))))) && (!has(volume.projected) || !has(dyn(volume.projected).defaultUser)) && (!has(volume.downwardAPI) || (!has(dyn(volume.downwardAPI).defaultUser) && (!has(volume.downwardAPI.items) || volume.downwardAPI.items.all(item, !has(dyn(item).user))))))`,
		root,
	)
}

func controllerJobSupportedWindowProjectionExpression(root string) string {
	return fmt.Sprintf(
		`dyn(%[1]s).spec.template.spec.volumes.all(volume, !has(volume.projected) || volume.projected.sources.all(source, (!has(source.configMap) || !has(source.configMap.items) || source.configMap.items.all(item, !has(dyn(item).user))) && (!has(source.secret) || !has(source.secret.items) || source.secret.items.all(item, !has(dyn(item).user))) && (!has(source.serviceAccountToken) || !has(dyn(source.serviceAccountToken).user)) && (!has(source.clusterTrustBundle) || !has(dyn(source.clusterTrustBundle).user)) && (!has(source.podCertificate) || !has(dyn(source.podCertificate).user)) && (!has(source.downwardAPI) || !has(source.downwardAPI.items) || source.downwardAPI.items.all(item, !has(dyn(item).user)))))`,
		root,
	)
}

func controllerJobSupportedWindowMountExpression(root string) string {
	return fmt.Sprintf(
		`dyn(%[1]s).spec.template.spec.containers.all(container, !has(container.volumeMounts) || container.volumeMounts.all(mount, !has(dyn(mount).bindMountOptions))) && dyn(%[1]s).spec.template.spec.initContainers.all(container, !has(container.volumeMounts) || container.volumeMounts.all(mount, !has(dyn(mount).bindMountOptions)))`,
		root,
	)
}

func controllerJobSupportedWindowContainerExpression(root string) string {
	return fmt.Sprintf(
		`(!has(dyn(%[1]s).spec.template.spec.ephemeralContainers) || dyn(%[1]s).spec.template.spec.ephemeralContainers.size() == 0) && dyn(%[1]s).spec.template.spec.containers.all(container, !has(container.lifecycle) && !has(container.livenessProbe) && !has(container.readinessProbe) && !has(container.startupProbe)) && dyn(%[1]s).spec.template.spec.initContainers.all(container, !has(container.lifecycle) && !has(container.livenessProbe) && !has(container.readinessProbe) && !has(container.startupProbe))`,
		root,
	)
}

func controllerJobPreviousObjectExpression(expression string) string {
	return `request.operation != "UPDATE" || (oldObject != null && ` + expression + `)`
}

// controllerChunkWriteValidations bounds the ConfigMap the controller may
// write for one plan chunk.
//
// The size ceiling is the base64 length of plancontract.ChunkBytes, because a
// policy sees binaryData as the encoded string the API server transports, and
// that is also the length counted against the 1 MiB object limit this bound
// exists to keep. There is no decoder to compare raw bytes with: the API
// server's CEL environment does not enable the encoder extension. The raw-byte
// contract is enforced where the value is already an integer, on
// spec.chunks[].size below and in the controller write webhook.
func controllerChunkWriteValidations(message string) []admissionregistrationv1.Validation {
	return controllerObjectValidations(message,
		`has(object.metadata.labels) && object.metadata.labels.size() == 2 && ["operator.ptah.run/plan", "operator.ptah.run/schema"].all(key, key in object.metadata.labels && object.metadata.labels[key] != "") && object.metadata.name.matches("^ptah-plan-[0-9a-f]{24}-[0-9]{3}$") && object.metadata.name.startsWith(object.metadata.labels["operator.ptah.run/plan"] + "-") && (!has(object.metadata.annotations) || object.metadata.annotations.size() == 0) && (!has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0) && (!has(object.metadata.generateName) || object.metadata.generateName == "") && !has(object.metadata.deletionTimestamp) && has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1 && object.metadata.ownerReferences[0].apiVersion == "operator.ptah.run/v1alpha1" && object.metadata.ownerReferences[0].kind == "PtahSchemaPlan" && object.metadata.ownerReferences[0].name == object.metadata.labels["operator.ptah.run/plan"] && object.metadata.ownerReferences[0].uid != "" && has(object.metadata.ownerReferences[0].controller) && object.metadata.ownerReferences[0].controller && has(object.metadata.ownerReferences[0].blockOwnerDeletion) && object.metadata.ownerReferences[0].blockOwnerDeletion`,
		`has(dyn(object).immutable) && dyn(object).immutable && (!has(dyn(object).data) || dyn(object).data.size() == 0) && has(dyn(object).binaryData) && dyn(object).binaryData.size() == 1 && "chunk" in dyn(object).binaryData && dyn(object).binaryData["chunk"].size() >= 1 && dyn(object).binaryData["chunk"].size() <= 699052`,
	)
}

func controllerPlanWriteValidations(message string) []admissionregistrationv1.Validation {
	// Fields are read through dyn, so the policy does not depend on the CRD
	// revision the API server type-checks it against.
	validations := controllerObjectValidations(message,
		`has(object.metadata.labels) && object.metadata.labels.size() == 1 && "operator.ptah.run/schema" in object.metadata.labels && object.metadata.labels["operator.ptah.run/schema"] != "" && object.metadata.name.matches("^ptah-plan-[0-9a-f]{24}$") && (!has(object.metadata.annotations) || object.metadata.annotations.size() == 0) && (!has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0) && (!has(object.metadata.generateName) || object.metadata.generateName == "") && !has(object.metadata.deletionTimestamp) && has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1 && object.metadata.ownerReferences[0].apiVersion == "operator.ptah.run/v1alpha1" && object.metadata.ownerReferences[0].kind == "PtahSchema" && object.metadata.ownerReferences[0].name == object.metadata.labels["operator.ptah.run/schema"] && object.metadata.ownerReferences[0].uid != "" && has(object.metadata.ownerReferences[0].controller) && object.metadata.ownerReferences[0].controller && has(object.metadata.ownerReferences[0].blockOwnerDeletion) && object.metadata.ownerReferences[0].blockOwnerDeletion && dyn(object).spec.schemaRef.name == object.metadata.labels["operator.ptah.run/schema"] && dyn(object).spec.schemaRef.uid == object.metadata.ownerReferences[0].uid`,
		controllerPlanContractExpression(),
		`dyn(object).spec.chunks.size() >= 1 && dyn(object).spec.chunks.size() <= 16 && dyn(object).spec.chunks.all(chunk, chunk.key == "chunk" && chunk.name.matches("^ptah-plan-[0-9a-f]{24}-[0-9]{3}$") && chunk.name.startsWith(object.metadata.name + "-") && chunk.index >= 0 && chunk.index < dyn(object).spec.chunks.size() && chunk.digest.matches("^sha256:[0-9a-f]{64}$") && chunk.size >= 1 && chunk.size <= 524288)`,
		`!has(dyn(object).status)`,
	)
	return validations
}

// controllerMigrationPlanWriteValidations bounds the manifest the controller
// may publish for a migration.
//
// A migration plan is a version sequence with per-migration checksums and the
// history it was computed against, so the contract checks those rather than the
// schema plan's chunk projection: there is no chunk store behind it, because
// the statements are in the artifact rather than in the plan.
func controllerMigrationPlanWriteValidations(message string) []admissionregistrationv1.Validation {
	return controllerObjectValidations(message,
		`has(object.metadata.labels) && object.metadata.labels.size() == 1 && "operator.ptah.run/migration" in object.metadata.labels && object.metadata.labels["operator.ptah.run/migration"] != "" && object.metadata.name.matches("^ptah-mplan-[0-9a-f]{24}$") && (!has(object.metadata.annotations) || object.metadata.annotations.size() == 0) && (!has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0) && (!has(object.metadata.generateName) || object.metadata.generateName == "") && !has(object.metadata.deletionTimestamp) && has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1 && object.metadata.ownerReferences[0].apiVersion == "operator.ptah.run/v1alpha1" && object.metadata.ownerReferences[0].kind == "PtahMigration" && object.metadata.ownerReferences[0].name == object.metadata.labels["operator.ptah.run/migration"] && object.metadata.ownerReferences[0].uid != "" && has(object.metadata.ownerReferences[0].controller) && object.metadata.ownerReferences[0].controller && has(object.metadata.ownerReferences[0].blockOwnerDeletion) && object.metadata.ownerReferences[0].blockOwnerDeletion && dyn(object).spec.migrationRef.name == object.metadata.labels["operator.ptah.run/migration"] && dyn(object).spec.migrationRef.uid == object.metadata.ownerReferences[0].uid`,
		`dyn(object).spec.contractVersion == 1 && dyn(object).spec.fingerprint.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.historyFingerprint.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.artifactDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.coordinationDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.targetIdentityDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.policyFingerprint.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.verificationPolicyUID != "" && dyn(object).spec.verificationPolicyDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.currentVersion >= 0 && dyn(object).spec.executionBindingID.matches("^v1-[0-9a-f]{32}$") && dyn(object).spec.controllerImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && dyn(object).spec.controllerImage == variables.releaseControllerImage && dyn(object).spec.controllerRevision != "" && dyn(object).spec.controllerStateVersion >= 1 && dyn(object).spec.controllerStateVersion == variables.releaseControllerState && dyn(object).spec.ptahVersion != "" && dyn(object).spec.executorImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && dyn(object).spec.runnerImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && dyn(object).spec.runnerProtocolVersion >= 1`,
		`dyn(object).spec.migrations.size() >= 1 && dyn(object).spec.migrations.size() <= 256 && dyn(object).spec.migrations.all(migration, migration.version >= 1 && migration.checksum != "" && migration.checksum.size() <= 128 && (!has(migration.transactionMode) || migration.transactionMode in ["file", "none"]))`,
		`!has(dyn(object).status)`,
	)
}

func controllerPlanContractExpression() string {
	common := `dyn(object).spec.fingerprint.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.contentDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.artifactDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.coordinationDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.targetIdentityDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.actualStateFingerprint.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.desiredStateFingerprint.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.policyFingerprint.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.verificationPolicyUID != "" && dyn(object).spec.verificationPolicyDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.executionBindingID.matches("^v1-[0-9a-f]{32}$") && dyn(object).spec.ptahVersion != "" && dyn(object).spec.executorImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && dyn(object).spec.runnerImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && dyn(object).spec.runnerProtocolVersion >= 1 && dyn(object).spec.dialect != "" && dyn(object).spec.statementCount >= 1 && dyn(object).spec.size >= 1 && dyn(object).spec.size <= 8388608`
	current := `dyn(object).spec.contractVersion == 3 && has(dyn(dyn(object).spec).controllerImage) && dyn(dyn(object).spec).controllerImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && dyn(dyn(object).spec).controllerImage == variables.releaseControllerImage && has(dyn(dyn(object).spec).controllerRevision) && dyn(dyn(object).spec).controllerRevision != "" && has(dyn(dyn(object).spec).controllerStateVersion) && dyn(dyn(object).spec).controllerStateVersion >= 1 && dyn(dyn(object).spec).controllerStateVersion == variables.releaseControllerState`
	return fmt.Sprintf(`(%s) && (%s)`, common, current)
}

func controllerObjectValidations(message string, expressions ...string) []admissionregistrationv1.Validation {
	validations := make([]admissionregistrationv1.Validation, len(expressions))
	for index, expression := range expressions {
		validations[index] = admissionregistrationv1.Validation{Expression: expression, Message: message}
	}
	return validations
}
