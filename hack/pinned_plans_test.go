package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A stored plan may be deleted only when nothing points at it any more, and
// "nothing" is a set of fields across four kinds. An operator pruning by age or
// by count has to know that set exactly: deleting a plan something still names
// takes away the bytes an approval authorized, the evidence an applied schema
// rests on, or -- worst -- the plan of a run nobody accounted for.
//
// So the set is derived from the API types rather than listed by hand, and the
// guide has to name every member. A field added later fails this test instead of
// silently making the documented procedure unsafe.
func pinnedPlanPaths(t *testing.T) []string {
	t.Helper()

	var paths []string
	visited := map[reflect.Type]int{}
	var walk func(structType reflect.Type, path string, depth int)
	walk = func(structType reflect.Type, path string, depth int) {
		if depth > 8 {
			return
		}
		for structType.Kind() == reflect.Ptr || structType.Kind() == reflect.Slice {
			structType = structType.Elem()
		}
		if structType.Kind() != reflect.Struct {
			return
		}
		if visited[structType] > 2 {
			return
		}
		visited[structType]++
		for index := 0; index < structType.NumField(); index++ {
			field := structType.Field(index)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				name = field.Name
			}
			next := path + "." + name
			// Any struct-typed field whose name mentions a plan is a pin,
			// not only the two spellings in use today. The families already
			// differ -- one carries an object reference, the other a plan
			// summary -- and matching the exact names would let a third
			// spelling be added without the guide noticing, which is the
			// failure this test exists to prevent.
			//
			// A string field is not a pin: a fingerprint identifies a plan
			// without naming the object, and nothing can be deleted by it.
			if strings.Contains(strings.ToLower(field.Name), "plan") && namesAnObject(field.Type) {
				paths = append(paths, next)
				continue
			}
			walk(field.Type, next, depth+1)
		}
	}
	for _, object := range []any{
		&operatorv1alpha1.PtahSchema{},
		&operatorv1alpha1.PtahMigration{},
		&operatorv1alpha1.PtahSchemaApproval{},
		&operatorv1alpha1.PtahMigrationApproval{},
	} {
		objectType := reflect.TypeOf(object).Elem()
		visited = map[reflect.Type]int{}
		walk(objectType, strings.ToLower(objectType.Name()), 0)
	}
	sort.Strings(paths)
	unique := paths[:0]
	for index, path := range paths {
		if index == 0 || path != paths[index-1] {
			unique = append(unique, path)
		}
	}
	if len(unique) < 5 {
		t.Fatalf("derived %v as the plan pins, and there are more than that", unique)
	}
	return unique
}

// namesAnObject reports whether this type can name a stored object, which is
// what a pruner needs in order to keep it.
func namesAnObject(fieldType reflect.Type) bool {
	for fieldType.Kind() == reflect.Ptr || fieldType.Kind() == reflect.Slice {
		fieldType = fieldType.Elem()
	}
	if fieldType.Kind() != reflect.Struct {
		return false
	}
	_, named := fieldType.FieldByName("Name")
	return named
}

func TestThePruningGuideNamesEveryPlanPin(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile(filepath.Join("..", "docs", "site", "src", "content",
		"docs", "use", "operations.md"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(contents)
	section := "## Pruning stored plans"
	start := strings.Index(page, section)
	if start < 0 {
		t.Fatalf("operations.md has no %q section, so nothing documents which plans may be deleted", section)
	}
	end := strings.Index(page[start+len(section):], "\n## ")
	if end < 0 {
		end = len(page) - start - len(section)
	}
	pruning := page[start : start+len(section)+end]

	for _, path := range pinnedPlanPaths(t) {
		if !strings.Contains(pruning, path) {
			t.Fatalf("the pruning guide does not name %s, so a procedure following it would delete a plan that field still pins", path)
		}
	}
}
