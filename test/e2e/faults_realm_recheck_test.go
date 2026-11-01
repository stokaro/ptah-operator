package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The reading the shared-alias row failed on: the first claimant still
// refused at 22:20:27Z with the deadline its cycle's Resolve claim had set,
// 23:38:10Z. realmRecheckScheduled has to refuse it, and to accept the same
// reading carrying a deadline a refusal sets.
func TestRealmRecheckScheduled(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", "shared-alias-refusal-deadline.json"))
	if err != nil {
		t.Fatal(err)
	}
	projection := cleanupProjection{}
	if err := json.Unmarshal(raw, &projection); err != nil {
		t.Fatal(err)
	}
	var native *schemaDiagnostic
	for index := range projection.Schemas {
		if projection.Schemas[index].Name == "e2e-fault-pg-alias-a" {
			native = &projection.Schemas[index]
		}
	}
	if native == nil {
		t.Fatalf("the reading holds no e2e-fault-pg-alias-a among %d schemas", len(projection.Schemas))
	}
	// The job printed the projection at this instant, right after collecting it.
	readAt := time.Date(2026, 11, 1, 22, 20, 27, 0, time.UTC)
	if native.Phase == nil || *native.Phase != string(ptahv1alpha1.PhaseBlocked) || native.NextReconciliationTime == nil {
		t.Fatal("the reading is not a refused claimant with a deadline")
	}
	schemaOf := func() *ptahv1alpha1.PtahSchema {
		schema := &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{
			Name: native.Name, UID: native.UID, Generation: native.Generation,
		}}
		schema.Status.ObservedGeneration = native.ObservedGeneration
		schema.Status.Phase = ptahv1alpha1.ReconciliationPhase(*native.Phase)
		next := *native.NextReconciliationTime
		schema.Status.NextReconciliationTime = &next
		return schema
	}
	accepts := func(schema *ptahv1alpha1.PtahSchema) bool { return realmRecheckScheduled(schema, readAt) }
	if accepts(schemaOf()) {
		t.Fatalf("the predicate accepted the reading the row failed on: deadline %s, read at %s",
			native.NextReconciliationTime.UTC().Format(time.RFC3339), readAt.Format(time.RFC3339))
	}

	deadline := func(offset time.Duration) func(*ptahv1alpha1.PtahSchema) {
		return func(schema *ptahv1alpha1.PtahSchema) {
			next := metav1.NewTime(readAt.Add(offset))
			schema.Status.NextReconciliationTime = &next
		}
	}
	// The latest deadline a refusal written at the instant of the read sets.
	fresh := func() *ptahv1alpha1.PtahSchema {
		schema := schemaOf()
		deadline(realmRecheckBound)(schema)
		return schema
	}
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"the deadline the failing run carried", func(schema *ptahv1alpha1.PtahSchema) {
			next := *native.NextReconciliationTime
			schema.Status.NextReconciliationTime = &next
		}},
		{"no deadline", func(schema *ptahv1alpha1.PtahSchema) { schema.Status.NextReconciliationTime = nil }},
		{"a second past the bound and its slack", deadline(realmRecheckBound + realmRecheckSlack + time.Second)},
		{"the next refresh of an hourly cycle", deadline(time.Hour)},
		{"the default interval", deadline(10 * time.Minute)},
	})
	ftAcceptsEach(t, fresh, accepts, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"a deadline already due", deadline(-5 * time.Second)},
		{"a deadline stamped before the read", deadline(30 * time.Second)},
		{"a deadline at the end of the slack", deadline(realmRecheckBound + realmRecheckSlack)},
	})
}
