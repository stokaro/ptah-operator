package planstore

import (
	"bytes"
	"context"
	"fmt"
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
	spec := operatorv1alpha1.PtahSchemaPlanSpec{
		ContractVersion:          1,
		SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
		ArtifactDigest:           "sha256:artifact",
		TargetIdentityDigest:     "sha256:target",
		ActualStateFingerprint:   "sha256:actual",
		DesiredStateFingerprint:  "sha256:desired",
		PolicyFingerprint:        "sha256:policy",
		VerificationPolicyDigest: "sha256:verification",
		PtahVersion:              "v0.3.0",
		ExecutorImage:            "example.invalid/ptah@sha256:executor",
		RunnerImage:              "example.invalid/operator@sha256:runner",
		RunnerProtocolVersion:    1,
		Dialect:                  "postgresql",
		StatementCount:           1,
	}
	spec.ContentDigest = fingerprint.DigestBytes(content)
	binding := fingerprint.PlanBinding{
		ContractVersion:          1,
		SchemaUID:                string(schema.UID),
		PlanContentDigest:        spec.ContentDigest,
		ArtifactDigest:           spec.ArtifactDigest,
		TargetIdentityDigest:     spec.TargetIdentityDigest,
		ActualStateFingerprint:   spec.ActualStateFingerprint,
		DesiredStateFingerprint:  spec.DesiredStateFingerprint,
		PolicyFingerprint:        spec.PolicyFingerprint,
		VerificationPolicyDigest: spec.VerificationPolicyDigest,
		PtahVersion:              spec.PtahVersion,
		ExecutorImage:            spec.ExecutorImage,
		RunnerImage:              spec.RunnerImage,
		RunnerProtocolVersion:    spec.RunnerProtocolVersion,
	}
	var err error
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
