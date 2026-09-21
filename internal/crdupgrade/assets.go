package crdupgrade

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

const (
	PtahSchemaCRDName         = "ptahschemas.operator.ptah.run"
	PtahSchemaApprovalCRDName = "ptahschemaapprovals.operator.ptah.run"
	PtahSchemaPlanCRDName     = "ptahschemaplans.operator.ptah.run"
	// The versioned-migration kinds. They are owned by the same manager as the
	// schema kinds because they share one release and one rollback fence: a
	// cluster that has the newer schema CRDs and not these would be running a
	// controller whose own API is half installed.
	PtahMigrationCRDName         = "ptahmigrations.operator.ptah.run"
	PtahMigrationApprovalCRDName = "ptahmigrationapprovals.operator.ptah.run"
	PtahMigrationPlanCRDName     = "ptahmigrationplans.operator.ptah.run"
	// SchemaVersionAnnotation is the monotonic rollback fence owned by the CRD
	// manager. Every generated CRD schema change must increase its value.
	SchemaVersionAnnotation = "operator.ptah.run/crd-schema-version"
	// SchemaDigestAnnotation binds a schema version to the normalized CRD spec
	// generated for that version. It prevents two different schemas from using
	// the same monotonic version.
	SchemaDigestAnnotation = "operator.ptah.run/crd-schema-digest"
	// ControllerStateVersionAnnotation is a durable release-level downgrade
	// fence. A manager may start only after every owned CRD records the newest
	// controller-state contract installed for the cluster.
	ControllerStateVersionAnnotation = "operator.ptah.run/controller-state-version"
	// CurrentCRDSchemaVersion must match CRD_SCHEMA_VERSION in the Makefile and
	// every generated CRD annotation.
	CurrentCRDSchemaVersion uint64 = 15
)

var expectedNames = []string{
	PtahMigrationApprovalCRDName,
	PtahMigrationPlanCRDName,
	PtahMigrationCRDName,
	PtahSchemaApprovalCRDName,
	PtahSchemaPlanCRDName,
	PtahSchemaCRDName,
}

// candidateAssets are generated from config/crd/bases by make manifests.
//
//go:embed assets/*.yaml
var candidateAssets embed.FS

// Candidates returns independently mutable copies of the candidate CRDs in
// deterministic name order.
func Candidates() ([]*apiextensionsv1.CustomResourceDefinition, error) {
	paths, err := fs.Glob(candidateAssets, "assets/*.yaml")
	if err != nil {
		return nil, fmt.Errorf("enumerate embedded CRDs: %w", err)
	}
	sort.Strings(paths)

	byName := make(map[string]*apiextensionsv1.CustomResourceDefinition, len(paths))
	for _, path := range paths {
		yamlBytes, readErr := candidateAssets.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("read embedded CRD %s: %w", path, readErr)
		}
		jsonBytes, convertErr := yaml.YAMLToJSON(yamlBytes)
		if convertErr != nil {
			return nil, fmt.Errorf("decode embedded CRD %s: %w", path, convertErr)
		}
		candidate := &apiextensionsv1.CustomResourceDefinition{}
		if unmarshalErr := json.Unmarshal(jsonBytes, candidate); unmarshalErr != nil {
			return nil, fmt.Errorf("unmarshal embedded CRD %s: %w", path, unmarshalErr)
		}
		if candidate.APIVersion != apiextensionsv1.SchemeGroupVersion.String() || candidate.Kind != "CustomResourceDefinition" {
			return nil, fmt.Errorf("embedded CRD %s has unexpected type %s %s", path, candidate.APIVersion, candidate.Kind)
		}
		if identityErr := validateCandidateIdentity(candidate); identityErr != nil {
			return nil, fmt.Errorf("embedded CRD %s: %w", path, identityErr)
		}
		if _, duplicate := byName[candidate.Name]; duplicate {
			return nil, fmt.Errorf("embedded CRD name %q is duplicated", candidate.Name)
		}
		byName[candidate.Name] = candidate
	}

	if len(byName) != len(expectedNames) {
		return nil, fmt.Errorf("embedded CRD set has %d entries, want %d", len(byName), len(expectedNames))
	}
	candidates := make([]*apiextensionsv1.CustomResourceDefinition, 0, len(expectedNames))
	for _, name := range expectedNames {
		candidate, found := byName[name]
		if !found {
			return nil, fmt.Errorf("embedded CRD set is missing %s", name)
		}
		candidates = append(candidates, candidate.DeepCopy())
	}
	return candidates, nil
}

// Names returns the complete immutable CRD allow-list used by RBAC and tests.
func Names() []string {
	return append([]string(nil), expectedNames...)
}
