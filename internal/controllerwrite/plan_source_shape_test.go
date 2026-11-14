package controllerwrite

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
)

// A plan computed from a source binds the source's revision in place of an
// artifact digest, and the revision's form is the kind's: a ConfigMap's UID,
// or the "sha256:" digest of the URL and the path for Git and HTTPS. Each row
// recomputes the fingerprint and the contract version over the source it
// holds, so a refusal can only come from the source check and an acceptance
// shows the rest of the shape held.
func TestValidatePlanShapeHoldsEachSourceKindToItsRevision(t *testing.T) {
	t.Parallel()
	shapeDigest := func(char byte) string { return "sha256:" + strings.Repeat(string(char), 64) }
	for _, row := range []struct {
		name    string
		source  operatorv1alpha1.PlanSourceBinding
		refused bool
	}{
		{name: "a ConfigMap by its UID", source: operatorv1alpha1.PlanSourceBinding{
			Kind: operatorv1alpha1.SourceKindConfigMap, Revision: "0b6f9a52-2d3e-4c1a-9f0e-6b1d2c3e4f50",
		}},
		{name: "a Git source by the digest of its URL and path", source: operatorv1alpha1.PlanSourceBinding{
			Kind: operatorv1alpha1.SourceKindGit, Revision: shapeDigest('4'),
		}},
		{name: "an HTTPS source by the digest of its URL and path", source: operatorv1alpha1.PlanSourceBinding{
			Kind: operatorv1alpha1.SourceKindHTTPS, Revision: shapeDigest('5'),
		}},
		{name: "an HTTPS source with its URL as the revision", refused: true, source: operatorv1alpha1.PlanSourceBinding{
			Kind: operatorv1alpha1.SourceKindHTTPS, Revision: "https://files.example.com/app/1.4.0/schema.hcl",
		}},
		{name: "an HTTPS source with an uppercase revision digest", refused: true, source: operatorv1alpha1.PlanSourceBinding{
			Kind: operatorv1alpha1.SourceKindHTTPS, Revision: strings.ToUpper(shapeDigest('a')),
		}},
		{name: "a Git source with its URL as the revision", refused: true, source: operatorv1alpha1.PlanSourceBinding{
			Kind: operatorv1alpha1.SourceKindGit, Revision: "https://git.example.com/acme/app.git",
		}},
		{name: "a kind the API does not name", refused: true, source: operatorv1alpha1.PlanSourceBinding{
			Kind: "OCI", Revision: shapeDigest('6'),
		}},
		{name: "an HTTPS source whose content digest is not one", refused: true, source: operatorv1alpha1.PlanSourceBinding{
			Kind: operatorv1alpha1.SourceKindHTTPS, Revision: shapeDigest('5'), ContentDigest: "sha256:abc",
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			source := row.source
			if source.ContentDigest == "" {
				source.ContentDigest = shapeDigest('7')
			}
			schema, plan := sourcePlanFixture(t, &source)
			err := validatePlanShape(plan, schema)
			switch {
			case row.refused && (err == nil || err.Error() != "plan contains an invalid source binding"):
				t.Fatalf("validatePlanShape() = %v, want the source binding refused", err)
			case !row.refused && err != nil:
				t.Fatalf("validatePlanShape() = %v, want the plan accepted", err)
			}
		})
	}
}

// sourcePlanFixture is a schema and a plan computed from source, prepared by
// the plan store, with the contract version and the fingerprint the source
// binding calls for.
func sourcePlanFixture(t *testing.T, source *operatorv1alpha1.PlanSourceBinding) (*operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaPlan) {
	t.Helper()
	shapeDigest := func(char byte) string { return "sha256:" + strings.Repeat(string(char), 64) }
	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "application", UID: "schema-uid"},
		Spec: operatorv1alpha1.PtahSchemaSpec{Target: operatorv1alpha1.DatabaseTargetSpec{
			Engine: operatorv1alpha1.DatabaseEnginePostgreSQL,
		}},
	}
	policy, err := schemaPolicyFingerprint(schema)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("CREATE TABLE orders (id bigint PRIMARY KEY);\n")
	spec := operatorv1alpha1.PtahSchemaPlanSpec{
		ContractVersion:         sourcebinding.ContractVersion(source),
		SchemaRef:               operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
		ContentDigest:           fingerprint.DigestBytes(content),
		Source:                  source,
		CoordinationDigest:      shapeDigest('c'),
		TargetIdentityDigest:    shapeDigest('d'),
		ActualStateFingerprint:  shapeDigest('a'),
		DesiredStateFingerprint: shapeDigest('b'),
		PolicyFingerprint:       policy,
		ExecutionBindingID:      "v1-" + strings.Repeat("9", 32),
		ControllerImage:         "example.test/controller@" + shapeDigest('1'),
		ControllerRevision:      "test-revision",
		ControllerStateVersion:  1,
		PtahVersion:             "v0.26.0",
		ExecutorImage:           "example.test/ptah@" + shapeDigest('2'),
		RunnerImage:             "example.test/runner@" + shapeDigest('3'),
		RunnerProtocolVersion:   1,
		Dialect:                 "postgres",
		StatementCount:          1,
	}
	if spec.Fingerprint, err = planstore.Binding(schema.UID, spec).Fingerprint(); err != nil {
		t.Fatal(err)
	}
	plan, _, err := planstore.Prepare(schema, spec, content)
	if err != nil {
		t.Fatalf("planstore.Prepare() = %v", err)
	}
	return schema, plan
}
