// Package planstore publishes immutable executable plans without placing SQL
// in a custom-resource status or controller log.
package planstore

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"regexp"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/plancontract"
)

const (
	// ChunkBytes leaves headroom below the Kubernetes object-size limit.
	ChunkBytes = plancontract.ChunkBytes
	// MaxChunks bounds projected-volume fan-out and API-server load.
	MaxChunks    = plancontract.MaxChunks
	MaxPlanBytes = int(plancontract.MaxExecutableBytes)

	ChunkDataKey = "chunk"
	LabelPlan    = "operator.ptah.dev/plan"
	LabelSchema  = "operator.ptah.dev/schema"
)

var sha256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Store uses direct API reads through Reader and mutating calls through Client.
// The distinction lets controllers bypass a stale cache before apply.
type Store struct {
	Client client.Client
	Reader client.Reader
}

// Prepare creates the deterministic manifest and chunks for exact plan bytes.
// The caller supplies every approval binding except content-derived fields.
func Prepare(
	schema *operatorv1alpha1.PtahSchema,
	spec operatorv1alpha1.PtahSchemaPlanSpec,
	content []byte,
) (*operatorv1alpha1.PtahSchemaPlan, [][]byte, error) {
	if schema == nil || schema.UID == "" {
		return nil, nil, fmt.Errorf("schema with a UID is required")
	}
	if len(content) == 0 {
		return nil, nil, fmt.Errorf("plan content is empty")
	}
	if len(content) > MaxPlanBytes {
		return nil, nil, fmt.Errorf("plan is %d bytes; maximum is %d", len(content), MaxPlanBytes)
	}
	contentDigest := fingerprint.DigestBytes(content)
	if spec.ContentDigest != "" && spec.ContentDigest != contentDigest {
		return nil, nil, fmt.Errorf("declared plan content digest does not match the content")
	}
	spec.ContentDigest = contentDigest
	spec.Size = int64(len(content))
	if !sha256Pattern.MatchString(spec.Fingerprint) {
		return nil, nil, fmt.Errorf("plan fingerprint must be a lowercase SHA-256 digest")
	}
	if spec.ContractVersion < 1 {
		return nil, nil, fmt.Errorf("plan contract version must be positive")
	}
	if spec.SchemaRef.Name != schema.Name || spec.SchemaRef.UID != schema.UID {
		return nil, nil, fmt.Errorf("plan schema reference does not match the owner")
	}

	name := "ptah-plan-" + spec.Fingerprint[len("sha256:"):len("sha256:")+24]
	chunks := split(content, ChunkBytes)
	if len(chunks) > MaxChunks {
		return nil, nil, fmt.Errorf("plan requires %d chunks; maximum is %d", len(chunks), MaxChunks)
	}
	spec.Chunks = make([]operatorv1alpha1.PlanChunkReference, len(chunks))
	for index, chunk := range chunks {
		spec.Chunks[index] = operatorv1alpha1.PlanChunkReference{
			Name:   fmt.Sprintf("%s-%03d", name, index),
			Key:    ChunkDataKey,
			Index:  int32(index),
			Digest: fingerprint.DigestBytes(chunk),
			Size:   int32(len(chunk)),
		}
	}
	blockDeletion := true
	controller := true
	plan := &operatorv1alpha1.PtahSchemaPlan{
		TypeMeta: metav1.TypeMeta{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahSchemaPlan"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: schema.Namespace,
			Name:      name,
			Labels: map[string]string{
				LabelSchema: schema.Name,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         operatorv1alpha1.GroupVersion.String(),
				Kind:               "PtahSchema",
				Name:               schema.Name,
				UID:                schema.UID,
				Controller:         &controller,
				BlockOwnerDeletion: &blockDeletion,
			}},
		},
		Spec: spec,
	}
	return plan, chunks, nil
}

// Publish resumes or completes the non-transactional Plan -> ConfigMaps ->
// Ready commit sequence. Existing objects must match byte-for-byte.
func (s Store) Publish(
	ctx context.Context,
	desired *operatorv1alpha1.PtahSchemaPlan,
	chunks [][]byte,
) (*operatorv1alpha1.PtahSchemaPlan, error) {
	if s.Client == nil {
		return nil, fmt.Errorf("plan store client is required")
	}
	if s.Reader == nil {
		s.Reader = s.Client
	}
	if desired == nil || len(chunks) != len(desired.Spec.Chunks) {
		return nil, fmt.Errorf("plan manifest and chunks do not match")
	}

	plan := desired.DeepCopy()
	if err := s.Client.Create(ctx, plan); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create plan manifest: %w", err)
		}
		plan = &operatorv1alpha1.PtahSchemaPlan{}
		if err := s.Reader.Get(ctx, client.ObjectKeyFromObject(desired), plan); err != nil {
			return nil, fmt.Errorf("read existing plan manifest: %w", err)
		}
		if err := sameManifest(desired, plan); err != nil {
			return nil, err
		}
	}

	published := make([]operatorv1alpha1.PublishedPlanChunkStatus, len(chunks))
	for index, chunk := range chunks {
		ref := plan.Spec.Chunks[index]
		if int(ref.Size) != len(chunk) || ref.Digest != fingerprint.DigestBytes(chunk) {
			return nil, fmt.Errorf("chunk %d does not match its manifest", index)
		}
		configMap := desiredChunk(plan, ref, chunk)
		if err := s.Client.Create(ctx, configMap); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("create plan chunk %d: %w", index, err)
			}
			configMap = &corev1.ConfigMap{}
			if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: plan.Namespace, Name: ref.Name}, configMap); err != nil {
				return nil, fmt.Errorf("read plan chunk %d: %w", index, err)
			}
		}
		if err := verifyChunk(plan, ref, chunk, configMap); err != nil {
			return nil, err
		}
		published[index] = operatorv1alpha1.PublishedPlanChunkStatus{Name: configMap.Name, UID: configMap.UID, Index: int32(index)}
	}

	latest := &operatorv1alpha1.PtahSchemaPlan{}
	if err := s.Reader.Get(ctx, client.ObjectKeyFromObject(plan), latest); err != nil {
		return nil, fmt.Errorf("read plan before committing storage: %w", err)
	}
	latest.Status.ObservedGeneration = latest.Generation
	latest.Status.PublishedChunks = published
	meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
		Type:               operatorv1alpha1.ConditionPlanStorageReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Published",
		Message:            fmt.Sprintf("Verified %d immutable plan chunks", len(chunks)),
		ObservedGeneration: latest.Generation,
		LastTransitionTime: metav1.Now(),
	})
	if err := s.Client.Status().Update(ctx, latest); err != nil {
		return nil, fmt.Errorf("commit plan storage status: %w", err)
	}
	return latest, nil
}

// Load reconstructs a Ready plan after checking manifest, object UIDs, sizes,
// and every digest through direct API reads.
func (s Store) Load(ctx context.Context, plan *operatorv1alpha1.PtahSchemaPlan) ([]byte, error) {
	if s.Reader == nil {
		if s.Client == nil {
			return nil, fmt.Errorf("plan store reader is required")
		}
		s.Reader = s.Client
	}
	if plan == nil || plan.UID == "" {
		return nil, fmt.Errorf("persisted plan is required")
	}
	if plan.Status.ObservedGeneration != plan.Generation ||
		!meta.IsStatusConditionTrue(plan.Status.Conditions, operatorv1alpha1.ConditionPlanStorageReady) {
		return nil, fmt.Errorf("plan storage is not committed")
	}
	if len(plan.Spec.Chunks) == 0 || len(plan.Spec.Chunks) != len(plan.Status.PublishedChunks) {
		return nil, fmt.Errorf("plan chunk manifest and committed status do not match")
	}

	published := append([]operatorv1alpha1.PublishedPlanChunkStatus(nil), plan.Status.PublishedChunks...)
	sort.Slice(published, func(i, j int) bool { return published[i].Index < published[j].Index })
	var content bytes.Buffer
	for index, ref := range plan.Spec.Chunks {
		committed := published[index]
		if ref.Index != int32(index) || committed.Index != int32(index) || committed.Name != ref.Name || committed.UID == "" {
			return nil, fmt.Errorf("plan chunk %d has an invalid ordering or commit binding", index)
		}
		configMap := &corev1.ConfigMap{}
		if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: plan.Namespace, Name: ref.Name}, configMap); err != nil {
			return nil, fmt.Errorf("read committed plan chunk %d: %w", index, err)
		}
		chunk := configMap.BinaryData[ref.Key]
		if configMap.UID != committed.UID {
			return nil, fmt.Errorf("plan chunk %d was replaced", index)
		}
		if err := verifyChunk(plan, ref, chunk, configMap); err != nil {
			return nil, err
		}
		if content.Len()+len(chunk) > MaxPlanBytes {
			return nil, fmt.Errorf("reconstructed plan exceeds %d bytes", MaxPlanBytes)
		}
		_, _ = content.Write(chunk)
	}
	if int64(content.Len()) != plan.Spec.Size || fingerprint.DigestBytes(content.Bytes()) != plan.Spec.ContentDigest {
		return nil, fmt.Errorf("reconstructed plan does not match its content binding")
	}
	return content.Bytes(), nil
}

// VolumeSources returns a deterministic read-only projection for an apply Job.
func VolumeSources(plan *operatorv1alpha1.PtahSchemaPlan) ([]corev1.VolumeProjection, error) {
	if plan == nil || len(plan.Spec.Chunks) == 0 || len(plan.Spec.Chunks) > MaxChunks {
		return nil, fmt.Errorf("plan has an invalid chunk manifest")
	}
	sources := make([]corev1.VolumeProjection, len(plan.Spec.Chunks))
	for index, ref := range plan.Spec.Chunks {
		if ref.Index != int32(index) || ref.Key == "" || ref.Name == "" {
			return nil, fmt.Errorf("plan chunk %d has an invalid projection", index)
		}
		sources[index] = corev1.VolumeProjection{ConfigMap: &corev1.ConfigMapProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name},
			Items:                []corev1.KeyToPath{{Key: ref.Key, Path: fmt.Sprintf("%03d.plan", index), Mode: mode(0o440)}},
		}}
	}
	return sources, nil
}

func desiredChunk(plan *operatorv1alpha1.PtahSchemaPlan, ref operatorv1alpha1.PlanChunkReference, content []byte) *corev1.ConfigMap {
	immutable := true
	controller := true
	blockDeletion := true
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: plan.Namespace,
			Name:      ref.Name,
			Labels: map[string]string{
				LabelPlan:   plan.Name,
				LabelSchema: plan.Spec.SchemaRef.Name,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         operatorv1alpha1.GroupVersion.String(),
				Kind:               "PtahSchemaPlan",
				Name:               plan.Name,
				UID:                plan.UID,
				Controller:         &controller,
				BlockOwnerDeletion: &blockDeletion,
			}},
		},
		Immutable:  &immutable,
		BinaryData: map[string][]byte{ref.Key: append([]byte(nil), content...)},
	}
}

func sameManifest(desired, actual *operatorv1alpha1.PtahSchemaPlan) error {
	if actual.DeletionTimestamp != nil {
		return fmt.Errorf("deterministic plan name is being deleted")
	}
	if !reflect.DeepEqual(desired.Spec, actual.Spec) || !reflect.DeepEqual(desired.OwnerReferences, actual.OwnerReferences) {
		return fmt.Errorf("deterministic plan name collides with different immutable content")
	}
	return nil
}

func verifyChunk(plan *operatorv1alpha1.PtahSchemaPlan, ref operatorv1alpha1.PlanChunkReference, expected []byte, configMap *corev1.ConfigMap) error {
	if configMap.Immutable == nil || !*configMap.Immutable {
		return fmt.Errorf("plan chunk %d is not immutable", ref.Index)
	}
	if configMap.Labels[LabelPlan] != plan.Name || configMap.Labels[LabelSchema] != plan.Spec.SchemaRef.Name {
		return fmt.Errorf("plan chunk %d labels do not match the manifest", ref.Index)
	}
	owned := false
	for _, owner := range configMap.OwnerReferences {
		if owner.UID == plan.UID && owner.Name == plan.Name && owner.Kind == "PtahSchemaPlan" {
			owned = true
		}
	}
	if !owned {
		return fmt.Errorf("plan chunk %d is not owned by the immutable plan UID", ref.Index)
	}
	actual, ok := configMap.BinaryData[ref.Key]
	if !ok || int32(len(actual)) != ref.Size || ref.Digest != fingerprint.DigestBytes(actual) || !bytes.Equal(actual, expected) {
		return fmt.Errorf("plan chunk %d does not match the manifest", ref.Index)
	}
	return nil
}

func split(content []byte, size int) [][]byte {
	chunks := make([][]byte, 0, (len(content)+size-1)/size)
	for offset := 0; offset < len(content); offset += size {
		end := min(offset+size, len(content))
		chunks = append(chunks, append([]byte(nil), content[offset:end]...))
	}
	return chunks
}

func mode(value int32) *int32 { return &value }
