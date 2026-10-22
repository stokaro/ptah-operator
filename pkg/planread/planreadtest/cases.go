package planreadtest

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/pkg/planread"
)

// Case is one stored state and the answer a plan reader owes for it.
type Case struct {
	// Name is a short identifier, stable across releases of this package.
	Name string
	// Description says what the state is and why the answer is what it is.
	Description string
	// Schema and Selection are what the reader is asked for, in Namespace.
	Schema    string
	Selection planread.Selection
	// Objects are what the API server holds. Load them with NewClient.
	Objects []client.Object
	Want    Want
}

// Want is the answer. Err is nil for a plan that reads, and otherwise the
// planread error the reader's error must match with errors.Is, and no other.
type Want struct {
	Err error
	// PlanName, Document and Statements describe a plan that reads.
	PlanName   string
	Document   []byte
	Statements []string
}

// Cases builds every case. Each starts from its own API server.
func Cases(t testing.TB) []Case {
	t.Helper()
	cases := []Case{
		spansChunks(t),
		appliedIsNotTheNewest(t),
		currentIsNotTheApplied(t),
		{
			Name:        "no-such-schema",
			Description: "The schema does not exist.",
			Schema:      "absent",
			Selection:   planread.Current,
			Objects:     NewLab(t, "storefront").Objects(t),
			Want:        Want{Err: planread.ErrSchemaNotFound},
		},
	}
	cases = append(cases, absentPlans(t)...)
	cases = append(cases, refusedPlans(t)...)
	return cases
}

// A plan larger than one chunk is one document, not one document per chunk.
func spansChunks(t testing.TB) Case {
	lab := NewLab(t, "storefront")
	statements := SpanningStatements()
	plan, document := lab.Publish(t, statements, "v1")
	if len(plan.Spec.Chunks) < 2 {
		t.Fatalf("the spanning plan fits in %d chunk(s), so the case measures nothing", len(plan.Spec.Chunks))
	}
	lab.SetCurrent(t, plan)
	return Case{
		Name:        "spans-chunks",
		Description: "A plan stored in several chunks, with a quote, an escape and a multi-byte character near the boundary, reads as one document.",
		Schema:      "storefront",
		Selection:   planread.Current,
		Objects:     lab.Objects(t),
		Want:        Want{PlanName: plan.Name, Document: document, Statements: statements},
	}
}

// Current and applied are two questions. Neither is "the newest plan object".
func twoPlans(t testing.TB) (*Lab, [2]*operatorv1alpha1.PtahSchemaPlan, [2][]byte, [2][]string) {
	lab := NewLab(t, "storefront")
	older := []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}
	newer := []string{"ALTER TABLE customers ADD COLUMN email TEXT"}
	first, firstDocument := lab.Publish(t, older, "v1")
	second, secondDocument := lab.Publish(t, newer, "v2")
	lab.SetApplied(t, first)
	lab.SetCurrent(t, second)
	return lab, [2]*operatorv1alpha1.PtahSchemaPlan{first, second},
		[2][]byte{firstDocument, secondDocument}, [2][]string{older, newer}
}

func appliedIsNotTheNewest(t testing.TB) Case {
	lab, plans, documents, statements := twoPlans(t)
	return Case{
		Name:        "applied-is-not-the-newest",
		Description: "The applied plan is the older one; a reader choosing by age would answer with the newer.",
		Schema:      "storefront",
		Selection:   planread.Applied,
		Objects:     lab.Objects(t),
		Want:        Want{PlanName: plans[0].Name, Document: documents[0], Statements: statements[0]},
	}
}

func currentIsNotTheApplied(t testing.TB) Case {
	lab, plans, documents, statements := twoPlans(t)
	return Case{
		Name:        "current-is-not-the-applied",
		Description: "The current plan is what the operator would run next, not what it last ran.",
		Schema:      "storefront",
		Selection:   planread.Current,
		Objects:     lab.Objects(t),
		Want:        Want{PlanName: plans[1].Name, Document: documents[1], Statements: statements[1]},
	}
}

// Absence is an answer, not an empty plan.
func absentPlans(t testing.TB) []Case {
	nothingApplied := NewLab(t, "storefront")
	plan, _ := nothingApplied.Publish(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	nothingApplied.SetCurrent(t, plan)

	noCurrent := NewLab(t, "storefront")
	plan, _ = noCurrent.Publish(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	noCurrent.SetApplied(t, plan)

	gone := NewLab(t, "storefront")
	plan, _ = gone.Publish(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	gone.SetApplied(t, plan)
	gone.DeletePlan(t, plan)

	return []Case{
		{
			Name:        "nothing-applied",
			Description: "A schema with a current plan and no confirmed apply is told so, not handed the plan it would run next.",
			Schema:      "storefront", Selection: planread.Applied,
			Objects: nothingApplied.Objects(t), Want: Want{Err: planread.ErrNoPlan},
		},
		{
			Name:        "no-current-plan",
			Description: "A schema whose status names no current plan.",
			Schema:      "storefront", Selection: planread.Current,
			Objects: noCurrent.Objects(t), Want: Want{Err: planread.ErrNoPlan},
		},
		{
			Name:        "applied-plan-gone",
			Description: "The applied record names a plan that has been deleted.",
			Schema:      "storefront", Selection: planread.Applied,
			Objects: gone.Objects(t), Want: Want{Err: planread.ErrNoPlan},
		},
	}
}

// A stored plan that fails a check is refused whole.
func refusedPlans(t testing.TB) []Case {
	one := []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}
	stored := func(arrange func(lab *Lab, plan *operatorv1alpha1.PtahSchemaPlan)) []client.Object {
		lab := NewLab(t, "storefront")
		plan, _ := lab.Publish(t, one, "v1")
		lab.SetCurrent(t, plan)
		arrange(lab, plan)
		return lab.Objects(t)
	}

	foreign := NewLab(t, "storefront")
	warehouse := foreign.AddSchema(t, "warehouse")
	pallets, _ := warehouse.Publish(t, []string{"CREATE TABLE pallets (id BIGINT PRIMARY KEY)"}, "v1")
	foreign.SetApplied(t, pallets)

	future := NewLab(t, "storefront")
	future.SetCurrent(t, future.PublishDocument(t, FutureDocument(t), "v9"))

	return []Case{
		{
			Name:        "recreated-under-the-same-name",
			Description: "The plan the status names was deleted and another object stored under its name. A name is reused; a UID is not.",
			Schema:      "storefront", Selection: planread.Current,
			Objects: stored(func(lab *Lab, plan *operatorv1alpha1.PtahSchemaPlan) { lab.Recreate(t, plan) }),
			Want:    Want{Err: planread.ErrIntegrity},
		},
		{
			Name:        "corrupted-chunk",
			Description: "A chunk's bytes no longer match the digest the plan records.",
			Schema:      "storefront", Selection: planread.Current,
			Objects: stored(func(lab *Lab, plan *operatorv1alpha1.PtahSchemaPlan) {
				lab.EditChunk(t, plan, func(chunk *operatorv1alpha1.PtahSchemaPlanChunk) { chunk.Spec.Data[0] ^= 0xff })
			}),
			Want: Want{Err: planread.ErrIntegrity},
		},
		{
			Name:        "missing-chunk",
			Description: "The plan's storage was committed and one of its chunks is gone.",
			Schema:      "storefront", Selection: planread.Current,
			Objects: stored(func(lab *Lab, plan *operatorv1alpha1.PtahSchemaPlan) { lab.DeleteChunk(t, plan) }),
			Want:    Want{Err: planread.ErrIntegrity},
		},
		{
			Name:        "chunk-of-another-owner",
			Description: "A chunk under the right name whose owner reference names another object.",
			Schema:      "storefront", Selection: planread.Current,
			Objects: stored(func(lab *Lab, plan *operatorv1alpha1.PtahSchemaPlan) {
				lab.EditChunk(t, plan, func(chunk *operatorv1alpha1.PtahSchemaPlanChunk) {
					chunk.OwnerReferences[0].UID = types.UID("uid-someone-else")
				})
			}),
			Want: Want{Err: planread.ErrIntegrity},
		},
		{
			Name:        "publication-unfinished",
			Description: "Publication is three writes that are not a transaction; only the last says the first two finished, and it is missing.",
			Schema:      "storefront", Selection: planread.Current,
			Objects: stored(func(lab *Lab, plan *operatorv1alpha1.PtahSchemaPlan) { lab.Uncommit(t, plan) }),
			Want:    Want{Err: planread.ErrIntegrity},
		},
		{
			Name:        "plan-of-another-schema",
			Description: "The record names, by exact name and UID, a plan of another schema in the same namespace.",
			Schema:      "storefront", Selection: planread.Applied,
			Objects: foreign.Objects(t),
			Want:    Want{Err: planread.ErrIntegrity},
		},
		{
			Name:        "unknown-document-format",
			Description: "A plan document from a build that is not this one is refused, not read on the assumption that the parts it recognizes are enough.",
			Schema:      "storefront", Selection: planread.Current,
			Objects: future.Objects(t),
			Want:    Want{Err: planread.ErrUnsupported},
		},
		{
			Name:        "unknown-plan-contract",
			Description: "A plan that records a plan contract this build does not read.",
			Schema:      "storefront", Selection: planread.Current,
			Objects: stored(func(lab *Lab, plan *operatorv1alpha1.PtahSchemaPlan) {
				lab.EditManifest(t, plan, func(spec *operatorv1alpha1.PtahSchemaPlanSpec) {
					spec.ContractVersion = fingerprint.SourcePlanContractVersion + 1
				})
			}),
			Want: Want{Err: planread.ErrUnsupported},
		},
	}
}
