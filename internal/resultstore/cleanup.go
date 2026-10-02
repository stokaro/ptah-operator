package resultstore

import (
	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MemberOf validates immutable child metadata even after its intent is lost.
// This permits collecting an orphan under its retained retirement fence. The
// caller must verify that fence, recovery pins, retention age, and Job absence.
func MemberOf(recorded *api.PtahResultRecord, b Binding) bool {
	name, err := Name(b)
	got := cleanupView(recorded)
	if err != nil || got == nil || got.UID == "" || len(got.OwnerReferences) != 1 || got.OwnerReferences[0].UID == "" {
		return false
	}
	validName := got.Spec.Type == "complete" && got.Name == name+"-complete"
	if got.Spec.Type == "chunk" {
		for n := 0; n < maxChunks; n++ {
			validName = validName || got.Name == chunkName(name, n)
		}
	}
	return validName && recordShape(got, record(b.Namespace, got.Name, got.Spec.Type,
		owner(apiVersion, "PtahResultRecord", name, got.OwnerReferences[0].UID), nil))
}

func cleanupView(record *api.PtahResultRecord) *api.PtahResultRecord {
	if record == nil {
		return nil
	}
	copy := record.DeepCopy()
	if !copy.DeletionTimestamp.IsZero() && len(copy.Finalizers) == 1 && copy.Finalizers[0] == metav1.FinalizerDeleteDependents {
		copy.Finalizers = nil
	}
	copy.DeletionTimestamp = nil
	return copy
}
