//go:build e2e

package e2e

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// readingMetadata is what a printed reading keeps of an object's metadata: its
// identity and generation, which the predicates compare, and none of what the
// API server keeps for its own bookkeeping.
func readingMetadata(object metav1.Object) map[string]any {
	return map[string]any{
		"name": object.GetName(), "namespace": object.GetNamespace(),
		"uid": object.GetUID(), "generation": object.GetGeneration(),
	}
}

// readingEvents is the Warning Events of one reason that one object owns, as a
// printed reading keeps them.
func readingEvents(events []corev1.Event, owner types.UID, reason string) []corev1.Event {
	var kept []corev1.Event
	for index := range events {
		event := events[index]
		if event.InvolvedObject.UID != owner || event.Reason != reason || event.Type != corev1.EventTypeWarning {
			continue
		}
		event.ManagedFields = nil
		kept = append(kept, event)
	}
	return kept
}
