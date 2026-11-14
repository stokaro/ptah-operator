package crd_test

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// driftCheckedMigrationBase is the migration base with a drift check whose
// shadow database is a Secret of its own.
func driftCheckedMigrationBase(namespace string) func() *unstructured.Unstructured {
	return func() *unstructured.Unstructured {
		migration := migrationBase(namespace)()
		if err := unstructured.SetNestedMap(migration.Object, map[string]any{
			"shadow": map[string]any{"urlFrom": map[string]any{"name": "application-shadow", "key": "url"}},
		}, "spec", "policy", "driftCheck"); err != nil {
			panic(err)
		}
		return migration
	}
}

// A drift check names its shadow database by a required Secret key, and never
// by the key the target is read from: the check replays the applied
// migrations into the shadow after dropping everything in it.
func TestPtahMigrationDriftCheckRefusals(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "drift-check-refusals")
	const required = "policy.driftCheck.shadow.urlFrom must name a required Secret key"
	assertRefusals(t, driftCheckedMigrationBase(namespace), []refusal{
		{
			name:   "no shadow",
			mutate: setting(map[string]any{}, "spec", "policy", "driftCheck"),
			want:   []cause{{"spec.policy.driftCheck.shadow", "Required value"}},
		},
		{
			name:   "a shadow without a Secret",
			mutate: setting(map[string]any{}, "spec", "policy", "driftCheck", "shadow"),
			want:   []cause{{"spec.policy.driftCheck.shadow.urlFrom", "Required value"}},
		},
		{
			name:   "an optional shadow Secret",
			mutate: setting(true, "spec", "policy", "driftCheck", "shadow", "urlFrom", "optional"),
			want:   []cause{{"spec", required}},
		},
		{
			name:   "a shadow Secret with no name",
			mutate: removing("spec", "policy", "driftCheck", "shadow", "urlFrom", "name"),
			want:   []cause{{"spec", required}},
		},
		{
			name:   "a shadow Secret key that is empty",
			mutate: setting("", "spec", "policy", "driftCheck", "shadow", "urlFrom", "key"),
			want:   []cause{{"spec", required}},
		},
		{
			name: "the target's own Secret key",
			mutate: setting(map[string]any{"name": "application-database", "key": "url"},
				"spec", "policy", "driftCheck", "shadow", "urlFrom"),
			want: []cause{{"spec", "policy.driftCheck.shadow.urlFrom must name another Secret key than target.urlFrom"}},
		},
	})
}

// The rule that keeps the shadow off the target's key compares the Secret and
// the key together, so another key of the same Secret, or the same key of
// another Secret, is a shadow of its own.
func TestADriftCheckAdmitsAShadowBesideItsTarget(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "drift-check-admitted")
	for _, test := range []struct {
		name   string
		secret map[string]any
	}{
		{name: "another key of the target's Secret", secret: map[string]any{"name": "application-database", "key": "shadow-url"}},
		{name: "the target's key in another Secret", secret: map[string]any{"name": "application-shadow", "key": "url"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration := driftCheckedMigrationBase(namespace)()
			set(t, migration, test.secret, "spec", "policy", "driftCheck", "shadow", "urlFrom")
			if err := dryRunCreate(migration); err != nil {
				t.Fatalf("the API server refused a shadow at %v: %v", test.secret, err)
			}
		})
	}
}

// The history read records the shadow's identity as a digest, the way it
// records the target's.
func TestStatusHoldsTheShadowIdentityToADigest(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "drift-check-history")
	migration := driftCheckedMigrationBase(namespace)()
	if err := api.Create(context.Background(), migration); err != nil {
		t.Fatalf("store the migration: %v", err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	history := func(shadow string) map[string]any {
		return map[string]any{
			"observedAt": "2026-10-24T07:43:00Z", "contractVersion": int64(1), "currentVersion": int64(0),
			"appliedCount": int64(0), "pendingCount": int64(1),
			"fingerprint": digest, "targetIdentityDigest": digest, "shadowIdentityDigest": shadow,
		}
	}

	stored := reread(t, migration)
	set(t, stored, history("sha256:"+strings.Repeat("b", 64)), "status", "history")
	if err := api.Status().Update(context.Background(), stored, client.DryRunAll); err != nil {
		t.Fatalf("the status refused a shadow identity that is a digest: %v", err)
	}
	refused := reread(t, migration)
	set(t, refused, history("postgres://shadow.example/ledger"), "status", "history")
	err := api.Status().Update(context.Background(), refused, client.DryRunAll)
	if mismatch := refusalMismatch(err, cause{"status.history.shadowIdentityDigest", "should match"}); mismatch != nil {
		t.Error(mismatch)
	}
}

// A plan binds the shadow's identity as a digest too, and a plan that binds
// one is stored.
func TestAPlanBindsTheShadowIdentityAsADigest(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "drift-check-plan")
	plan := resource("PtahMigrationPlan", namespace, "ptah-mplan-19581e27de7ced00ff1ce50b", migrationPlanSpec())
	set(t, plan, "sha256:"+strings.Repeat("b", 64), "spec", "shadowIdentityDigest")
	if err := dryRunCreate(plan); err != nil {
		t.Fatalf("the API server refused a plan binding a shadow identity: %v", err)
	}
	set(t, plan, "application-shadow", "spec", "shadowIdentityDigest")
	if mismatch := refusalMismatch(dryRunCreate(plan), cause{"spec.shadowIdentityDigest", "should match"}); mismatch != nil {
		t.Error(mismatch)
	}
}
