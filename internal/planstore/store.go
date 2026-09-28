// Package planstore publishes immutable executable plans without placing SQL
// in a custom-resource status or controller log.
//
// A plan's bytes live in PtahSchemaPlanChunk objects the plan owns, so reading
// a plan takes one RBAC rule on that kind. An Apply Pod holds no Kubernetes
// credential and the kubelet projects no custom resource into a volume, so the
// Apply reads the same bytes through immutable ConfigMaps of the same names,
// which Project writes from verified chunks just before the Apply Job is
// created. The ConfigMaps exist only for plans that reached an Apply.
package planstore

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/plancontract"
)

const (
	// ChunkBytes leaves headroom below the Kubernetes object-size limit.
	ChunkBytes = plancontract.ChunkBytes
	// MaxChunks bounds projected-volume fan-out and API-server load.
	MaxChunks    = plancontract.MaxChunks
	MaxPlanBytes = int(plancontract.MaxExecutableBytes)

	// ProjectionDataKey is the one key an Apply's projection ConfigMap holds
	// its chunk under.
	ProjectionDataKey = "chunk"
	LabelPlan         = "operator.ptah.run/plan"
	LabelSchema       = "operator.ptah.run/schema"

	// namePrefix opens the name every schema plan is published under.
	namePrefix = "ptah-plan-"
)

// ErrProjectionConflict reports a ConfigMap under a projection name that is
// not the projection this plan needs. A retry cannot fix it: the name is
// derived from the plan, so only another plan, with another name, can.
var ErrProjectionConflict = errors.New("plan projection conflicts with an existing ConfigMap")

var (
	sha256Pattern             = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	executionBindingIDPattern = regexp.MustCompile(`^v1-[0-9a-f]{32}$`)
	imageDigestPattern        = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
)

// Name is the plan's deterministic object name. Two publications of the same
// plan are the same object, so a controller that restarted mid-publication
// cannot leave a second copy of one decision.
func Name(planFingerprint string) (string, error) {
	if !sha256Pattern.MatchString(planFingerprint) {
		return "", fmt.Errorf("plan fingerprint must be a lowercase SHA-256 digest")
	}
	return namePrefix + planFingerprint[len("sha256:"):len("sha256:")+24], nil
}

// Binding returns the approval identity a plan spec stands for, read from the
// spec as published. The manager computes a plan's fingerprint from it before
// publishing, and the controller-write webhook recomputes it before admitting
// the plan, so the two cannot disagree about what a field means.
func Binding(schemaUID types.UID, spec operatorv1alpha1.PtahSchemaPlanSpec) fingerprint.PlanBinding {
	privileges := make([]string, 0, len(spec.PrivilegeChanges))
	for _, kind := range spec.PrivilegeChanges {
		privileges = append(privileges, string(kind))
	}
	return fingerprint.PlanBinding{
		ContractVersion:          spec.ContractVersion,
		SchemaUID:                string(schemaUID),
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
		ControllerStateVersion:   spec.ControllerStateVersion,
		PtahVersion:              spec.PtahVersion,
		ExecutorImage:            spec.ExecutorImage,
		RunnerProtocolVersion:    spec.RunnerProtocolVersion,
		Destructive:              spec.Destructive,
		PrivilegeChanges:         privileges,
		StatementCount:           spec.StatementCount,
	}
}

// Store uses direct API reads through Reader and mutating calls through Client.
// The distinction lets controllers bypass a stale cache before apply.
type Store struct {
	Client client.Client
	Reader client.Reader
}

// Prepare creates the deterministic manifest and chunks for exact plan bytes.
// The caller supplies every plan binding except the content-derived fields.
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
	if err := validatePlanContract(spec); err != nil {
		return nil, nil, err
	}
	if spec.SchemaRef.Name != schema.Name || spec.SchemaRef.UID != schema.UID {
		return nil, nil, fmt.Errorf("plan schema reference does not match the owner")
	}

	name, err := Name(spec.Fingerprint)
	if err != nil {
		return nil, nil, err
	}
	chunks := split(content, ChunkBytes)
	if len(chunks) > MaxChunks {
		return nil, nil, fmt.Errorf("plan requires %d chunks; maximum is %d", len(chunks), MaxChunks)
	}
	spec.Chunks = make([]operatorv1alpha1.PlanChunkReference, len(chunks))
	for index, chunk := range chunks {
		spec.Chunks[index] = operatorv1alpha1.PlanChunkReference{
			Name:   fmt.Sprintf("%s-%03d", name, index),
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

// Publish resumes or completes the non-transactional Plan -> chunks -> Ready
// commit sequence. Existing objects must match byte-for-byte.
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
	if err := validatePlanContract(desired.Spec); err != nil {
		return nil, err
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
	for index, content := range chunks {
		ref := plan.Spec.Chunks[index]
		if int(ref.Size) != len(content) || ref.Digest != fingerprint.DigestBytes(content) {
			return nil, fmt.Errorf("chunk %d does not match its manifest", index)
		}
		chunk := DesiredChunk(plan, ref, content)
		if err := s.Client.Create(ctx, chunk); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("create plan chunk %d: %w", index, err)
			}
			chunk = &operatorv1alpha1.PtahSchemaPlanChunk{}
			if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: plan.Namespace, Name: ref.Name}, chunk); err != nil {
				return nil, fmt.Errorf("read plan chunk %d: %w", index, err)
			}
		}
		if err := verifyChunk(plan, ref, content, chunk); err != nil {
			return nil, err
		}
		published[index] = operatorv1alpha1.PublishedPlanChunkStatus{Name: chunk.Name, UID: chunk.UID, Index: int32(index)}
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
	if err := validatePlanContract(plan.Spec); err != nil {
		return nil, err
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
		chunk := &operatorv1alpha1.PtahSchemaPlanChunk{}
		if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: plan.Namespace, Name: ref.Name}, chunk); err != nil {
			return nil, fmt.Errorf("read committed plan chunk %d: %w", index, err)
		}
		if chunk.UID != committed.UID {
			return nil, fmt.Errorf("plan chunk %d was replaced", index)
		}
		data := chunk.Spec.Data
		if err := verifyChunk(plan, ref, data, chunk); err != nil {
			return nil, err
		}
		if content.Len()+len(data) > MaxPlanBytes {
			return nil, fmt.Errorf("reconstructed plan exceeds %d bytes", MaxPlanBytes)
		}
		_, _ = content.Write(data)
	}
	if int64(content.Len()) != plan.Spec.Size || fingerprint.DigestBytes(content.Bytes()) != plan.Spec.ContentDigest {
		return nil, fmt.Errorf("reconstructed plan does not match its content binding")
	}
	return content.Bytes(), nil
}

// Project writes the immutable ConfigMaps an Apply Pod mounts the plan
// through, one per chunk and under the chunk's own name, from content that Load
// returned for the same plan. It resumes: a projection an earlier attempt
// wrote is read back and must match byte for byte.
//
// The ConfigMaps are owned by the plan and deleted with it. The Pod's runner
// checks the whole document against the plan's content digest before it runs
// anything, so a projection is a transport rather than a second store: what it
// carries is decided by the chunks and the digest, not by the ConfigMap.
//
// An error wrapping ErrProjectionConflict means a ConfigMap under one of the
// names is not this plan's projection. Any other error is the API's and may
// clear on a retry.
func (s Store) Project(ctx context.Context, plan *operatorv1alpha1.PtahSchemaPlan, content []byte) error {
	if s.Client == nil {
		return fmt.Errorf("plan store client is required")
	}
	if s.Reader == nil {
		s.Reader = s.Client
	}
	if plan == nil || plan.UID == "" {
		return fmt.Errorf("persisted plan is required")
	}
	if err := validatePlanContract(plan.Spec); err != nil {
		return err
	}
	if int64(len(content)) != plan.Spec.Size || fingerprint.DigestBytes(content) != plan.Spec.ContentDigest {
		return fmt.Errorf("plan content does not match its content binding")
	}
	chunks := split(content, ChunkBytes)
	if len(chunks) != len(plan.Spec.Chunks) {
		return fmt.Errorf("plan content does not split into its %d chunks", len(plan.Spec.Chunks))
	}
	for index, data := range chunks {
		ref := plan.Spec.Chunks[index]
		if ref.Index != int32(index) || int(ref.Size) != len(data) || ref.Digest != fingerprint.DigestBytes(data) {
			return fmt.Errorf("plan chunk %d does not match its manifest", index)
		}
		configMap := DesiredProjection(plan, ref, data)
		if err := s.Client.Create(ctx, configMap); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create plan projection %d: %w", index, err)
			}
			configMap = &corev1.ConfigMap{}
			if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: plan.Namespace, Name: ref.Name}, configMap); err != nil {
				return fmt.Errorf("read plan projection %d: %w", index, err)
			}
		}
		if err := VerifyProjection(plan, ref, configMap); err != nil {
			return fmt.Errorf("%w: %w", ErrProjectionConflict, err)
		}
	}
	return nil
}

// VolumeSources returns a deterministic read-only projection for an apply Job.
// It names the ConfigMaps Project writes.
func VolumeSources(plan *operatorv1alpha1.PtahSchemaPlan) ([]corev1.VolumeProjection, error) {
	if plan == nil || len(plan.Spec.Chunks) == 0 || len(plan.Spec.Chunks) > MaxChunks {
		return nil, fmt.Errorf("plan has an invalid chunk manifest")
	}
	if err := validatePlanContract(plan.Spec); err != nil {
		return nil, err
	}
	sources := make([]corev1.VolumeProjection, len(plan.Spec.Chunks))
	for index, ref := range plan.Spec.Chunks {
		if ref.Index != int32(index) || ref.Name == "" {
			return nil, fmt.Errorf("plan chunk %d has an invalid projection", index)
		}
		sources[index] = corev1.VolumeProjection{ConfigMap: &corev1.ConfigMapProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name},
			Items:                []corev1.KeyToPath{{Key: ProjectionDataKey, Path: fmt.Sprintf("%03d.plan", index), Mode: mode(0o440)}},
		}}
	}
	return sources, nil
}

func validatePlanContract(spec operatorv1alpha1.PtahSchemaPlanSpec) error {
	if err := fingerprint.ValidatePlanContractVersion(spec.ContractVersion); err != nil {
		return err
	}
	if !executionBindingIDPattern.MatchString(spec.ExecutionBindingID) {
		return fmt.Errorf("plan requires a valid execution binding ID")
	}
	if !imageDigestPattern.MatchString(spec.ControllerImage) {
		return fmt.Errorf("plan requires a digest-pinned controller image")
	}
	if err := controllerstate.ValidateRevision(spec.ControllerRevision); err != nil {
		return fmt.Errorf("plan has an invalid controller revision: %w", err)
	}
	if spec.ControllerStateVersion < 1 {
		return fmt.Errorf("plan requires a positive controller state version")
	}
	return nil
}

// DesiredChunk is the exact PtahSchemaPlanChunk the store writes for one chunk
// of a persisted plan.
func DesiredChunk(
	plan *operatorv1alpha1.PtahSchemaPlan,
	ref operatorv1alpha1.PlanChunkReference,
	content []byte,
) *operatorv1alpha1.PtahSchemaPlanChunk {
	return &operatorv1alpha1.PtahSchemaPlanChunk{
		TypeMeta:   metav1.TypeMeta{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahSchemaPlanChunk"},
		ObjectMeta: chunkMetadata(plan, ref),
		Spec:       operatorv1alpha1.PtahSchemaPlanChunkSpec{Data: append([]byte(nil), content...)},
	}
}

// DesiredProjection is the exact ConfigMap Project writes for one chunk of a
// persisted plan.
func DesiredProjection(
	plan *operatorv1alpha1.PtahSchemaPlan,
	ref operatorv1alpha1.PlanChunkReference,
	content []byte,
) *corev1.ConfigMap {
	immutable := true
	return &corev1.ConfigMap{
		ObjectMeta: chunkMetadata(plan, ref),
		Immutable:  &immutable,
		BinaryData: map[string][]byte{ProjectionDataKey: append([]byte(nil), content...)},
	}
}

// chunkMetadata is what a chunk and its projection both carry: the chunk's
// name, the plan and schema labels, and the plan as the one blocking
// controller owner, so both are deleted with the plan.
func chunkMetadata(plan *operatorv1alpha1.PtahSchemaPlan, ref operatorv1alpha1.PlanChunkReference) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Namespace: plan.Namespace,
		Name:      ref.Name,
		Labels: map[string]string{
			LabelPlan:   plan.Name,
			LabelSchema: plan.Spec.SchemaRef.Name,
		},
		OwnerReferences: []metav1.OwnerReference{blockingPlanOwnerReference(plan)},
	}
}

// sameManifest accepts an existing plan as the one being published when
// everything but the record of its publisher matches. The fingerprint, and
// with it the name, leaves the manager's image, revision and runner image
// out, so a later release of the manager computing the same plan finds the
// one an earlier release published. That plan keeps its original record.
func sameManifest(desired, actual *operatorv1alpha1.PtahSchemaPlan) error {
	if actual.DeletionTimestamp != nil {
		return fmt.Errorf("deterministic plan name is being deleted")
	}
	published := actual.Spec.DeepCopy()
	published.ControllerImage = desired.Spec.ControllerImage
	published.ControllerRevision = desired.Spec.ControllerRevision
	published.RunnerImage = desired.Spec.RunnerImage
	if !reflect.DeepEqual(desired.Spec, *published) || !reflect.DeepEqual(desired.OwnerReferences, actual.OwnerReferences) {
		return fmt.Errorf("deterministic plan name collides with different immutable content")
	}
	return nil
}

func verifyChunk(
	plan *operatorv1alpha1.PtahSchemaPlan,
	ref operatorv1alpha1.PlanChunkReference,
	expected []byte,
	chunk *operatorv1alpha1.PtahSchemaPlanChunk,
) error {
	if err := verifyChunkMetadata(plan, ref, chunk.ObjectMeta); err != nil {
		return err
	}
	actual := chunk.Spec.Data
	if int32(len(actual)) != ref.Size || ref.Digest != fingerprint.DigestBytes(actual) || !bytes.Equal(actual, expected) {
		return fmt.Errorf("plan chunk %d does not match the manifest", ref.Index)
	}
	return nil
}

// VerifyProjection holds an Apply's projection ConfigMap to the chunk it
// carries: immutable, named, labeled and owned the way Project writes it, and
// holding exactly the bytes the plan's manifest records for that chunk.
func VerifyProjection(
	plan *operatorv1alpha1.PtahSchemaPlan,
	ref operatorv1alpha1.PlanChunkReference,
	configMap *corev1.ConfigMap,
) error {
	if configMap.Immutable == nil || !*configMap.Immutable {
		return fmt.Errorf("plan projection %d is not immutable", ref.Index)
	}
	if err := verifyChunkMetadata(plan, ref, configMap.ObjectMeta); err != nil {
		return err
	}
	actual, ok := configMap.BinaryData[ProjectionDataKey]
	if !ok || len(configMap.BinaryData) != 1 || len(configMap.Data) != 0 ||
		int32(len(actual)) != ref.Size || ref.Digest != fingerprint.DigestBytes(actual) {
		return fmt.Errorf("plan projection %d does not match the manifest", ref.Index)
	}
	return nil
}

func verifyChunkMetadata(
	plan *operatorv1alpha1.PtahSchemaPlan,
	ref operatorv1alpha1.PlanChunkReference,
	object metav1.ObjectMeta,
) error {
	if object.Name != ref.Name {
		return fmt.Errorf("plan chunk %d is not named %s", ref.Index, ref.Name)
	}
	if object.Labels[LabelPlan] != plan.Name || object.Labels[LabelSchema] != plan.Spec.SchemaRef.Name {
		return fmt.Errorf("plan chunk %d labels do not match the manifest", ref.Index)
	}
	expectedOwnerReferences := []metav1.OwnerReference{blockingPlanOwnerReference(plan)}
	if !reflect.DeepEqual(object.OwnerReferences, expectedOwnerReferences) {
		return fmt.Errorf("plan chunk %d does not have the exact blocking plan owner reference", ref.Index)
	}
	return nil
}

func blockingPlanOwnerReference(plan *operatorv1alpha1.PtahSchemaPlan) metav1.OwnerReference {
	controller := true
	blockDeletion := true
	return metav1.OwnerReference{
		APIVersion:         operatorv1alpha1.GroupVersion.String(),
		Kind:               "PtahSchemaPlan",
		Name:               plan.Name,
		UID:                plan.UID,
		Controller:         &controller,
		BlockOwnerDeletion: &blockDeletion,
	}
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
