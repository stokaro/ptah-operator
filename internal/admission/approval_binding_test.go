package admission

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// An approval carries its decision -- which resource, which plan, and that
// plan's fingerprint -- and who made it. It carries no copy of what the plan
// binds. The fingerprint names every input the plan was decided from, so a
// copy would be a second place the plan's identity is spelled out: one more
// field to add in both families for every new binding, and one more value
// for the webhook and the controller to compare with the plan, or forget to
// (stokaro/ptah-operator#460). This reads the field list off the API types,
// so a copied binding that comes back fails by name.
func TestAnApprovalCarriesItsDecisionAndWhoMadeItAndNothingElse(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		spec   any
		fields []string
	}{
		"PtahSchemaApproval": {
			spec:   operatorv1alpha1.PtahSchemaApprovalSpec{},
			fields: []string{"schemaRef", "planRef", "planFingerprint", "approver", "approvedAt", "mutationRequestUID"},
		},
		"PtahMigrationApproval": {
			spec:   operatorv1alpha1.PtahMigrationApprovalSpec{},
			fields: []string{"migrationRef", "planRef", "planFingerprint", "approver", "approvedAt", "mutationRequestUID"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			specType := reflect.TypeOf(test.spec)
			got := make([]string, 0, specType.NumField())
			for index := range specType.NumField() {
				tag := specType.Field(index).Tag.Get("json")
				got = append(got, strings.Split(tag, ",")[0])
			}
			if !slices.Equal(got, test.fields) {
				t.Fatalf("%s spec carries %v, want exactly %v: a plan binding copied onto the approval "+
					"is one the plan fingerprint already names", name, got, test.fields)
			}
		})
	}
}
