package v1alpha1_test

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

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
