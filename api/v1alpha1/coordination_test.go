package v1alpha1_test

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestStatusTargetBindingsCannotExposeCoordinationKey(t *testing.T) {
	t.Parallel()

	binding := operatorv1alpha1.DatabaseTargetBinding{
		Engine: operatorv1alpha1.DatabaseEnginePostgreSQL,
		URLFrom: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "database"},
			Key:                  "url",
		},
	}
	status := operatorv1alpha1.PtahSchemaStatus{
		ActiveOperation: &operatorv1alpha1.ActiveOperationStatus{Target: binding.DeepCopy()},
		PendingObservation: &operatorv1alpha1.PendingObservationStatus{
			Target: binding,
		},
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "coordinationKey") {
		t.Fatalf("status serialization exposed a coordination key field: %s", encoded)
	}
}

func TestPendingObservationDeepCopiesAdmissionSnapshot(t *testing.T) {
	t.Parallel()

	pending := &operatorv1alpha1.PendingObservationStatus{
		AdmissionSnapshot: &operatorv1alpha1.PodAdmissionSnapshot{
			ServiceAccount: operatorv1alpha1.ServiceAccountAdmissionSnapshot{
				ImagePullSecrets: []corev1.LocalObjectReference{{Name: "registry-auth"}},
			},
			LimitRanges: []operatorv1alpha1.LimitRangeAdmissionSnapshot{{
				DefaultRequests: map[corev1.ResourceName]resource.Quantity{
					corev1.ResourceCPU: resource.MustParse("100m"),
				},
			}},
		},
	}
	copy := pending.DeepCopy()
	if copy.AdmissionSnapshot == pending.AdmissionSnapshot {
		t.Fatal("DeepCopy() retained the pending admission snapshot pointer")
	}

	pending.AdmissionSnapshot.ServiceAccount.ImagePullSecrets[0].Name = "replacement"
	requests := pending.AdmissionSnapshot.LimitRanges[0].DefaultRequests
	requests[corev1.ResourceCPU] = resource.MustParse("900m")
	if got := copy.AdmissionSnapshot.ServiceAccount.ImagePullSecrets[0].Name; got != "registry-auth" {
		t.Fatalf("DeepCopy() image pull Secret = %q, want registry-auth", got)
	}
	if got := copy.AdmissionSnapshot.LimitRanges[0].DefaultRequests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("100m")) != 0 {
		t.Fatalf("DeepCopy() CPU request = %s, want 100m", got.String())
	}
}
