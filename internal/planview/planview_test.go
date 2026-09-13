package planview_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"

	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/planview"
)

// A plan larger than one chunk is one document, not one document per chunk.
//
// The store splits the serialized plan by bytes, so a boundary falls wherever
// 512 KiB falls: inside a SQL string, inside a JSON escape, inside a UTF-8
// character. Reading each chunk on its own is how that split becomes visible to
// a reader as a parse error, and it is what this exists to make unnecessary.
func TestLoadReadsAPlanThatSpansChunks(t *testing.T) {
	t.Parallel()
	lab := newLab(t, "storefront")
	// Long enough to cross a boundary, and made of the characters a boundary
	// must not be allowed to cut: a quote, a backslash escape and a multi-byte
	// character all sit inside the SQL.
	filler := strings.Repeat(`x'\"й`, planstore.ChunkBytes/8)
	statements := []string{
		"CREATE TABLE customers (id BIGINT PRIMARY KEY)",
		"COMMENT ON TABLE customers IS '" + filler + "'",
		"CREATE INDEX customers_email ON customers (email)",
	}
	plan, document := lab.store(t, statements, "v1")
	if len(plan.Spec.Chunks) < 2 {
		t.Fatalf("the plan fits in %d chunk(s), so this measured nothing", len(plan.Spec.Chunks))
	}
	lab.current(t, plan)

	view, err := planview.Load(context.Background(), lab.reader(), "application", "storefront", planview.Current)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !bytes.Equal(view.Document, document) {
		t.Fatal("the document read back is not the document that was stored")
	}
	if view.StatementCount != len(statements) {
		t.Fatalf("the plan carries %d statements, want %d", view.StatementCount, len(statements))
	}
	var rendered bytes.Buffer
	if err := planview.Render(&rendered, view, planview.SQL); err != nil {
		t.Fatalf("Render(sql) error = %v", err)
	}
	got := sqlOf(rendered.String())
	if len(got) != len(statements) {
		t.Fatalf("rendered %d statements, want %d", len(got), len(statements))
	}
	for index, statement := range statements {
		if got[index] != statement {
			t.Fatalf("statement %d came back as %.60q..., want %.60q...", index, got[index], statement)
		}
	}
}

// Current and applied are two questions. Neither is "the newest plan object".
func TestLoadKeepsCurrentAndAppliedApart(t *testing.T) {
	t.Parallel()
	lab := newLab(t, "storefront")
	first, _ := lab.store(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	second, _ := lab.store(t, []string{"ALTER TABLE customers ADD COLUMN email TEXT"}, "v2")
	// Applied is the older plan, and the newer one is what the operator would
	// run next: choosing by age would answer both questions wrong.
	lab.applied(t, first)
	lab.current(t, second)

	applied, err := planview.Load(context.Background(), lab.reader(), "application", "storefront", planview.Applied)
	if err != nil {
		t.Fatalf("Load(applied) error = %v", err)
	}
	if applied.PlanName != first.Name {
		t.Fatalf("applied resolved to %s, and the applied record names %s", applied.PlanName, first.Name)
	}
	if applied.CompletedAt == nil {
		t.Fatal("the applied view does not say when the apply finished")
	}

	current, err := planview.Load(context.Background(), lab.reader(), "application", "storefront", planview.Current)
	if err != nil {
		t.Fatalf("Load(current) error = %v", err)
	}
	if current.PlanName != second.Name {
		t.Fatalf("current resolved to %s, and the status names %s", current.PlanName, second.Name)
	}
	if current.CompletedAt != nil {
		t.Fatal("the current view claims an apply that has not happened")
	}
}

// A record written before the reference existed is still readable, by the
// fingerprint it does carry and the schema it was written for.
func TestLoadFindsTheAppliedPlanOfARecordWithNoReference(t *testing.T) {
	t.Parallel()
	lab := newLab(t, "storefront")
	plan, _ := lab.store(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	// A second plan of the same schema, so the search has something to be
	// wrong about.
	lab.store(t, []string{"ALTER TABLE customers ADD COLUMN email TEXT"}, "v2")
	lab.appliedBeforeTheReference(t, plan)

	view, err := planview.Load(context.Background(), lab.reader(), "application", "storefront", planview.Applied)
	if err != nil {
		t.Fatalf("Load(applied) error = %v", err)
	}
	if view.PlanName != plan.Name {
		t.Fatalf("the fallback resolved to %s, want %s", view.PlanName, plan.Name)
	}
}

// Absence is an answer. A schema with nothing applied is told so, rather than
// handed the plan it would run next.
func TestLoadFailurePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		arrange   func(t *testing.T, lab *lab)
		selection planview.Selection
		schema    string
		want      error
		message   string
	}{
		{
			name:      "no such schema",
			arrange:   func(t *testing.T, lab *lab) {},
			selection: planview.Current,
			schema:    "absent",
			want:      planview.ErrSchemaNotFound,
		},
		{
			name:      "nothing applied",
			arrange:   func(t *testing.T, lab *lab) { lab.current(t, first(t, lab)) },
			selection: planview.Applied,
			schema:    "storefront",
			want:      planview.ErrNoPlan,
			message:   "records no confirmed apply",
		},
		{
			name:      "no current plan",
			arrange:   func(t *testing.T, lab *lab) { lab.applied(t, first(t, lab)) },
			selection: planview.Current,
			schema:    "storefront",
			want:      planview.ErrNoPlan,
			message:   "has no current plan",
		},
		{
			name: "the applied plan is gone",
			arrange: func(t *testing.T, lab *lab) {
				plan := first(t, lab)
				lab.applied(t, plan)
				if err := lab.client.Delete(context.Background(), plan); err != nil {
					t.Fatalf("delete the plan: %v", err)
				}
			},
			selection: planview.Applied,
			schema:    "storefront",
			want:      planview.ErrNoPlan,
			message:   "is gone",
		},
		{
			name: "a record with no reference and no matching plan",
			arrange: func(t *testing.T, lab *lab) {
				plan := first(t, lab)
				lab.appliedBeforeTheReference(t, plan)
				if err := lab.client.Delete(context.Background(), plan); err != nil {
					t.Fatalf("delete the plan: %v", err)
				}
			},
			selection: planview.Applied,
			schema:    "storefront",
			want:      planview.ErrNoPlan,
			message:   "no stored plan",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lab := newLab(t, "storefront")
			test.arrange(t, lab)

			view, err := planview.Load(context.Background(), lab.reader(), "application", test.schema, test.selection)
			if !errors.Is(err, test.want) {
				t.Fatalf("Load() error = %v, want %v", err, test.want)
			}
			if test.message != "" && !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Load() said %q, which does not carry %q", err, test.message)
			}
			if len(view.Document) != 0 {
				t.Fatal("Load() returned a document with its error")
			}
		})
	}
}

// A name is reused when an object is recreated, and a UID is not.
func TestLoadRefusesAPlanRecreatedUnderTheSameName(t *testing.T) {
	t.Parallel()
	lab := newLab(t, "storefront")
	plan, _ := lab.store(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	lab.applied(t, plan)
	// The same name, a different object: what the record named is gone.
	replaced := plan.DeepCopy()
	if err := lab.client.Delete(context.Background(), plan); err != nil {
		t.Fatalf("delete the plan: %v", err)
	}
	replaced.ResourceVersion = ""
	replaced.UID = types.UID("uid-recreated")
	if err := lab.client.Create(context.Background(), replaced); err != nil {
		t.Fatalf("recreate the plan: %v", err)
	}

	view, err := planview.Load(context.Background(), lab.reader(), "application", "storefront", planview.Applied)
	if err == nil {
		t.Fatal("Load() accepted a different object under the name the record holds")
	}
	if !strings.Contains(err.Error(), "different object under the same name") {
		t.Fatalf("Load() said %q", err)
	}
	if len(view.Document) != 0 {
		t.Fatal("Load() returned a document with its error")
	}
}

// The store's checks are the point of reading through it. A chunk that no
// longer matches its digest ends the read, and nothing of the plan comes back.
func TestLoadRefusesACorruptedChunkAndReturnsNothing(t *testing.T) {
	t.Parallel()
	lab := newLab(t, "storefront")
	plan, _ := lab.store(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	lab.current(t, plan)

	chunk := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: "application", Name: plan.Spec.Chunks[0].Name}
	if err := lab.client.Get(context.Background(), key, chunk); err != nil {
		t.Fatalf("read the chunk: %v", err)
	}
	// Immutable in a cluster; the point here is what a reader is told when the
	// bytes no longer match what the plan says they are.
	chunk.BinaryData[plan.Spec.Chunks[0].Key][0] ^= 0xff
	if err := lab.client.Update(context.Background(), chunk); err != nil {
		t.Fatalf("rewrite the chunk: %v", err)
	}

	view, err := planview.Load(context.Background(), lab.reader(), "application", "storefront", planview.Current)
	if err == nil {
		t.Fatal("Load() accepted a chunk that does not match its digest")
	}
	if len(view.Document) != 0 {
		t.Fatal("Load() returned a document with its error")
	}
}

// The command reads three kinds of object and no others. A Secret, a Pod log or
// an exec would be access a reader has to be granted, and this needs none.
func TestLoadReadsOnlyThePlansOwnObjects(t *testing.T) {
	t.Parallel()
	lab := newLab(t, "storefront")
	plan, _ := lab.store(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	lab.current(t, plan)
	reader := lab.reader()

	if _, err := planview.Load(context.Background(), reader, "application", "storefront", planview.Current); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := []string{
		"get *v1alpha1.PtahSchema",
		"get *v1alpha1.PtahSchemaPlan",
		"get *v1.ConfigMap",
	}
	got := reader.touched()
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("the viewer read %v, and the command is documented to need %v", got, want)
	}
}

// The compatibility path is the one that lists, and only it.
func TestLoadListsOnlyForARecordWithNoReference(t *testing.T) {
	t.Parallel()
	lab := newLab(t, "storefront")
	plan, _ := lab.store(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	lab.appliedBeforeTheReference(t, plan)
	reader := lab.reader()

	if _, err := planview.Load(context.Background(), reader, "application", "storefront", planview.Applied); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !contains(reader.touched(), "list *v1alpha1.PtahSchemaPlanList") {
		t.Fatalf("the fallback did not list the namespace's plans: %v", reader.touched())
	}
}

func first(t *testing.T, lab *lab) *operatorv1alpha1.PtahSchemaPlan {
	t.Helper()
	plan, _ := lab.store(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	return plan
}

// Two plans carrying one fingerprint is a question the fallback may not answer
// by itself. Taking the first would be choosing which SQL a reader is shown.
func TestLoadRefusesAnAmbiguousFallback(t *testing.T) {
	t.Parallel()
	lab := newLab(t, "storefront")
	plan, _ := lab.store(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	twin := plan.DeepCopy()
	twin.Name = plan.Name + "-twin"
	twin.ResourceVersion = ""
	twin.UID = ""
	if err := lab.client.Create(context.Background(), twin); err != nil {
		t.Fatalf("store the twin: %v", err)
	}
	lab.appliedBeforeTheReference(t, plan)

	view, err := planview.Load(context.Background(), lab.reader(), "application", "storefront", planview.Applied)
	if !errors.Is(err, planview.ErrAmbiguous) {
		t.Fatalf("Load() error = %v, want %v", err, planview.ErrAmbiguous)
	}
	if !strings.Contains(err.Error(), twin.Name) || !strings.Contains(err.Error(), plan.Name) {
		t.Fatalf("Load() said %q, which does not name both plans", err)
	}
	if len(view.Document) != 0 {
		t.Fatal("Load() returned a document with its error")
	}
}

// A document this build does not know how to read is said to be that, rather
// than interpreted on the assumption that the parts it recognizes are enough.
func TestLoadRefusesAFormatItDoesNotRead(t *testing.T) {
	t.Parallel()
	lab := newLab(t, "storefront")
	plan := lab.storeDocument(t, futureFormat(t), "v9")
	lab.current(t, plan)

	view, err := planview.Load(context.Background(), lab.reader(), "application", "storefront", planview.Current)
	if !errors.Is(err, planview.ErrUnsupported) {
		t.Fatalf("Load() error = %v, want %v", err, planview.ErrUnsupported)
	}
	if len(view.Document) != 0 {
		t.Fatal("Load() returned a document with its error")
	}
}

// A plan whose storage was never committed is not a plan to read. Publication
// is three writes that are not a transaction -- the manifest, the chunks, the
// ready condition -- and only the last one says the first two finished.
func TestLoadRefusesAPlanWhosePublicationDidNotFinish(t *testing.T) {
	t.Parallel()
	lab := newLab(t, "storefront")
	plan, _ := lab.store(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	lab.current(t, plan)
	lab.uncommit(t, plan)

	view, err := planview.Load(context.Background(), lab.reader(), "application", "storefront", planview.Current)
	if err == nil {
		t.Fatal("Load() read a plan whose storage was never committed")
	}
	if !strings.Contains(err.Error(), "not committed") {
		t.Fatalf("Load() said %q", err)
	}
	if len(view.Document) != 0 {
		t.Fatal("Load() returned a document with its error")
	}
}

// One namespace, two schemas, and a record that names its own plan. The
// fallback matches on the schema as well as the fingerprint, so a plan of the
// other schema is not an answer.
func TestLoadKeepsTwoSchemasApart(t *testing.T) {
	t.Parallel()
	storefront := newLab(t, "storefront")
	warehouse := newLab(t, "warehouse")
	own, _ := storefront.store(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	warehouse.store(t, []string{"CREATE TABLE pallets (id BIGINT PRIMARY KEY)"}, "v1")
	storefront.appliedBeforeTheReference(t, own)

	view, err := planview.Load(context.Background(), storefront.reader(), "application", "storefront", planview.Applied)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if view.PlanName != own.Name {
		t.Fatalf("the fallback resolved to %s, want %s", view.PlanName, own.Name)
	}
}
