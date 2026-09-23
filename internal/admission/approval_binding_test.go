package admission

import (
	"reflect"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// fillComparableFields gives every string-like and int32 field a value derived
// from its own name, so two structs filled this way agree on every field they
// share a name for. Nothing else is touched: the matchers read no other kind.
func fillComparableFields(target any) {
	value := reflect.ValueOf(target).Elem()
	structType := value.Type()
	for index := range structType.NumField() {
		field := value.Field(index)
		switch field.Kind() {
		case reflect.String:
			field.SetString(structType.Field(index).Name + "-value")
		case reflect.Int32:
			field.SetInt(int64(index + 1))
		}
	}
}

// copyComparableFields makes the approval agree with the plan wherever they
// name the same thing. renamed carries the pairs that do not share a name.
func copyComparableFields(target any, source any, renamed map[string]string) {
	destination := reflect.ValueOf(target).Elem()
	origin := reflect.ValueOf(source)
	destinationType := destination.Type()
	for index := range destinationType.NumField() {
		name := destinationType.Field(index).Name
		if alias, ok := renamed[name]; ok {
			name = alias
		}
		from := origin.FieldByName(name)
		if !from.IsValid() || from.Type() != destination.Field(index).Type() {
			continue
		}
		switch from.Kind() {
		case reflect.String, reflect.Int32:
			destination.Field(index).Set(from)
		}
	}
}

func perturb(field reflect.Value) {
	switch field.Kind() {
	case reflect.String:
		field.SetString(field.String() + "-changed")
	case reflect.Int32:
		field.SetInt(field.Int() + 1)
	}
}

// comparableField reports whether the matchers could read this field at all.
// A struct, a slice or a time is carried by the approval and compared
// elsewhere, by identity rather than by value.
func comparableField(field reflect.StructField) bool {
	return field.Type.Kind() == reflect.String || field.Type.Kind() == reflect.Int32
}

// Every binding an approval carries has to be compared with the plan it names,
// because the approval is the only thing that says a person agreed to this
// exact plan. A field the approval carries and the matcher does not read lets
// an approval stand for a plan that differs in it.
//
// The comparisons live in a map and a list, so coverage cannot see a missing
// entry: one mismatching field exercises the whole loop. Six of them could be
// deleted with the admission suite staying green. These tests read the fields
// off the API types instead, so an entry that goes missing -- or a field added
// without one -- fails by name.

func TestEveryBindingAMigrationApprovalCarriesIsComparedWithItsPlan(t *testing.T) {
	t.Parallel()

	// Carried by the approval and not a binding: identity, and who agreed.
	exempt := map[string]string{
		"MigrationRef":       "identity, checked against the request's own object",
		"PlanRef":            "identity, checked against the migration's current plan",
		"Approver":           "who agreed, not what was agreed to",
		"ApprovedAt":         "when, not what",
		"MutationRequestUID": "stamped by the webhook, not a property of the plan",
	}

	var plan operatorv1alpha1.PtahMigrationPlanSpec
	fillComparableFields(&plan)
	var base operatorv1alpha1.PtahMigrationApprovalSpec
	copyComparableFields(&base, plan, map[string]string{"PlanFingerprint": "Fingerprint"})
	if err := migrationApprovalMatchesPlan(base, plan); err != nil {
		t.Fatalf("the constructed pair does not match, so nothing below proves anything: %v", err)
	}

	specType := reflect.TypeOf(operatorv1alpha1.PtahMigrationApprovalSpec{})
	for index := range specType.NumField() {
		field := specType.Field(index)
		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()

			if reason, ok := exempt[field.Name]; ok {
				if comparableField(field) && reason == "" {
					t.Fatalf("%s is exempt for no stated reason", field.Name)
				}
				return
			}
			if !comparableField(field) {
				t.Fatalf("%s is carried by the approval, is not a plain value, and is not "+
					"listed as compared elsewhere", field.Name)
			}
			mutated := base
			perturb(reflect.ValueOf(&mutated).Elem().Field(index))
			if err := migrationApprovalMatchesPlan(mutated, plan); err == nil {
				t.Fatalf("%s is carried by the approval and never compared with the plan: an "+
					"approval stands for a plan that differs in it", field.Name)
			}
		})
	}
}

func TestEveryBindingASchemaApprovalCarriesIsComparedWithItsPlan(t *testing.T) {
	t.Parallel()

	exempt := map[string]string{
		"SchemaRef":          "identity, checked against the request's own object",
		"PlanRef":            "identity, checked against the schema's current plan",
		"Approver":           "who agreed, not what was agreed to",
		"ApprovedAt":         "when, not what",
		"MutationRequestUID": "stamped by the webhook, not a property of the plan",
	}

	var plan operatorv1alpha1.PtahSchemaPlanSpec
	fillComparableFields(&plan)
	var base operatorv1alpha1.PtahSchemaApprovalSpec
	copyComparableFields(&base, plan, map[string]string{"PlanFingerprint": "Fingerprint"})
	if err := approvalMatchesPlan(base, plan); err != nil {
		t.Fatalf("the constructed pair does not match, so nothing below proves anything: %v", err)
	}

	specType := reflect.TypeOf(operatorv1alpha1.PtahSchemaApprovalSpec{})
	for index := range specType.NumField() {
		field := specType.Field(index)
		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()

			if _, ok := exempt[field.Name]; ok {
				return
			}
			if !comparableField(field) {
				t.Fatalf("%s is carried by the approval, is not a plain value, and is not "+
					"listed as compared elsewhere", field.Name)
			}
			mutated := base
			perturb(reflect.ValueOf(&mutated).Elem().Field(index))
			if err := approvalMatchesPlan(mutated, plan); err == nil {
				t.Fatalf("%s is carried by the approval and never compared with the plan: an "+
					"approval stands for a plan that differs in it", field.Name)
			}
		})
	}
}
