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

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/pkg/planread"
	"github.com/stokaro/ptah-operator/pkg/planread/planreadtest"
)

const (
	unreadValue = planreadtest.UnreadValue
	rowsDigest  = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	// addedKey is the field the rows that keep Ptah's indentation add as text.
	addedKey = "added_by_a_newer_ptah"
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

// withAddedKey adds one field as text to a document as Ptah writes it, which is
// indented, so the walk runs over those bytes and not over a recoding of them.
func withAddedKey(t *testing.T, document []byte) []byte {
	t.Helper()
	const top = "{\n  \""
	if !bytes.HasPrefix(document, []byte(top)) {
		t.Fatalf("the document does not open as Ptah writes one: %.40q", document)
	}
	added := "{\n  \"" + addedKey + "\": \"" + unreadValue + "\",\n  \""
	return append([]byte(added), document[len(top):]...)
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

func display(lab *planreadtest.Lab, selection planread.Selection) (planread.DisplayPlan, error) {
	return planread.LoadForDisplay(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", selection)
}

func strict(lab *planreadtest.Lab, selection planread.Selection) (planread.Plan, error) {
	return planread.Load(context.Background(), lab.Client(), planreadtest.Namespace, "storefront", selection)
}

// holdNoValue holds what a read returned, and its error, to not showing the value
// of a field it did not read.
func holdNoValue(t *testing.T, read any, err error) {
	t.Helper()
	encoded, marshalErr := json.Marshal(read)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, seen := range []string{string(encoded), fmt.Sprintf("%+v", read), fmt.Sprint(err)} {
		if strings.Contains(seen, unreadValue) {
			t.Fatalf("the read handed over the value of a field it did not read:\n%.2000s", seen)
		}
	}
}

// Every stored state answers a display read as the cases declare. The cases that
// fail a check are the ones Load answers, so a display read loosens none of them.
func TestLoadForDisplayAnswersEveryCase(t *testing.T) {
	t.Parallel()
	for _, c := range planreadtest.DisplayCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			reader := planreadtest.NewClient(t, c.Objects...)

			plan, err := planread.LoadForDisplay(context.Background(), reader, planreadtest.Namespace, c.Schema, c.Selection)

			holdNoValue(t, plan, err)
			if c.Want.Err != nil {
				for _, sentinel := range sentinels {
					if got, want := errors.Is(err, sentinel), sentinel == c.Want.Err; got != want {
						t.Fatalf("errors.Is(%v, %v) = %t, want %t", err, sentinel, got, want)
					}
				}
				if !reflect.DeepEqual(plan, planread.DisplayPlan{}) {
					t.Fatalf("the read returned part of a plan with its error: %+v", plan)
				}
				return
			}
			if err != nil {
				t.Fatalf("the read returned an error: %v", err)
			}
			if plan.PlanName != c.Want.PlanName || !slices.Equal(plan.UnreadFields, c.Want.UnreadFields) {
				t.Fatalf("the read is %s with the unread fields %v, want %s with %v",
					plan.PlanName, plan.UnreadFields, c.Want.PlanName, c.Want.UnreadFields)
			}
			if len(plan.Statements) != len(c.Want.Statements) {
				t.Fatalf("the read returned %d statements, want %d", len(plan.Statements), len(c.Want.Statements))
			}
			for index, statement := range plan.Statements {
				if statement.SQL != c.Want.Statements[index] {
					t.Fatalf("statement %d came back as %.60q..., want %.60q...", index, statement.SQL, c.Want.Statements[index])
				}
			}
		})
	}
}

// A plan read whole is the same plan through either read, so the text format,
// which takes the display read, prints what the others print.
func TestAPlanReadWholeIsOnePlanThroughEitherRead(t *testing.T) {
	t.Parallel()
	lab := stored(t, planreadtest.Document(t, planreadtest.SpanningStatements()))
	for _, selection := range []planread.Selection{planread.Current, planread.Applied} {
		whole, err := strict(lab, selection)
		if err != nil {
			t.Fatalf("Load(%s) error = %v", selection, err)
		}
		shown, err := display(lab, selection)
		if err != nil {
			t.Fatalf("LoadForDisplay(%s) error = %v", selection, err)
		}
		if !reflect.DeepEqual(shown, whole.Display()) {
			t.Fatalf("the two reads of %s differ:\n%+v\n%+v", selection, shown, whole.Display())
		}
		if len(shown.UnreadFields) != 0 {
			t.Fatalf("a plan read whole has the unread fields %v", shown.UnreadFields)
		}
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
				plan, err := display(lab, selection)
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

				whole, err := strict(lab, selection)
				if len(test.unread) == 0 {
					if err != nil {
						t.Fatalf("Load(%s) refused a document with nothing unread: %v", selection, err)
					}
					if !bytes.Equal(whole.Document, document) {
						t.Fatal("a plan with nothing unread did not come back byte for byte")
					}
					continue
				}
				if !errors.Is(err, planread.ErrUnsupported) || !errors.Is(err, planread.ErrUnreadFields) || len(whole.Statements) != 0 {
					t.Fatalf("Load(%s) = %d statements, %v; want ErrUnsupported and ErrUnreadFields", selection, len(whole.Statements), err)
				}
				for _, name := range test.unread {
					if !strings.Contains(err.Error(), name) {
						t.Fatalf("Load(%s) does not name %s: %v", selection, name, err)
					}
				}
				holdNoValue(t, whole, err)
			}
		})
	}
}

// A refusal that is not about unread fields does not say it is. A caller tells
// the person a display read would show this plan only on ErrUnreadFields, and a
// display read would not show either of these.
func TestAnotherRefusalIsNotUnreadFields(t *testing.T) {
	t.Parallel()
	contract := planreadtest.NewLab(t, "storefront")
	plan, _ := contract.Publish(t, oneStatement, "v1")
	contract.SetCurrent(t, plan)
	contract.EditManifest(t, plan, func(spec *operatorv1alpha1.PtahSchemaPlanSpec) { spec.ContractVersion += 2 })
	for name, lab := range map[string]*planreadtest.Lab{
		"another format version": stored(t, planreadtest.FutureDocument(t)),
		"another plan contract":  contract,
	} {
		_, err := strict(lab, planread.Current)
		if !errors.Is(err, planread.ErrUnsupported) || errors.Is(err, planread.ErrUnreadFields) {
			t.Fatalf("%s: Load() error = %v, want ErrUnsupported and not ErrUnreadFields", name, err)
		}
		if _, err := display(lab, planread.Current); !errors.Is(err, planread.ErrUnsupported) {
			t.Fatalf("%s: LoadForDisplay() error = %v, want ErrUnsupported", name, err)
		}
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

	whole, err := strict(stored(t, bare), planread.Current)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if whole.Statements[0].SQL != read {
		t.Fatalf("Load() read %q, so this row does not hold what it says", whole.Statements[0].SQL)
	}

	shown, err := display(stored(t, withField), planread.Current)
	if err != nil {
		t.Fatalf("LoadForDisplay() error = %v", err)
	}
	if !reflect.DeepEqual(shown.Statements, whole.Statements) {
		t.Fatalf("a display read shows %+v, and Load reads %+v", shown.Statements, whole.Statements)
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
	want, err := strict(stored(t, planreadtest.Document(t, statements)), planread.Current)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if want.Statements[0].Severity != "destructive" || want.Statements[0].StoredSeverity != "safe" ||
		len(want.UnrecordedPrivilegeChanges) == 0 {
		t.Fatalf("the plain read did not raise anything, so this row measures nothing: %+v", want)
	}

	got, err := display(stored(t, planreadtest.DocumentWithUnreadFields(t, statements)), planread.Current)
	if err != nil {
		t.Fatalf("LoadForDisplay() error = %v", err)
	}
	if !reflect.DeepEqual(got.Statements, want.Statements) {
		t.Fatalf("the statements read as %+v, and without the fields as %+v", got.Statements, want.Statements)
	}
	if !slices.Equal(got.UnrecordedPrivilegeChanges, want.UnrecordedPrivilegeChanges) {
		t.Fatalf("the privileges read as %v, and without the fields as %v", got.UnrecordedPrivilegeChanges, want.UnrecordedPrivilegeChanges)
	}
}

// Every plan document Ptah wrote that the repository keeps, with one key added as
// text, runs the walk over Ptah's own indented bytes and over every declared
// field Ptah wrote. A declared field the walk missed comes back as a second
// unread name, and a statement it damaged comes back different from what Load
// reads in the document without the key.
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
			holdKeyAdded(t, document, func(spec *operatorv1alpha1.PtahSchemaPlanSpec) { spec.Dialect = header.Dialect })
		})
	}
}

// No document Ptah wrote that the repository keeps holds schemas_beyond_url, or
// every declared field at once, so this one does, indented as Ptah writes it.
func TestBothReadsTakeEveryFieldTheBuildDeclares(t *testing.T) {
	t.Parallel()
	var decoded map[string]any
	if err := json.Unmarshal(planreadtest.Document(t, oneStatement), &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["exclude"] = []any{"audit_*"}
	decoded["schemas_beyond_url"] = []any{"billing"}
	decoded["scope"] = map[string]any{"kind": "schema", "name": "shop"}
	decoded["rows_fingerprint"] = rowsDigest
	decoded["managed_rows"] = []any{map[string]any{
		"schema": "shop", "table": "customers", "keys": []any{"id"}, "columns": []any{"id", "email"},
	}}
	statement := statementOf(decoded, 0)
	statement["reason"] = "does not remove data"
	statement["transactions"] = []any{"none"}
	statement["owned"] = true
	statement["access"] = "widens"
	statement["access_reason"] = "a permissive policy"
	document, err := json.MarshalIndent(decoded, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	holdKeyAdded(t, document)
}

// holdKeyAdded holds a document Load reads whole to the same answer with one key
// added: Load refuses it, and a display read names that key and nothing else and
// shows what Load reads.
func holdKeyAdded(t *testing.T, document []byte, adjust ...func(*operatorv1alpha1.PtahSchemaPlanSpec)) {
	t.Helper()
	whole, err := strict(stored(t, document, adjust...), planread.Current)
	if err != nil {
		t.Fatalf("Load() refused the document as it was written: %v", err)
	}
	lab := stored(t, withAddedKey(t, document), adjust...)
	shown, err := display(lab, planread.Current)
	if err != nil {
		t.Fatalf("LoadForDisplay() error = %v", err)
	}
	if !slices.Equal(shown.UnreadFields, []string{addedKey}) {
		t.Fatalf("LoadForDisplay() names %v, want only %s", shown.UnreadFields, addedKey)
	}
	if !reflect.DeepEqual(shown.Statements, whole.Statements) {
		t.Fatalf("the statements read as\n%+v\nand without the key as\n%+v", shown.Statements, whole.Statements)
	}
	if !reflect.DeepEqual(shown.ManagedRows, whole.ManagedRows) ||
		!slices.Equal(shown.UnrecordedPrivilegeChanges, whole.UnrecordedPrivilegeChanges) {
		t.Fatalf("the row sets or privileges differ: %+v, and without the key %+v", shown, whole.Display())
	}
	holdNoValue(t, shown, err)
	refused, err := strict(lab, planread.Current)
	if !errors.Is(err, planread.ErrUnreadFields) || len(refused.Statements) != 0 {
		t.Fatalf("Load() = %d statements, %v; want ErrUnreadFields", len(refused.Statements), err)
	}
	holdNoValue(t, refused, err)
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
			// An unread field can take the place of a declared one. The statement
			// it leaves has no SQL, which the decoder refuses.
			"a field that takes the place of a statement's SQL",
			withUnread(func(d map[string]any) {
				delete(statementOf(d, 0), "sql")
				statementOf(d, 0)["native"] = unreadValue
			}),
		},
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

			shown, err := display(lab, planread.Current)
			if !errors.Is(err, planread.ErrUnsupported) {
				t.Fatalf("LoadForDisplay() error = %v, want ErrUnsupported", err)
			}
			if !reflect.DeepEqual(shown, planread.DisplayPlan{}) {
				t.Fatalf("LoadForDisplay() returned part of a plan with its error: %+v", shown)
			}
			holdNoValue(t, shown, err)

			whole, err := strict(lab, planread.Current)
			if !errors.Is(err, planread.ErrUnsupported) {
				t.Fatalf("Load() error = %v, want ErrUnsupported", err)
			}
			if len(whole.Statements) != 0 || len(whole.Document) != 0 || whole.PlanUID != "" {
				t.Fatal("Load() returned part of a plan with its error")
			}
			holdNoValue(t, whole, err)
		})
	}
}

// A display read prints the name of a field it did not read, and a newer Ptah
// adds an identifier. A key of another shape is not a field added to a plan, and
// printing it could put a credential or an escape sequence on a terminal. Neither
// read prints it: a display read refuses the plan, and so does Load, whose
// refusal reaches a terminal through approve and a page through the console.
func TestNeitherReadQuotesAKeyItWouldNotPrint(t *testing.T) {
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

			shown, err := display(lab, planread.Current)
			if !errors.Is(err, planread.ErrUnsupported) {
				t.Fatalf("LoadForDisplay() error = %v, want ErrUnsupported", err)
			}
			if strings.Contains(err.Error(), test.quoted) {
				t.Fatalf("the display read's refusal quotes the key: %v", err)
			}
			if !reflect.DeepEqual(shown, planread.DisplayPlan{}) {
				t.Fatal("LoadForDisplay() returned part of a plan with its error")
			}

			_, err = strict(lab, planread.Current)
			if !errors.Is(err, planread.ErrUnsupported) {
				t.Fatalf("Load() error = %v, want ErrUnsupported", err)
			}
			if strings.Contains(err.Error(), test.quoted) {
				t.Fatalf("Load's refusal quotes the key: %v", err)
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
	plan, err := display(lab, planread.Current)
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
			if _, err := display(lab, planread.Current); err != nil {
				t.Fatalf("LoadForDisplay() refused the plan before it was changed, so this row measures nothing: %v", err)
			}
			test.arrange(t, lab, plan)

			read, err := display(lab, planread.Current)

			if !errors.Is(err, test.want) {
				t.Fatalf("LoadForDisplay() error = %v, want %v", err, test.want)
			}
			if !reflect.DeepEqual(read, planread.DisplayPlan{}) {
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
