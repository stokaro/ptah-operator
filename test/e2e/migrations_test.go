package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

// mtRefusesEach holds a check to accepting its fixture and refusing each
// mutation of it. A mutation that the check accepts is a clause nothing
// measures.
func mtRefusesEach[T any](t *testing.T, fixture func() T, check func(T) error, mutations map[string]func(T)) {
	t.Helper()
	if err := check(fixture()); err != nil {
		t.Fatalf("the fixture was refused: %v", err)
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			value := fixture()
			mutate(value)
			if check(value) == nil {
				t.Fatalf("a reading with %s was accepted", name)
			}
		})
	}
}

func TestRealmDigestMatchesTheOperators(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ engine, kind, realm string }{
		{"postgresql", "PostgreSQL", "e2e-realm-postgresql"},
		{"mysql", "MySQL", "e2e-realm-mysql"},
	} {
		t.Run(test.engine, func(t *testing.T) {
			t.Parallel()
			got, err := realmDigest(test.engine, test.realm)
			if err != nil {
				t.Fatal(err)
			}
			want, err := fingerprint.DatabaseRealmDigest(test.kind, test.realm)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("realmDigest(%s, %s) = %s, the operator derives %s", test.engine, test.realm, got, want)
			}
			// And it names that realm alone: another realm, or a coordination
			// key of the same name, is another digest.
			other, err := realmDigest(test.engine, test.realm+"-other")
			if err != nil {
				t.Fatal(err)
			}
			key, err := coordinationDigest(test.engine, "ptah-e2e", test.realm)
			if err != nil {
				t.Fatal(err)
			}
			if other == got || key == got {
				t.Fatalf("realmDigest(%s, %s) collided with another realm or a key", test.engine, test.realm)
			}
		})
	}
}

func TestMigrationEngineFor(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		engine string
		want   migrationEngine
	}{
		{"postgresql", migrationEngine{name: "postgresql", kind: "PostgreSQL", service: "e2e-postgresql", sourceSecret: "e2e-postgresql-db", port: "5432"}},
		{"mysql", migrationEngine{name: "mysql", kind: "MySQL", service: "e2e-mysql", sourceSecret: "e2e-mysql-db", port: "3306"}},
	} {
		t.Run(test.engine, func(t *testing.T) {
			t.Parallel()
			got, err := migrationEngineFor(test.engine)
			if err != nil || got != test.want {
				t.Fatalf("migrationEngineFor(%s) = %+v, %v; want %+v", test.engine, got, err, test.want)
			}
		})
	}
	for _, engine := range []string{"", "postgres", "PostgreSQL", "MySQL", "mariadb"} {
		t.Run("refuses "+engine, func(t *testing.T) {
			t.Parallel()
			if _, err := migrationEngineFor(engine); err == nil {
				t.Fatalf("migrationEngineFor(%q) accepted an engine the phase does not run", engine)
			}
		})
	}
}

func TestMigrationRepository(t *testing.T) {
	t.Parallel()
	for rerun, want := range map[string]string{"": "migrations", "r1": "migrations-r1", "r1759000000": "migrations-r1759000000"} {
		if got, err := migrationRepository(rerun); err != nil || got != want {
			t.Errorf("migrationRepository(%q) = %q, %v; want %q", rerun, got, err, want)
		}
	}
	for _, rerun := range []string{"1", "r", "rx1", "R1", "r1 ", " r1", "r1-2"} {
		if _, err := migrationRepository(rerun); err == nil {
			t.Errorf("migrationRepository(%q) accepted a rerun marker hack/e2e-rerun-phase.sh never writes", rerun)
		}
	}
}

func TestMigrationEngineDatabaseURL(t *testing.T) {
	t.Parallel()
	for engine, want := range map[string]string{
		"postgresql": "postgres://ptah_e2e:secret@e2e-postgresql.ptah-e2e.svc.cluster.local:5432/ptah_e2e_migrations?sslmode=disable",
		"mysql":      "mysql://ptah_e2e:secret@tcp(e2e-mysql.ptah-e2e.svc.cluster.local:3306)/ptah_e2e_migrations",
	} {
		selected, err := migrationEngineFor(engine)
		if err != nil {
			t.Fatal(err)
		}
		if got := selected.databaseURL("ptah-e2e", migrationDatabaseUser, "secret", "ptah_e2e_migrations"); got != want {
			t.Errorf("%s databaseURL = %s, want %s", engine, got, want)
		}
	}
}

func TestCurrentSchemaFilter(t *testing.T) {
	t.Parallel()
	for engine, want := range map[string]string{"postgresql": "table_schema='public'", "mysql": "table_schema=DATABASE()"} {
		selected, err := migrationEngineFor(engine)
		if err != nil {
			t.Fatal(err)
		}
		if got := selected.currentSchemaFilter(); got != want {
			t.Errorf("%s currentSchemaFilter = %s, want %s", engine, got, want)
		}
	}
}

func TestValidKindCluster(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"a", "ptah-e2e", "ptah-e2e-1", "0kind"} {
		if err := validKindCluster(name); err != nil {
			t.Errorf("validKindCluster(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"", "-kind", "kind-", "Kind", "kind_e2e", "kind.e2e", "kind e2e"} {
		if validKindCluster(name) == nil {
			t.Errorf("validKindCluster(%q) accepted a name that is no DNS label", name)
		}
	}
}

func TestMigrationInputsOK(t *testing.T) {
	t.Parallel()
	pinned := "example.invalid/ptah@" + builderDigest('d')
	if err := migrationInputsOK("postgresql", "postgresql", pinned, pinned); err != nil {
		t.Fatalf("migrationInputsOK refused a phase's own engine and pinned images: %v", err)
	}
	for name, err := range map[string]error{
		"another engine":       migrationInputsOK("mysql", "postgresql", pinned),
		"no engine":            migrationInputsOK("", "mysql", pinned),
		"a tag":                migrationInputsOK("mysql", "mysql", "example.invalid/ptah:v1"),
		"an uppercase digest":  migrationInputsOK("mysql", "mysql", "example.invalid/ptah@sha256:"+strings.Repeat("D", 64)),
		"a short digest":       migrationInputsOK("mysql", "mysql", "example.invalid/ptah@sha256:dd"),
		"one unpinned of many": migrationInputsOK("mysql", "mysql", pinned, "example.invalid/runner:latest"),
	} {
		if err == nil {
			t.Errorf("migrationInputsOK accepted %s", name)
		}
	}
}

// mtStatus decodes a status document as the API would return it.
func mtStatus(t *testing.T, document string) ptahv1alpha1.PtahMigrationStatus {
	t.Helper()
	var migration ptahv1alpha1.PtahMigration
	if err := json.Unmarshal([]byte(document), &migration); err != nil {
		t.Fatal(err)
	}
	return migration.Status
}

// The cases the migration filter self-test held
// migration-partial-refusal.jq to, and the reading the operator produced when
// a lifecycle failed on it: the phase is Resolving while the refusal holds.
func TestBlockedRefusalHeld(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", "partial-run-left-a-dirty-revision.json"))
	if err != nil {
		t.Fatal(err)
	}
	var reading ptahv1alpha1.PtahMigration
	if err := json.Unmarshal(content, &reading); err != nil {
		t.Fatal(err)
	}
	if reading.Status.Phase == ptahv1alpha1.MigrationPhaseBlocked {
		t.Fatal("the recorded reading is Blocked; it was recorded because it was not")
	}
	if !blockedRefusalHeld(reading.Status) {
		t.Fatal("the refusal the operator held while it resolved again was refused")
	}
	for _, test := range []struct {
		name   string
		status string
		held   bool
	}{
		{"blocked, between cycles", `{"status":{"phase":"Blocked","conditions":[
			{"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown"},
			{"type":"Ready","status":"False","reason":"ApplyOutcomeUnknown"}]}}`, true},
		{"the refusal lapsed", `{"status":{"phase":"Blocked","conditions":[
			{"type":"Blocked","status":"False","reason":"HistoryMatched"},
			{"type":"Ready","status":"False","reason":"MigrationsPending"}]}}`, false},
		{"a plan was published while blocked", `{"status":{"phase":"Blocked","plan":{"name":"ptah-mplan-0","uid":"u"},
			"conditions":[{"type":"Blocked","status":"True","reason":"HistoryDirty"},
			{"type":"Ready","status":"False","reason":"HistoryDirty"}]}}`, false},
		{"the resource was called ready", `{"status":{"phase":"Blocked","conditions":[
			{"type":"Blocked","status":"True","reason":"HistoryDirty"},
			{"type":"Ready","status":"True","reason":"HistoryMatched"}]}}`, false},
		{"no conditions at all", `{"status":{"phase":"Blocked"}}`, false},
		{"an empty condition list", `{"status":{"phase":"Blocked","conditions":[]}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := blockedRefusalHeld(mtStatus(t, test.status)); got != test.held {
				t.Fatalf("blockedRefusalHeld = %t, want %t", got, test.held)
			}
		})
	}
}

// The cases the self-test held gated-apply-pod.jq to. Both refusals it
// names are mistakes the filter actually made: reading the phase alone, and
// any() over an empty list.
func TestGatedApplyPods(t *testing.T) {
	t.Parallel()
	gated := func(name, node string, phase corev1.PodPhase, selector map[string]string) corev1.Pod {
		pod := corev1.Pod{}
		pod.Name, pod.Spec.NodeName, pod.Status.Phase, pod.Spec.NodeSelector = name, node, phase, selector
		return pod
	}
	gate := map[string]string{applyGateLabel: "open"}
	for _, test := range []struct {
		name string
		pods []corev1.Pod
		held bool
	}{
		{"one Pod the gate is holding", []corev1.Pod{gated("apply-abc", "", corev1.PodPending, gate)}, true},
		{"two Pods the gate is holding", []corev1.Pod{
			gated("apply-abc", "", corev1.PodPending, gate), gated("apply-def", "", corev1.PodPending, gate),
		}, true},
		{"a Pod carrying no selector, between creation and scheduling", []corev1.Pod{gated("apply-abc", "", corev1.PodPending, nil)}, false},
		{"a selector for some other gate", []corev1.Pod{
			gated("apply-abc", "", corev1.PodPending, map[string]string{"kubernetes.io/os": "linux"}),
		}, false},
		{"the gate label closed", []corev1.Pod{
			gated("apply-abc", "", corev1.PodPending, map[string]string{applyGateLabel: "closed"}),
		}, false},
		{"the Job has not created a Pod yet", nil, false},
		{"Pending, and already bound to a node", []corev1.Pod{gated("apply-abc", "kind-worker", corev1.PodPending, gate)}, false},
		{"the runner is already going", []corev1.Pod{gated("apply-abc", "kind-worker", corev1.PodRunning, gate)}, false},
		{"one held and one placed", []corev1.Pod{
			gated("apply-abc", "", corev1.PodPending, gate), gated("apply-def", "kind-worker", corev1.PodPending, gate),
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := gatedApplyPods(test.pods); got != test.held {
				t.Fatalf("gatedApplyPods = %t, want %t", got, test.held)
			}
		})
	}
}

func TestCarriesMigrationSQL(t *testing.T) {
	t.Parallel()
	for name, document := range map[string]string{
		"a nested statement":    `{"status":{"lastRun":{"message":"CREATE TABLE e2e_migration_widgets (id int)"}}}`,
		"a statement in a list": `{"spec":{"migrations":[{"description":"x"},{"description":"alter  table x add y"}]}}`,
		"an insert":             `{"message":"Insert\tInto e2e_migration_widgets values (1)"}`,
		"a bare string":         `"create table x"`,
	} {
		var decoded any
		if err := json.Unmarshal([]byte(document), &decoded); err != nil {
			t.Fatal(err)
		}
		if !carriesMigrationSQL(decoded) {
			t.Errorf("%s was not found", name)
		}
	}
	for name, document := range map[string]string{
		"a key, not a value":        `{"create table x":"y"}`,
		"versions and checksums":    `{"spec":{"migrations":[{"version":1,"checksum":"h1:abc","description":"create widgets"}]}}`,
		"another statement":         `{"message":"CREATE INDEX idx ON x (y)"}`,
		"the words apart":           `{"message":"create a table"}`,
		"numbers and booleans only": `{"a":1,"b":true,"c":null}`,
	} {
		var decoded any
		if err := json.Unmarshal([]byte(document), &decoded); err != nil {
			t.Fatal(err)
		}
		if carriesMigrationSQL(decoded) {
			t.Errorf("%s was taken for SQL", name)
		}
	}
}

const mtDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func mtCondition(kind string, status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: kind, Status: status, Reason: reason}
}

func mtAwaiting() *ptahv1alpha1.PtahMigration {
	return &ptahv1alpha1.PtahMigration{Status: ptahv1alpha1.PtahMigrationStatus{
		Phase:            ptahv1alpha1.MigrationPhaseAwaitingApproval,
		Artifact:         &ptahv1alpha1.OCIArtifactAccessBinding{Digest: mtDigest},
		ExecutionBinding: &ptahv1alpha1.ExecutionBindingStatus{ControllerStateVersion: 2},
		History: &ptahv1alpha1.MigrationHistoryStatus{
			ContractVersion: 1, PendingCount: 3, Fingerprint: builderDigest('2'), TargetIdentityDigest: builderDigest('3'),
		},
		Plan: &ptahv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-abc", UID: "plan-uid"},
		Conditions: []metav1.Condition{
			mtCondition("ApprovalRequired", metav1.ConditionTrue, "AwaitingApproval"),
			mtCondition("ArtifactVerified", metav1.ConditionTrue, "Verified"),
			mtCondition("Ready", metav1.ConditionFalse, "AwaitingApproval"),
		},
	}}
}

func TestAwaitingApprovalGate(t *testing.T) {
	t.Parallel()
	mtRefusesEach(t, mtAwaiting,
		func(migration *ptahv1alpha1.PtahMigration) error { return awaitingApprovalGate(migration, mtDigest, 2) },
		map[string]func(*ptahv1alpha1.PtahMigration){
			"another phase": func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseBlocked },
			"another digest": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.Artifact.Digest = builderDigest('9')
			},
			"no artifact":               func(m *ptahv1alpha1.PtahMigration) { m.Status.Artifact = nil },
			"no history":                func(m *ptahv1alpha1.PtahMigration) { m.Status.History = nil },
			"another history contract":  func(m *ptahv1alpha1.PtahMigration) { m.Status.History.ContractVersion = 2 },
			"a version already applied": func(m *ptahv1alpha1.PtahMigration) { m.Status.History.CurrentVersion = 1 },
			"an applied count":          func(m *ptahv1alpha1.PtahMigration) { m.Status.History.AppliedCount = 1 },
			"two pending":               func(m *ptahv1alpha1.PtahMigration) { m.Status.History.PendingCount = 2 },
			"a dirty history":           func(m *ptahv1alpha1.PtahMigration) { m.Status.History.Dirty = true },
			"a modified version":        func(m *ptahv1alpha1.PtahMigration) { m.Status.History.ModifiedVersions = []int64{1} },
			"no history fingerprint":    func(m *ptahv1alpha1.PtahMigration) { m.Status.History.Fingerprint = "" },
			"no target identity":        func(m *ptahv1alpha1.PtahMigration) { m.Status.History.TargetIdentityDigest = "sha256:xy" },
			"another controller state":  func(m *ptahv1alpha1.PtahMigration) { m.Status.ExecutionBinding.ControllerStateVersion = 3 },
			"no execution binding":      func(m *ptahv1alpha1.PtahMigration) { m.Status.ExecutionBinding = nil },
			"no plan":                   func(m *ptahv1alpha1.PtahMigration) { m.Status.Plan = nil },
			"a plan of another kind":    func(m *ptahv1alpha1.PtahMigration) { m.Status.Plan.Name = "ptah-plan-abc" },
			"a run recorded": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.LastRun = &ptahv1alpha1.MigrationRunStatus{Outcome: ptahv1alpha1.MigrationRunOutcomeApplied}
			},
			"approval not required": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.Conditions[0].Status = metav1.ConditionFalse
			},
			"approval required for another reason": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.Conditions[0].Reason = "ApprovalRevoked"
			},
			"an unverified artifact": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.Conditions[1].Status = metav1.ConditionFalse
			},
			"called ready":  func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[2].Status = metav1.ConditionTrue },
			"no conditions": func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions = nil },
		})
}

func mtPlan() *ptahv1alpha1.PtahMigrationPlan {
	plan := &ptahv1alpha1.PtahMigrationPlan{}
	plan.Name, plan.UID = "ptah-mplan-abc", "plan-uid"
	plan.Spec = ptahv1alpha1.PtahMigrationPlanSpec{
		ContractVersion: 1, MigrationRef: ptahv1alpha1.ImmutableObjectReference{Name: "e2e-migrations-postgresql", UID: "migration-uid"},
		ArtifactDigest: mtDigest, CoordinationDigest: builderDigest('c'),
		Fingerprint: builderDigest('4'), HistoryFingerprint: builderDigest('5'),
		ControllerImage: "example.invalid/manager@" + builderDigest('f'), ControllerStateVersion: 2,
		Migrations: []ptahv1alpha1.PlannedMigration{
			{Version: 1, Checksum: "h1:one"}, {Version: 2, Checksum: "h1:two"}, {Version: 3, Checksum: "h1:three"},
		},
	}
	return plan
}

func TestPlanSequence(t *testing.T) {
	t.Parallel()
	controller := controllerIdentity{image: "example.invalid/manager@" + builderDigest('f'), revision: "r", stateVersion: "2"}
	mtRefusesEach(t, mtPlan,
		func(plan *ptahv1alpha1.PtahMigrationPlan) error {
			return planSequence(plan, "e2e-migrations-postgresql", mtDigest, builderDigest('c'), controller, 2)
		},
		map[string]func(*ptahv1alpha1.PtahMigrationPlan){
			"another contract":          func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.ContractVersion = 2 },
			"another migration":         func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.MigrationRef.Name = "other" },
			"another artifact":          func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.ArtifactDigest = builderDigest('9') },
			"another realm":             func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.CoordinationDigest = builderDigest('9') },
			"a history already applied": func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.CurrentVersion = 1 },
			"no fingerprint":            func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.Fingerprint = "" },
			"no history fingerprint":    func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.HistoryFingerprint = "sha256:zz" },
			"a missing version": func(p *ptahv1alpha1.PtahMigrationPlan) {
				p.Spec.Migrations = p.Spec.Migrations[:2]
			},
			"versions out of order": func(p *ptahv1alpha1.PtahMigrationPlan) {
				p.Spec.Migrations[0], p.Spec.Migrations[1] = p.Spec.Migrations[1], p.Spec.Migrations[0]
			},
			"no migrations":         func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.Migrations = nil },
			"a missing checksum":    func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.Migrations[1].Checksum = "" },
			"a checkpoint":          func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.Migrations[2].Checkpoint = true },
			"another manager":       func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.ControllerImage = "example.invalid/other" },
			"another state version": func(p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.ControllerStateVersion = 3 },
		})
}

func mtApproval() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahMigrationApproval",
		"metadata": map[string]any{"name": "approval"},
		"spec": map[string]any{
			"migrationRef":       map[string]any{"name": "e2e-migrations-postgresql", "uid": "migration-uid"},
			"planRef":            map[string]any{"name": "ptah-mplan-abc", "uid": "plan-uid"},
			"planFingerprint":    builderDigest('4'),
			"approver":           map[string]any{"username": "kubernetes-admin", "groups": []any{"system:masters"}},
			"approvedAt":         "2026-09-28T12:00:00Z",
			"mutationRequestUID": "request-uid",
		},
	}}
}

func TestMigrationApprovalStamped(t *testing.T) {
	t.Parallel()
	spec := func(approval *unstructured.Unstructured) map[string]any {
		return approval.Object["spec"].(map[string]any)
	}
	mtRefusesEach(t, mtApproval,
		func(approval *unstructured.Unstructured) error {
			return migrationApprovalStamped(approval, "e2e-migrations-postgresql", mtPlan())
		},
		map[string]func(*unstructured.Unstructured){
			"no spec": func(a *unstructured.Unstructured) { delete(a.Object, "spec") },
			"a field copied from the plan": func(a *unstructured.Unstructured) {
				spec(a)["artifactDigest"] = mtDigest
			},
			"no approver": func(a *unstructured.Unstructured) { delete(spec(a), "approver") },
			"an anonymous approver": func(a *unstructured.Unstructured) {
				spec(a)["approver"] = map[string]any{"username": ""}
			},
			"no approval time":    func(a *unstructured.Unstructured) { spec(a)["approvedAt"] = nil },
			"no mutation request": func(a *unstructured.Unstructured) { spec(a)["mutationRequestUID"] = "" },
			"another migration": func(a *unstructured.Unstructured) {
				spec(a)["migrationRef"] = map[string]any{"name": "other", "uid": "migration-uid"}
			},
			"another migration UID": func(a *unstructured.Unstructured) {
				spec(a)["migrationRef"] = map[string]any{"name": "e2e-migrations-postgresql", "uid": "other-uid"}
			},
			"another plan": func(a *unstructured.Unstructured) {
				spec(a)["planRef"] = map[string]any{"name": "ptah-mplan-other", "uid": "plan-uid"}
			},
			"another plan UID": func(a *unstructured.Unstructured) {
				spec(a)["planRef"] = map[string]any{"name": "ptah-mplan-abc", "uid": "other-uid"}
			},
			"a plan reference with more in it": func(a *unstructured.Unstructured) {
				spec(a)["planRef"] = map[string]any{"name": "ptah-mplan-abc", "uid": "plan-uid", "namespace": "x"}
			},
			"another plan fingerprint": func(a *unstructured.Unstructured) { spec(a)["planFingerprint"] = builderDigest('9') },
		})
}

func mtSettled() *ptahv1alpha1.PtahMigration {
	finished := metav1.NewTime(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	return &ptahv1alpha1.PtahMigration{Status: ptahv1alpha1.PtahMigrationStatus{
		Phase:    ptahv1alpha1.MigrationPhaseInSync,
		Artifact: &ptahv1alpha1.OCIArtifactAccessBinding{Digest: mtDigest},
		History:  &ptahv1alpha1.MigrationHistoryStatus{ContractVersion: 1, CurrentVersion: 3, AppliedCount: 3},
		LastRun: &ptahv1alpha1.MigrationRunStatus{
			Outcome: ptahv1alpha1.MigrationRunOutcomeApplied, JobName: "ptah-m-apply-x", JobUID: "apply-uid",
			AppliedVersions: []int64{3, 1, 2}, FinishedAt: &finished,
		},
		Conditions: []metav1.Condition{
			mtCondition("Ready", metav1.ConditionTrue, "HistoryMatched"),
			mtCondition("ApprovalRequired", metav1.ConditionFalse, "NotRequired"),
			mtCondition("Blocked", metav1.ConditionFalse, "HistoryMatched"),
		},
	}}
}

func TestSettledInSync(t *testing.T) {
	t.Parallel()
	mtRefusesEach(t, mtSettled,
		func(migration *ptahv1alpha1.PtahMigration) error { return settledInSync(migration, mtDigest) },
		map[string]func(*ptahv1alpha1.PtahMigration){
			"another phase":      func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseReading },
			"another digest":     func(m *ptahv1alpha1.PtahMigration) { m.Status.Artifact.Digest = builderDigest('9') },
			"no artifact":        func(m *ptahv1alpha1.PtahMigration) { m.Status.Artifact = nil },
			"no history":         func(m *ptahv1alpha1.PtahMigration) { m.Status.History = nil },
			"at version 2":       func(m *ptahv1alpha1.PtahMigration) { m.Status.History.CurrentVersion = 2 },
			"two applied":        func(m *ptahv1alpha1.PtahMigration) { m.Status.History.AppliedCount = 2 },
			"one pending":        func(m *ptahv1alpha1.PtahMigration) { m.Status.History.PendingCount = 1 },
			"a dirty history":    func(m *ptahv1alpha1.PtahMigration) { m.Status.History.Dirty = true },
			"a modified version": func(m *ptahv1alpha1.PtahMigration) { m.Status.History.ModifiedVersions = []int64{2} },
			"no run":             func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun = nil },
			"a partial run": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomePartial
			},
			"a run of two versions":     func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.AppliedVersions = []int64{1, 2} },
			"a run of no version":       func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.AppliedVersions = nil },
			"a run naming no Job":       func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.JobName = "" },
			"a run naming no Job UID":   func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.JobUID = "" },
			"a run that never finished": func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.FinishedAt = nil },
			"a plan left": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.Plan = &ptahv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-x"}
			},
			"an operation in flight": func(m *ptahv1alpha1.PtahMigration) {
				m.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{Type: ptahv1alpha1.MigrationOperationHistory}
			},
			"not ready":                func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[0].Status = metav1.ConditionFalse },
			"ready for another reason": func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[0].Reason = "UpToDate" },
			"approval still required":  func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[1].Status = metav1.ConditionTrue },
			"blocked":                  func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[2].Status = metav1.ConditionTrue },
			"no conditions":            func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions = nil },
		})
}

func TestReconciledWithoutWork(t *testing.T) {
	t.Parallel()
	mtRefusesEach(t, mtSettled,
		func(migration *ptahv1alpha1.PtahMigration) error {
			return reconciledWithoutWork(migration, "apply-uid")
		},
		map[string]func(*ptahv1alpha1.PtahMigration){
			"left InSync":      func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseReading },
			"no history":       func(m *ptahv1alpha1.PtahMigration) { m.Status.History = nil },
			"a moved version":  func(m *ptahv1alpha1.PtahMigration) { m.Status.History.CurrentVersion = 4 },
			"work pending":     func(m *ptahv1alpha1.PtahMigration) { m.Status.History.PendingCount = 1 },
			"another run":      func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.JobUID = "second-apply-uid" },
			"no run":           func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun = nil },
			"a plan published": func(m *ptahv1alpha1.PtahMigration) { m.Status.Plan = &ptahv1alpha1.ImmutableObjectReference{Name: "p"} },
		})
}

const (
	mtAwaitingView = `Migration:      ptah-e2e/e2e-migrations-postgresql
Phase:          AwaitingApproval
Artifact:       oci://registry/migrations/postgresql@sha256:11
Current:        0
Pending:        3

Plan ptah-mplan-abc, 3 migrations from version 0:
  1 create widgets
  2 add color
  3 recolor one widget
`
	mtSyncedView = `Migration:      ptah-e2e/e2e-migrations-postgresql
Phase:          InSync
Current:        3
Pending:        0

No plan is published.
Run applied: 1, 2, 3
`
)

// The greps assert_kubectl_ptah_migration ran, held to a view of each phase
// and to the view with each claim broken.
func TestKubectlPtahMigrationView(t *testing.T) {
	t.Parallel()
	check := func(view string, phase ptahv1alpha1.MigrationPhase) error {
		return kubectlPtahMigrationView([]byte(view), "ptah-e2e", "e2e-migrations-postgresql", phase, "ptah-mplan-abc")
	}
	if err := check(mtAwaitingView, ptahv1alpha1.MigrationPhaseAwaitingApproval); err != nil {
		t.Fatalf("the approval gate view was refused: %v", err)
	}
	if err := check(mtSyncedView, ptahv1alpha1.MigrationPhaseInSync); err != nil {
		t.Fatalf("the settled view was refused: %v", err)
	}
	if err := check(strings.Replace(mtSyncedView, "Run applied: 1, 2, 3", "Run applied: 3,1 ,2", 1), ptahv1alpha1.MigrationPhaseInSync); err != nil {
		t.Fatalf("the settled view with the versions in another order was refused: %v", err)
	}
	for _, test := range []struct {
		name, view string
		phase      ptahv1alpha1.MigrationPhase
	}{
		{"another phase", mtAwaitingView, ptahv1alpha1.MigrationPhaseInSync},
		{"the phase indented", strings.Replace(mtSyncedView, "Phase:          InSync", " Phase:          InSync", 1), ptahv1alpha1.MigrationPhaseInSync},
		{"another resource", strings.Replace(mtSyncedView, "e2e-migrations-postgresql", "e2e-migrations-mysql", 1), ptahv1alpha1.MigrationPhaseInSync},
		{"a statement", mtSyncedView + "  CREATE TABLE e2e_migration_widgets\n", ptahv1alpha1.MigrationPhaseInSync},
		{"an update", mtAwaitingView + "update e2e_migration_widgets set color = 'blue'\n", ptahv1alpha1.MigrationPhaseAwaitingApproval},
		{"another plan", strings.Replace(mtAwaitingView, "ptah-mplan-abc", "ptah-mplan-def", 1), ptahv1alpha1.MigrationPhaseAwaitingApproval},
		{"the order reversed", strings.Replace(strings.Replace(mtAwaitingView, "  1 create", "  9 create", 1), "  3 recolor", "  1 recolor", 1), ptahv1alpha1.MigrationPhaseAwaitingApproval},
		{"a migration missing", strings.Replace(mtAwaitingView, "  2 add color\n", "", 1), ptahv1alpha1.MigrationPhaseAwaitingApproval},
		{"no pending count", strings.Replace(mtAwaitingView, "Pending:        3", "Pending:        2", 1), ptahv1alpha1.MigrationPhaseAwaitingApproval},
		{"a plan still shown", strings.Replace(mtSyncedView, "No plan is published.", "Plan ptah-mplan-abc, 3 migrations from version 0:", 1), ptahv1alpha1.MigrationPhaseInSync},
		{"two versions applied", strings.Replace(mtSyncedView, "Run applied: 1, 2, 3", "Run applied: 1, 2", 1), ptahv1alpha1.MigrationPhaseInSync},
		{"a version spelled otherwise", strings.Replace(mtSyncedView, "Run applied: 1, 2, 3", "Run applied: 01, 2, 3", 1), ptahv1alpha1.MigrationPhaseInSync},
		{"a field that is no version", strings.Replace(mtSyncedView, "Run applied: 1, 2, 3", "Run applied: 1, 2, 3, x", 1), ptahv1alpha1.MigrationPhaseInSync},
		{"no run", strings.Replace(mtSyncedView, "Run applied: 1, 2, 3\n", "", 1), ptahv1alpha1.MigrationPhaseInSync},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if check(test.view, test.phase) == nil {
				t.Fatalf("a view with %s was accepted", test.name)
			}
		})
	}
}

func TestJobOperations(t *testing.T) {
	t.Parallel()
	job := func(operation string) batchv1.Job {
		return batchv1.Job{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelOperation: operation}}}
	}
	jobs := []batchv1.Job{job("resolve"), job("verify"), job("history"), job("apply"), job("history"), job("resolve")}
	if got := jobOperations(jobs); got != "apply,history,resolve,verify" {
		t.Fatalf("jobOperations = %s", got)
	}
	if got := jobOperations(jobs[:3]); got != "history,resolve,verify" {
		t.Fatalf("a lifecycle without its Apply reads as %s", got)
	}
}

func TestConsumedPlanRefusal(t *testing.T) {
	t.Parallel()
	for _, message := range []string{
		`admission webhook "vmigrationapproval.operator.ptah.run" denied the request: the plan is no longer current`,
		"the Migration is not awaiting approval",
		"an APPROVAL cannot be accepted while an operation is in flight",
	} {
		if !consumedPlanRefusal.MatchString(message) {
			t.Errorf("%q was not taken for a refusal that says what it refused", message)
		}
	}
	for _, message := range []string{"connection refused", "context deadline exceeded", "Internal error occurred"} {
		if consumedPlanRefusal.MatchString(message) {
			t.Errorf("%q was taken for a refusal that says what it refused", message)
		}
	}
}

func TestConditionSummaryLeavesTheMessageOut(t *testing.T) {
	t.Parallel()
	summary := conditionSummary([]metav1.Condition{{
		Type: "Blocked", Status: metav1.ConditionTrue, Reason: "HistoryDirty", Message: "postgres://user:secret@host",
	}})
	want := []map[string]string{{"type": "Blocked", "status": "True", "reason": "HistoryDirty"}}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(summary, want) || strings.Contains(string(encoded), "secret") {
		t.Fatalf("conditionSummary = %s", encoded)
	}
	if got := conditionSummary(nil); got == nil || len(got) != 0 {
		t.Fatalf("conditionSummary(nil) = %v, want an empty list", got)
	}
}

func TestTrimmedSQLIsTrOfEverySpace(t *testing.T) {
	t.Parallel()
	for output, want := range map[string]string{
		"3\n":            "3",
		" 3 \n":          "3",
		"\t blue\r\n":    "blue",
		"a b\nc\n":       "abc",
		"":               "",
		"\n\n":           "",
		"no whitespace":  "nowhitespace",
		"x\fy\vz":        "xyz",
		"  PgSleep  \n ": "PgSleep",
	} {
		if got := trimmedSQL(output); got != want {
			t.Errorf("trimmedSQL(%q) = %q, want %q", output, got, want)
		}
	}
}

func TestSQLLines(t *testing.T) {
	t.Parallel()
	for output, want := range map[string][]string{
		"1\n2\n3\n":   {"1", "2", "3"},
		"1\n\n3":      {"1", "3"},
		"":            nil,
		"\n":          nil,
		"one row\n\n": {"one row"},
	} {
		if got := sqlLines(output); !slices.Equal(got, want) {
			t.Errorf("sqlLines(%q) = %q, want %q", output, got, want)
		}
	}
}
