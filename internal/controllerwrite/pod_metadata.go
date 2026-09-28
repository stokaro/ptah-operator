package controllerwrite

import (
	"fmt"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// validateClaimedMetadata holds one of a Job's metadata maps to the claim
// without rebuilding the Job: every key the claim fixes carries the claim's
// value, and every other key is one spec.execution.podMetadata may declare,
// in number and in namespace. What those declared keys were when the Job was
// dispatched is not re-derived from the spec, which may have changed since;
// the caller pins them through the template digest the claim's admission
// snapshot recorded.
func validateClaimedMetadata(actual, claimed map[string]string) error {
	for key, value := range claimed {
		if actual[key] != value {
			return fmt.Errorf("%s is not the claim's value", key)
		}
	}
	declared := 0
	for key := range actual {
		if _, fixed := claimed[key]; fixed {
			continue
		}
		if workload.ReservedPodMetadataKey(key) {
			return fmt.Errorf("%s is a reserved key the claim does not fix", key)
		}
		declared++
	}
	if declared > operatorv1alpha1.MaxPodMetadataEntries {
		return fmt.Errorf("%d declared keys exceed the %d spec.execution.podMetadata allows",
			declared, operatorv1alpha1.MaxPodMetadataEntries)
	}
	return nil
}

// carriesEnvelopeKeys reports whether annotations hold every one of keys and,
// beyond them, only keys spec.execution.podMetadata may declare. A reserved
// key outside the set means another shape: an Apply's plan binding on a Job
// judged as read-only, or a key no builder writes.
func carriesEnvelopeKeys(annotations map[string]string, keys []string) bool {
	for _, key := range keys {
		if _, found := annotations[key]; !found {
			return false
		}
	}
	expected := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		expected[key] = struct{}{}
	}
	for key := range annotations {
		if _, found := expected[key]; found {
			continue
		}
		if workload.ReservedPodMetadataKey(key) {
			return false
		}
	}
	return true
}
