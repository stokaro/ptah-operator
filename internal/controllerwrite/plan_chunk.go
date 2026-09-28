package controllerwrite

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/plancontract"
	"github.com/stokaro/ptah-operator/internal/planstore"
)

var (
	planChunkResource = metav1.GroupVersionResource{
		Group:    operatorv1alpha1.GroupVersion.Group,
		Version:  operatorv1alpha1.GroupVersion.Version,
		Resource: "ptahschemaplanchunks",
	}
	planChunkKind = metav1.GroupVersionKind{
		Group:   operatorv1alpha1.GroupVersion.Group,
		Version: operatorv1alpha1.GroupVersion.Version,
		Kind:    "PtahSchemaPlanChunk",
	}
)

// validatePlanChunkCreate admits a chunk only while the plan that owns it is
// being published: the schema's active operation is the harvested Plan whose
// result the plan was computed from, and the chunk holds exactly the bytes the
// plan's manifest records under its name.
func (v *Validator) validatePlanChunkCreate(ctx context.Context, req admissionv1.AdmissionRequest) error {
	if len(req.OldObject.Raw) != 0 {
		return badRequestf("PtahSchemaPlanChunk create unexpectedly contains an old object")
	}
	chunk := &operatorv1alpha1.PtahSchemaPlanChunk{}
	if err := decodeObject(req.Object.Raw, chunk, planChunkKind); err != nil {
		return err
	}
	if err := validateRequestIdentity(req, &chunk.ObjectMeta); err != nil {
		return err
	}
	plan, schema, err := v.chunkOwner(ctx, chunk.Namespace, chunk.OwnerReferences, "PtahSchemaPlanChunk")
	if err != nil {
		return err
	}
	if err := validatePlanPublicationContext(plan, schema); err != nil {
		return denyf("plan chunk does not belong to the active Plan operation: %v", err)
	}
	ref, ok := findChunkReference(plan.Spec.Chunks, chunk.Name)
	if !ok {
		return denyf("PtahSchemaPlanChunk name is not present in the immutable plan chunk manifest")
	}
	if err := validateChunkObject(chunk, plan, ref); err != nil {
		return denyf("PtahSchemaPlanChunk does not match its immutable plan chunk reference: %v", err)
	}
	return nil
}

// validateProjectionCreate admits a ConfigMap only as one piece of the
// projection an Apply mounts its plan through, and only while that Apply can
// still be dispatched: the schema's active operation is an Apply of its current
// plan whose Job does not exist and whose dispatch has not started. The bytes
// are held to the plan's manifest, which the approval named by fingerprint.
func (v *Validator) validateProjectionCreate(ctx context.Context, req admissionv1.AdmissionRequest) error {
	if len(req.OldObject.Raw) != 0 {
		return badRequestf("ConfigMap create unexpectedly contains an old object")
	}
	configMap := &corev1.ConfigMap{}
	if err := decodeObject(req.Object.Raw, configMap, configMapKind); err != nil {
		return err
	}
	if err := validateRequestIdentity(req, &configMap.ObjectMeta); err != nil {
		return err
	}
	plan, schema, err := v.chunkOwner(ctx, configMap.Namespace, configMap.OwnerReferences, "ConfigMap")
	if err != nil {
		return err
	}
	if err := validateApplyDispatchContext(plan, schema); err != nil {
		return denyf("plan projection does not belong to an Apply that can still be dispatched: %v", err)
	}
	ref, ok := findChunkReference(plan.Spec.Chunks, configMap.Name)
	if !ok {
		return denyf("ConfigMap name is not present in the immutable plan chunk manifest")
	}
	if err := validateProjection(configMap, plan, ref); err != nil {
		return denyf("ConfigMap does not match its immutable plan chunk reference: %v", err)
	}
	return nil
}

// chunkOwner reads the plan a chunk or a projection names as its one exact
// controller owner, and the schema that owns the plan, directly from the API
// server, and holds the plan to the manifest contract.
func (v *Validator) chunkOwner(
	ctx context.Context,
	namespace string,
	references []metav1.OwnerReference,
	kind string,
) (*operatorv1alpha1.PtahSchemaPlan, *operatorv1alpha1.PtahSchema, error) {
	owner, err := exactControllerOwner(references, operatorv1alpha1.GroupVersion.String(), "PtahSchemaPlan")
	if err != nil {
		return nil, nil, denyf("%s does not have one exact PtahSchemaPlan controller owner: %v", kind, err)
	}
	plan := &operatorv1alpha1.PtahSchemaPlan{}
	key := client.ObjectKey{Namespace: namespace, Name: owner.Name}
	if err := v.Reader.Get(ctx, key, plan); err != nil {
		return nil, nil, internalf("directly read plan manifest %s/%s: %v", key.Namespace, key.Name, err)
	}
	if plan.UID == "" || plan.UID != owner.UID {
		return nil, nil, denyf("%s owner does not match the current PtahSchemaPlan UID", kind)
	}
	planOwner, err := exactControllerOwner(plan.OwnerReferences, operatorv1alpha1.GroupVersion.String(), "PtahSchema")
	if err != nil {
		return nil, nil, denyf("owning PtahSchemaPlan has no exact PtahSchema controller owner: %v", err)
	}
	schema, err := v.readSchema(ctx, plan.Namespace, planOwner)
	if err != nil {
		return nil, nil, err
	}
	if err := validatePlanMetadata(plan, schema); err != nil {
		return nil, nil, denyf("owning PtahSchemaPlan metadata is invalid: %v", err)
	}
	if err := validatePlanShape(plan, schema); err != nil {
		return nil, nil, denyf("owning PtahSchemaPlan manifest is invalid: %v", err)
	}
	return plan, schema, nil
}

// validateApplyDispatchContext is the state Project runs in: an Apply claim of
// the schema's current plan, before the one Job create it is allowed and
// before DispatchStarted, with the plan's storage committed.
func validateApplyDispatchContext(plan *operatorv1alpha1.PtahSchemaPlan, schema *operatorv1alpha1.PtahSchema) error {
	operation := schema.Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.OperationApply || operation.JobName == "" {
		return errors.New("schema has no active Apply operation")
	}
	if operation.JobUID != "" || operation.DispatchStarted {
		return errors.New("the active Apply has already crossed its dispatch boundary")
	}
	current := schema.Status.Plan
	if current == nil || current.Name != plan.Name || current.UID != plan.UID || current.Fingerprint != plan.Spec.Fingerprint {
		return errors.New("plan is not the schema's current plan")
	}
	if len(plan.Status.PublishedChunks) != len(plan.Spec.Chunks) ||
		!apiMeta.IsStatusConditionTrue(plan.Status.Conditions, operatorv1alpha1.ConditionPlanStorageReady) {
		return errors.New("plan storage is not committed")
	}
	return nil
}

// validateApplyProjection reads what the Apply Pod will mount -- the
// projection ConfigMaps its volume names -- and requires them to rebuild the
// plan's content digest, after checking that the plan's own storage is
// committed. The runner checks the same digest inside the Pod; this is the
// check that stops a Job from being created toward bytes it would refuse.
func (v *Validator) validateApplyProjection(ctx context.Context, plan *operatorv1alpha1.PtahSchemaPlan) error {
	if len(plan.Status.PublishedChunks) != len(plan.Spec.Chunks) {
		return denyf("Apply plan does not bind every published chunk")
	}
	for index, ref := range plan.Spec.Chunks {
		published := plan.Status.PublishedChunks[index]
		if published.Index != int32(index) || published.Name != ref.Name || published.UID == "" {
			return denyf("Apply plan published chunk %d has an invalid identity binding", index)
		}
	}

	type loadedProjection struct {
		configMap *corev1.ConfigMap
		err       error
	}
	loaded := make([]loadedProjection, len(plan.Spec.Chunks))
	var reads sync.WaitGroup
	reads.Add(len(plan.Spec.Chunks))
	for index, ref := range plan.Spec.Chunks {
		go func() {
			defer reads.Done()

			configMap := &corev1.ConfigMap{}
			key := client.ObjectKey{Namespace: plan.Namespace, Name: ref.Name}
			if err := v.Reader.Get(ctx, key, configMap); err != nil {
				loaded[index].err = internalf("directly read Apply plan projection %s/%s: %v", key.Namespace, key.Name, err)
				return
			}
			if err := validateProjection(configMap, plan, ref); err != nil {
				loaded[index].err = denyf("Apply plan projection %d is invalid: %v", index, err)
				return
			}
			loaded[index].configMap = configMap
		}()
	}
	reads.Wait()

	var content bytes.Buffer
	for index := range plan.Spec.Chunks {
		if loaded[index].err != nil {
			return loaded[index].err
		}
		configMap := loaded[index].configMap
		if configMap == nil {
			return internalf("Apply plan projection %d completed without a result", index)
		}
		data := configMap.BinaryData[planstore.ProjectionDataKey]
		if content.Len()+len(data) > int(plancontract.MaxExecutableBytes) {
			return denyf("Apply plan projection exceeds the executable plan size limit")
		}
		_, _ = content.Write(data)
	}
	if int64(content.Len()) != plan.Spec.Size || fingerprint.DigestBytes(content.Bytes()) != plan.Spec.ContentDigest {
		return denyf("Apply plan projection does not reconstruct the immutable content binding")
	}
	return nil
}

// validateChunkObject holds a chunk to its manifest reference: exactly the
// recorded bytes, and exactly the metadata the store writes.
func validateChunkObject(
	chunk *operatorv1alpha1.PtahSchemaPlanChunk,
	plan *operatorv1alpha1.PtahSchemaPlan,
	ref operatorv1alpha1.PlanChunkReference,
) error {
	if chunk == nil || plan == nil {
		return errors.New("chunk validation inputs are incomplete")
	}
	data := chunk.Spec.Data
	if len(data) != int(ref.Size) || fingerprint.DigestBytes(data) != ref.Digest {
		return errors.New("chunk data does not match its declared size and digest")
	}
	return validateChunkMetadata(chunk.ObjectMeta, plan, ref)
}

// validateProjection holds a projection ConfigMap to the chunk it carries:
// immutable, one binary value under the fixed key with the recorded bytes, and
// exactly the metadata the store writes.
func validateProjection(
	configMap *corev1.ConfigMap,
	plan *operatorv1alpha1.PtahSchemaPlan,
	ref operatorv1alpha1.PlanChunkReference,
) error {
	if configMap == nil || plan == nil {
		return errors.New("projection validation inputs are incomplete")
	}
	if configMap.Immutable == nil || !*configMap.Immutable {
		return errors.New("projection is not immutable")
	}
	if len(configMap.Data) != 0 || len(configMap.BinaryData) != 1 {
		return errors.New("projection must contain exactly one BinaryData value and no string data")
	}
	content, ok := configMap.BinaryData[planstore.ProjectionDataKey]
	if !ok || len(content) != int(ref.Size) || fingerprint.DigestBytes(content) != ref.Digest {
		return errors.New("projection payload does not match its declared key, size, and digest")
	}
	return validateChunkMetadata(configMap.ObjectMeta, plan, ref)
}

func validateChunkMetadata(
	metadata metav1.ObjectMeta,
	plan *operatorv1alpha1.PtahSchemaPlan,
	ref operatorv1alpha1.PlanChunkReference,
) error {
	if _, err := exactNamedControllerOwner(
		metadata.OwnerReferences,
		operatorv1alpha1.GroupVersion.String(),
		"PtahSchemaPlan",
		plan.Name,
		plan.UID,
	); err != nil {
		return err
	}
	expected := metav1.ObjectMeta{
		Namespace: metadata.Namespace,
		Name:      ref.Name,
		Labels: map[string]string{
			planstore.LabelPlan:   plan.Name,
			planstore.LabelSchema: plan.Spec.SchemaRef.Name,
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion:         operatorv1alpha1.GroupVersion.String(),
			Kind:               "PtahSchemaPlan",
			Name:               plan.Name,
			UID:                plan.UID,
			Controller:         boolPointer(true),
			BlockOwnerDeletion: boolPointer(true),
		}},
	}
	actual := metadata.DeepCopy()
	scrubCreateServerMetadata(actual)
	if !reflect.DeepEqual(actual, &expected) {
		return errors.New("metadata contains fields outside the immutable storage contract")
	}
	return nil
}
