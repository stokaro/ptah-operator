package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

type retentionArchive struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type pausedResource struct {
	target batchTarget
	spec   map[string]any
}

func inactiveForMaintenance(object *unstructured.Unstructured) bool {
	for _, field := range []string{"activeOperation", "pendingObservation", "pendingLockRelease"} {
		value, found, err := unstructured.NestedFieldNoCopy(object.Object, "status", field)
		if err != nil || found && value != nil {
			return false
		}
	}
	observed, _, err := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
	return err == nil && observed == object.GetGeneration() && object.GetDeletionTimestamp() == nil
}

func maintenanceSuspended(object *unstructured.Unstructured, paused pausedResource) bool {
	suspended, _, err := unstructured.NestedBool(object.Object, "spec", "suspend")
	if err != nil || !suspended || !inactiveForMaintenance(object) || object.GetUID() != paused.target.uid || object.GetGeneration() != paused.target.generation || !reflect.DeepEqual(object.Object["spec"], paused.spec) {
		return false
	}
	conditions, _, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil {
		return false
	}
	for _, value := range conditions {
		condition, ok := value.(map[string]any)
		if ok && condition["type"] == "Ready" && condition["status"] == "False" && condition["reason"] == "Suspended" && condition["observedGeneration"] == object.GetGeneration() {
			return true
		}
	}
	return false
}

func (s *scenarios) exportAndSuspend(ctx context.Context, family string, index, round int) (pausedResource, retentionArchive, error) {
	return s.exportAndSuspendTo(ctx, family, index, round, filepath.Join(s.evidenceDir, "retention", fmt.Sprintf("round-%02d", round)))
}

func (s *scenarios) exportAndSuspendTo(ctx context.Context, family string, index, round int, directory string) (pausedResource, retentionArchive, error) {
	resource, name, field, reference := schemaResource, s.schemaName(index), "desired", s.schemaReference(index, round)
	if family == "migration" {
		resource, name, field, reference = migrationResource, s.migrationName(index), "artifact", s.migrationReference(index, round)
	}
	target := batchTarget{family: family, resource: resource, namespace: s.in.namespaceFor(index), name: name, reference: reference}
	client := s.dynamic.Resource(resource).Namespace(target.namespace)
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return pausedResource{}, retentionArchive{}, err
		}
		original, err := client.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return pausedResource{}, retentionArchive{}, err
		}
		current, _, err := unstructured.NestedString(original.Object, "spec", field, "ociRef")
		suspended, _, suspendErr := unstructured.NestedBool(original.Object, "spec", "suspend")
		if err != nil || suspendErr != nil || suspended || current != reference || original.GetUID() == "" || original.GetResourceVersion() == "" || original.GetLabels()[capacityLabel] != s.load.Name {
			return pausedResource{}, retentionArchive{}, fmt.Errorf("maintenance %s changed its identity, input or suspension", name)
		}
		if !inactiveForMaintenance(original) {
			if err := waitCapacityPoll(ctx); err != nil {
				return pausedResource{}, retentionArchive{}, fmt.Errorf("maintenance %s did not become idle: %w", name, err)
			}
			continue
		}
		path, digest, _, err := s.exportOwnedPlans(ctx, original, family, directory, fmt.Sprintf("%s-%s-attempt-%d.json", family, name, attempt))
		if err != nil {
			return pausedResource{}, retentionArchive{}, err
		}
		target.uid, target.generation = original.GetUID(), original.GetGeneration()+1
		spec, found, err := unstructured.NestedMap(original.Object, "spec")
		if err != nil || !found {
			return pausedResource{}, retentionArchive{}, fmt.Errorf("maintenance resource lacks spec")
		}
		spec["suspend"] = true
		paused := pausedResource{target, spec}
		relative, err := filepath.Rel(s.evidenceDir, path)
		archive := retentionArchive{relative, digest}
		if err != nil || !filepath.IsLocal(relative) {
			return pausedResource{}, archive, fmt.Errorf("maintenance export escaped its directory")
		}
		raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": original.GetUID(), "resourceVersion": original.GetResourceVersion()}, "spec": map[string]any{"suspend": true}})
		updated, err := client.Patch(ctx, name, types.MergePatchType, raw, metav1.PatchOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		// A transport error can hide a successful write. Keep its original
		// identity in the recovery set before returning the error.
		if err != nil {
			return paused, archive, err
		}
		if updated.GetUID() != target.uid || updated.GetNamespace() != target.namespace || updated.GetName() != target.name || updated.GetGeneration() != target.generation || !reflect.DeepEqual(updated.Object["spec"], spec) {
			return paused, archive, fmt.Errorf("suspension changed resource identity or unrelated spec")
		}
		return paused, archive, nil
	}
}

// Suspension stops dispatch through the controller's own gate. Also inspect
// Jobs and Pods, since clearing a status claim alone cannot prove SQL stopped.
func (s *scenarios) maintenanceQuiet(ctx context.Context, paused []pausedResource) (bool, error) {
	if len(paused) != s.load.Schemas+s.load.Migrations || len(paused) == 0 {
		return false, fmt.Errorf("maintenance did not suspend the entire workload")
	}
	for _, p := range paused {
		object, err := s.dynamic.Resource(p.target.resource).Namespace(p.target.namespace).Get(ctx, p.target.name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if object.GetUID() != p.target.uid || object.GetGeneration() != p.target.generation || !reflect.DeepEqual(object.Object["spec"], p.spec) {
			return false, fmt.Errorf("maintenance resource %s changed while suspended", p.target.name)
		}
		if !maintenanceSuspended(object, p) {
			return false, nil
		}
	}
	if s.clientset == nil {
		return false, fmt.Errorf("maintenance requires live Job and Pod inventories")
	}
	for _, ns := range workloadNamespaces(s.in.namespace, s.in.namespaces) {
		jobs, err := s.clientset.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		for i := range jobs.Items {
			job := &jobs.Items[i]
			_, _, done := jobEnd(job)
			if !done || job.Status.Active > 0 {
				return false, nil
			}
		}
		pods, err := s.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				return false, nil
			}
		}
	}
	return true, nil
}

func (s *scenarios) waitMaintenanceQuiet(ctx context.Context, paused []pausedResource) error {
	for {
		ready, err := s.maintenanceQuiet(ctx, paused)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		if err := waitCapacityPoll(ctx); err != nil {
			return fmt.Errorf("workload still has active operations, Jobs, Pods or unobserved suspension: %w", err)
		}
	}
}

func (s *scenarios) resumeMaintenance(ctx context.Context, paused []pausedResource) ([]batchTarget, error) {
	var targets []batchTarget
	var failures []error
	for _, p := range paused {
		target, err := s.resumeMaintenanceResource(ctx, p)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		targets = append(targets, target)
	}
	return targets, errors.Join(failures...)
}

func (s *scenarios) resumeMaintenanceResource(ctx context.Context, p pausedResource) (batchTarget, error) {
	client := s.dynamic.Resource(p.target.resource).Namespace(p.target.namespace)
	for {
		current, err := client.Get(ctx, p.target.name, metav1.GetOptions{})
		if err != nil {
			return batchTarget{}, err
		}
		expected := map[string]any{}
		for k, v := range p.spec {
			expected[k] = v
		}
		expected["suspend"] = false
		if current.GetUID() != p.target.uid || current.GetDeletionTimestamp() != nil {
			return batchTarget{}, fmt.Errorf("refusing to resume a replaced or deleting workload")
		}
		target := p.target
		// The suspension or resume response may have been lost. Only an exact
		// unchanged spec at its original or resumed generation is harmless.
		if (current.GetGeneration() == p.target.generation-1 || current.GetGeneration() == p.target.generation+1) && reflect.DeepEqual(current.Object["spec"], expected) {
			target.generation = current.GetGeneration()
			return target, nil
		}
		if current.GetGeneration() != p.target.generation || !reflect.DeepEqual(current.Object["spec"], p.spec) {
			return batchTarget{}, fmt.Errorf("refusing to overwrite an intervening spec while resuming")
		}
		raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": current.GetUID(), "resourceVersion": current.GetResourceVersion()}, "spec": map[string]any{"suspend": false}})
		updated, err := client.Patch(ctx, p.target.name, types.MergePatchType, raw, metav1.PatchOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return batchTarget{}, err
		}
		if updated.GetUID() != p.target.uid || updated.GetNamespace() != p.target.namespace || updated.GetName() != p.target.name || updated.GetGeneration() != p.target.generation+1 || !reflect.DeepEqual(updated.Object["spec"], expected) {
			return batchTarget{}, fmt.Errorf("resume changed resource identity or unrelated spec")
		}
		target.generation = updated.GetGeneration()
		return target, nil
	}
}

func (s *scenarios) resumeAndConverge(ctx context.Context, paused []pausedResource) error {
	start := time.Now().UTC()
	targets, err := s.resumeMaintenance(ctx, paused)
	if err != nil {
		return err
	}
	return s.waitBatch(ctx, targets, start)
}
