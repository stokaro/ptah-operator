package planstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	for name, object := range map[string]any{
		"chunk":      DesiredChunk(plan, ref, chunk),
		"projection": DesiredProjection(plan, ref, chunk),
	} {
		encoded, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) >= 1<<20 {
			t.Fatalf("JSON/base64 encoded maximum %s = %d bytes, want metadata headroom below 1 MiB", name, len(encoded))
		}
	}
}

func TestChunkAndProjectionUseBlockingPlanOwner(t *testing.T) {
	t.Parallel()

	_, plan, chunks := fixture(t, []byte("small exact plan"))
	plan.UID = "plan-uid"
	for name, owners := range map[string][]metav1.OwnerReference{
		"chunk":      DesiredChunk(plan, plan.Spec.Chunks[0], chunks[0]).OwnerReferences,
		"projection": DesiredProjection(plan, plan.Spec.Chunks[0], chunks[0]).OwnerReferences,
	} {
		if len(owners) != 1 {
			t.Fatalf("%s owner references = %#v, want one PtahSchemaPlan owner", name, owners)
		}
		owner := owners[0]
		if owner.APIVersion != operatorv1alpha1.GroupVersion.String() || owner.Kind != "PtahSchemaPlan" ||
			owner.Name != plan.Name || owner.UID != plan.UID || owner.Controller == nil || !*owner.Controller ||
			owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
			t.Fatalf("%s owner reference = %#v, want exact blocking PtahSchemaPlan owner", name, owner)
		}
	}
}

func TestPrepareAcceptsOnlyTheCurrentPlanContract(t *testing.T) {
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

	missingState := current.Spec
	missingState.ControllerStateVersion = 0
	if _, _, err := Prepare(schema, missingState, content); err == nil || !strings.Contains(err.Error(), "controller state version") {
		t.Fatalf("Prepare(current without controller state version) error = %v, want controller state refusal", err)
	}

	for _, version := range []int32{1, 2, fingerprint.CurrentPlanContractVersion + 1} {
		other := current.Spec
		other.ContractVersion = version
		if _, _, err := Prepare(schema, other, content); err == nil || !strings.Contains(err.Error(), "unsupported plan contract version") {
			t.Fatalf("Prepare(contract version %d) error = %v, want unsupported-version refusal", version, err)
		}
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
	chunk := &operatorv1alpha1.PtahSchemaPlanChunk{}
	key := client.ObjectKey{Namespace: published.Namespace, Name: published.Spec.Chunks[0].Name}
	if err := store.Client.Get(context.Background(), key, chunk); err != nil {
		t.Fatal(err)
	}
	if err := store.Client.Delete(context.Background(), chunk); err != nil {
		t.Fatal(err)
	}
	replacement := DesiredChunk(published, published.Spec.Chunks[0], chunks[0])
	if err := store.Client.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), published); err == nil {
		t.Fatal("Load() accepted a delete-and-recreate chunk")
	}
}

func TestLoadRejectsInexactChunkOwnerReference(t *testing.T) {
	t.Parallel()

	falseValue := false
	tests := map[string]func([]metav1.OwnerReference) []metav1.OwnerReference{
		"wrong API version": func(owners []metav1.OwnerReference) []metav1.OwnerReference {
			owners[0].APIVersion = "operator.ptah.run/v999"
			return owners
		},
		"wrong kind": func(owners []metav1.OwnerReference) []metav1.OwnerReference {
			owners[0].Kind = "PtahSchema"
			return owners
		},
		"wrong name": func(owners []metav1.OwnerReference) []metav1.OwnerReference {
			owners[0].Name = "different-plan"
			return owners
		},
		"wrong UID": func(owners []metav1.OwnerReference) []metav1.OwnerReference {
			owners[0].UID = "different-plan-uid"
			return owners
		},
		"missing controller": func(owners []metav1.OwnerReference) []metav1.OwnerReference {
			owners[0].Controller = nil
			return owners
		},
		"non-controller": func(owners []metav1.OwnerReference) []metav1.OwnerReference {
			owners[0].Controller = &falseValue
			return owners
		},
		"missing block owner deletion": func(owners []metav1.OwnerReference) []metav1.OwnerReference {
			owners[0].BlockOwnerDeletion = nil
			return owners
		},
		"does not block owner deletion": func(owners []metav1.OwnerReference) []metav1.OwnerReference {
			owners[0].BlockOwnerDeletion = &falseValue
			return owners
		},
		"duplicate exact owner": func(owners []metav1.OwnerReference) []metav1.OwnerReference {
			return append(owners, owners[0])
		},
		"extra owner": func(owners []metav1.OwnerReference) []metav1.OwnerReference {
			return append(owners, metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: "other-uid"})
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema, desired, chunks := fixture(t, []byte("small exact plan"))
			store := fakeStore(t, schema)
			published, err := store.Publish(context.Background(), desired, chunks)
			if err != nil {
				t.Fatal(err)
			}

			chunk := &operatorv1alpha1.PtahSchemaPlanChunk{}
			key := client.ObjectKey{Namespace: published.Namespace, Name: published.Spec.Chunks[0].Name}
			if err := store.Client.Get(context.Background(), key, chunk); err != nil {
				t.Fatal(err)
			}
			chunk.OwnerReferences = mutate(chunk.OwnerReferences)
			if err := store.Client.Update(context.Background(), chunk); err != nil {
				t.Fatal(err)
			}

			if _, err := store.Load(context.Background(), published); err == nil ||
				!strings.Contains(err.Error(), "exact blocking plan owner reference") {
				t.Fatalf("Load() error = %v, want exact owner-reference refusal", err)
			}
		})
	}
}

// TestPublishStoresChunksAndNoConfigMap holds publication to the chunk kind: a
// plan that is published and never applied leaves no ConfigMap behind, so
// whoever reads the namespace's ConfigMaps does not read it.
func TestPublishStoresChunksAndNoConfigMap(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("plan-line\n"), ChunkBytes/10+1)
	schema, desired, chunks := fixture(t, content)
	store := fakeStore(t, schema)

	published, err := store.Publish(context.Background(), desired, chunks)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	stored := &operatorv1alpha1.PtahSchemaPlanChunkList{}
	if err := store.Client.List(context.Background(), stored, client.InNamespace(published.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(stored.Items) != len(published.Spec.Chunks) || len(stored.Items) < 2 {
		t.Fatalf("Publish() stored %d chunk objects, want the %d the plan names", len(stored.Items), len(published.Spec.Chunks))
	}
	for index, committed := range published.Status.PublishedChunks {
		chunk := &operatorv1alpha1.PtahSchemaPlanChunk{}
		if err := store.Client.Get(context.Background(), client.ObjectKey{Namespace: published.Namespace, Name: committed.Name}, chunk); err != nil {
			t.Fatalf("committed chunk %d: %v", index, err)
		}
		if chunk.UID != committed.UID || !bytes.Equal(chunk.Spec.Data, chunks[index]) {
			t.Fatalf("committed chunk %d is not the stored object and bytes", index)
		}
	}
	configMaps := &corev1.ConfigMapList{}
	if err := store.Client.List(context.Background(), configMaps, client.InNamespace(published.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(configMaps.Items) != 0 {
		t.Fatalf("Publish() wrote %d ConfigMaps, want none", len(configMaps.Items))
	}
}

// TestProjectWritesWhatTheApplyPodMounts projects a published plan and reads
// the ConfigMaps back in the order the Apply Job's volume names them: the
// bytes the Pod would concatenate are the plan.
func TestProjectWritesWhatTheApplyPodMounts(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("plan-line\n"), ChunkBytes/10+1)
	schema, desired, chunks := fixture(t, content)
	store := fakeStore(t, schema)
	published, err := store.Publish(context.Background(), desired, chunks)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), published)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Project(context.Background(), published, loaded); err != nil {
		t.Fatalf("Project() error = %v", err)
	}

	sources, err := VolumeSources(published)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != len(published.Spec.Chunks) || len(sources) < 2 {
		t.Fatalf("VolumeSources() = %d sources, want the %d chunks", len(sources), len(published.Spec.Chunks))
	}
	var mounted bytes.Buffer
	for index, source := range sources {
		configMap := &corev1.ConfigMap{}
		key := client.ObjectKey{Namespace: published.Namespace, Name: source.ConfigMap.Name}
		if err := store.Client.Get(context.Background(), key, configMap); err != nil {
			t.Fatalf("projection %d: %v", index, err)
		}
		if err := VerifyProjection(published, published.Spec.Chunks[index], configMap); err != nil {
			t.Fatalf("projection %d: %v", index, err)
		}
		if len(source.ConfigMap.Items) != 1 || source.ConfigMap.Items[0].Key != ProjectionDataKey {
			t.Fatalf("projection %d mounts %#v, want the one key the store writes", index, source.ConfigMap.Items)
		}
		mounted.Write(configMap.BinaryData[ProjectionDataKey])
	}
	if !bytes.Equal(mounted.Bytes(), content) {
		t.Fatal("the projected ConfigMaps do not carry the plan")
	}

	// A second pass, as after a restart before the Job was created, finds
	// the projection and accepts it.
	if err := store.Project(context.Background(), published, loaded); err != nil {
		t.Fatalf("resumed Project() error = %v", err)
	}
}

func TestProjectRefusesWhatIsNotThisPlansProjection(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*corev1.ConfigMap){
		"other bytes": func(configMap *corev1.ConfigMap) {
			configMap.BinaryData[ProjectionDataKey] = []byte("select 'other';\n")
		},
		"mutable": func(configMap *corev1.ConfigMap) {
			mutable := false
			configMap.Immutable = &mutable
		},
		"another owner": func(configMap *corev1.ConfigMap) {
			configMap.OwnerReferences[0].UID = "another-plan-uid"
		},
		"a second key": func(configMap *corev1.ConfigMap) {
			configMap.BinaryData["other"] = []byte("x")
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			content := []byte("small exact plan")
			schema, desired, chunks := fixture(t, content)
			store := fakeStore(t, schema)
			published, err := store.Publish(context.Background(), desired, chunks)
			if err != nil {
				t.Fatal(err)
			}
			squatter := DesiredProjection(published, published.Spec.Chunks[0], chunks[0])
			mutate(squatter)
			if err := store.Client.Create(context.Background(), squatter); err != nil {
				t.Fatal(err)
			}
			err = store.Project(context.Background(), published, content)
			if !errors.Is(err, ErrProjectionConflict) {
				t.Fatalf("Project() error = %v, want %v", err, ErrProjectionConflict)
			}
		})
	}
}

func TestProjectRefusesContentThatIsNotThePlan(t *testing.T) {
	t.Parallel()
	content := []byte("small exact plan")
	schema, desired, chunks := fixture(t, content)
	store := fakeStore(t, schema)
	published, err := store.Publish(context.Background(), desired, chunks)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Project(context.Background(), published, []byte("small other plan"))
	if err == nil || errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("Project(other content) error = %v, want a content-binding refusal", err)
	}
	configMaps := &corev1.ConfigMapList{}
	if err := store.Client.List(context.Background(), configMaps, client.InNamespace(published.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(configMaps.Items) != 0 {
		t.Fatalf("Project(other content) wrote %d ConfigMaps", len(configMaps.Items))
	}
}

func fixture(t *testing.T, content []byte) (*operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaPlan, [][]byte) {
	t.Helper()
	schema := &operatorv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app", UID: "schema-uid"}}
	coordinationDigest, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", schema.Namespace, "prod/team-a/app")
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
	spec.Fingerprint, err = Binding(schema.UID, spec).Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	desired, chunks, err := Prepare(schema, spec, content)
	if err != nil {
		t.Fatal(err)
	}
	return schema, desired, chunks
}

// TestBindingReadsEverySpecFieldTheFingerprintHolds changes one plan spec field
// at a time. A field the fingerprint binds must change it, and the publisher's
// record must not: the webhook recomputes the fingerprint from the published
// spec through Binding, so a field Binding forgot is a field nothing checks.
func TestBindingReadsEverySpecFieldTheFingerprintHolds(t *testing.T) {
	t.Parallel()

	_, plan, _ := fixture(t, []byte("plan"))
	base := plan.Spec
	want, err := Binding("schema-uid", base).Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if want != base.Fingerprint {
		t.Fatalf("fixture fingerprint %q is not Binding's %q", base.Fingerprint, want)
	}
	bound := map[string]func(*operatorv1alpha1.PtahSchemaPlanSpec){
		"content digest":             func(s *operatorv1alpha1.PtahSchemaPlanSpec) { s.ContentDigest += "-new" },
		"artifact digest":            func(s *operatorv1alpha1.PtahSchemaPlanSpec) { s.ArtifactDigest += "-new" },
		"coordination digest":        func(s *operatorv1alpha1.PtahSchemaPlanSpec) { s.CoordinationDigest += "-new" },
		"target identity digest":     func(s *operatorv1alpha1.PtahSchemaPlanSpec) { s.TargetIdentityDigest += "-new" },
		"actual state fingerprint":   func(s *operatorv1alpha1.PtahSchemaPlanSpec) { s.ActualStateFingerprint += "-new" },
		"desired state fingerprint":  func(s *operatorv1alpha1.PtahSchemaPlanSpec) { s.DesiredStateFingerprint += "-new" },
		"policy fingerprint":         func(s *operatorv1alpha1.PtahSchemaPlanSpec) { s.PolicyFingerprint += "-new" },
		"verification policy UID":    func(s *operatorv1alpha1.PtahSchemaPlanSpec) { s.VerificationPolicyUID += "-new" },
		"verification policy digest": func(s *operatorv1alpha1.PtahSchemaPlanSpec) { s.VerificationPolicyDigest += "-new" },
		"execution binding ID": func(s *operatorv1alpha1.PtahSchemaPlanSpec) {
			s.ExecutionBindingID = "v1-44444444444444444444444444444444"
		},
		"controller state version": func(s *operatorv1alpha1.PtahSchemaPlanSpec) { s.ControllerStateVersion++ },
		"Ptah version":             func(s *operatorv1alpha1.PtahSchemaPlanSpec) { s.PtahVersion += "-new" },
		"executor image":           func(s *operatorv1alpha1.PtahSchemaPlanSpec) { s.ExecutorImage += "-new" },
		"runner protocol version":  func(s *operatorv1alpha1.PtahSchemaPlanSpec) { s.RunnerProtocolVersion++ },
		"destructive":              func(s *operatorv1alpha1.PtahSchemaPlanSpec) { s.Destructive = !s.Destructive },
		"privilege changes": func(s *operatorv1alpha1.PtahSchemaPlanSpec) {
			s.PrivilegeChanges = []operatorv1alpha1.PrivilegeChange{operatorv1alpha1.PrivilegeChangeGrant}
		},
		"statement count": func(s *operatorv1alpha1.PtahSchemaPlanSpec) { s.StatementCount++ },
	}
	for name, mutate := range bound {
		changed := *base.DeepCopy()
		mutate(&changed)
		got, err := Binding("schema-uid", changed).Fingerprint()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got == want {
			t.Errorf("changing the %s kept the fingerprint", name)
		}
	}
	if other, err := Binding("other-schema-uid", base).Fingerprint(); err != nil || other == want {
		t.Errorf("another schema UID kept the fingerprint (err %v)", err)
	}

	recorded := *base.DeepCopy()
	recorded.ControllerImage = "example.invalid/manager@sha256:" + strings.Repeat("d", 64)
	recorded.ControllerRevision = "another-revision"
	recorded.RunnerImage = "example.invalid/operator@sha256:other"
	if got, err := Binding("schema-uid", recorded).Fingerprint(); err != nil || got != want {
		t.Errorf("the publisher's record changed the fingerprint to %q (err %v)", got, err)
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
