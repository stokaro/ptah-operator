package crdupgrade

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
)

func TestGeneratedExecutionIdentityCompatibilityContract(t *testing.T) {
	candidates := mustCandidates(t)
	schemaRoot := candidateVersionSchema(t, candidateByName(candidates, PtahSchemaCRDName))
	planRoot := candidateVersionSchema(t, candidateByName(candidates, PtahSchemaPlanCRDName))
	approvalRoot := candidateVersionSchema(t, candidateByName(candidates, PtahSchemaApprovalCRDName))

	requiredIdentity := []string{"controllerImage", "controllerRevision", "controllerStateVersion"}
	executionBinding := schemaProperty(t, schemaRoot, "status", "executionBinding")
	assertOptional(t, "PtahSchema status.executionBinding", executionBinding, requiredIdentity...)

	for _, location := range []struct {
		name   string
		schema apiextensionsv1.JSONSchemaProps
	}{
		{name: "PtahSchema status.plan", schema: schemaProperty(t, schemaRoot, "status", "plan")},
		{name: "PtahSchema status.applied", schema: schemaProperty(t, schemaRoot, "status", "applied")},
		{name: "PtahSchema status.pendingObservation.plan", schema: schemaProperty(t, schemaRoot, "status", "pendingObservation", "plan")},
		{name: "PtahSchemaPlan spec", schema: schemaProperty(t, planRoot, "spec")},
		{name: "PtahSchemaApproval spec", schema: schemaProperty(t, approvalRoot, "spec")},
	} {
		assertOptional(t, location.name, location.schema, requiredIdentity...)
	}

	contractVersion := schemaProperty(t, planRoot, "spec", "contractVersion")
	if contractVersion.Minimum == nil || *contractVersion.Minimum != 1 {
		t.Fatalf("PtahSchemaPlan spec.contractVersion minimum = %v, want 1", contractVersion.Minimum)
	}
	if contractVersion.Maximum == nil || *contractVersion.Maximum != 3 {
		t.Fatalf("PtahSchemaPlan spec.contractVersion maximum = %v, want 3", contractVersion.Maximum)
	}
}

func TestGeneratedCRDsCarrySchemaIdentityFence(t *testing.T) {
	for _, candidate := range mustCandidates(t) {
		version, err := schemaVersion(candidate, false)
		if err != nil {
			t.Fatalf("candidate %s: %v", candidate.Name, err)
		}
		if version != 1 {
			t.Fatalf("candidate %s schema version = %d, want 1", candidate.Name, version)
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

func assertOptional(t *testing.T, location string, schema apiextensionsv1.JSONSchemaProps, fields ...string) {
	t.Helper()
	required := make(map[string]struct{}, len(schema.Required))
	for _, field := range schema.Required {
		required[field] = struct{}{}
	}
	for _, field := range fields {
		if _, found := required[field]; found {
			t.Fatalf("%s unexpectedly requires legacy-compatible field %s", location, field)
		}
		if _, found := schema.Properties[field]; !found {
			t.Fatalf("%s omits compatibility field %s", location, field)
		}
	}
}
