package crd_test

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A plan, its chunks and an approval are records: Apply rebuilds the plan from
// its chunks and rehashes it, and an approval authorizes one plan once, so none
// of them may change after it is stored. A realm names one physical database, so its engine may not change
// either. Each rule is a transition rule, which the API server evaluates only on
// update, so each row stores an object first.
//
// The admitted half matters as much as the refusal: `self == oldSelf` on the
// wrong level would freeze the metadata too, and a label is how a person marks
// a record without touching what it says.
func TestImmutableRecordsRefuseEveryChangeToWhatTheyRecord(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "immutable")
	tests := []struct {
		name   string
		object *unstructured.Unstructured
		change func(t *testing.T, object *unstructured.Unstructured)
		want   cause
	}{
		{
			name:   "PtahSchemaPlan destructive flag",
			object: resource("PtahSchemaPlan", namespace, "ptah-plan-71c480df93d6ae2f14efe3c4", schemaPlanSpec()),
			change: setting(true, "spec", "destructive"),
			want:   cause{"spec", "a plan is immutable; generate a new plan instead"},
		},
		{
			name:   "PtahSchemaPlan chunk digest",
			object: resource("PtahSchemaPlan", namespace, "ptah-plan-9c56cc51b374c3ba189210d5", schemaPlanSpec()),
			change: setting(artifactDigest, "spec", "contentDigest"),
			want:   cause{"spec", "a plan is immutable; generate a new plan instead"},
		},
		{
			name:   "PtahSchemaPlanChunk data",
			object: resource("PtahSchemaPlanChunk", namespace, "ptah-plan-71c480df93d6ae2f14efe3c4-000", planChunkSpec()),
			change: setting("eyJmb3JtYXRfdmVyc2lvbiI6Mn0K", "spec", "data"),
			want:   cause{"spec", "a plan chunk is immutable; generate a new plan instead"},
		},
		{
			name:   "PtahMigrationPlan migration list",
			object: resource("PtahMigrationPlan", namespace, "ptah-mplan-19581e27de7ced00ff1ce50b", migrationPlanSpec()),
			change: setting([]any{map[string]any{"version": int64(13), "checksum": contentDigest}}, "spec", "migrations"),
			want:   cause{"spec", "a plan is immutable; generate a new plan instead"},
		},
		{
			name:   "PtahSchemaApproval plan fingerprint",
			object: resource("PtahSchemaApproval", namespace, "approve-application-1", schemaApprovalSpec()),
			change: setting(desiredStateFingerprint, "spec", "planFingerprint"),
			want:   cause{"spec", "an approval is immutable; create a new approval instead"},
		},
		{
			name:   "PtahSchemaApproval approver",
			object: resource("PtahSchemaApproval", namespace, "approve-application-2", schemaApprovalSpec()),
			change: setting("mallory@example.com", "spec", "approver", "username"),
			want:   cause{"spec", "an approval is immutable; create a new approval instead"},
		},
		{
			name:   "PtahMigrationApproval plan reference",
			object: resource("PtahMigrationApproval", namespace, "approve-orders-14", migrationApprovalSpec()),
			change: setting("c5e8a7d2-3b41-4f6e-9a08-1d2c3b4a5e6f", "spec", "planRef", "uid"),
			want:   cause{"spec", "an approval is immutable; create a new approval instead"},
		},
		{
			name:   "PtahRealm engine",
			object: resource("PtahRealm", "", "immutable-engine-realm", realmSpec()),
			change: setting("MySQL", "spec", "engine"),
			want:   cause{"spec.engine", "engine is immutable; a realm is one physical database"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := api.Create(context.Background(), test.object); err != nil {
				t.Fatalf("store %s %s: %v", test.object.GetKind(), test.object.GetName(), err)
			}

			changed := reread(t, test.object)
			test.change(t, changed)
			err := api.Update(context.Background(), changed, client.DryRunAll)
			if mismatch := refusalMismatch(err, test.want); mismatch != nil {
				t.Errorf("update of %s %s: %v", test.object.GetKind(), test.object.GetName(), mismatch)
			}

			labeled := reread(t, test.object)
			labeled.SetLabels(map[string]string{"example.com/reviewed": "true"})
			if err := api.Update(context.Background(), labeled); err != nil {
				t.Fatalf("the API server refused a label on %s %s, which changes nothing the rule guards: %v",
					test.object.GetKind(), test.object.GetName(), err)
			}
			if got := reread(t, test.object).GetLabels()["example.com/reviewed"]; got != "true" {
				t.Fatalf("the label on %s %s reads %q after the update, want \"true\"",
					test.object.GetKind(), test.object.GetName(), got)
			}
		})
	}
}

// A realm's engine is fixed, and the rest of it is the administrator's to
// change: which namespaces may claim the database, and whether they may share
// it.
func TestARealmAdmitsAChangeToWhoMayClaimIt(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	realm := resource("PtahRealm", "", "moving-realm", realmSpec())
	if err := api.Create(context.Background(), realm); err != nil {
		t.Fatalf("store PtahRealm %s: %v", realm.GetName(), err)
	}
	changed := reread(t, realm)
	set(t, changed, []any{"orders-next", "reporting"}, "spec", "namespaces")
	set(t, changed, "Shared", "spec", "sharing")
	if err := api.Update(context.Background(), changed); err != nil {
		t.Fatalf("the API server refused new namespaces and sharing on PtahRealm %s: %v", realm.GetName(), err)
	}
	stored := reread(t, realm)
	if sharing, _, _ := unstructured.NestedString(stored.Object, "spec", "sharing"); sharing != "Shared" {
		t.Fatalf("PtahRealm %s stored sharing %q, want Shared", realm.GetName(), sharing)
	}
}
