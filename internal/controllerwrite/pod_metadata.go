package controllerwrite

import (
	"github.com/stokaro/ptah-operator/internal/workload"
)

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
