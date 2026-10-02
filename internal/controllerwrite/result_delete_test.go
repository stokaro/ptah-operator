package controllerwrite

import (
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestCredentialDeletePropagation(t *testing.T) {
	for _, test := range []struct {
		name, options string
		allow         bool
	}{
		{name: "default", allow: true},
		{name: "foreground", options: `{"propagationPolicy":"Foreground"}`, allow: true},
		{name: "background", options: `{"propagationPolicy":"Background"}`, allow: true},
		{name: "legacy cascade", options: `{"orphanDependents":false}`, allow: true},
		{name: "orphan", options: `{"propagationPolicy":"Orphan"}`},
		{name: "legacy orphan", options: `{"orphanDependents":true}`},
		{name: "malformed", options: `{"propagationPolicy":`},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateCredentialDeleteOptions(admissionv1.AdmissionRequest{Options: runtime.RawExtension{Raw: []byte(test.options)}})
			if (err == nil) != test.allow {
				t.Fatalf("allow=%v: %v", test.allow, err)
			}
		})
	}
}
