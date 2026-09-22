package crdupgrade

import (
	"bytes"
	"context"
	"strings"
	"testing"

	celgo "github.com/google/cel-go/cel"
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
// plan larger than one chunk and runs the ConfigMaps the store actually created
// through the sealed chunk contract.
//
// The guard bounds dyn(object).binaryData["chunk"].size(), and in a policy that
// value is the base64 string the API server transports rather than the decoded
// bytes. So the ceiling has to be the base64 length of a full chunk: a raw-byte
// ceiling refuses the controller's own write, and because both the policy and
// the webhook fail closed, the stricter one decides.
func TestControllerChunkWriteGuardAdmitsTheChunksThePlanStoreWrites(t *testing.T) {
	t.Parallel()

	// Two chunks, the first at the exact size the store writes.
	content := bytes.Repeat([]byte("select 1;\n"), planstore.ChunkBytes/10+64)
	chunks := publishedPlanChunks(t, content)
	if len(chunks) < 2 || len(chunks[0].BinaryData[planstore.ChunkDataKey]) != planstore.ChunkBytes {
		t.Fatalf("published %d chunks, first %d bytes; want a full %d-byte chunk and a remainder",
			len(chunks), len(chunks[0].BinaryData[planstore.ChunkDataKey]), planstore.ChunkBytes)
	}

	for index, configMap := range chunks {
		object := celObject(t, configMap)
		for validation, expression := range validationExpressions(controllerChunkWriteValidations("refused")) {
			if !evaluateChunkContract(t, expression, object) {
				t.Fatalf("validation %d refused plan chunk %d (%d raw bytes) the plan store wrote:\n%s",
					validation, index, len(configMap.BinaryData[planstore.ChunkDataKey]), expression)
			}
		}
	}

	// The control: the ceiling still refuses a chunk past the contract. Base64
	// rounds up to three-byte groups, so the first oversize the transported
	// length can show is two bytes past a full chunk.
	oversized := chunks[0].DeepCopy()
	oversized.BinaryData[planstore.ChunkDataKey] = bytes.Repeat([]byte{'x'}, planstore.ChunkBytes+2)
	object := celObject(t, oversized)
	refused := false
	for _, expression := range validationExpressions(controllerChunkWriteValidations("refused")) {
		if !evaluateChunkContract(t, expression, object) {
			refused = true
			break
		}
	}
	if !refused {
		t.Fatal("the chunk contract admitted a ConfigMap larger than the plan store may write")
	}
}

// publishedPlanChunks returns the chunk ConfigMaps a plan publish leaves behind.
func publishedPlanChunks(t *testing.T, content []byte) []*corev1.ConfigMap {
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

	written := make([]*corev1.ConfigMap, len(published.Spec.Chunks))
	for index, ref := range published.Spec.Chunks {
		configMap := &corev1.ConfigMap{}
		key := types.NamespacedName{Namespace: published.Namespace, Name: ref.Name}
		if err := api.Get(context.Background(), key, configMap); err != nil {
			t.Fatal(err)
		}
		written[index] = configMap
	}
	return written
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
		"variables": map[string]any{"isAnyAdmissionConvergenceProbe": false},
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
