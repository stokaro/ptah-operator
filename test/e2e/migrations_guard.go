package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The readings of the apply-policy guard, existing-schema adoption and
// checkpoint bootstrap rows of the migration phase.

// decodeManifests reads every document of a YAML or JSON manifest file, as
// kubectl create --dry-run=client -o json did, and flattens a List into its
// items. An empty document is skipped.
func decodeManifests(content []byte) ([]map[string]any, error) {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(content), 4096)
	var documents []map[string]any
	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return documents, nil
		}
		if err != nil {
			return nil, err
		}
		if len(document) == 0 {
			continue
		}
		if document["kind"] == "List" {
			items, _ := document["items"].([]any)
			for _, item := range items {
				object, ok := item.(map[string]any)
				if !ok {
					return nil, errors.New("a List item is not an object")
				}
				documents = append(documents, object)
			}
			continue
		}
		documents = append(documents, document)
	}
}

// guardAuthorGrantPending names only the authorizer's refusal of the exact
// initial write. Admission and other failures must not become RBAC retries.
func guardAuthorGrantPending(message, user, namespace string) bool {
	denial := fmt.Sprintf(`ptahmigrations.operator.ptah.run is forbidden: User %q cannot create resource "ptahmigrations" in API group "operator.ptah.run" in the namespace %q`, user, namespace)
	return strings.HasSuffix(strings.TrimSpace(message), denial)
}

// guardAuthorRole is the example desired-state author Role and RoleBinding,
// moved into the namespace, renamed for the engine, and bound to the group
// given: the starting point a reader copies rather than a Role written for
// the test. Anything but one Role and one RoleBinding is refused.
func guardAuthorRole(documents []map[string]any, namespace, suffix, group string) ([]map[string]any, error) {
	var kinds []string
	for _, document := range documents {
		kind, _ := document["kind"].(string)
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"Role", "RoleBinding"}) {
		return nil, errors.New("the author example is not a Role and a RoleBinding")
	}
	name := "e2e-desired-state-author-" + suffix
	adapted := make([]map[string]any, 0, len(documents))
	for _, document := range documents {
		object := deepCopyMap(document)
		metadata, _ := object["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
			object["metadata"] = metadata
		}
		metadata["namespace"], metadata["name"] = namespace, name
		if object["kind"] == "RoleBinding" {
			roleRef, _ := object["roleRef"].(map[string]any)
			if roleRef == nil {
				roleRef = map[string]any{}
				object["roleRef"] = roleRef
			}
			roleRef["name"] = name
			object["subjects"] = []any{map[string]any{
				"apiGroup": "rbac.authorization.k8s.io", "kind": "Group", "name": group,
			}}
		}
		adapted = append(adapted, object)
	}
	return adapted, nil
}

// guardApproverRole is the approver's Role and RoleBinding: approvals, and
// the plans and resource an approval names, and nothing that edits desired
// state.
func guardApproverRole(namespace, name, group string) []map[string]any {
	return []map[string]any{
		{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
			"metadata": map[string]any{"namespace": namespace, "name": name},
			"rules": []any{
				map[string]any{
					"apiGroups": []any{"operator.ptah.run"}, "resources": []any{"ptahmigrationapprovals"},
					"verbs": []any{"get", "create"},
				},
				map[string]any{
					"apiGroups": []any{"operator.ptah.run"}, "resources": []any{"ptahmigrationplans", "ptahmigrations"},
					"verbs": []any{"get", "list"},
				},
			},
		},
		{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
			"metadata": map[string]any{"namespace": namespace, "name": name},
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": name},
			"subjects": []any{map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Group", "name": group}},
		},
	}
}

// guardAdministratorGroup is the group the apply-policy guard exempts that
// the harness identity carries: its first group other than
// system:authenticated. An identity with none has nothing the guard exempts.
func guardAdministratorGroup(groups []string) (string, error) {
	for _, group := range groups {
		if group != "system:authenticated" {
			return group, nil
		}
	}
	return "", errors.New("the harness identity carries no group the apply-policy guard exempts")
}

// applyPolicyGuardInstalled is the release's own guard: exactly one policy,
// whose one exemptGroups variable names the group as a JSON string literal,
// and exactly one binding, bound to Deny alone. A policy with no binding, or
// one bound to Warn, reads exactly like one in force, so the binding is read
// too.
func applyPolicyGuardInstalled(policies []admissionregistrationv1.ValidatingAdmissionPolicy,
	bindings []admissionregistrationv1.ValidatingAdmissionPolicyBinding, group string,
) error {
	if len(policies) != 1 || len(bindings) != 1 {
		return fmt.Errorf("%d policies and %d bindings carry the apply-policy-guard component", len(policies), len(bindings))
	}
	literal := `"` + jsonStringBody(group) + `"`
	var exempts []bool
	for _, variable := range policies[0].Spec.Variables {
		if variable.Name == "exemptGroups" {
			exempts = append(exempts, strings.Contains(variable.Expression, literal))
		}
	}
	if !slices.Equal(exempts, []bool{true}) {
		return fmt.Errorf("the policy's exemptGroups do not name %s", literal)
	}
	actions := bindings[0].Spec.ValidationActions
	if len(actions) != 1 || actions[0] != admissionregistrationv1.Deny {
		return fmt.Errorf("the binding's validation actions are %v, not Deny", actions)
	}
	return nil
}

// guardPlanAwaiting is the guard migration waiting at its approval gate with
// a plan published.
func guardPlanAwaiting(migration *ptahv1alpha1.PtahMigration) bool {
	return migration.Status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval &&
		migration.Status.Plan != nil && migration.Status.Plan.Name != ""
}

// guardPlanApplied is the guard migration in sync after an Applied run.
func guardPlanApplied(migration *ptahv1alpha1.PtahMigration) bool {
	return migration.Status.Phase == ptahv1alpha1.MigrationPhaseInSync &&
		migration.Status.LastRun != nil && migration.Status.LastRun.Outcome == ptahv1alpha1.MigrationRunOutcomeApplied
}

// guardConvergedUnderAlways is the guard migration in sync with Always in
// place, having observed the generation the author's edit produced.
func guardConvergedUnderAlways(migration *ptahv1alpha1.PtahMigration, generation int64) bool {
	return migration.Spec.Policy.Apply == ptahv1alpha1.ApplyPolicyAlways &&
		migration.Status.ObservedGeneration >= generation &&
		migration.Status.Phase == ptahv1alpha1.MigrationPhaseInSync
}

// approvalNamesApprover is an approval stamped with the approver who made it
// and the group the approver carried.
func approvalNamesApprover(approval *ptahv1alpha1.PtahMigrationApproval, approver, group string) bool {
	return approval.Spec.Approver.Username == approver && slices.Contains(approval.Spec.Approver.Groups, group)
}

// existingSchemaHeld is the adoption row itself: the operator reads an empty
// history from a database that is not empty, and holds the whole sequence at
// the approval gate instead of recording any part of it as applied.
func existingSchemaHeld(migration *ptahv1alpha1.PtahMigration) bool {
	status := migration.Status
	history := status.History
	if history == nil {
		history = &ptahv1alpha1.MigrationHistoryStatus{}
	}
	return status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval &&
		history.CurrentVersion == 0 && history.AppliedCount == 0 && history.PendingCount == 3 && !history.Dirty &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue, "AwaitingApproval") &&
		conditionStatus(status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionFalse) &&
		status.LastRun == nil
}

// adoptedHistorySettled is the migration once a person recorded the history:
// in sync on the three migrations without a run of its own.
func adoptedHistorySettled(migration *ptahv1alpha1.PtahMigration) bool {
	status := migration.Status
	history := status.History
	return status.Phase == ptahv1alpha1.MigrationPhaseInSync && history != nil &&
		history.CurrentVersion == 3 && history.AppliedCount == 3 && history.PendingCount == 0 &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue, "HistoryMatched") &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionFalse, "HistoryMatched") &&
		status.LastRun == nil
}

// adopterIsolated is the adoption Job as it was created: one container, whose
// every Secret reference is the adoption database's and nothing else, so it
// reaches the database and no registry.
func adopterIsolated(job *batchv1.Job, secret string) bool {
	containers := job.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		return false
	}
	var names []string
	for _, variable := range containers[0].Env {
		if reference := envSecretRef(variable); reference != nil {
			names = append(names, reference.Name)
		}
	}
	slices.Sort(names)
	return slices.Equal(slices.Compact(names), []string{secret})
}

// checkpointGate is a fresh database started at checkpoint 3: the two
// migrations the checkpoint carries accounted for rather than pending, and
// the checkpoint and the migration after it waiting for a person.
func checkpointGate(migration *ptahv1alpha1.PtahMigration) bool {
	status := migration.Status
	history := status.History
	return status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval && history != nil &&
		history.CurrentVersion == 0 && history.CheckpointVersion == 3 && history.AppliedCount == 2 &&
		history.PendingCount == 2 && !history.Dirty && len(history.ModifiedVersions) == 0 &&
		len(history.OutOfOrderVersions) == 0 &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue, "AwaitingApproval")
}

// checkpointPlanVersions is the plan a checkpoint gate asks a person to
// approve: the checkpoint and the migration after it.
func checkpointPlanVersions(plan *ptahv1alpha1.PtahMigrationPlan) bool {
	var versions []int64
	for _, planned := range plan.Spec.Migrations {
		versions = append(versions, planned.Version)
	}
	return slices.Equal(versions, []int64{3, 4})
}

// checkpointSettled is the bootstrapped migration after its run: in sync at
// version 4, the checkpoint still in the reading because it goes on
// describing what it replaced, and the run that applied 3 and 4.
func checkpointSettled(migration *ptahv1alpha1.PtahMigration) bool {
	status := migration.Status
	history, run := status.History, status.LastRun
	return status.Phase == ptahv1alpha1.MigrationPhaseInSync && history != nil &&
		history.CurrentVersion == 4 && history.PendingCount == 0 && !history.Dirty && status.Plan == nil &&
		history.CheckpointVersion == 3 && run != nil && run.Outcome == ptahv1alpha1.MigrationRunOutcomeApplied &&
		slices.Equal(run.AppliedVersions, []int64{3, 4}) &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue, "HistoryMatched")
}

// checkpointNotReapproving is a bootstrapped migration that asks for nothing
// on a later pass: no plan, neither awaiting approval nor blocked, and no
// approval required. The covered migrations report themselves pending once
// the bootstrap is behind the database, and a resource that recounted them
// would publish a plan for what the checkpoint replaced. jq could not iterate
// absent conditions, so a status without them is refused.
func checkpointNotReapproving(migration *ptahv1alpha1.PtahMigration) bool {
	status := migration.Status
	return status.Plan == nil && status.Phase != ptahv1alpha1.MigrationPhaseAwaitingApproval &&
		status.Phase != ptahv1alpha1.MigrationPhaseBlocked && status.Conditions != nil &&
		!conditionStatus(status.Conditions, ptahv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue)
}

// checkpointResettled is the bootstrapped migration after it read its
// history again: in sync at version 4 with nothing pending and no plan.
func checkpointResettled(migration *ptahv1alpha1.PtahMigration) bool {
	status := migration.Status
	return status.Phase == ptahv1alpha1.MigrationPhaseInSync && status.History != nil &&
		status.History.PendingCount == 0 && status.History.CurrentVersion == 4 && status.Plan == nil
}
