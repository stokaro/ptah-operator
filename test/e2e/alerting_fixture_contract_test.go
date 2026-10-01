package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuralcel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	validationfield "k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// Duration structs serialize zero as "0s" despite omitempty. API defaulting
// therefore cannot repair a fixture that discarded its producer's timeout.
func TestAlFixtureLockTimeoutSurvivesPolicyReplacement(t *testing.T) {
	for _, family := range []string{"schema", "migration"} {
		t.Run(family, func(t *testing.T) {
			path := filepath.Join("..", "..", "config", "crd", "bases", "operator.ptah.run_ptah"+family+"s.yaml")
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var crd apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(body, &crd); err != nil {
				t.Fatal(err)
			}
			rule := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["policy"].Properties["lockTimeout"]
			var internal apiextensions.JSONSchemaProps
			if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&rule, &internal, nil); err != nil {
				t.Fatal(err)
			}
			schema, err := structuralschema.NewStructural(&internal)
			if err != nil {
				t.Fatal(err)
			}
			validator := structuralcel.NewValidator(schema, false, celconfig.PerCallLimit)
			if validator == nil {
				t.Fatal("lock timeout has no validation rule")
			}
			valid := func(value any) bool {
				errs, _ := validator.Validate(context.Background(), validationfield.NewPath("spec", "policy", "lockTimeout"), schema, value, nil, celconfig.RuntimeCELCostBudget)
				return len(errs) == 0
			}
			if valid("0s") {
				t.Fatal("the CI failure no longer exercises a refusal")
			}
			for _, policy := range []ptahv1.ApplyPolicy{ptahv1.ApplyPolicyNever, ptahv1.ApplyPolicyOnApproval} {
				timeout := metav1.Duration{Duration: 47 * time.Second}
				var template client.Object = &ptahv1.PtahSchema{Spec: ptahv1.PtahSchemaSpec{Policy: ptahv1.ReconciliationPolicy{LockTimeout: timeout}}}
				if family == "migration" {
					template = &ptahv1.PtahMigration{Spec: ptahv1.PtahMigrationSpec{Policy: ptahv1.MigrationPolicy{LockTimeout: timeout}}}
				}
				fixture, err := alNegativeFixture(template, "control", "target", policy)
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(fixture)
				if err != nil {
					t.Fatal(err)
				}
				var document map[string]any
				if err := json.Unmarshal(encoded, &document); err != nil {
					t.Fatal(err)
				}
				value := document["spec"].(map[string]any)["policy"].(map[string]any)["lockTimeout"]
				// Apply the same field defaulting as the API: an explicit zero
				// remains present and must still be refused by its CEL rule.
				wrapper := &structuralschema.Structural{Properties: map[string]structuralschema.Structural{"lockTimeout": *schema}}
				object := map[string]any{"lockTimeout": value}
				structuraldefaulting.Default(object, wrapper)
				if object["lockTimeout"] != "47s" || !valid(object["lockTimeout"]) {
					t.Fatalf("%s fixture emitted invalid timeout %v", policy, value)
				}
				if !strings.Contains(string(encoded), `"apply":"`+string(policy)+`"`) {
					t.Fatal("requested policy was lost")
				}
			}
		})
	}
}
