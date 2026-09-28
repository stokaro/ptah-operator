package crdupgrade

import (
	"bytes"
	"context"
	"strings"
	"testing"

	celgo "github.com/google/cel-go/cel"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// TestControllerChunkWriteGuardAdmitsTheChunksThePlanStoreWrites publishes a
// plan larger than one chunk and runs the chunk objects the store actually
// created through the sealed chunk contract, and the ConfigMaps it projects
// for an Apply through the projection contract.
//
// Each guard bounds a value that in a policy is the base64 string the API
// server transports rather than the decoded bytes: spec.data of the chunk,
// binaryData["chunk"] of the projection. So the ceiling has to be the base64
// length of a full chunk: a raw-byte ceiling refuses the controller's own
// write, and because both the policy and the webhook fail closed, the stricter
// one decides.
func TestControllerChunkWriteGuardAdmitsTheChunksThePlanStoreWrites(t *testing.T) {
	t.Parallel()

	// Two chunks, the first at the exact size the store writes.
	content := bytes.Repeat([]byte("select 1;\n"), planstore.ChunkBytes/10+64)
	chunks, projections := publishedPlanChunks(t, content)
	if len(chunks) < 2 || len(chunks[0].Spec.Data) != planstore.ChunkBytes ||
		len(projections) != len(chunks) || len(projections[0].BinaryData[planstore.ProjectionDataKey]) != planstore.ChunkBytes {
		t.Fatalf("published %d chunks and %d projections, first %d bytes; want a full %d-byte chunk and a remainder",
			len(chunks), len(projections), len(chunks[0].Spec.Data), planstore.ChunkBytes)
	}

	for index, chunk := range chunks {
		object := celObject(t, chunk)
		for validation, expression := range validationExpressions(controllerChunkWriteValidations("refused")) {
			if !evaluateChunkContract(t, expression, object) {
				t.Fatalf("validation %d refused plan chunk %d (%d raw bytes) the plan store wrote:\n%s",
					validation, index, len(chunk.Spec.Data), expression)
			}
		}
	}
	for index, configMap := range projections {
		object := celObject(t, configMap)
		for validation, expression := range validationExpressions(controllerProjectionWriteValidations("refused")) {
			if !evaluateChunkContract(t, expression, object) {
				t.Fatalf("validation %d refused plan projection %d (%d raw bytes) the plan store wrote:\n%s",
					validation, index, len(configMap.BinaryData[planstore.ProjectionDataKey]), expression)
			}
		}
	}

	// The controls: each ceiling still refuses a chunk past the contract.
	// Base64 rounds up to three-byte groups, so the first oversize the
	// transported length can show is two bytes past a full chunk.
	oversizedChunk := chunks[0].DeepCopy()
	oversizedChunk.Spec.Data = bytes.Repeat([]byte{'x'}, planstore.ChunkBytes+2)
	if admitsAll(t, controllerChunkWriteValidations("refused"), celObject(t, oversizedChunk)) {
		t.Fatal("the chunk contract admitted a PtahSchemaPlanChunk larger than the plan store may write")
	}
	oversizedProjection := projections[0].DeepCopy()
	oversizedProjection.BinaryData[planstore.ProjectionDataKey] = bytes.Repeat([]byte{'x'}, planstore.ChunkBytes+2)
	if admitsAll(t, controllerProjectionWriteValidations("refused"), celObject(t, oversizedProjection)) {
		t.Fatal("the projection contract admitted a ConfigMap larger than the plan store may write")
	}
	// And neither contract admits the other's object: a ConfigMap is not a
	// chunk, and a chunk is not something a Pod can mount.
	if admitsAll(t, controllerChunkWriteValidations("refused"), celObject(t, projections[0])) {
		t.Fatal("the chunk contract admitted a projection ConfigMap")
	}
	if admitsAll(t, controllerProjectionWriteValidations("refused"), celObject(t, chunks[0])) {
		t.Fatal("the projection contract admitted a PtahSchemaPlanChunk")
	}
}

func admitsAll(t *testing.T, validations []admissionregistrationv1.Validation, object map[string]any) bool {
	t.Helper()
	for _, expression := range validationExpressions(validations) {
		if !evaluateChunkContract(t, expression, object) {
			return false
		}
	}
	return true
}

// publishedPlanChunks returns the chunk objects a plan publish leaves behind,
// and the ConfigMaps a projection of the same plan writes.
func publishedPlanChunks(t *testing.T, content []byte) ([]*operatorv1alpha1.PtahSchemaPlanChunk, []*corev1.ConfigMap) {
	t.Helper()

	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: "schema-uid"},
	}
	spec := operatorv1alpha1.PtahSchemaPlanSpec{
		ContractVersion:          fingerprint.CurrentPlanContractVersion,
		SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
		Fingerprint:              "sha256:" + strings.Repeat("a", 64),
		ArtifactDigest:           "sha256:" + strings.Repeat("4", 64),
		CoordinationDigest:       "sha256:" + strings.Repeat("6", 64),
		TargetIdentityDigest:     "sha256:" + strings.Repeat("7", 64),
		ActualStateFingerprint:   "sha256:" + strings.Repeat("8", 64),
		DesiredStateFingerprint:  "sha256:" + strings.Repeat("9", 64),
		PolicyFingerprint:        "sha256:" + strings.Repeat("b", 64),
		VerificationPolicyUID:    "verification-policy-uid",
		VerificationPolicyDigest: "sha256:" + strings.Repeat("c", 64),
		ExecutionBindingID:       "v1-33333333333333333333333333333333",
		ControllerImage:          "example.test/controller@sha256:" + strings.Repeat("1", 64),
		ControllerRevision:       "controller-test-revision",
		ControllerStateVersion:   ourStateVersion,
		PtahVersion:              "v0.3.0",
		ExecutorImage:            "example.test/executor@sha256:" + strings.Repeat("2", 64),
		RunnerImage:              "example.test/runner@sha256:" + strings.Repeat("3", 64),
		RunnerProtocolVersion:    int32(runner.ProtocolVersion),
		Dialect:                  "postgresql",
		StatementCount:           1,
	}
	desired, chunks, err := planstore.Prepare(schema, spec, content)
	if err != nil {
		t.Fatal(err)
	}
	// The API server assigns the plan UID the chunk owner reference carries.
	desired.UID = "plan-uid"

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	api := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&operatorv1alpha1.PtahSchemaPlan{}).
		WithObjects(schema).Build()
	store := planstore.Store{Client: api, Reader: api}
	published, err := store.Publish(context.Background(), desired, chunks)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := store.Project(context.Background(), published, content); err != nil {
		t.Fatalf("Project() error = %v", err)
	}

	written := make([]*operatorv1alpha1.PtahSchemaPlanChunk, len(published.Spec.Chunks))
	projected := make([]*corev1.ConfigMap, len(published.Spec.Chunks))
	for index, ref := range published.Spec.Chunks {
		key := types.NamespacedName{Namespace: published.Namespace, Name: ref.Name}
		chunk := &operatorv1alpha1.PtahSchemaPlanChunk{}
		if err := api.Get(context.Background(), key, chunk); err != nil {
			t.Fatal(err)
		}
		written[index] = chunk
		configMap := &corev1.ConfigMap{}
		if err := api.Get(context.Background(), key, configMap); err != nil {
			t.Fatal(err)
		}
		projected[index] = configMap
	}
	return written, projected
}

func evaluateChunkContract(t *testing.T, expression string, object map[string]any) bool {
	t.Helper()

	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("oldObject", celgo.DynType),
		celgo.Variable("request", celgo.DynType),
		celgo.Variable("params", celgo.DynType),
		celgo.Variable("variables", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := environment.Compile(expression)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile chunk contract: %v", issues.Err())
	}
	program, err := environment.Program(ast)
	if err != nil {
		t.Fatalf("build chunk contract: %v", err)
	}
	result, _, err := program.Eval(map[string]any{
		"object":    object,
		"oldObject": nil,
		"request":   map[string]any{"operation": "CREATE"},
		"params":    map[string]any{},
		"variables": map[string]any{},
	})
	if err != nil {
		// The API server applies its failure policy to an expression that
		// cannot be evaluated, so treat it as the refusal it would become.
		return false
	}
	admitted, ok := result.Value().(bool)
	if !ok {
		t.Fatalf("chunk contract result = %T(%v), want bool", result.Value(), result.Value())
	}
	return admitted
}
