package crdupgrade

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
)

// Every binding of an execution requires what decides a plan's meaning when it
// runs, and every record of one also requires the manager that published it.
// The binding itself carries nothing about that manager: a manager release
// that changes only its own build keeps the epoch and every approval. An
// approval carries nothing about the execution at all: it names the plan by
// UID and fingerprint, and the plan is where the binding is. The plan contract
// has one version, and nothing in the API is optional only so that an earlier
// shape could still be read.
func TestGeneratedExecutionIdentityContract(t *testing.T) {
	candidates := mustCandidates(t)
	schemaRoot := candidateVersionSchema(t, candidateByName(candidates, PtahSchemaCRDName))
	planRoot := candidateVersionSchema(t, candidateByName(candidates, PtahSchemaPlanCRDName))
	approvalRoot := candidateVersionSchema(t, candidateByName(candidates, PtahSchemaApprovalCRDName))
	migrationRoot := candidateVersionSchema(t, candidateByName(candidates, PtahMigrationCRDName))
	migrationPlanRoot := candidateVersionSchema(t, candidateByName(candidates, PtahMigrationPlanCRDName))
	migrationApprovalRoot := candidateVersionSchema(t, candidateByName(candidates, PtahMigrationApprovalCRDName))

	bound := []string{"controllerStateVersion", "ptahVersion", "executorImage", "runnerProtocolVersion"}
	publisher := []string{"controllerImage", "controllerRevision", "runnerImage"}

	for _, location := range []struct {
		name   string
		schema apiextensionsv1.JSONSchemaProps
	}{
		{name: "PtahSchema status.executionBinding", schema: schemaProperty(t, schemaRoot, "status", "executionBinding")},
		{name: "PtahMigration status.executionBinding", schema: schemaProperty(t, migrationRoot, "status", "executionBinding")},
	} {
		assertRequired(t, location.name, location.schema, append([]string{"epoch"}, bound...)...)
		for _, field := range publisher {
			if _, found := location.schema.Properties[field]; found {
				t.Errorf("%s carries %s, so a manager release would retire it", location.name, field)
			}
		}
	}

	for _, location := range []struct {
		name   string
		schema apiextensionsv1.JSONSchemaProps
	}{
		{name: "PtahSchemaApproval spec", schema: schemaProperty(t, approvalRoot, "spec")},
		{name: "PtahMigrationApproval spec", schema: schemaProperty(t, migrationApprovalRoot, "spec")},
	} {
		assertRequired(t, location.name, location.schema, "planRef", "planFingerprint")
		for _, field := range append(append([]string{"executionBindingID"}, bound...), publisher...) {
			if _, found := location.schema.Properties[field]; found {
				t.Errorf("%s carries %s, which the plan fingerprint it names already binds", location.name, field)
			}
		}
	}

	for _, location := range []struct {
		name   string
		schema apiextensionsv1.JSONSchemaProps
	}{
		{name: "PtahSchema status.plan", schema: schemaProperty(t, schemaRoot, "status", "plan")},
		{name: "PtahSchema status.applied", schema: schemaProperty(t, schemaRoot, "status", "applied")},
		{name: "PtahSchema status.pendingObservation.plan", schema: schemaProperty(t, schemaRoot, "status", "pendingObservation", "plan")},
		{name: "PtahSchemaPlan spec", schema: schemaProperty(t, planRoot, "spec")},
		{name: "PtahMigrationPlan spec", schema: schemaProperty(t, migrationPlanRoot, "spec")},
	} {
		required := append(append([]string{"executionBindingID"}, bound...), publisher...)
		assertRequired(t, location.name, location.schema, required...)
	}

	contractVersion := schemaProperty(t, planRoot, "spec", "contractVersion")
	if len(contractVersion.Enum) != 1 || string(contractVersion.Enum[0].Raw) != "3" {
		t.Fatalf("PtahSchemaPlan spec.contractVersion enum = %v, want exactly [3]", contractVersion.Enum)
	}
}

func TestGeneratedCRDsCarrySchemaIdentityFence(t *testing.T) {
	for _, candidate := range mustCandidates(t) {
		version, err := schemaVersion(candidate, false)
		if err != nil {
			t.Fatalf("candidate %s: %v", candidate.Name, err)
		}
		if version != CurrentCRDSchemaVersion {
			t.Fatalf("candidate %s schema version = %d, want %d", candidate.Name, version, CurrentCRDSchemaVersion)
		}
		digest, err := schemaDigest(candidate, false)
		if err != nil {
			t.Fatalf("candidate %s: %v", candidate.Name, err)
		}
		computed, err := ComputeSchemaDigest(candidate)
		if err != nil {
			t.Fatalf("candidate %s: %v", candidate.Name, err)
		}
		if digest != computed {
			t.Fatalf("candidate %s schema digest = %q, want %q", candidate.Name, digest, computed)
		}
		stateVersion, err := controllerStateVersion(candidate, false)
		if err != nil {
			t.Fatalf("candidate %s: %v", candidate.Name, err)
		}
		if stateVersion != uint64(controllerstate.CurrentVersion) {
			t.Fatalf("candidate %s controller-state version = %d, want %d", candidate.Name, stateVersion, controllerstate.CurrentVersion)
		}
	}
}

func candidateVersionSchema(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	for _, version := range crd.Spec.Versions {
		if version.Name == "v1alpha1" {
			if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				t.Fatalf("CRD %s v1alpha1 schema is missing", crd.Name)
			}
			return *version.Schema.OpenAPIV3Schema.DeepCopy()
		}
	}
	t.Fatalf("CRD %s has no v1alpha1 version", crd.Name)
	return apiextensionsv1.JSONSchemaProps{}
}

func schemaProperty(t *testing.T, root apiextensionsv1.JSONSchemaProps, path ...string) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	current := root
	for _, segment := range path {
		next, found := current.Properties[segment]
		if !found {
			t.Fatalf("generated schema property %s is missing", segment)
		}
		current = next
	}
	return current
}

func assertRequired(t *testing.T, location string, schema apiextensionsv1.JSONSchemaProps, fields ...string) {
	t.Helper()
	required := make(map[string]struct{}, len(schema.Required))
	for _, field := range schema.Required {
		required[field] = struct{}{}
	}
	for _, field := range fields {
		if _, found := required[field]; !found {
			t.Fatalf("%s does not require %s; required=%v", location, field, schema.Required)
		}
	}
}
