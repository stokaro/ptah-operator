package planread_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/pkg/planread"
	"github.com/stokaro/ptah-operator/pkg/planread/planreadtest"
)

var sentinels = []error{planread.ErrSchemaNotFound, planread.ErrNoPlan, planread.ErrUnsupported, planread.ErrIntegrity}

// Every stored state in the shared cases gets the answer the case declares,
// and a refusal matches its own error and no other one: a caller that branches
// on the error must not be able to read a refused plan as an absent one.
func TestLoadAnswersEveryCase(t *testing.T) {
	t.Parallel()
	cases := planreadtest.Cases(t)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			reader := planreadtest.NewClient(t, c.Objects...)

			plan, err := planread.Load(context.Background(), reader, planreadtest.Namespace, c.Schema, c.Selection)

			if c.Want.Err != nil {
				for _, sentinel := range sentinels {
					if got, want := errors.Is(err, sentinel), sentinel == c.Want.Err; got != want {
						t.Fatalf("errors.Is(%v, %v) = %t, want %t", err, sentinel, got, want)
					}
				}
				if len(plan.Document) != 0 || len(plan.Statements) != 0 || plan.PlanUID != "" {
					t.Fatalf("Load() returned part of a plan with its error: %d bytes, %d statements",
						len(plan.Document), len(plan.Statements))
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if plan.PlanName != c.Want.PlanName {
				t.Fatalf("Load() resolved %s, want %s", plan.PlanName, c.Want.PlanName)
			}
			if !bytes.Equal(plan.Document, c.Want.Document) {
				t.Fatal("the document read back is not the document that was stored")
			}
			if len(plan.Statements) != len(c.Want.Statements) {
				t.Fatalf("Load() returned %d statements, want %d", len(plan.Statements), len(c.Want.Statements))
			}
			for index, statement := range plan.Statements {
				if statement.SQL != c.Want.Statements[index] {
					t.Fatalf("statement %d came back as %.60q..., want %.60q...", index, statement.SQL, c.Want.Statements[index])
				}
			}
			if plan.Manifest.UID != plan.PlanUID || plan.Manifest.Spec.Fingerprint != plan.Fingerprint {
				t.Fatal("the manifest returned is not the plan that was read")
			}
		})
	}
}

// The cases have to reach every answer. A list that lost its refusals would
// pass the test above by having nothing left to refuse.
func TestCasesReachEveryAnswer(t *testing.T) {
	t.Parallel()
	reached := map[error]int{}
	reads := 0
	for _, c := range planreadtest.Cases(t) {
		if c.Want.Err == nil {
			reads++
			continue
		}
		reached[c.Want.Err]++
	}
	if reads < 3 {
		t.Fatalf("%d cases read a plan; the spanning, applied and current cases make three", reads)
	}
	for _, sentinel := range sentinels {
		if reached[sentinel] == 0 {
			t.Fatalf("no case answers %v", sentinel)
		}
	}
}

// The SQL a reader gets is the SQL the plan holds: the same statements, in
// the same order, nothing re-split on a semicolon.
func TestSQLKeepsTheStatementsThePlanHolds(t *testing.T) {
	t.Parallel()
	statements := []string{
		"CREATE TABLE customers (id BIGINT PRIMARY KEY)",
		"CREATE FUNCTION touch() RETURNS trigger AS $$ BEGIN NEW.seen := now(); RETURN NEW; END; $$ LANGUAGE plpgsql",
		"COMMENT ON TABLE customers IS 'one; two; three'",
	}
	lab := planreadtest.NewLab(t, "storefront")
	stored, _ := lab.Publish(t, statements, "v1")
	lab.SetCurrent(t, stored)

	plan, err := planread.Load(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", planread.Current)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	var want strings.Builder
	for _, statement := range statements {
		want.WriteString(statement)
		want.WriteString(";\n")
	}
	if plan.SQL() != want.String() {
		t.Fatalf("SQL() is\n%s\nand the plan holds\n%s", plan.SQL(), want.String())
	}
}

// A reader without the grant on chunks is told so by the API server, and
// that answer reaches the caller unchanged: it is a missing grant, not a
// broken plan, and the caller has to be able to say which.
func TestLoadPassesAForbiddenChunkReadThrough(t *testing.T) {
	t.Parallel()
	lab := planreadtest.NewLab(t, "storefront")
	stored, _ := lab.Publish(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}, "v1")
	lab.SetCurrent(t, stored)

	plan, err := planread.Load(context.Background(), chunkForbiddingReader{Reader: lab.Client()},
		planreadtest.Namespace, "storefront", planread.Current)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("Load() error = %v, want Forbidden", err)
	}
	for _, sentinel := range sentinels {
		if errors.Is(err, sentinel) {
			t.Fatalf("a Forbidden read matched %v", sentinel)
		}
	}
	if len(plan.Document) != 0 {
		t.Fatal("Load() returned a document with its error")
	}
}

// The reader asks for three kinds of object and no others. A Secret, a
// ConfigMap, a Pod log or an exec would be access a reader has to be granted
// beyond the operator's own kinds, and this needs none.
func TestLoadReadsOnlyThePlansOwnObjects(t *testing.T) {
	t.Parallel()
	lab := planreadtest.NewLab(t, "storefront")
	stored, _ := lab.Publish(t, planreadtest.SpanningStatements(), "v1")
	lab.SetCurrent(t, stored)
	reader := &recorder{Reader: lab.Client()}

	if _, err := planread.Load(context.Background(), reader, planreadtest.Namespace, "storefront", planread.Current); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := "get *v1alpha1.PtahSchema, get *v1alpha1.PtahSchemaPlan, get *v1alpha1.PtahSchemaPlanChunk"
	if got := strings.Join(reader.distinct(), ", "); got != want {
		t.Fatalf("the reader asked for %s, and a plan read needs %s", got, want)
	}
}

// A privilege kind the SQL shows and the plan does not record is kept apart
// from the recorded ones: the manager did not hold the plan for it.
func TestLoadKeepsUnrecordedPrivilegeChangesApart(t *testing.T) {
	t.Parallel()
	lab := planreadtest.NewLab(t, "storefront")
	stored := lab.PublishDocument(t, planreadtest.Document(t, []string{
		`ALTER TABLE "public"."customers" OWNER TO "app"`,
		`GRANT SELECT ON TABLE "public"."customers" TO PUBLIC`,
	}), "v1", func(spec *operatorv1alpha1.PtahSchemaPlanSpec) {
		spec.PrivilegeChanges = []operatorv1alpha1.PrivilegeChange{operatorv1alpha1.PrivilegeChangeGrant}
	})
	lab.SetCurrent(t, stored)

	plan, err := planread.Load(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", planread.Current)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := strings.Join(plan.PrivilegeChanges, ","); got != "Grant" {
		t.Fatalf("recorded privilege changes = %q, want Grant", got)
	}
	if got := strings.Join(plan.UnrecordedPrivilegeChanges, ","); got != "Ownership" {
		t.Fatalf("unrecorded privilege changes = %q, want Ownership", got)
	}
}

// chunkForbiddingReader answers a chunk read the way the API server answers a
// reader bound to a role without the chunk rule.
type chunkForbiddingReader struct{ client.Reader }

func (r chunkForbiddingReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*operatorv1alpha1.PtahSchemaPlanChunk); ok {
		resource := operatorv1alpha1.GroupVersion.WithResource("ptahschemaplanchunks").GroupResource()
		return apierrors.NewForbidden(resource, key.Name, errors.New("no rule grants it"))
	}
	return r.Reader.Get(ctx, key, object, options...)
}

// recorder is a read-only client that remembers what was asked of it.
type recorder struct {
	client.Reader
	kinds []string
}

func (r *recorder) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	r.kinds = append(r.kinds, fmt.Sprintf("get %T", object))
	return r.Reader.Get(ctx, key, object, options...)
}

func (r *recorder) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	r.kinds = append(r.kinds, fmt.Sprintf("list %T", list))
	return r.Reader.List(ctx, list, options...)
}

func (r *recorder) distinct() []string {
	var seen []string
	for _, kind := range r.kinds {
		if !contains(seen, kind) {
			seen = append(seen, kind)
		}
	}
	return seen
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
