package main

import (
	"strings"
	"testing"
)

// A schema that owes proof after an Apply reaches only read-only work, and it
// is the order of the pass that makes that true: the branch returns before the
// engine check, the policy check, suspension, the realm census and the
// generation gate. There is no guard saying so. Move the branch three lines
// down and every one of those refusals runs first, rewriting the resource's
// conditions and phase while the question the record asks is untouched.
//
// I tried to demonstrate this with a reconcile instead, and could not: every
// schema fixture that owes proof also carries a live read-only claim, and the
// refusals I reached for -- an unsupported engine, an invalid exclude selector
// -- break the operation the proof depends on. The test passed while measuring
// nothing, which the mutation showed. Ordering is a property of the source, so
// this reads the source.
//
// The migration family answers the same interleaving with a guard rather than
// an order, since #309, which is why this covers one family and says so.

const schemaReconcileSource = "internal/controller/schema_controller.go"

// refusalsTheProofOutranks are the branches that must not be reached while a
// schema owes proof, each named by something only that branch contains.
var refusalsTheProofOutranks = []struct{ name, marker string }{
	{"the engine support check", "reconcileEngineSupport(ctx, schema)"},
	{"the exclude policy check", "schemaselector.Validate(schema.Spec.Policy.Exclude)"},
	{"the suspension branch", "schema.Spec.Suspend"},
	{"the realm census", "takeRealmCensus("},
	{"the generation gate", "ObservedGeneration != schema.Generation"},
}

const proofBranchMarker = "schema.Status.PendingObservation != nil"

// The proof branch comes before every refusal it outranks.
func TestProofOutranksEveryRefusalInTheSchemaPass(t *testing.T) {
	t.Parallel()
	body := schemaReconcileBody(t)
	proof := strings.Index(body, proofBranchMarker)
	if proof < 0 {
		t.Fatalf("%s no longer reads %s in its pass, so the proof branch is gone or renamed",
			schemaReconcileSource, proofBranchMarker)
	}
	for _, refusal := range refusalsTheProofOutranks {
		at := strings.Index(body, refusal.marker)
		if at < 0 {
			t.Errorf("the pass no longer contains %s; this check cannot say whether proof outranks it",
				refusal.name)
			continue
		}
		if at < proof {
			t.Errorf("%s runs before the pass returns into the proof it owes, so a refusal rewrites the resource while an Apply may still be unaccounted for",
				refusal.name)
		}
	}
}

// schemaReconcileBody returns the body of the schema reconcile method, which
// is the only place this ordering exists.
func schemaReconcileBody(t *testing.T) string {
	t.Helper()
	source := string(readRepositoryFile(t, schemaReconcileSource))
	const signature = "func (r *SchemaReconciler) reconcile("
	start := strings.Index(source, signature)
	if start < 0 {
		t.Fatalf("%s has no %s", schemaReconcileSource, signature)
	}
	end := strings.Index(source[start+len(signature):], "\nfunc ")
	if end < 0 {
		t.Fatalf("the reconcile method is not followed by another function")
	}
	return source[start : start+len(signature)+end]
}
