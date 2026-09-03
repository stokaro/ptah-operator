package planstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
)

func TestPublishAndLoadRoundTrip(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("plan-line\n"), ChunkBytes/10+1)
	schema, desired, chunks := fixture(t, content)
	store := fakeStore(t, schema)

	published, err := store.Publish(context.Background(), desired, chunks)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	loaded, err := store.Load(context.Background(), published)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !bytes.Equal(loaded, content) {
		t.Fatal("Load() did not reconstruct exact plan bytes")
	}
	if len(published.Spec.Chunks) < 2 {
		t.Fatal("test plan was not chunked")
	}
}

func TestExecutablePlanSizeBoundary(t *testing.T) {
	// Keep this serial: publishing the exact supported limit intentionally
	// retains multiple full-size copies while fake API objects are verified.
	content := bytes.Repeat([]byte("<"), MaxPlanBytes-1)
	content = append(content, '\n')
	schema, desired, chunks := fixture(t, content)
	if len(chunks) != MaxChunks {
		t.Fatalf("exact-limit chunks = %d, want %d", len(chunks), MaxChunks)
	}
	store := fakeStore(t, schema)
	published, err := store.Publish(context.Background(), desired, chunks)
	if err != nil {
		t.Fatalf("Publish(exact limit) error = %v", err)
	}
	loaded, err := store.Load(context.Background(), published)
	if err != nil {
		t.Fatalf("Load(exact limit) error = %v", err)
	}
	if !bytes.Equal(loaded, content) {
		t.Fatal("Load(exact limit) changed plan bytes")
	}

	oversized := append(append([]byte(nil), content...), 'x')
	if _, _, err := Prepare(schema, desired.Spec, oversized); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("Prepare(limit+1) error = %v, want maximum-size refusal", err)
	}
}

func TestChunkLeavesKubernetesBase64TransportHeadroom(t *testing.T) {
	t.Parallel()

	_, plan, _ := fixture(t, []byte("small plan"))
	plan.UID = "plan-uid"
	ref := plan.Spec.Chunks[0]
	ref.Size = int32(ChunkBytes)
	chunk := bytes.Repeat([]byte{0xff}, ChunkBytes)
	ref.Digest = fingerprint.DigestBytes(chunk)
	encoded, err := json.Marshal(desiredChunk(plan, ref, chunk))
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) >= 1<<20 {
		t.Fatalf("JSON/base64 encoded maximum chunk = %d bytes, want metadata headroom below 1 MiB", len(encoded))
	}
}

func TestPrepareExecutionEpochCompatibility(t *testing.T) {
	t.Parallel()

	content := []byte("small exact plan")
	schema, current, _ := fixture(t, content)
	missingEpoch := current.Spec
	missingEpoch.ExecutionBindingID = ""
	if _, _, err := Prepare(schema, missingEpoch, content); err == nil || !strings.Contains(err.Error(), "execution binding ID") {
		t.Fatalf("Prepare(current without execution epoch) error = %v, want execution binding refusal", err)
	}
	missingManager := current.Spec
	missingManager.ControllerImage = ""
	missingManager.ControllerRevision = ""
	missingManager.ControllerStateVersion = 0
	if _, _, err := Prepare(schema, missingManager, content); err == nil || !strings.Contains(err.Error(), "controller image") {
		t.Fatalf("Prepare(current without manager identity) error = %v, want controller refusal", err)
	}
	invalidRevision := current.Spec
	invalidRevision.ControllerRevision = "release\ncandidate"
	if _, _, err := Prepare(schema, invalidRevision, content); err == nil || !strings.Contains(err.Error(), "control characters") {
		t.Fatalf("Prepare(current with control-character revision) error = %v, want revision refusal", err)
	}

	epochContract := current.Spec
	epochContract.ContractVersion = fingerprint.ExecutionEpochPlanContractVersion
	epochContract.ControllerImage = ""
	epochContract.ControllerRevision = ""
	epochContract.ControllerStateVersion = 0
	var err error
	epochContract.Fingerprint, err = planBinding(schema, epochContract).Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint legacy v2 plan: %v", err)
	}
	if _, _, err := Prepare(schema, epochContract, content); err != nil {
		t.Fatalf("Prepare(v2 without manager identity) error = %v, want backward-compatible storage", err)
	}

	legacy := current.Spec
	legacy.ContractVersion = fingerprint.LegacyPlanContractVersion
	legacy.ExecutionBindingID = ""
	legacy.ControllerImage = ""
	legacy.ControllerRevision = ""
	legacy.ControllerStateVersion = 0
	legacy.Fingerprint, err = planBinding(schema, legacy).Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint legacy v1 plan: %v", err)
	}
	if _, _, err := Prepare(schema, legacy, content); err != nil {
		t.Fatalf("Prepare(v1 without execution epoch) error = %v, want backward-compatible storage", err)
	}

	future := current.Spec
	future.ContractVersion = fingerprint.CurrentPlanContractVersion + 1
	if _, _, err := Prepare(schema, future, content); err == nil || !strings.Contains(err.Error(), "unsupported plan contract version") {
		t.Fatalf("Prepare(future contract) error = %v, want unsupported-version refusal", err)
	}
}

func TestPublishIsCrashResumable(t *testing.T) {
	t.Parallel()
	schema, desired, chunks := fixture(t, []byte("small exact plan"))
	store := fakeStore(t, schema)
	first, err := store.Publish(context.Background(), desired, chunks)
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	second, err := store.Publish(context.Background(), desired, chunks)
	if err != nil {
		t.Fatalf("second Publish() error = %v", err)
	}
	if first.UID != second.UID || first.Status.PublishedChunks[0].UID != second.Status.PublishedChunks[0].UID {
		t.Fatal("Publish() replaced an immutable object while resuming")
	}
}

func TestLoadRejectsReplacedChunk(t *testing.T) {
	t.Parallel()
	schema, desired, chunks := fixture(t, []byte("small exact plan"))
	store := fakeStore(t, schema)
	published, err := store.Publish(context.Background(), desired, chunks)
	if err != nil {
		t.Fatal(err)
	}
	chunk := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: published.Namespace, Name: published.Spec.Chunks[0].Name}
	if err := store.Client.Get(context.Background(), key, chunk); err != nil {
		t.Fatal(err)
	}
	if err := store.Client.Delete(context.Background(), chunk); err != nil {
		t.Fatal(err)
	}
	replacement := desiredChunk(published, published.Spec.Chunks[0], chunks[0])
	if err := store.Client.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), published); err == nil {
		t.Fatal("Load() accepted a delete-and-recreate chunk")
	}
}

func fixture(t *testing.T, content []byte) (*operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaPlan, [][]byte) {
	t.Helper()
	schema := &operatorv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app", UID: "schema-uid"}}
	coordinationDigest, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", "prod/team-a/app")
	if err != nil {
		t.Fatal(err)
	}
	spec := operatorv1alpha1.PtahSchemaPlanSpec{
		ContractVersion:          fingerprint.CurrentPlanContractVersion,
		SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
		ArtifactDigest:           "sha256:artifact",
		CoordinationDigest:       coordinationDigest,
		TargetIdentityDigest:     "sha256:target",
		ActualStateFingerprint:   "sha256:actual",
		DesiredStateFingerprint:  "sha256:desired",
		PolicyFingerprint:        "sha256:policy",
		VerificationPolicyUID:    "verification-policy-uid",
		VerificationPolicyDigest: "sha256:verification",
		ExecutionBindingID:       "v1-33333333333333333333333333333333",
		ControllerImage:          "example.invalid/manager@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		ControllerRevision:       "controller-test-revision",
		ControllerStateVersion:   1,
		PtahVersion:              "v0.3.0",
		ExecutorImage:            "example.invalid/ptah@sha256:executor",
		RunnerImage:              "example.invalid/operator@sha256:runner",
		RunnerProtocolVersion:    int32(runner.ProtocolVersion),
		Dialect:                  "postgresql",
		StatementCount:           1,
	}
	spec.ContentDigest = fingerprint.DigestBytes(content)
	binding := planBinding(schema, spec)
	spec.Fingerprint, err = binding.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	desired, chunks, err := Prepare(schema, spec, content)
	if err != nil {
		t.Fatal(err)
	}
	return schema, desired, chunks
}

func planBinding(schema *operatorv1alpha1.PtahSchema, spec operatorv1alpha1.PtahSchemaPlanSpec) fingerprint.PlanBinding {
	return fingerprint.PlanBinding{
		ContractVersion:          spec.ContractVersion,
		SchemaUID:                string(schema.UID),
		PlanContentDigest:        spec.ContentDigest,
		ArtifactDigest:           spec.ArtifactDigest,
		CoordinationDigest:       spec.CoordinationDigest,
		TargetIdentityDigest:     spec.TargetIdentityDigest,
		ActualStateFingerprint:   spec.ActualStateFingerprint,
		DesiredStateFingerprint:  spec.DesiredStateFingerprint,
		PolicyFingerprint:        spec.PolicyFingerprint,
		VerificationPolicyUID:    string(spec.VerificationPolicyUID),
		VerificationPolicyDigest: spec.VerificationPolicyDigest,
		ExecutionBindingID:       spec.ExecutionBindingID,
		ControllerImage:          spec.ControllerImage,
		ControllerRevision:       spec.ControllerRevision,
		ControllerStateVersion:   spec.ControllerStateVersion,
		PtahVersion:              spec.PtahVersion,
		ExecutorImage:            spec.ExecutorImage,
		RunnerImage:              spec.RunnerImage,
		RunnerProtocolVersion:    spec.RunnerProtocolVersion,
	}
}

func fakeStore(t *testing.T, objects ...client.Object) Store {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	api := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&operatorv1alpha1.PtahSchemaPlan{}).
		WithObjects(objects...).Build()
	server := &uidAssigningClient{Client: api}
	return Store{Client: server, Reader: server}
}

// uidAssigningClient supplies the server-generated metadata the fake client
// intentionally omits.
type uidAssigningClient struct {
	client.Client
	next atomic.Int64
}

func (c *uidAssigningClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if object.GetUID() == "" {
		object.SetUID(types.UID(fmt.Sprintf("uid-%s-%d", object.GetName(), c.next.Add(1))))
	}
	return c.Client.Create(ctx, object, options...)
}
