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
	PtahSchemaCRDName         = "ptahschemas.operator.ptah.dev"
	PtahSchemaApprovalCRDName = "ptahschemaapprovals.operator.ptah.dev"
	PtahSchemaPlanCRDName     = "ptahschemaplans.operator.ptah.dev"
	// SchemaVersionAnnotation is the monotonic rollback fence owned by the CRD
	// manager. Every generated CRD schema change must increase its value.
	SchemaVersionAnnotation = "operator.ptah.dev/crd-schema-version"
	// SchemaDigestAnnotation binds a schema version to the normalized CRD spec
	// generated for that version. It prevents two different schemas from using
	// the same monotonic version.
	SchemaDigestAnnotation = "operator.ptah.dev/crd-schema-digest"
	// ControllerStateVersionAnnotation is a durable release-level downgrade
	// fence. A manager may start only after every owned CRD records the newest
	// controller-state contract installed for the cluster.
	ControllerStateVersionAnnotation = "operator.ptah.dev/controller-state-version"
	// CurrentCRDSchemaVersion must match CRD_SCHEMA_VERSION in the Makefile and
	// every generated CRD annotation.
	CurrentCRDSchemaVersion uint64 = 1
)

const predecessorRevision = "2c516a4b61073fefa694907d9f8623767d9e5542"

// predecessorSchemaDigests is the one annotation-free CRD set that may be
// adopted automatically. The values are normalized-spec digests from the
// exact predecessor revision above. Recognition is all-or-nothing so an
// unknown or mixed legacy schema cannot acquire trusted identity metadata.
var predecessorSchemaDigests = map[string]string{
	PtahSchemaApprovalCRDName: "sha256:3c2034e1292b0d074341262c06ed511af7a45835d4da98d9ff2a8003475e296a",
	PtahSchemaPlanCRDName:     "sha256:83ee43f4ae970c262e1abd0d619fa7b6761d52ed70a16f97272cd3893d01cd7a",
	PtahSchemaCRDName:         "sha256:5b71df79920a3599a61be7fe3ecb94153860c8c5c22e8d43b19138ecc7370288",
}

var expectedNames = []string{
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
