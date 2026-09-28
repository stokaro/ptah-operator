package webhook_test

import (
	"context"
	"maps"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const acknowledgmentWebhook = "mmigrationrunacknowledgment.operator.ptah.run"

// acknowledgmentFixture is a migration holding a run nobody accounted for, and
// the grant an approver needs to acknowledge it.
type acknowledgmentFixture struct {
	namespace string
	migration *operatorv1alpha1.PtahMigration
}

func createMigration(t *testing.T, namespace, name string) *operatorv1alpha1.PtahMigration {
	t.Helper()
	ctx := context.Background()
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": operatorv1alpha1.GroupVersion.String(),
		"kind":       "PtahMigration",
		"metadata":   map[string]any{"namespace": namespace, "name": name},
		"spec": map[string]any{
			"target": map[string]any{
				"engine":          "PostgreSQL",
				"coordinationKey": "production/" + name + "-primary",
				"urlFrom":         map[string]any{"name": name + "-database", "key": "url"},
			},
			"artifact": map[string]any{
				"ociRef":                 "oci://ghcr.io/example/" + name + "-migrations:1.0.0",
				"verificationPolicyFrom": map[string]any{"name": verificationPolicyName, "key": verificationPolicyKey},
			},
			"execution": map[string]any{"serviceAccountName": executionAccount},
		},
	}}
	if err := admin.Create(ctx, object); err != nil {
		t.Fatalf("create PtahMigration %s/%s: %v", namespace, name, err)
	}
	migration := &operatorv1alpha1.PtahMigration{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(object), migration); err != nil {
		t.Fatalf("read PtahMigration %s/%s back: %v", namespace, name, err)
	}
	return migration
}

func newAcknowledgmentFixture(t *testing.T) acknowledgmentFixture {
	t.Helper()
	namespace := newNamespace(t, "acknowledgments")
	migration := createMigration(t, namespace, "ledger")
	migration.Status = operatorv1alpha1.PtahMigrationStatus{
		ObservedGeneration: migration.Generation,
		Phase:              operatorv1alpha1.MigrationPhaseBlocked,
		ExecutionBinding:   executionBinding(),
		UnresolvedRun: &operatorv1alpha1.UnresolvedMigrationRunStatus{
			Outcome:     operatorv1alpha1.MigrationRunOutcomeUnknown,
			OperationID: digest("7"),
			PlanRef:     operatorv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-0123456789abcdef01234567", UID: "plan-uid"},
			RecordedAt:  now(),
		},
	}
	writeStatus(t, migration)

	grant(t, namespace, "acknowledgers", rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: approverRole},
		rbacv1.PolicyRule{
			APIGroups: []string{operatorv1alpha1.GroupVersion.Group},
			Resources: []string{"ptahmigrationrunacknowledgments"},
			Verbs:     []string{"create", "get"},
		})
	return acknowledgmentFixture{namespace: namespace, migration: migration}
}

// acknowledgment is what a person writes: which migration, and which run.
func (fixture acknowledgmentFixture) acknowledgment(name, operationID string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": operatorv1alpha1.GroupVersion.String(),
		"kind":       "PtahMigrationRunAcknowledgment",
		"metadata":   map[string]any{"namespace": fixture.namespace, "name": name},
		"spec": map[string]any{
			"migrationRef": map[string]any{"name": fixture.migration.Name, "uid": string(fixture.migration.UID)},
			"operationID":  operationID,
		},
	}}
}

func TestRunAcknowledgmentWebhooks(t *testing.T) {
	plane.Require(t)
	fixture := newAcknowledgmentFixture(t)
	ctx := context.Background()
	recorded := fixture.migration.Status.UnresolvedRun.OperationID

	t.Run("an acknowledgment is stamped with the authenticated identity", func(t *testing.T) {
		t.Parallel()
		acknowledgment := fixture.acknowledgment("ledger-run-accounted-for", recorded)
		// Nobody acknowledges in somebody else's name: the stored identity
		// is the one the API server authenticated.
		if err := unstructured.SetNestedField(acknowledgment.Object, map[string]any{
			"username": "someone-else", "uid": "forged", "groups": []any{"system:masters"},
		}, "spec", "acknowledgedBy"); err != nil {
			t.Fatal(err)
		}
		if err := approverClient(t).Create(ctx, acknowledgment); err != nil {
			t.Fatalf("create PtahMigrationRunAcknowledgment %s as %s: %v",
				client.ObjectKeyFromObject(acknowledgment), approverName, err)
		}
		stored := &operatorv1alpha1.PtahMigrationRunAcknowledgment{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(acknowledgment), stored); err != nil {
			t.Fatalf("read PtahMigrationRunAcknowledgment %s back: %v", client.ObjectKeyFromObject(acknowledgment), err)
		}
		spec := stored.Spec
		wantGroups := []string{approverRole, "system:authenticated"}
		if spec.AcknowledgedBy.Username != approverName || spec.AcknowledgedBy.UID != approverUID ||
			!slices.Equal(spec.AcknowledgedBy.Groups, wantGroups) {
			t.Fatalf("stored acknowledger = %+v, want %s (uid %s) in %v", spec.AcknowledgedBy, approverName, approverUID, wantGroups)
		}
		if spec.AcknowledgedAt.IsZero() || spec.MutationRequestUID == "" {
			t.Fatalf("stored acknowledgment has acknowledgedAt %v and mutationRequestUID %q; the mutating webhook stamps both",
				spec.AcknowledgedAt, spec.MutationRequestUID)
		}
		if spec.OperationID != recorded || spec.MigrationRef.UID != fixture.migration.UID {
			t.Fatalf("stored decision = %s/%s, want %s/%s", spec.MigrationRef.UID, spec.OperationID, fixture.migration.UID, recorded)
		}
		raw := &unstructured.Unstructured{}
		raw.SetGroupVersionKind(operatorv1alpha1.GroupVersion.WithKind("PtahMigrationRunAcknowledgment"))
		if err := admin.Get(ctx, client.ObjectKeyFromObject(acknowledgment), raw); err != nil {
			t.Fatalf("read the acknowledgment back unstructured: %v", err)
		}
		storedSpec, _, _ := unstructured.NestedMap(raw.Object, "spec")
		fields := slices.Sorted(maps.Keys(storedSpec))
		wantFields := []string{"acknowledgedAt", "acknowledgedBy", "migrationRef", "mutationRequestUID", "operationID"}
		if !slices.Equal(fields, wantFields) {
			t.Fatalf("stored acknowledgment spec carries %v, want exactly %v", fields, wantFields)
		}

		// A label is how a person marks it reviewed; the decision stays.
		stored.Labels = map[string]string{"reviewed": "true"}
		if err := admin.Update(ctx, stored); err != nil {
			t.Fatalf("a metadata-only update of %s was refused: %v", client.ObjectKeyFromObject(stored), err)
		}
	})

	t.Run("an acknowledgment of another run is refused", func(t *testing.T) {
		t.Parallel()
		err := approverClient(t).Create(ctx, fixture.acknowledgment("ledger-other-run", digest("8")))
		requireDenied(t, err, acknowledgmentWebhook, "not the one this acknowledgment names")
	})

	t.Run("an acknowledgment of a migration recreated under the same name is refused", func(t *testing.T) {
		t.Parallel()
		acknowledgment := fixture.acknowledgment("ledger-recreated", recorded)
		if err := unstructured.SetNestedField(acknowledgment.Object, "a-migration-that-was-deleted", "spec", "migrationRef", "uid"); err != nil {
			t.Fatal(err)
		}
		err := approverClient(t).Create(ctx, acknowledgment)
		requireDenied(t, err, acknowledgmentWebhook, "referenced migration UID does not match")
	})

	t.Run("an acknowledgment of a migration with nothing unresolved is refused", func(t *testing.T) {
		t.Parallel()
		settled := createMigration(t, fixture.namespace, "ledger-settled")
		acknowledgment := fixture.acknowledgment("ledger-settled-run", recorded)
		if err := unstructured.SetNestedField(acknowledgment.Object, map[string]any{
			"name": settled.Name, "uid": string(settled.UID),
		}, "spec", "migrationRef"); err != nil {
			t.Fatal(err)
		}
		err := approverClient(t).Create(ctx, acknowledgment)
		requireDenied(t, err, acknowledgmentWebhook, "records no unresolved run")
	})
}
