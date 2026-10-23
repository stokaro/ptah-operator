package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
)

// sourcePolicyConfigMap is an immutable source policy in the fixture
// namespace.
func sourcePolicyConfigMap(name, uid, document string) *corev1.ConfigMap {
	immutable := true
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: name, UID: types.UID(uid)},
		Immutable:  &immutable,
		Data:       map[string]string{"policy.yaml": document},
	}
}

func withSourcePolicy(spec *operatorv1alpha1.SourceSpec, name string) {
	spec.SourcePolicyFrom = &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "policy.yaml"}
}

func requireRefused(t *testing.T, schema *operatorv1alpha1.PtahSchema, want string) {
	t.Helper()
	failed := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	if schema.Status.Phase != operatorv1alpha1.PhaseFailed || failed == nil || !strings.Contains(failed.Message, want) {
		t.Fatalf("phase %s, failure %#v; want Failed naming %q", schema.Status.Phase, failed, want)
	}
	if schema.Status.Source.Snapshot != nil || schema.Status.Source.Git != nil || schema.Status.Plan != nil {
		t.Fatalf("a refused source left status %#v and plan %#v", schema.Status.Source, schema.Status.Plan)
	}
}

// Each rule refuses the source it names before anything is written or
// dispatched; a policy that allows the source binds into the status.
func TestASourcePolicyRefusesAConfigMapSourceItDoesNotAllow(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		document string
		want     string
	}{
		"another ConfigMap":   {document: "allowedConfigMaps:\n  - other-schema\n", want: "allowedConfigMaps does not name app-schema"},
		"too much content":    {document: "maxContentBytes: 4\n", want: "more than maxContentBytes 4"},
		"no ConfigMap at all": {document: "allowedConfigMaps: []\n", want: "allowedConfigMaps does not name app-schema"},
		"a misspelled rule":   {document: "allowedConfigMap:\n  - app-schema\n", want: "not a policy document"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := configMapSchemaFixture()
			withSourcePolicy(&schema.Spec.Desired, "source-policy")
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, sourceConfigMap("CREATE TABLE a (id int);\n"),
				sourcePolicyConfigMap("source-policy", "policy-uid", row.document))
			actual := reconcileSchema(t, reconciler, api, schema)
			requireRefused(t, actual, row.want)
			if written := snapshots(t, api); len(written) != 0 {
				t.Fatalf("a refused source wrote the snapshot %s", written[0].Name)
			}
		})
	}
	t.Run("a mutable policy", func(t *testing.T) {
		t.Parallel()
		schema := configMapSchemaFixture()
		withSourcePolicy(&schema.Spec.Desired, "source-policy")
		mutable := sourcePolicyConfigMap("source-policy", "policy-uid", "maxContentBytes: 100\n")
		mutable.Immutable = nil
		reconciler, api := fakeReconciler(t, staticLogs{}, schema, sourceConfigMap("x"), mutable)
		requireRefused(t, reconcileSchema(t, reconciler, api, schema), "immutable: true")
	})
	t.Run("an allowed source", func(t *testing.T) {
		t.Parallel()
		schema := configMapSchemaFixture()
		withSourcePolicy(&schema.Spec.Desired, "source-policy")
		reconciler, api := fakeReconciler(t, staticLogs{}, schema, sourceConfigMap("CREATE TABLE a (id int);\n"),
			sourcePolicyConfigMap("source-policy", "policy-uid", "allowedConfigMaps:\n  - app-schema\nmaxContentBytes: 1024\n"))
		actual := reconcileSchema(t, reconciler, api, schema)
		if actual.Status.Phase != operatorv1alpha1.PhaseObserving || actual.Status.Source.SourcePolicy == nil ||
			actual.Status.Source.SourcePolicy.UID != "policy-uid" {
			t.Fatalf("phase %s, source %#v; want Observing with the policy bound", actual.Status.Phase, actual.Status.Source)
		}
		if _, source := sourcebinding.SchemaPlanSource(actual.Status.Source); source == nil || source.Policy == nil ||
			*source.Policy != *actual.Status.Source.SourcePolicy {
			t.Fatalf("a plan would bind %#v, not the policy", source)
		}
	})
}

// Another policy object resolves the source again and retires the plan made
// under the old one; the same policy keeps it.
func TestAReplacedSourcePolicyRetiresThePlan(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		replacement *corev1.ConfigMap
		retired     bool
	}{
		"the same policy": {replacement: nil, retired: false},
		"another object with the same bytes": {
			replacement: sourcePolicyConfigMap("source-policy", "replacement-uid", "maxContentBytes: 1024\n"), retired: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := configMapSchemaFixture()
			withSourcePolicy(&schema.Spec.Desired, "source-policy")
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, sourceConfigMap("CREATE TABLE a (id int);\n"),
				sourcePolicyConfigMap("source-policy", "policy-uid", "maxContentBytes: 1024\n"))
			resolved := reconcileSchema(t, reconciler, api, schema)
			// A plan as one would stand once observation and planning ran.
			resolved.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{Name: "app-plan", UID: "plan-uid", Fingerprint: testDigest}
			resolved.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
			if err := api.Status().Update(context.Background(), resolved); err != nil {
				t.Fatal(err)
			}
			if row.replacement != nil {
				stored := &corev1.ConfigMap{}
				if err := api.Get(context.Background(), client.ObjectKeyFromObject(row.replacement), stored); err != nil {
					t.Fatal(err)
				}
				if err := api.Delete(context.Background(), stored); err != nil {
					t.Fatal(err)
				}
				if err := api.Create(context.Background(), row.replacement); err != nil {
					t.Fatal(err)
				}
			}
			moved, err := sourcePolicyMoved(context.Background(), api, "team-a", resolved.Spec.Desired,
				resolved.Status.Source.SourcePolicy, schemaSourceResolved(resolved))
			if err != nil || moved != row.retired {
				t.Fatalf("sourcePolicyMoved() = %v, %v; want %v", moved, err, row.retired)
			}
			// The Resolve a moved policy claims reads the source again under
			// the policy that holds now.
			current := &operatorv1alpha1.PtahSchema{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), current); err != nil {
				t.Fatal(err)
			}
			if _, err := reconciler.resolveConfigMapSource(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			actual := &operatorv1alpha1.PtahSchema{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
				t.Fatal(err)
			}
			if kept := actual.Status.Plan != nil; kept == row.retired {
				t.Fatalf("plan kept = %v, want %v", kept, !row.retired)
			}
			wantUID := types.UID("policy-uid")
			if row.retired {
				wantUID = "replacement-uid"
			}
			if actual.Status.Source.SourcePolicy == nil || actual.Status.Source.SourcePolicy.UID != wantUID {
				t.Fatalf("policy %#v, want %s", actual.Status.Source.SourcePolicy, wantUID)
			}
		})
	}
}

// A Git host the policy does not allow gets no Resolve Job: the credential
// is never sent there.
func TestASourcePolicyRefusesAGitHostBeforeAnyJob(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	schema.Spec.Desired = operatorv1alpha1.SourceSpec{Git: gitSourceSpec()}
	withSourcePolicy(&schema.Spec.Desired, "source-policy")
	reconciler, api := fakeReconciler(t, staticLogs{}, schema,
		sourcePolicyConfigMap("source-policy", "policy-uid", "allowedGitHosts:\n  - git.internal.example\n"))
	actual := reconcileSchema(t, reconciler, api, schema)
	requireRefused(t, actual, "allowedGitHosts does not name git.example.com")
	if actual.Status.ActiveOperation != nil {
		t.Fatalf("a refused host was claimed: %#v", actual.Status.ActiveOperation)
	}
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("a refused host got %d Jobs: %v", len(jobs.Items), err)
	}
}

// What a Git resolution read is held to the size rule before it is recorded.
func TestASourcePolicyRefusesAGitResolutionThatHoldsTooMuch(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		bytes    int64
		recorded bool
	}{
		"within the bound": {bytes: 100, recorded: true},
		"past the bound":   {bytes: 101, recorded: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := gitResolvingSchema(t, nil)
			withSourcePolicy(&schema.Spec.Desired, "source-policy")
			bindActiveInput(t, schema)
			frame := gitResolveFrameOfSize(t, testDigest, gitCommitA, gitContentOne, row.bytes)
			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod,
				sourcePolicyConfigMap("source-policy", "policy-uid", "maxContentBytes: 100\n"))
			actual := reconcileSchema(t, reconciler, api, schema)
			if recorded := actual.Status.Source.Git != nil; recorded != row.recorded {
				t.Fatalf("recorded %v, want %v: status %#v", recorded, row.recorded, actual.Status)
			}
			if !row.recorded {
				failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
				if failed == nil || !strings.Contains(failed.Message, "more than maxContentBytes 100") {
					t.Fatalf("failure %#v; want the size rule named", failed)
				}
				return
			}
			if actual.Status.Source.SourcePolicy == nil || actual.Status.Source.SourcePolicy.UID != "policy-uid" {
				t.Fatalf("the resolution was recorded without its policy: %#v", actual.Status.Source)
			}
		})
	}
}

func TestAMigrationSourcePolicyIsCheckedAndBound(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		allowed string
		bound   bool
	}{
		"another ConfigMap": {allowed: "inventory-migrations", bound: false},
		"its ConfigMap":     {allowed: "orders-migrations", bound: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			migration.Spec.Artifact = operatorv1alpha1.SourceSpec{ConfigMap: &operatorv1alpha1.ConfigMapSourceSpec{
				Name: "orders-migrations", Items: []operatorv1alpha1.ConfigMapSourceItem{{Key: "0001.up.sql", Path: "0001_init.up.sql"}},
			}}
			withSourcePolicy(&migration.Spec.Artifact, "source-policy")
			source := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-migrations", UID: "configmap-uid"},
				Data:       map[string]string{"0001.up.sql": "CREATE TABLE a (id int);\n"},
			}
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, source,
				sourcePolicyConfigMap("source-policy", "policy-uid", "allowedConfigMaps:\n  - "+row.allowed+"\n"))
			for range 2 {
				if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
			}
			actual := readMigration(t, api, migration)
			if !row.bound {
				if actual.Status.Phase != operatorv1alpha1.MigrationPhaseFailed || actual.Status.Artifact != nil || actual.Status.SourcePolicy != nil {
					t.Fatalf("phase %s, artifact %#v; want Failed with nothing resolved", actual.Status.Phase, actual.Status.Artifact)
				}
				return
			}
			if actual.Status.Artifact == nil || actual.Status.SourcePolicy == nil || actual.Status.SourcePolicy.UID != "policy-uid" {
				t.Fatalf("artifact %#v, policy %#v; want the snapshot and the policy bound", actual.Status.Artifact, actual.Status.SourcePolicy)
			}
			if _, plan := sourcebinding.MigrationPlanSource(actual.Status.Artifact, actual.Status.ConfigMap, actual.Status.SourcePolicy); plan == nil || plan.Policy == nil {
				t.Fatalf("a plan would bind %#v, not the policy", plan)
			}
		})
	}
}

func gitResolveFrameOfSize(t *testing.T, operationID, commit, content string, size int64) []byte {
	t.Helper()
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve,
		OperationID: operationID, ResolvedCommit: commit, ResolvedContentDigest: content, ResolvedContentBytes: size,
	})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}
