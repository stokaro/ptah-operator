package planread_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/pkg/planread"
	"github.com/stokaro/ptah-operator/pkg/planread/planreadtest"
)

const (
	unreadValue = planreadtest.UnreadValue
	rowsDigest  = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

var oneStatement = []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}

// edited is a plan document after an edit a test makes to its decoded form.
func edited(t *testing.T, statements []string, edit func(map[string]any)) []byte {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(planreadtest.Document(t, statements), &decoded); err != nil {
		t.Fatal(err)
	}
	edit(decoded)
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// statementOf is one statement of a decoded document, to add a field to.
func statementOf(document map[string]any, index int) map[string]any {
	return document["statements"].([]any)[index].(map[string]any)
}

// withMember is a plan document with a member written as raw JSON at its top,
// for keys and shapes json.Marshal would not write.
func withMember(t *testing.T, member string) []byte {
	t.Helper()
	document := planreadtest.Document(t, oneStatement)
	return append([]byte("{"+member+","), document[1:]...)
}

// stored publishes a document as both the current and the applied plan.
func stored(t *testing.T, document []byte, adjust ...func(*operatorv1alpha1.PtahSchemaPlanSpec)) *planreadtest.Lab {
	t.Helper()
	lab := planreadtest.NewLab(t, "storefront")
	plan := lab.PublishDocument(t, document, "v1", adjust...)
	lab.SetCurrent(t, plan)
	lab.SetApplied(t, plan)
	return lab
}

func holdNoValue(t *testing.T, plan planread.Plan, err error) {
	t.Helper()
	encoded, marshalErr := json.Marshal(plan)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, seen := range []string{string(encoded), fmt.Sprintf("%+v", plan), fmt.Sprint(err)} {
		if strings.Contains(seen, unreadValue) {
			t.Fatalf("the read handed over the value of a field it did not read:\n%.2000s", seen)
		}
	}
}

// Every stored state answers a display read as the cases declare, and a state
// that fails a check answers it the way it answers Load.
func TestLoadForDisplayAnswersEveryCase(t *testing.T) {
	t.Parallel()
	for _, c := range planreadtest.DisplayCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			reader := planreadtest.NewClient(t, c.Objects...)

			plan, err := planread.LoadForDisplay(context.Background(), reader, planreadtest.Namespace, c.Schema, c.Selection)

			holdAnswer(t, c, plan, err)
			holdNoValue(t, plan, err)
		})
	}
}

// The two lists are one list of stored states. The only answer that differs is
// the one for a document with unread fields, so a display read cannot have
// loosened any check on the stored objects without a case saying so.
func TestTheTwoReadsDifferOnlyOnUnreadFields(t *testing.T) {
	t.Parallel()
	strict, display := planreadtest.Cases(t), planreadtest.DisplayCases(t)
	if len(strict) != len(display) {
		t.Fatalf("%d cases for Load and %d for a display read", len(strict), len(display))
	}
	differing := 0
	for index, c := range strict {
		if c.Name != display[index].Name {
			t.Fatalf("case %d is %s for Load and %s for a display read", index, c.Name, display[index].Name)
		}
		if !errors.Is(c.Want.Err, display[index].Want.Err) {
			differing++
			if c.Name != "unread-fields" {
				t.Fatalf("%s: Load answers %v and a display read %v", c.Name, c.Want.Err, display[index].Want.Err)
			}
		}
	}
	if differing != 1 {
		t.Fatalf("%d cases differ; the case with unread fields is the one that should", differing)
	}
}

// Each of these documents is what a newer Ptah could store. A row that expects
// a name is one Load refuses and a display read shows, and a row that expects
// none is one both read. Unread and strict agree on every row: a field the walk
// took for declared would slip past it, and one it took for unknown would be
// stripped from a plan Load reads.
func TestLoadForDisplayNamesWhatItDoesNotRead(t *testing.T) {
	t.Parallel()
	two := []string{"CREATE TABLE a (id BIGINT)", "CREATE TABLE b (id BIGINT)"}
	rowSet := func(extra map[string]any) []any {
		set := map[string]any{"table": "customers", "keys": []any{"id"}, "columns": []any{"id", "email"}}
		for key, value := range extra {
			set[key] = value
		}
		return []any{set}
	}
	for _, test := range []struct {
		name     string
		document func(t *testing.T) []byte
		unread   []string
	}{
		{
			name: "a field on the plan",
			document: func(t *testing.T) []byte {
				return edited(t, oneStatement, func(d map[string]any) { d["annotations"] = map[string]any{"provider": unreadValue} })
			},
			unread: []string{"annotations"},
		},
		{
			name: "a field on a statement",
			document: func(t *testing.T) []byte {
				return edited(t, two, func(d map[string]any) { statementOf(d, 1)["inputs"] = []any{unreadValue} })
			},
			unread: []string{"statements[].inputs"},
		},
		{
			name: "the same field on two statements is one name",
			document: func(t *testing.T) []byte {
				return edited(t, two, func(d map[string]any) {
					statementOf(d, 0)["inputs"] = []any{unreadValue}
					statementOf(d, 1)["inputs"] = []any{unreadValue}
				})
			},
			unread: []string{"statements[].inputs"},
		},
		{
			name: "a field in the scope",
			document: func(t *testing.T) []byte {
				return edited(t, oneStatement, func(d map[string]any) {
					d["scope"] = map[string]any{"kind": "realm", "schemas": []any{unreadValue}}
				})
			},
			unread: []string{"scope.schemas"},
		},
		{
			name: "a field on a declared row set",
			document: func(t *testing.T) []byte {
				return edited(t, oneStatement, func(d map[string]any) {
					d["rows_fingerprint"] = rowsDigest
					d["managed_rows"] = rowSet(map[string]any{"sample": unreadValue})
				})
			},
			unread: []string{"managed_rows[].sample"},
		},
		{
			name: "what a field holds is not walked",
			document: func(t *testing.T) []byte {
				return edited(t, oneStatement, func(d map[string]any) {
					d["annotations"] = map[string]any{"inputs": map[string]any{"statements": []any{map[string]any{"sql": unreadValue}}}}
				})
			},
			unread: []string{"annotations"},
		},
		{
			name: "several fields come back sorted",
			document: func(t *testing.T) []byte {
				return edited(t, oneStatement, func(d map[string]any) {
					d["zeta"] = unreadValue
					d["alpha"] = unreadValue
					statementOf(d, 0)["mid"] = unreadValue
				})
			},
			unread: []string{"alpha", "statements[].mid", "zeta"},
		},
		{
			// The strict decoder stores these keys in the fields they fold to,
			// so the walk does too, and it names what it left out by the field.
			name: "an unread field under a declared key in another case",
			document: func(t *testing.T) []byte {
				return edited(t, oneStatement, func(d map[string]any) {
					d["SCOPE"] = map[string]any{"KIND": "realm", "extra": unreadValue}
				})
			},
			unread: []string{"scope.extra"},
		},
		{
			// The decoder keeps this name for what it reads from the SQL, so a
			// document cannot supply it.
			name:     "the privilege list the operator computes itself",
			document: func(t *testing.T) []byte { return withMember(t, `"PrivilegeChanges":["`+unreadValue+`"]`) },
			unread:   []string{"PrivilegeChanges"},
		},
		{
			name:     "a field name of the longest length a read prints",
			document: func(t *testing.T) []byte { return withMember(t, `"`+strings.Repeat("a", 128)+`":"`+unreadValue+`"`) },
			unread:   []string{strings.Repeat("a", 128)},
		},
		{
			name:     "a document as it was published",
			document: func(t *testing.T) []byte { return planreadtest.Document(t, oneStatement) },
		},
		{
			// PR #835 declared scope. A plan that carries it reads whole,
			// through both reads.
			name: "the scope Ptah writes since v0.19.0",
			document: func(t *testing.T) []byte {
				return edited(t, oneStatement, func(d map[string]any) { d["scope"] = map[string]any{"kind": "schema", "name": "shop"} })
			},
		},
		{
			name: "a declared row set",
			document: func(t *testing.T) []byte {
				return edited(t, oneStatement, func(d map[string]any) {
					d["rows_fingerprint"] = rowsDigest
					d["managed_rows"] = rowSet(nil)
				})
			},
		},
		{
			// encoding/json folds case, so the strict decoder stores this key
			// in the field it declares.
			name: "a declared key in another case",
			document: func(t *testing.T) []byte {
				return edited(t, oneStatement, func(d map[string]any) { d["SCOPE"] = map[string]any{"KIND": "realm"} })
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			document := test.document(t)
			lab := stored(t, document)
			for _, selection := range []planread.Selection{planread.Current, planread.Applied} {
				plan, err := planread.LoadForDisplay(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", selection)
				if err != nil {
					t.Fatalf("LoadForDisplay(%s) error = %v", selection, err)
				}
				if !slices.Equal(plan.UnreadFields, test.unread) {
					t.Fatalf("LoadForDisplay(%s) names %v, want %v", selection, plan.UnreadFields, test.unread)
				}
				if len(plan.Statements) == 0 || plan.Selection != selection {
					t.Fatalf("LoadForDisplay(%s) returned no plan: %+v", selection, plan)
				}
				holdNoValue(t, plan, err)

				strict, err := planread.Load(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", selection)
				if len(test.unread) == 0 {
					if err != nil {
						t.Fatalf("Load(%s) refused a document with nothing unread: %v", selection, err)
					}
					if !bytes.Equal(plan.Document, document) || !bytes.Equal(strict.Document, document) {
						t.Fatal("a plan with nothing unread did not come back byte for byte")
					}
					continue
				}
				if !errors.Is(err, planread.ErrUnsupported) || len(strict.Statements) != 0 {
					t.Fatalf("Load(%s) = %d statements, %v; want ErrUnsupported", selection, len(strict.Statements), err)
				}
				holdNoValue(t, strict, err)
				if len(plan.Document) != 0 {
					t.Fatalf("LoadForDisplay(%s) handed over the stored document, which holds the unread values", selection)
				}
			}
		})
	}
}

// A display read shows the plan Load would show without the field: the operator
// raises what it raises and reads the privileges it reads, so a statement does
// not look safer because a field beside it went unread.
func TestLoadForDisplayReadsThePlanAsLoadDoes(t *testing.T) {
	t.Parallel()
	statements := []string{
		`ALTER TABLE "public"."customers" ALTER COLUMN "email" DROP DEFAULT`,
		`GRANT SELECT ON TABLE "public"."customers" TO PUBLIC`,
	}
	bare := stored(t, planreadtest.Document(t, statements))
	want, err := planread.Load(context.Background(), bare.Client(), planreadtest.Namespace, "storefront", planread.Current)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if want.Statements[0].Severity != "destructive" || want.Statements[0].StoredSeverity != "safe" ||
		len(want.UnrecordedPrivilegeChanges) == 0 {
		t.Fatalf("the plain read did not raise anything, so this row measures nothing: %+v", want)
	}

	lab := stored(t, planreadtest.DocumentWithUnreadFields(t, statements))
	got, err := planread.LoadForDisplay(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", planread.Current)
	if err != nil {
		t.Fatalf("LoadForDisplay() error = %v", err)
	}
	if !reflect.DeepEqual(got.Statements, want.Statements) {
		t.Fatalf("the statements read as %+v, and without the fields as %+v", got.Statements, want.Statements)
	}
	if got.SQL() != want.SQL() {
		t.Fatalf("the SQL reads as %q, and without the fields as %q", got.SQL(), want.SQL())
	}
	if !slices.Equal(got.UnrecordedPrivilegeChanges, want.UnrecordedPrivilegeChanges) {
		t.Fatalf("the privileges read as %v, and without the fields as %v", got.UnrecordedPrivilegeChanges, want.UnrecordedPrivilegeChanges)
	}
}

// encoding/json stores a repeated key, or the same key in another case, over the
// one before it. Taking a field out leaves the rest in their order, so a display
// read shows the statements every other reader of those bytes takes. A walk that
// sorted the keys would show the first set here and read the second elsewhere.
func TestLoadForDisplayKeepsTheOrderOfARepeatedKey(t *testing.T) {
	t.Parallel()
	const read = "CREATE TABLE last_one_read (id BIGINT)"
	written := bytes.TrimSuffix(planreadtest.Document(t, []string{"DROP TABLE first_one_written"}), []byte("}"))
	again := `,"Statements":[{"sql":"` + read + `","severity":"safe","reason":""}]`
	bare := append(slices.Clone(written), []byte(again+"}")...)
	withField := append(slices.Clone(written), []byte(again+`,"annotations":"`+unreadValue+`"}`)...)

	strict, err := planread.Load(context.Background(), stored(t, bare).Client(), planreadtest.Namespace, "storefront", planread.Current)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if strict.Statements[0].SQL != read {
		t.Fatalf("Load() read %q, so this row does not hold what it says", strict.Statements[0].SQL)
	}

	shown, err := planread.LoadForDisplay(context.Background(), stored(t, withField).Client(), planreadtest.Namespace, "storefront", planread.Current)
	if err != nil {
		t.Fatalf("LoadForDisplay() error = %v", err)
	}
	if !reflect.DeepEqual(shown.Statements, strict.Statements) {
		t.Fatalf("a display read shows %+v, and Load reads %+v", shown.Statements, strict.Statements)
	}
}

// Every document here carries a field this build does not declare, and a read
// must not take that for a licence to skip what the decoder holds. A plan the
// operator would refuse is refused by both reads, whole, and the refusal does
// not quote the field's value.
func TestLoadForDisplayRefusesWhatTheDecoderRefuses(t *testing.T) {
	t.Parallel()
	withUnread := func(edit func(map[string]any)) func(t *testing.T) []byte {
		return func(t *testing.T) []byte {
			return edited(t, oneStatement, func(d map[string]any) {
				d["annotations"] = unreadValue
				edit(d)
			})
		}
	}
	for _, test := range []struct {
		name     string
		document func(t *testing.T) []byte
	}{
		{"another format version", withUnread(func(d map[string]any) { d["format_version"] = 99 })},
		{"another dialect", withUnread(func(d map[string]any) { d["dialect"] = "mysql" })},
		{"no state fingerprint", withUnread(func(d map[string]any) { delete(d, "to_fingerprint") })},
		{"no statements", withUnread(func(d map[string]any) { d["statements"] = []any{} })},
		{"statements of another shape", withUnread(func(d map[string]any) { d["statements"] = "DROP TABLE customers" })},
		{"a scope of another shape", withUnread(func(d map[string]any) { d["scope"] = "realm" })},
		{"a statement of an unknown severity", withUnread(func(d map[string]any) { statementOf(d, 0)["severity"] = "fine" })},
		{"credential-bearing DDL", withUnread(func(d map[string]any) { statementOf(d, 0)["sql"] = "CREATE ROLE app PASSWORD 'secret'" })},
		{"a rows fingerprint with no row set", withUnread(func(d map[string]any) { d["rows_fingerprint"] = rowsDigest })},
		{
			"a second JSON value",
			func(t *testing.T) []byte {
				return append(edited(t, oneStatement, func(d map[string]any) { d["annotations"] = unreadValue }), []byte(" {}")...)
			},
		},
		{"not JSON after the first member", func(t *testing.T) []byte { return []byte(`{"annotations":"` + unreadValue + `",`) }},
		{"more fields than a list prints", func(t *testing.T) []byte {
			return edited(t, oneStatement, func(d map[string]any) {
				for index := range 65 {
					d[fmt.Sprintf("field_%d", index)] = unreadValue
				}
			})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lab := stored(t, test.document(t))
			for name, read := range map[string]func(context.Context, client.Reader, string, string, planread.Selection) (planread.Plan, error){
				"Load": planread.Load, "LoadForDisplay": planread.LoadForDisplay,
			} {
				plan, err := read(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", planread.Current)
				if !errors.Is(err, planread.ErrUnsupported) {
					t.Fatalf("%s() error = %v, want ErrUnsupported", name, err)
				}
				if len(plan.Statements) != 0 || len(plan.Document) != 0 || plan.PlanUID != "" {
					t.Fatalf("%s() returned part of a plan with its error", name)
				}
				holdNoValue(t, plan, err)
			}
		})
	}
}

// A display read prints the name of a field it did not read, and a newer Ptah
// adds an identifier. A key of another shape is not a field added to a plan, and
// printing it could put a credential or an escape sequence on a terminal, so the
// read refuses the plan and does not quote the key.
func TestLoadForDisplayRefusesANameItWouldNotPrint(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		document func(t *testing.T) []byte
		quoted   string
	}{
		{"an escape sequence", func(t *testing.T) []byte { return withMember(t, `"\u001b[2J":1`) }, "[2J"},
		{"a URL with a password", func(t *testing.T) []byte { return withMember(t, `"postgres://app:hunter2@db/shop":1`) }, "hunter2"},
		{"a space", func(t *testing.T) []byte { return withMember(t, `"two words":1`) }, "two words"},
		{"a dot, which a path uses", func(t *testing.T) []byte { return withMember(t, `"a.b":1`) }, "a.b"},
		{"a leading digit", func(t *testing.T) []byte { return withMember(t, `"1st":1`) }, "1st"},
		{"nothing at all", func(t *testing.T) []byte { return withMember(t, `"":1`) }, `""`},
		{"letters outside ASCII", func(t *testing.T) []byte { return withMember(t, `"поле":1`) }, "поле"},
		{"more bytes than a name", func(t *testing.T) []byte { return withMember(t, `"`+strings.Repeat("a", 129)+`":1`) }, strings.Repeat("a", 129)},
		{"a statement's field", func(t *testing.T) []byte {
			return edited(t, oneStatement, func(d map[string]any) { statementOf(d, 0)["two words"] = 1 })
		}, "two words"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lab := stored(t, test.document(t))

			plan, err := planread.LoadForDisplay(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", planread.Current)

			if !errors.Is(err, planread.ErrUnsupported) {
				t.Fatalf("LoadForDisplay() error = %v, want ErrUnsupported", err)
			}
			if strings.Contains(err.Error(), test.quoted) {
				t.Fatalf("the refusal quotes the key: %v", err)
			}
			if len(plan.Statements) != 0 || plan.PlanUID != "" {
				t.Fatal("LoadForDisplay() returned part of a plan with its error")
			}
			if _, err := planread.Load(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", planread.Current); !errors.Is(err, planread.ErrUnsupported) {
				t.Fatalf("Load() error = %v, want ErrUnsupported", err)
			}
		})
	}
}

// The bound on the list is its limit and not one short of it: 64 fields read.
func TestLoadForDisplayReadsAsManyFieldsAsItPrints(t *testing.T) {
	t.Parallel()
	lab := stored(t, edited(t, oneStatement, func(d map[string]any) {
		for index := range 64 {
			d[fmt.Sprintf("field_%d", index)] = unreadValue
		}
	}))
	plan, err := planread.LoadForDisplay(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", planread.Current)
	if err != nil {
		t.Fatalf("LoadForDisplay() error = %v", err)
	}
	if len(plan.UnreadFields) != 64 {
		t.Fatalf("LoadForDisplay() names %d fields, want 64", len(plan.UnreadFields))
	}
}

// A display read makes every check on the stored objects, on a document it
// would otherwise show. The store's checks come first: none of these reads
// reaches the decoder's tolerance.
func TestLoadForDisplayChecksTheStoredObjects(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		arrange func(t *testing.T, lab *planreadtest.Lab, plan *operatorv1alpha1.PtahSchemaPlan)
		want    error
	}{
		{"a chunk that does not match its digest", func(t *testing.T, lab *planreadtest.Lab, plan *operatorv1alpha1.PtahSchemaPlan) {
			lab.EditChunk(t, plan, func(chunk *operatorv1alpha1.PtahSchemaPlanChunk) { chunk.Spec.Data[0] ^= 0xff })
		}, planread.ErrIntegrity},
		{"a chunk that is gone", func(t *testing.T, lab *planreadtest.Lab, plan *operatorv1alpha1.PtahSchemaPlan) {
			lab.DeleteChunk(t, plan)
		}, planread.ErrIntegrity},
		{"a chunk of another owner", func(t *testing.T, lab *planreadtest.Lab, plan *operatorv1alpha1.PtahSchemaPlan) {
			lab.EditChunk(t, plan, func(chunk *operatorv1alpha1.PtahSchemaPlanChunk) {
				chunk.OwnerReferences[0].UID = types.UID("uid-someone-else")
			})
		}, planread.ErrIntegrity},
		{"a publication that did not finish", func(t *testing.T, lab *planreadtest.Lab, plan *operatorv1alpha1.PtahSchemaPlan) {
			lab.Uncommit(t, plan)
		}, planread.ErrIntegrity},
		{"another object under the name", func(t *testing.T, lab *planreadtest.Lab, plan *operatorv1alpha1.PtahSchemaPlan) {
			lab.Recreate(t, plan)
		}, planread.ErrIntegrity},
		{"a contract this build does not read", func(t *testing.T, lab *planreadtest.Lab, plan *operatorv1alpha1.PtahSchemaPlan) {
			lab.EditManifest(t, plan, func(spec *operatorv1alpha1.PtahSchemaPlanSpec) { spec.ContractVersion += 2 })
		}, planread.ErrUnsupported},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lab := planreadtest.NewLab(t, "storefront")
			plan := lab.PublishDocument(t, planreadtest.DocumentWithUnreadFields(t, oneStatement), "v1")
			lab.SetCurrent(t, plan)
			if _, err := planread.LoadForDisplay(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", planread.Current); err != nil {
				t.Fatalf("LoadForDisplay() refused the plan before it was changed, so this row measures nothing: %v", err)
			}
			test.arrange(t, lab, plan)

			read, err := planread.LoadForDisplay(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", planread.Current)

			if !errors.Is(err, test.want) {
				t.Fatalf("LoadForDisplay() error = %v, want %v", err, test.want)
			}
			if len(read.Statements) != 0 || read.PlanUID != "" {
				t.Fatal("LoadForDisplay() returned part of a plan with its error")
			}
		})
	}
}

// A display read needs no grant Load does not, and a missing one is the API
// server's answer, unchanged.
func TestLoadForDisplayReadsAsTheCaller(t *testing.T) {
	t.Parallel()
	lab := stored(t, planreadtest.DocumentWithUnreadFields(t, oneStatement))

	plan, err := planread.LoadForDisplay(context.Background(), chunkForbiddingReader{Reader: lab.Client()},
		planreadtest.Namespace, "storefront", planread.Current)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("LoadForDisplay() error = %v, want Forbidden", err)
	}
	if len(plan.Statements) != 0 {
		t.Fatal("LoadForDisplay() returned statements with its error")
	}

	reader := &recorder{Reader: lab.Client()}
	if _, err := planread.LoadForDisplay(context.Background(), reader, planreadtest.Namespace, "storefront", planread.Current); err != nil {
		t.Fatalf("LoadForDisplay() error = %v", err)
	}
	want := "get *v1alpha1.PtahSchema, get *v1alpha1.PtahSchemaPlan, get *v1alpha1.PtahSchemaPlanChunk"
	if got := strings.Join(reader.distinct(), ", "); got != want {
		t.Fatalf("the reader asked for %s, and a plan read needs %s", got, want)
	}
}

// Every plan document Ptah wrote that the repository keeps reads whole through
// both reads. A declared field the walk missed would show up here as unread, on
// the fields Ptah really writes.
func TestBothReadsTakeThePlansPtahWrote(t *testing.T) {
	t.Parallel()
	var paths []string
	for _, pattern := range []string{
		filepath.Join("..", "..", "testdata", "e2e", "readings", "*-plan.json"),
		filepath.Join("..", "..", "internal", "dataplane", "testdata", "plan-*.json"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, matches...)
	}
	const declaredDocuments = 7
	if len(paths) < declaredDocuments {
		t.Fatalf("found %d plan documents, and the repository keeps at least %d: %v", len(paths), declaredDocuments, paths)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()
			document, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var header struct {
				Dialect string `json:"dialect"`
			}
			if err := json.Unmarshal(document, &header); err != nil {
				t.Fatal(err)
			}
			lab := stored(t, document, func(spec *operatorv1alpha1.PtahSchemaPlanSpec) { spec.Dialect = header.Dialect })

			strict, err := planread.Load(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", planread.Current)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			shown, err := planread.LoadForDisplay(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", planread.Current)
			if err != nil {
				t.Fatalf("LoadForDisplay() error = %v", err)
			}
			if len(shown.UnreadFields) != 0 {
				t.Fatalf("a document Ptah wrote has the unread fields %v", shown.UnreadFields)
			}
			if !bytes.Equal(shown.Document, document) || !reflect.DeepEqual(shown.Statements, strict.Statements) {
				t.Fatal("the two reads of one document differ")
			}
		})
	}
}
