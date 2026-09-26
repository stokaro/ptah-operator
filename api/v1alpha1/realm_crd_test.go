package v1alpha1_test

import (
	"context"
	"path/filepath"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuralcel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	structurallisttype "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

// generatedValidator is what the API server runs over one kind on the way in:
// the OpenAPI schema, the list semantics and the CEL rules.
type generatedValidator struct {
	name       string
	structural *structuralschema.Structural
	cel        *structuralcel.Validator
	schema     *apiextensions.JSONSchemaProps
}

func loadGeneratedValidator(t *testing.T, file string) generatedValidator {
	t.Helper()

	crd := loadGeneratedCRD(t, filepath.Join(repositoryRoot(t), "config", "crd", "bases", file))
	schema := storageVersionSchema(t, crd)
	structural, err := structuralschema.NewStructural(schema)
	if err != nil {
		t.Fatalf("build structural %s schema: %v", file, err)
	}
	validator := structuralcel.NewValidator(structural, true, celconfig.PerCallLimit)
	if validator == nil {
		t.Fatalf("generated %s schema did not compile a CEL validator", file)
	}
	return generatedValidator{name: file, structural: structural, cel: validator, schema: schema}
}

// validate returns every refusal the API server would make, with old nil on a
// create.
func (v generatedValidator) validate(t *testing.T, object, old map[string]interface{}) field.ErrorList {
	t.Helper()

	structuraldefaulting.Default(object, v.structural)
	if old != nil {
		structuraldefaulting.Default(old, v.structural)
	}
	schemaValidator, _, err := apiservervalidation.NewSchemaValidator(v.schema)
	if err != nil {
		t.Fatalf("build the %s schema validator: %v", v.name, err)
	}
	errs := apiservervalidation.ValidateCustomResource(field.NewPath(""), object, schemaValidator)
	errs = append(errs, structurallisttype.ValidateListSetsAndMaps(field.NewPath(""), v.structural, object)...)
	var oldObject interface{}
	if old != nil {
		oldObject = old
	}
	celErrs, _ := v.cel.Validate(context.Background(), field.NewPath(""), v.structural, object, oldObject,
		celconfig.RuntimeCELCostBudget)
	return append(errs, celErrs...)
}

// A target names its realm one way. A key and a realm together would leave
// the resource in two censuses and under two Leases, and neither would name
// nothing at all, so the API refuses both shapes before a controller sees them.
func TestGeneratedTargetsNameExactlyOneRealm(t *testing.T) {
	t.Parallel()

	for _, kind := range []struct {
		name   string
		file   string
		source string
	}{
		{name: "PtahSchema", file: "operator.ptah.run_ptahschemas.yaml", source: "desired"},
		{name: "PtahMigration", file: "operator.ptah.run_ptahmigrations.yaml", source: "artifact"},
	} {
		kind := kind
		t.Run(kind.name, func(t *testing.T) {
			t.Parallel()

			validator := loadGeneratedValidator(t, kind.file)
			for _, row := range []struct {
				name     string
				key      bool
				realm    bool
				accepted bool
			}{
				{name: "a namespace key", key: true, accepted: true},
				{name: "a realm", realm: true, accepted: true},
				{name: "both", key: true, realm: true, accepted: false},
				{name: "neither", accepted: false},
			} {
				target := map[string]interface{}{
					"engine":  "PostgreSQL",
					"urlFrom": map[string]interface{}{"name": "database", "key": "url"},
				}
				if row.key {
					target["coordinationKey"] = "tenant-a/orders"
				}
				if row.realm {
					target["realmRef"] = map[string]interface{}{"name": "orders-primary"}
				}
				object := map[string]interface{}{
					"apiVersion": "operator.ptah.run/v1alpha1",
					"kind":       kind.name,
					"metadata":   map[string]interface{}{"name": "orders", "namespace": "tenant-a"},
					"spec": map[string]interface{}{
						"target": target,
						kind.source: map[string]interface{}{
							"ociRef":                 "oci://registry.example/artifact:current",
							"verificationPolicyFrom": map[string]interface{}{"name": "verification", "key": "policy.yaml"},
						},
					},
				}
				errs := validator.validate(t, object, nil)
				if row.accepted && len(errs) != 0 {
					t.Fatalf("%s: a target naming %s was refused: %v", kind.name, row.name, errs.ToAggregate())
				}
				if !row.accepted && len(errs) == 0 {
					t.Fatalf("%s: a target naming %s was accepted", kind.name, row.name)
				}
			}
		})
	}
}

func TestGeneratedPtahRealmCRDIsAClusterScopedGrant(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repositoryRoot(t), "config", "crd", "bases", "operator.ptah.run_ptahrealms.yaml")
	crd := loadGeneratedCRD(t, path)
	if errs := apiextensionsvalidation.ValidateCustomResourceDefinition(context.Background(), crd); len(errs) != 0 {
		t.Fatalf("API server rejected the generated PtahRealm CRD: %v", errs.ToAggregate())
	}
	if crd.Spec.Scope != apiextensions.ClusterScoped {
		t.Fatalf("PtahRealm scope = %q, want Cluster: a grant that lived in a namespace would be writable by that namespace", crd.Spec.Scope)
	}
	// Nothing reconciles a realm's status, and a status subresource would be
	// a second write path into the grant for anything holding it.
	for _, version := range crd.Spec.Versions {
		if version.Subresources != nil && version.Subresources.Status != nil {
			t.Fatalf("PtahRealm %s serves a status subresource nothing writes", version.Name)
		}
	}
}

func realmObject(engine string, namespaces []interface{}, sharing string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "operator.ptah.run/v1alpha1",
		"kind":       "PtahRealm",
		"metadata":   map[string]interface{}{"name": "orders-primary"},
		"spec": map[string]interface{}{
			"engine": engine, "namespaces": namespaces, "sharing": sharing,
		},
	}
}

func TestGeneratedPtahRealmCRDValidatesTheGrant(t *testing.T) {
	t.Parallel()

	validator := loadGeneratedValidator(t, "operator.ptah.run_ptahrealms.yaml")
	for _, row := range []struct {
		name     string
		object   map[string]interface{}
		accepted bool
	}{
		{name: "a complete grant", object: realmObject("PostgreSQL", []interface{}{"team-a", "team-b"}, "Shared"), accepted: true},
		{name: "exclusive", object: realmObject("MySQL", []interface{}{"team-a"}, "Exclusive"), accepted: true},
		{name: "no namespace", object: realmObject("PostgreSQL", []interface{}{}, "Shared")},
		{name: "a namespace twice", object: realmObject("PostgreSQL", []interface{}{"team-a", "team-a"}, "Shared")},
		{name: "a name that is not a namespace", object: realmObject("PostgreSQL", []interface{}{"Team-A"}, "Shared")},
		{name: "a selector-shaped entry", object: realmObject("PostgreSQL", []interface{}{"*"}, "Shared")},
		{name: "no sharing stated", object: realmObject("PostgreSQL", []interface{}{"team-a"}, "")},
	} {
		errs := validator.validate(t, row.object, nil)
		if row.accepted && len(errs) != 0 {
			t.Fatalf("%s was refused: %v", row.name, errs.ToAggregate())
		}
		if !row.accepted && len(errs) == 0 {
			t.Fatalf("%s was accepted", row.name)
		}
	}
}

// A realm is one physical database, and a database does not change engine.
// The namespaces and the sharing do change: that is what administering a realm
// is.
func TestGeneratedPtahRealmCRDKeepsItsEngine(t *testing.T) {
	t.Parallel()

	validator := loadGeneratedValidator(t, "operator.ptah.run_ptahrealms.yaml")
	stored := func() map[string]interface{} {
		return realmObject("PostgreSQL", []interface{}{"team-a"}, "Exclusive")
	}
	if errs := validator.validate(t,
		realmObject("MySQL", []interface{}{"team-a"}, "Exclusive"), stored()); len(errs) == 0 {
		t.Fatal("a realm's engine was changed on update")
	}
	if errs := validator.validate(t,
		realmObject("PostgreSQL", []interface{}{"team-a", "team-b"}, "Shared"), stored()); len(errs) != 0 {
		t.Fatalf("an administrator could not change who a realm admits: %v", errs.ToAggregate())
	}
}
