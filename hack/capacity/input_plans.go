package main

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/planstore"
)

type inputPlanProof struct {
	Slot            int                              `json:"slot"`
	Round           int                              `json:"round"`
	Band            string                           `json:"band"`
	Namespace       string                           `json:"namespace"`
	SchemaUID       types.UID                        `json:"schemaUID"`
	Plan            *operatorv1alpha1.PtahSchemaPlan `json:"plan"`
	Bytes           int                              `json:"bytes"`
	ProjectedChunks int                              `json:"projectedChunks"`
}

func validateAppliedInput(schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan, reference string) error {
	applied := schema.Status.Applied
	if applied == nil || schema.UID == "" || plan.UID == "" ||
		applied.PlanRef.Name != plan.Name || applied.PlanRef.UID != plan.UID ||
		applied.PlanFingerprint != plan.Spec.Fingerprint ||
		applied.ArtifactDigest != digestOf(reference) || plan.Spec.ArtifactDigest != applied.ArtifactDigest ||
		plan.Namespace != schema.Namespace || plan.Spec.SchemaRef.Name != schema.Name || plan.Spec.SchemaRef.UID != schema.UID {
		return fmt.Errorf("applied plan is not bound to the schema and expected input artifact")
	}
	return nil
}

func validatePlanBand(band string, size int) error {
	bounds, ok := map[string][2]int{"small": {3 * 1024, 4 * 1024}, "medium": {240 * 1024, 256 * 1024}, "large": {960 * 1024, 1024 * 1024}}[band]
	if !ok || size < bounds[0] || size > bounds[1] {
		return fmt.Errorf("stored native plan has %d bytes outside the %s band", size, band)
	}
	return nil
}

// Read the committed store and the actual Apply projections. Calibration alone
// cannot establish the size or identity of what the operator executed.
func (s *scenarios) verifyInputPlans(ctx context.Context, round int) error {
	if s.in.catalog == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if s.inputReader == nil {
		return fmt.Errorf("input plan reader is missing")
	}
	store := planstore.Store{Reader: s.inputReader}
	for index, input := range s.in.catalog.Schemas {
		if round > 0 && index >= s.load.ChangeBatch {
			continue
		}
		object := &operatorv1alpha1.PtahSchema{}
		key := types.NamespacedName{Namespace: s.in.namespaceFor(index), Name: s.schemaName(index)}
		if err := s.inputReader.Get(ctx, key, object); err != nil {
			return err
		}
		if object.Status.Applied == nil {
			return fmt.Errorf("%s has no applied plan", key)
		}
		plan := &operatorv1alpha1.PtahSchemaPlan{}
		if err := s.inputReader.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: object.Status.Applied.PlanRef.Name}, plan); err != nil {
			return err
		}
		if err := validateAppliedInput(object, plan, input.References[round]); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		raw, err := store.Load(ctx, plan)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if err := validatePlanBand(input.Band, len(raw)); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		for _, ref := range plan.Spec.Chunks {
			projection := &corev1.ConfigMap{}
			if err := s.inputReader.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: ref.Name}, projection); err != nil {
				return err
			}
			if err := planstore.VerifyProjection(plan, ref, projection); err != nil {
				return err
			}
		}
		s.inputPlans = append(s.inputPlans, inputPlanProof{Slot: index, Round: round, Band: input.Band, Namespace: key.Namespace, SchemaUID: object.UID, Plan: plan, Bytes: len(raw), ProjectedChunks: len(plan.Spec.Chunks)})
	}
	return nil
}
