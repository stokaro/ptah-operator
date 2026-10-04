package main

import (
	"context"
	"fmt"
	"reflect"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/planstore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Validate the inventory's captured bytes, rather than a second API reading
// which might not be the objects archived as the maintenance evidence.
func verifyRetentionPayloads(ctx context.Context, inventory retentionInventory) error {
	chunks := exportedChunks{}
	projections := map[client.ObjectKey]*corev1.ConfigMap{}
	for _, entry := range inventory.Objects {
		switch entry.Resource {
		case planChunkResource:
			var chunk operatorv1alpha1.PtahSchemaPlanChunk
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(entry.Object.Object, &chunk); err != nil {
				return err
			}
			chunks[client.ObjectKeyFromObject(&chunk)] = &chunk
		case configMapResource:
			var projection corev1.ConfigMap
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(entry.Object.Object, &projection); err != nil {
				return err
			}
			projections[client.ObjectKeyFromObject(&projection)] = &projection
		}
	}
	for id, value := range inventory.plans() {
		if id.Family != "schema" {
			continue
		}
		var plan operatorv1alpha1.PtahSchemaPlan
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(value.Object, &plan); err != nil {
			return err
		}
		if _, err := (planstore.Store{Reader: chunks}).Load(ctx, &plan); err != nil {
			return fmt.Errorf("retained plan %s: %w", id.Name, err)
		}
		for _, ref := range plan.Spec.Chunks {
			if projection := projections[client.ObjectKey{Namespace: id.Namespace, Name: ref.Name}]; projection != nil {
				if err := planstore.VerifyProjection(&plan, ref, projection); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func unchangedRetainedChildren(before, after retentionInventory, id retainedPlanID) bool {
	original, current := ownedChildren(before, id), ownedChildren(after, id)
	if len(original) != len(current) {
		return false
	}
	for _, child := range original {
		matched := false
		for _, next := range current {
			if child.Resource == next.Resource && child.Object.GetUID() == next.Object.GetUID() && child.Object.GetName() == next.Object.GetName() &&
				reflect.DeepEqual(child.Object.Object["spec"], next.Object.Object["spec"]) && reflect.DeepEqual(child.Object.Object["data"], next.Object.Object["data"]) && reflect.DeepEqual(child.Object.Object["binaryData"], next.Object.Object["binaryData"]) && reflect.DeepEqual(child.Object.GetOwnerReferences(), next.Object.GetOwnerReferences()) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
