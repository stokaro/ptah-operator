package v1alpha1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structurallisttype "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/stokaro/ptah-operator/internal/plancontract"
)

func TestGeneratedCRDsContainSafetyCriticalFields(t *testing.T) {
	t.Parallel()

	repositoryRoot := repositoryRoot(t)
	tests := []struct {
		name     string
		file     string
		required []string
		obsolete []string
	}{
		{
			name: "schema",
			file: "operator.ptah.dev_ptahschemas.yaml",
			required: []string{
				"required:\n        - spec",
				"executionNotAfter:",
				"terminationGracePeriodSeconds:",
				"leaseEpoch:",
				"pendingLockRelease:",
				"applyJobName:",
				"applyPodUIDs:",
				"applyPodCount:",
				"verificationPolicyUID:",
				"maxLength: 256\n                      minLength: 1",
				"maxItems: 128\n                    type: array\n                    x-kubernetes-list-type: set",
			},
			obsolete: []string{
				"admissionRequestUID:",
				"observationIgnore:",
				"\n              ignore:",
			},
		},
		{
			name: "plan",
			file: "operator.ptah.dev_ptahschemaplans.yaml",
			required: []string{
				"required:\n        - spec",
				"verificationPolicyUID:",
			},
			obsolete: []string{
				"admissionRequestUID:",
			},
		},
		{
			name: "approval",
			file: "operator.ptah.dev_ptahschemaapprovals.yaml",
			required: []string{
				"required:\n        - spec",
				"mutationRequestUID:",
				"verificationPolicyUID:",
			},
			obsolete: []string{
				"admissionRequestUID:",
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			generatedPath := filepath.Join(repositoryRoot, "config", "crd", "bases", test.file)
			generated := readFile(t, generatedPath)
			for _, field := range test.required {
				if !bytes.Contains(generated, []byte(field)) {
					t.Errorf("generated CRD %s does not contain required field %q", test.file, field)
				}
			}
			for _, field := range test.obsolete {
				if bytes.Contains(generated, []byte(field)) {
					t.Errorf("generated CRD %s still contains obsolete field %q", test.file, field)
				}
			}

			chartPath := filepath.Join(repositoryRoot, "charts", "ptah-operator", "crds", test.file)
			chart := readFile(t, chartPath)
			if !bytes.Equal(generated, chart) {
				t.Errorf("chart CRD %s is not byte-identical to the generated source", test.file)
			}
		})
	}
}

func TestGeneratedCRDsPassAPIServerValidation(t *testing.T) {
	t.Parallel()

	repositoryRoot := repositoryRoot(t)
	for _, name := range []string{
		"operator.ptah.dev_ptahschemas.yaml",
		"operator.ptah.dev_ptahschemaplans.yaml",
		"operator.ptah.dev_ptahschemaapprovals.yaml",
	} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			crd := loadGeneratedCRD(t, filepath.Join(repositoryRoot, "config", "crd", "bases", name))
			if errs := apiextensionsvalidation.ValidateCustomResourceDefinition(context.Background(), crd); len(errs) != 0 {
				t.Fatalf("API server rejected generated CRD %s: %v", name, errs.ToAggregate())
			}
		})
	}
}

func TestGeneratedPtahSchemaCRDRejectsDuplicateExclusions(t *testing.T) {
	t.Parallel()

	crd := loadGeneratedCRD(t, filepath.Join(
		repositoryRoot(t),
		"config", "crd", "bases", "operator.ptah.dev_ptahschemas.yaml",
	))
	structural, err := structuralschema.NewStructural(storageVersionSchema(t, crd))
	if err != nil {
		t.Fatalf("build structural PtahSchema schema: %v", err)
	}

	customResource := map[string]interface{}{
		"spec": map[string]interface{}{
			"policy": map[string]interface{}{
				"exclude": []interface{}{"schema.users", "schema.users"},
			},
		},
	}
	errs := structurallisttype.ValidateListSetsAndMaps(field.NewPath("ptahschema"), structural, customResource)
	if len(errs) == 0 {
		t.Fatal("API server list validation accepted duplicate spec.policy.exclude selectors")
	}
	if errs[0].Type != field.ErrorTypeDuplicate {
		t.Fatalf("duplicate exclusion validation error = %v, want %s", errs.ToAggregate(), field.ErrorTypeDuplicate)
	}
}

func TestGeneratedPtahSchemaCRDBindsRegistryAccessPolicy(t *testing.T) {
	t.Parallel()

	crd := loadGeneratedCRD(t, filepath.Join(
		repositoryRoot(t),
		"config", "crd", "bases", "operator.ptah.dev_ptahschemas.yaml",
	))
	spec := storageVersionSchema(t, crd).Properties["spec"]
	desired := spec.Properties["desired"]
	auth := desired.Properties["registryAuthFrom"]
	authRegistryKey := auth.Properties["registryKey"]
	assertFixedRegistryAuthorityKey(t, "spec.desired.registryAuthFrom.registryKey", authRegistryKey)
	if _, selectable := auth.Properties["caSHA256Key"]; selectable {
		t.Error("spec.desired.registryAuthFrom exposes a selectable CA digest Secret key")
	}

	transport := desired.Properties["transport"]
	wantRules := map[string]bool{
		"!self.plainHTTP || !has(self.caFrom)":                false,
		"!self.plainHTTP || !has(self.clientCertificateFrom)": false,
		"!has(self.clientCertificateFrom)":                    false,
	}
	for _, validation := range transport.XValidations {
		if _, ok := wantRules[validation.Rule]; ok {
			wantRules[validation.Rule] = true
		}
	}
	for rule, found := range wantRules {
		if !found {
			t.Errorf("spec.desired.transport is missing validation %q", rule)
		}
	}
}

func assertFixedRegistryAuthorityKey(t *testing.T, path string, property apiextensions.JSONSchemaProps) {
	t.Helper()
	if property.Default == nil || *property.Default != "registry" {
		t.Errorf("%s default = %#v, want registry", path, property.Default)
	}
	if len(property.Enum) != 1 || property.Enum[0] != "registry" {
		t.Errorf("%s enum = %#v, want [registry]", path, property.Enum)
	}
}

func TestGeneratedPtahSchemaPlanSizeContract(t *testing.T) {
	t.Parallel()

	repositoryRoot := repositoryRoot(t)
	typesSource := readFile(t, filepath.Join(repositoryRoot, "api", "v1alpha1", "ptahschemaplan_types.go"))
	for _, marker := range []string{
		"+kubebuilder:validation:Maximum=" + strconv.FormatInt(plancontract.MaxExecutableBytes, 10),
		"+kubebuilder:validation:Maximum=" + strconv.Itoa(plancontract.ChunkBytes),
		"+kubebuilder:validation:MaxItems=" + strconv.Itoa(plancontract.MaxChunks),
		"+kubebuilder:validation:Maximum=" + strconv.Itoa(plancontract.MaxChunks-1),
	} {
		if !bytes.Contains(typesSource, []byte(marker)) {
			t.Errorf("PtahSchemaPlan markers do not contain %q", marker)
		}
	}

	crd := loadGeneratedCRD(t, filepath.Join(
		repositoryRoot,
		"config", "crd", "bases", "operator.ptah.dev_ptahschemaplans.yaml",
	))
	spec := storageVersionSchema(t, crd).Properties["spec"]
	size := spec.Properties["size"]
	if size.Minimum == nil || *size.Minimum != 1 || size.Maximum == nil ||
		*size.Maximum != float64(plancontract.MaxExecutableBytes) {
		t.Fatalf("spec.size bounds = [%v,%v], want [1,%d]", size.Minimum, size.Maximum, plancontract.MaxExecutableBytes)
	}

	chunks := spec.Properties["chunks"]
	if chunks.MinItems == nil || *chunks.MinItems != 1 || chunks.MaxItems == nil ||
		*chunks.MaxItems != int64(plancontract.MaxChunks) || chunks.XListType == nil || *chunks.XListType != "map" {
		t.Fatalf("spec.chunks contract = min %v, max %v, list type %v", chunks.MinItems, chunks.MaxItems, chunks.XListType)
	}
	chunk := chunks.Items.Schema
	if chunk == nil {
		t.Fatal("spec.chunks has no item schema")
	}
	chunkIndex := chunk.Properties["index"]
	if chunkIndex.Minimum == nil || *chunkIndex.Minimum != 0 || chunkIndex.Maximum == nil ||
		*chunkIndex.Maximum != float64(plancontract.MaxChunks-1) {
		t.Fatalf("chunk index bounds = [%v,%v], want [0,%d]", chunkIndex.Minimum, chunkIndex.Maximum, plancontract.MaxChunks-1)
	}
	chunkSize := chunk.Properties["size"]
	if chunkSize.Minimum == nil || *chunkSize.Minimum != 1 || chunkSize.Maximum == nil ||
		*chunkSize.Maximum != float64(plancontract.ChunkBytes) {
		t.Fatalf("chunk size bounds = [%v,%v], want [1,%d]", chunkSize.Minimum, chunkSize.Maximum, plancontract.ChunkBytes)
	}

	published := storageVersionSchema(t, crd).Properties["status"].Properties["publishedChunks"]
	if published.MaxItems == nil || *published.MaxItems != int64(plancontract.MaxChunks) ||
		published.XListType == nil || *published.XListType != "map" {
		t.Fatalf("status.publishedChunks contract = max %v, list type %v", published.MaxItems, published.XListType)
	}
}

func TestGeneratedPtahSchemaAdmissionSnapshotBounds(t *testing.T) {
	t.Parallel()

	crd := loadGeneratedCRD(t, filepath.Join(
		repositoryRoot(t),
		"config", "crd", "bases", "operator.ptah.dev_ptahschemas.yaml",
	))
	status := storageVersionSchema(t, crd).Properties["status"]
	activeOperation := status.Properties["activeOperation"]
	snapshot := activeOperation.Properties["admissionSnapshot"]
	if snapshot.Type != "object" {
		t.Fatalf("status.activeOperation.admissionSnapshot type = %q", snapshot.Type)
	}

	limitRanges := snapshot.Properties["limitRanges"]
	if limitRanges.MaxItems == nil || *limitRanges.MaxItems != 32 || limitRanges.Items == nil || limitRanges.Items.Schema == nil {
		t.Fatalf("admissionSnapshot.limitRanges max/items = %v/%#v", limitRanges.MaxItems, limitRanges.Items)
	}
	limitRange := limitRanges.Items.Schema
	for _, name := range []string{"defaultRequests", "defaultLimits"} {
		property := limitRange.Properties[name]
		if property.MaxProperties == nil || *property.MaxProperties != 64 {
			t.Errorf("admissionSnapshot.limitRanges[].%s maxProperties = %v, want 64", name, property.MaxProperties)
		}
	}

	runtimeClass := snapshot.Properties["runtimeClass"]
	for _, name := range []string{"overhead", "nodeSelector"} {
		property := runtimeClass.Properties[name]
		if property.MaxProperties == nil || *property.MaxProperties != 64 {
			t.Errorf("admissionSnapshot.runtimeClass.%s maxProperties = %v, want 64", name, property.MaxProperties)
		}
	}
	tolerations := runtimeClass.Properties["tolerations"]
	if tolerations.MaxItems == nil || *tolerations.MaxItems != 64 {
		t.Errorf("admissionSnapshot.runtimeClass.tolerations maxItems = %v, want 64", tolerations.MaxItems)
	}
	imagePullSecrets := snapshot.Properties["serviceAccount"].Properties["imagePullSecrets"]
	if imagePullSecrets.MaxItems == nil || *imagePullSecrets.MaxItems != 64 {
		t.Errorf("admissionSnapshot.serviceAccount.imagePullSecrets maxItems = %v, want 64", imagePullSecrets.MaxItems)
	}

	for _, name := range []string{"defaultNotReadyTolerationSeconds", "defaultUnreachableTolerationSeconds"} {
		property := snapshot.Properties[name]
		if property.Minimum == nil || *property.Minimum != 0 || property.Maximum != nil {
			t.Errorf("admissionSnapshot.%s bounds = [%v,%v], want nonnegative int64", name, property.Minimum, property.Maximum)
		}
	}
	for _, name := range []string{
		"defaultTolerationsEnabled", "extendedResourceTolerationEnabled", "alwaysPullImagesEnabled",
	} {
		if property := snapshot.Properties[name]; property.Type != "boolean" {
			t.Errorf("admissionSnapshot.%s type = %q, want boolean", name, property.Type)
		}
	}

	object := snapshot.Properties["serviceAccount"].Properties["object"]
	if maximum := object.Properties["uid"].MaxLength; maximum == nil || *maximum != 128 {
		t.Errorf("admissionSnapshot object UID maxLength = %v, want 128", maximum)
	}
	if maximum := object.Properties["resourceVersion"].MaxLength; maximum == nil || *maximum != 128 {
		t.Errorf("admissionSnapshot object resourceVersion maxLength = %v, want 128", maximum)
	}
}

func loadGeneratedCRD(t *testing.T, path string) *apiextensions.CustomResourceDefinition {
	t.Helper()

	yamlDocument := readFile(t, path)
	jsonDocument, err := yaml.ToJSON(yamlDocument)
	if err != nil {
		t.Fatalf("convert %s to JSON: %v", path, err)
	}
	versioned := &apiextensionsv1.CustomResourceDefinition{}
	if err := json.Unmarshal(jsonDocument, versioned); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	internal := &apiextensions.CustomResourceDefinition{}
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(versioned, internal, nil); err != nil {
		t.Fatalf("convert %s to the API server's internal CRD type: %v", path, err)
	}

	// storedVersions is server-owned state populated when the CRD is created.
	// Seed it before invoking the API server's complete update-capable validator.
	for _, version := range internal.Spec.Versions {
		if version.Storage {
			internal.Status.StoredVersions = []string{version.Name}
			return internal
		}
	}
	t.Fatalf("generated CRD %s has no storage version", path)
	return nil
}

func storageVersionSchema(t *testing.T, crd *apiextensions.CustomResourceDefinition) *apiextensions.JSONSchemaProps {
	t.Helper()

	for index := range crd.Spec.Versions {
		version := &crd.Spec.Versions[index]
		if version.Storage {
			if version.Schema != nil && version.Schema.OpenAPIV3Schema != nil {
				return version.Schema.OpenAPIV3Schema
			}
			if crd.Spec.Validation != nil && crd.Spec.Validation.OpenAPIV3Schema != nil {
				return crd.Spec.Validation.OpenAPIV3Schema
			}
			t.Fatalf("storage version %s has no OpenAPI schema", version.Name)
		}
	}
	t.Fatal("generated CRD has no storage version schema")
	return nil
}

func repositoryRoot(t *testing.T) string {
	t.Helper()

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate generated CRD test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return contents
}
