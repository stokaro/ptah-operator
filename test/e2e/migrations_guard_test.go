package e2e

import (
	"os"
	"slices"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// mgRefuses holds a predicate to accepting its fixture and refusing every
// mutation, each of which breaks one clause.
func mgRefuses[T any](t *testing.T, fixture func() T, accepts func(T) bool, mutations map[string]func(T)) {
	t.Helper()
	if !accepts(fixture()) {
		t.Fatal("the fixture was refused")
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			candidate := fixture()
			mutate(candidate)
			if accepts(candidate) {
				t.Fatalf("a reading with %s was accepted", name)
			}
		})
	}
}

func mgCondition(kind string, status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: kind, Status: status, Reason: reason}
}

func TestDecodeManifestsReadsTheAuthorExample(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../examples/desired-state-author-role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	documents, err := decodeManifests(content)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, document := range documents {
		kinds = append(kinds, document["kind"].(string))
	}
	if !slices.Equal(kinds, []string{"Role", "RoleBinding"}) {
		t.Fatalf("the example decodes as %v", kinds)
	}
	for name, test := range map[string]struct {
		content string
		kinds   []string
		fails   bool
	}{
		"a List is flattened": {
			content: `{"apiVersion":"v1","kind":"List","items":[{"kind":"Role"},{"kind":"RoleBinding"}]}`,
			kinds:   []string{"Role", "RoleBinding"},
		},
		"an empty document is skipped": {content: "---\nkind: Role\n---\n---\nkind: RoleBinding\n", kinds: []string{"Role", "RoleBinding"}},
		"a malformed document fails":   {content: "kind: [Role\n", fails: true},
		"a List of scalars fails":      {content: `{"kind":"List","items":["Role"]}`, fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			documents, err := decodeManifests([]byte(test.content))
			if test.fails {
				if err == nil {
					t.Fatal("a malformed manifest was decoded")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var kinds []string
			for _, document := range documents {
				kinds = append(kinds, document["kind"].(string))
			}
			if !slices.Equal(kinds, test.kinds) {
				t.Fatalf("decoded %v, want %v", kinds, test.kinds)
			}
		})
	}
}

func TestGuardAuthorRoleAdaptsTheExample(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../examples/desired-state-author-role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	documents, err := decodeManifests(content)
	if err != nil {
		t.Fatal(err)
	}
	adapted, err := guardAuthorRole(documents, "ptah-e2e", "mysql", "e2e:desired-state-authors-mysql")
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range adapted {
		metadata := document["metadata"].(map[string]any)
		if metadata["namespace"] != "ptah-e2e" || metadata["name"] != "e2e-desired-state-author-mysql" {
			t.Fatalf("%s kept %v", document["kind"], metadata)
		}
		switch document["kind"] {
		case "Role":
			if !sameJSON(document["rules"], documents[0]["rules"]) {
				t.Fatalf("the example's rules changed: %v", document["rules"])
			}
		case "RoleBinding":
			if document["roleRef"].(map[string]any)["name"] != "e2e-desired-state-author-mysql" {
				t.Fatalf("the binding names role %v", document["roleRef"])
			}
			want := []any{map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Group", "name": "e2e:desired-state-authors-mysql"}}
			if !sameJSON(document["subjects"], want) {
				t.Fatalf("the binding binds %v", document["subjects"])
			}
		}
	}
	if documents[0]["metadata"].(map[string]any)["namespace"] != "application" {
		t.Fatal("adapting the example changed the documents it read")
	}
	for name, kinds := range map[string][]string{
		"a Role alone":                  {"Role"},
		"a second Role":                 {"Role", "Role", "RoleBinding"},
		"a ClusterRoleBinding in place": {"Role", "ClusterRoleBinding"},
		"nothing":                       nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var documents []map[string]any
			for _, kind := range kinds {
				documents = append(documents, map[string]any{"kind": kind, "metadata": map[string]any{}})
			}
			if _, err := guardAuthorRole(documents, "ns", "postgresql", "group"); err == nil {
				t.Fatalf("an example of %v was adapted", kinds)
			}
		})
	}
}

func TestGuardApproverRoleGrantsApprovalsAndReadsOnly(t *testing.T) {
	t.Parallel()
	documents := guardApproverRole("ptah-e2e", "e2e-migration-approver-postgresql", "e2e:migration-approvers-postgresql")
	if len(documents) != 2 || documents[0]["kind"] != "Role" || documents[1]["kind"] != "RoleBinding" {
		t.Fatalf("the approver's grant is %v", documents)
	}
	want := []any{
		map[string]any{"apiGroups": []any{"operator.ptah.run"}, "resources": []any{"ptahmigrationapprovals"}, "verbs": []any{"get", "create"}},
		map[string]any{"apiGroups": []any{"operator.ptah.run"}, "resources": []any{"ptahmigrationplans", "ptahmigrations"}, "verbs": []any{"get", "list"}},
	}
	if !sameJSON(documents[0]["rules"], want) {
		t.Fatalf("the approver's Role grants %v", documents[0]["rules"])
	}
	subjects := documents[1]["subjects"].([]any)
	if len(subjects) != 1 || subjects[0].(map[string]any)["name"] != "e2e:migration-approvers-postgresql" ||
		documents[1]["roleRef"].(map[string]any)["name"] != "e2e-migration-approver-postgresql" {
		t.Fatalf("the approver's binding is %v", documents[1])
	}
}

func TestGuardAdministratorGroupIsTheFirstExemptable(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		groups []string
		want   string
	}{
		"kind's administrator":       {groups: []string{"kubeadm:cluster-admins", "system:authenticated"}, want: "kubeadm:cluster-admins"},
		"authenticated listed first": {groups: []string{"system:authenticated", "e2e:admins", "e2e:other"}, want: "e2e:admins"},
		"only authenticated":         {groups: []string{"system:authenticated"}},
		"no group at all":            {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			group, err := guardAdministratorGroup(test.groups)
			if test.want == "" {
				if err == nil {
					t.Fatalf("%v yielded %s", test.groups, group)
				}
				return
			}
			if err != nil || group != test.want {
				t.Fatalf("%v yielded %q, %v; want %s", test.groups, group, err, test.want)
			}
		})
	}
}

type mgGuard struct {
	policies []admissionregistrationv1.ValidatingAdmissionPolicy
	bindings []admissionregistrationv1.ValidatingAdmissionPolicyBinding
}

func mgGuardFixture() *mgGuard {
	policy := admissionregistrationv1.ValidatingAdmissionPolicy{}
	policy.Spec.Variables = []admissionregistrationv1.Variable{
		{Name: "exemptGroups", Expression: `["kubeadm:cluster-admins", "e2e:static-operators"]`},
		{Name: "requesterIsExempt", Expression: "variables.exemptGroups.exists(group, group in request.userInfo.groups)"},
	}
	binding := admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	binding.Spec.ValidationActions = []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}
	return &mgGuard{
		policies: []admissionregistrationv1.ValidatingAdmissionPolicy{policy},
		bindings: []admissionregistrationv1.ValidatingAdmissionPolicyBinding{binding},
	}
}

func TestApplyPolicyGuardInstalled(t *testing.T) {
	t.Parallel()
	accepts := func(guard *mgGuard) bool {
		return applyPolicyGuardInstalled(guard.policies, guard.bindings, "kubeadm:cluster-admins") == nil
	}
	mgRefuses(t, mgGuardFixture, accepts, map[string]func(*mgGuard){
		"no policy":        func(g *mgGuard) { g.policies = nil },
		"a second policy":  func(g *mgGuard) { g.policies = append(g.policies, g.policies[0]) },
		"no binding":       func(g *mgGuard) { g.bindings = nil },
		"a second binding": func(g *mgGuard) { g.bindings = append(g.bindings, g.bindings[0]) },
		"the group not exempt": func(g *mgGuard) {
			g.policies[0].Spec.Variables[0].Expression = `["e2e:static-operators"]`
		},
		"the group named outside a string literal": func(g *mgGuard) {
			g.policies[0].Spec.Variables[0].Expression = `[kubeadm:cluster-admins]`
		},
		"a longer group that only contains it": func(g *mgGuard) {
			g.policies[0].Spec.Variables[0].Expression = `["kubeadm:cluster-admins-and-more"]`
		},
		"no exemptGroups variable": func(g *mgGuard) { g.policies[0].Spec.Variables[0].Name = "exempt" },
		"a second exemptGroups variable that exempts nobody": func(g *mgGuard) {
			g.policies[0].Spec.Variables = append(g.policies[0].Spec.Variables,
				admissionregistrationv1.Variable{Name: "exemptGroups", Expression: "[]"})
		},
		"a binding that only warns": func(g *mgGuard) {
			g.bindings[0].Spec.ValidationActions = []admissionregistrationv1.ValidationAction{admissionregistrationv1.Warn}
		},
		"a binding that denies and audits": func(g *mgGuard) {
			g.bindings[0].Spec.ValidationActions = append(g.bindings[0].Spec.ValidationActions, admissionregistrationv1.Audit)
		},
	})
}

func mgMigration() *ptahv1alpha1.PtahMigration {
	migration := &ptahv1alpha1.PtahMigration{}
	migration.Name, migration.Generation = "e2e-apply-guard-postgresql", 4
	migration.Spec.Policy.Apply = ptahv1alpha1.ApplyPolicyAlways
	migration.Status.ObservedGeneration = 4
	migration.Status.Phase = ptahv1alpha1.MigrationPhaseInSync
	migration.Status.Plan = &ptahv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-1", UID: "u-plan"}
	migration.Status.LastRun = &ptahv1alpha1.MigrationRunStatus{Outcome: ptahv1alpha1.MigrationRunOutcomeApplied}
	return migration
}

func TestGuardMigrationReadings(t *testing.T) {
	t.Parallel()
	t.Run("a plan awaiting approval", func(t *testing.T) {
		t.Parallel()
		fixture := func() *ptahv1alpha1.PtahMigration {
			migration := mgMigration()
			migration.Status.Phase = ptahv1alpha1.MigrationPhaseAwaitingApproval
			return migration
		}
		mgRefuses(t, fixture, guardPlanAwaiting, map[string]func(*ptahv1alpha1.PtahMigration){
			"another phase":   func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhasePlanning },
			"no plan":         func(m *ptahv1alpha1.PtahMigration) { m.Status.Plan = nil },
			"a nameless plan": func(m *ptahv1alpha1.PtahMigration) { m.Status.Plan.Name = "" },
		})
	})
	t.Run("the approved plan applied", func(t *testing.T) {
		t.Parallel()
		mgRefuses(t, mgMigration, guardPlanApplied, map[string]func(*ptahv1alpha1.PtahMigration){
			"another phase": func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseApplying },
			"no run":        func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun = nil },
			"a failed run":  func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeFailed },
			"an unknown run": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeUnknown
			},
		})
	})
	t.Run("converged with Always in place", func(t *testing.T) {
		t.Parallel()
		accepts := func(m *ptahv1alpha1.PtahMigration) bool { return guardConvergedUnderAlways(m, 4) }
		mgRefuses(t, mgMigration, accepts, map[string]func(*ptahv1alpha1.PtahMigration){
			"OnApproval":            func(m *ptahv1alpha1.PtahMigration) { m.Spec.Policy.Apply = ptahv1alpha1.ApplyPolicyOnApproval },
			"an older generation":   func(m *ptahv1alpha1.PtahMigration) { m.Status.ObservedGeneration = 3 },
			"still verifying":       func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseVerifyingHistory },
			"an unset apply policy": func(m *ptahv1alpha1.PtahMigration) { m.Spec.Policy.Apply = "" },
		})
		later := mgMigration()
		later.Status.ObservedGeneration = 5
		if !accepts(later) {
			t.Fatal("a later generation observed was refused")
		}
	})
}

func TestApprovalNamesApprover(t *testing.T) {
	t.Parallel()
	fixture := func() *ptahv1alpha1.PtahMigrationApproval {
		approval := &ptahv1alpha1.PtahMigrationApproval{}
		approval.Spec.Approver = ptahv1alpha1.ApprovalIdentity{
			Username: "e2e-approver-mysql", Groups: []string{"e2e:migration-approvers-mysql", "system:authenticated"},
		}
		return approval
	}
	accepts := func(a *ptahv1alpha1.PtahMigrationApproval) bool {
		return approvalNamesApprover(a, "e2e-approver-mysql", "e2e:migration-approvers-mysql")
	}
	mgRefuses(t, fixture, accepts, map[string]func(*ptahv1alpha1.PtahMigrationApproval){
		"another approver":  func(a *ptahv1alpha1.PtahMigrationApproval) { a.Spec.Approver.Username = "e2e-author-mysql" },
		"the group missing": func(a *ptahv1alpha1.PtahMigrationApproval) { a.Spec.Approver.Groups = []string{"system:authenticated"} },
		"no groups":         func(a *ptahv1alpha1.PtahMigrationApproval) { a.Spec.Approver.Groups = nil },
	})
}

func mgHeldExisting() *ptahv1alpha1.PtahMigration {
	migration := &ptahv1alpha1.PtahMigration{}
	migration.Status.Phase = ptahv1alpha1.MigrationPhaseAwaitingApproval
	migration.Status.History = &ptahv1alpha1.MigrationHistoryStatus{PendingCount: 3}
	migration.Status.Conditions = []metav1.Condition{
		mgCondition("ApprovalRequired", metav1.ConditionTrue, "AwaitingApproval"),
		mgCondition("Ready", metav1.ConditionFalse, "AwaitingApproval"),
	}
	return migration
}

func TestExistingSchemaHeld(t *testing.T) {
	t.Parallel()
	mgRefuses(t, mgHeldExisting, existingSchemaHeld, map[string]func(*ptahv1alpha1.PtahMigration){
		"another phase":        func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseInSync },
		"no history":           func(m *ptahv1alpha1.PtahMigration) { m.Status.History = nil },
		"a current version":    func(m *ptahv1alpha1.PtahMigration) { m.Status.History.CurrentVersion = 1 },
		"an applied migration": func(m *ptahv1alpha1.PtahMigration) { m.Status.History.AppliedCount = 1 },
		"two pending":          func(m *ptahv1alpha1.PtahMigration) { m.Status.History.PendingCount = 2 },
		"a dirty history":      func(m *ptahv1alpha1.PtahMigration) { m.Status.History.Dirty = true },
		"approval for another reason": func(m *ptahv1alpha1.PtahMigration) {
			m.Status.Conditions[0].Reason = "HistoryDirty"
		},
		"no approval required": func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[0].Status = metav1.ConditionFalse },
		"called ready":         func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[1].Status = metav1.ConditionTrue },
		"no conditions":        func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions = nil },
		"a recorded run": func(m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun = &ptahv1alpha1.MigrationRunStatus{Outcome: ptahv1alpha1.MigrationRunOutcomeUpToDate}
		},
	})
}

func mgAdopted() *ptahv1alpha1.PtahMigration {
	migration := &ptahv1alpha1.PtahMigration{}
	migration.Status.Phase = ptahv1alpha1.MigrationPhaseInSync
	migration.Status.History = &ptahv1alpha1.MigrationHistoryStatus{CurrentVersion: 3, AppliedCount: 3}
	migration.Status.Conditions = []metav1.Condition{
		mgCondition("Ready", metav1.ConditionTrue, "HistoryMatched"),
		mgCondition("Blocked", metav1.ConditionFalse, "HistoryMatched"),
	}
	return migration
}

func TestAdoptedHistorySettled(t *testing.T) {
	t.Parallel()
	mgRefuses(t, mgAdopted, adoptedHistorySettled, map[string]func(*ptahv1alpha1.PtahMigration){
		"another phase":          func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseReading },
		"no history":             func(m *ptahv1alpha1.PtahMigration) { m.Status.History = nil },
		"at version 2":           func(m *ptahv1alpha1.PtahMigration) { m.Status.History.CurrentVersion = 2 },
		"two applied":            func(m *ptahv1alpha1.PtahMigration) { m.Status.History.AppliedCount = 2 },
		"one pending":            func(m *ptahv1alpha1.PtahMigration) { m.Status.History.PendingCount = 1 },
		"not ready":              func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[0].Status = metav1.ConditionFalse },
		"ready for a reason":     func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[0].Reason = "UpToDate" },
		"blocked":                func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[1].Status = metav1.ConditionTrue },
		"unblocked for a reason": func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[1].Reason = "Settled" },
		"a run of its own": func(m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun = &ptahv1alpha1.MigrationRunStatus{Outcome: ptahv1alpha1.MigrationRunOutcomeApplied}
		},
	})
}

func TestAdopterIsolated(t *testing.T) {
	t.Parallel()
	secretEnvVar := func(name, secret, key string) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key,
		}}}
	}
	fixture := func() *batchv1.Job {
		job := &batchv1.Job{}
		job.Spec.Template.Spec.Containers = []corev1.Container{{Name: "adopter", Env: []corev1.EnvVar{
			{Name: "HOME", Value: "/work"},
			secretEnvVar("PTAH_E2E_TARGET_URL", "e2e-postgresql-adopt-db", "url"),
			secretEnvVar("PTAH_E2E_SHADOW_URL", "e2e-postgresql-adopt-db", "shadowUrl"),
		}}}
		return job
	}
	accepts := func(job *batchv1.Job) bool { return adopterIsolated(job, "e2e-postgresql-adopt-db") }
	mgRefuses(t, fixture, accepts, map[string]func(*batchv1.Job){
		"a second container": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers = append(job.Spec.Template.Spec.Containers, corev1.Container{Name: "sidecar"})
		},
		"a registry credential": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].Env = append(job.Spec.Template.Spec.Containers[0].Env,
				secretEnvVar("PTAH_OCI_PASSWORD", "e2e-registry-auth", "password"))
		},
		"no database credential": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].Env = job.Spec.Template.Spec.Containers[0].Env[:1]
		},
	})
}

func mgCheckpointGate() *ptahv1alpha1.PtahMigration {
	migration := &ptahv1alpha1.PtahMigration{}
	migration.Status.Phase = ptahv1alpha1.MigrationPhaseAwaitingApproval
	migration.Status.Plan = &ptahv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-c"}
	migration.Status.History = &ptahv1alpha1.MigrationHistoryStatus{CheckpointVersion: 3, AppliedCount: 2, PendingCount: 2}
	migration.Status.Conditions = []metav1.Condition{mgCondition("ApprovalRequired", metav1.ConditionTrue, "AwaitingApproval")}
	return migration
}

func TestCheckpointGate(t *testing.T) {
	t.Parallel()
	mgRefuses(t, mgCheckpointGate, checkpointGate, map[string]func(*ptahv1alpha1.PtahMigration){
		"another phase":        func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseBlocked },
		"no history":           func(m *ptahv1alpha1.PtahMigration) { m.Status.History = nil },
		"a current version":    func(m *ptahv1alpha1.PtahMigration) { m.Status.History.CurrentVersion = 2 },
		"no checkpoint":        func(m *ptahv1alpha1.PtahMigration) { m.Status.History.CheckpointVersion = 0 },
		"three applied":        func(m *ptahv1alpha1.PtahMigration) { m.Status.History.AppliedCount = 3 },
		"four pending":         func(m *ptahv1alpha1.PtahMigration) { m.Status.History.PendingCount = 4 },
		"dirty":                func(m *ptahv1alpha1.PtahMigration) { m.Status.History.Dirty = true },
		"a modified version":   func(m *ptahv1alpha1.PtahMigration) { m.Status.History.ModifiedVersions = []int64{1} },
		"an out-of-order file": func(m *ptahv1alpha1.PtahMigration) { m.Status.History.OutOfOrderVersions = []int64{2} },
		"no approval required": func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[0].Status = metav1.ConditionFalse },
		"approval for another reason": func(m *ptahv1alpha1.PtahMigration) {
			m.Status.Conditions[0].Reason = "PlanPending"
		},
	})
	plan := func() *ptahv1alpha1.PtahMigrationPlan {
		return &ptahv1alpha1.PtahMigrationPlan{Spec: ptahv1alpha1.PtahMigrationPlanSpec{
			Migrations: []ptahv1alpha1.PlannedMigration{{Version: 3, Checkpoint: true}, {Version: 4}},
		}}
	}
	mgRefuses(t, plan, checkpointPlanVersions, map[string]func(*ptahv1alpha1.PtahMigrationPlan){
		"the covered migrations too": func(p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.Migrations = append([]ptahv1alpha1.PlannedMigration{{Version: 1}, {Version: 2}}, p.Spec.Migrations...)
		},
		"the checkpoint alone": func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.Migrations = p.Spec.Migrations[:1] },
		"out of order":         func(p *ptahv1alpha1.PtahMigrationPlan) { slices.Reverse(p.Spec.Migrations) },
	})
}

func mgCheckpointSettled() *ptahv1alpha1.PtahMigration {
	migration := &ptahv1alpha1.PtahMigration{}
	migration.Status.Phase = ptahv1alpha1.MigrationPhaseInSync
	migration.Status.History = &ptahv1alpha1.MigrationHistoryStatus{CurrentVersion: 4, CheckpointVersion: 3, AppliedCount: 4}
	migration.Status.LastRun = &ptahv1alpha1.MigrationRunStatus{
		Outcome: ptahv1alpha1.MigrationRunOutcomeApplied, AppliedVersions: []int64{3, 4}, JobUID: types.UID("u-run"),
	}
	migration.Status.Conditions = []metav1.Condition{mgCondition("Ready", metav1.ConditionTrue, "HistoryMatched")}
	return migration
}

func TestCheckpointSettled(t *testing.T) {
	t.Parallel()
	mgRefuses(t, mgCheckpointSettled, checkpointSettled, map[string]func(*ptahv1alpha1.PtahMigration){
		"another phase":          func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseApplying },
		"no history":             func(m *ptahv1alpha1.PtahMigration) { m.Status.History = nil },
		"at version 3":           func(m *ptahv1alpha1.PtahMigration) { m.Status.History.CurrentVersion = 3 },
		"one pending":            func(m *ptahv1alpha1.PtahMigration) { m.Status.History.PendingCount = 1 },
		"dirty":                  func(m *ptahv1alpha1.PtahMigration) { m.Status.History.Dirty = true },
		"a plan left":            func(m *ptahv1alpha1.PtahMigration) { m.Status.Plan = &ptahv1alpha1.ImmutableObjectReference{Name: "p"} },
		"the checkpoint dropped": func(m *ptahv1alpha1.PtahMigration) { m.Status.History.CheckpointVersion = 0 },
		"no run":                 func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun = nil },
		"a partial run": func(m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomePartial
		},
		"the covered migrations run": func(m *ptahv1alpha1.PtahMigration) {
			m.Status.LastRun.AppliedVersions = []int64{1, 2, 3, 4}
		},
		"the run in another order": func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.AppliedVersions = []int64{4, 3} },
		"not ready":                func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[0].Status = metav1.ConditionFalse },
		"ready for a reason":       func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[0].Reason = "UpToDate" },
	})
}

func TestCheckpointStaysSettled(t *testing.T) {
	t.Parallel()
	mgRefuses(t, mgCheckpointSettled, checkpointNotReapproving, map[string]func(*ptahv1alpha1.PtahMigration){
		"a plan published":  func(m *ptahv1alpha1.PtahMigration) { m.Status.Plan = &ptahv1alpha1.ImmutableObjectReference{Name: "p"} },
		"awaiting approval": func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseAwaitingApproval },
		"blocked":           func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseBlocked },
		"no conditions":     func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions = nil },
		"approval required": func(m *ptahv1alpha1.PtahMigration) {
			m.Status.Conditions = append(m.Status.Conditions, mgCondition("ApprovalRequired", metav1.ConditionTrue, "AwaitingApproval"))
		},
	})
	// A settled resource still resolves, verifies and reads at its interval.
	for _, phase := range []ptahv1alpha1.MigrationPhase{
		ptahv1alpha1.MigrationPhaseResolving, ptahv1alpha1.MigrationPhaseVerifying, ptahv1alpha1.MigrationPhaseReading,
	} {
		cycling := mgCheckpointSettled()
		cycling.Status.Phase = phase
		if !checkpointNotReapproving(cycling) {
			t.Errorf("a settled resource %s mid-cycle was refused", phase)
		}
	}
}

func TestCheckpointResettled(t *testing.T) {
	t.Parallel()
	mgRefuses(t, mgCheckpointSettled, checkpointResettled, map[string]func(*ptahv1alpha1.PtahMigration){
		"mid-cycle":    func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseReading },
		"no history":   func(m *ptahv1alpha1.PtahMigration) { m.Status.History = nil },
		"one pending":  func(m *ptahv1alpha1.PtahMigration) { m.Status.History.PendingCount = 1 },
		"at version 3": func(m *ptahv1alpha1.PtahMigration) { m.Status.History.CurrentVersion = 3 },
		"a plan":       func(m *ptahv1alpha1.PtahMigration) { m.Status.Plan = &ptahv1alpha1.ImmutableObjectReference{Name: "p"} },
	})
}

// The administrator's group is found in the exemptGroups literal the chart
// renders, as toJson spells it.
func TestApplyPolicyGuardReadsTheChartsLiteral(t *testing.T) {
	t.Parallel()
	guard := mgGuardFixture()
	guard.policies[0].Spec.Variables[0].Expression = `["e2e:admins \"quoted\""]`
	if err := applyPolicyGuardInstalled(guard.policies, guard.bindings, `e2e:admins "quoted"`); err != nil {
		t.Fatalf("a group with a quote was not found in its JSON literal: %v", err)
	}
	guard.policies[0].Spec.Variables[0].Expression = `["e2e:admins "quoted""]`
	if err := applyPolicyGuardInstalled(guard.policies, guard.bindings, `e2e:admins "quoted"`); err == nil {
		t.Fatal("a group spelled without JSON escapes was taken for the literal")
	}
}
