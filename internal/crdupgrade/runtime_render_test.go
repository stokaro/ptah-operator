package crdupgrade

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestRenderedAdmissionSingletonMatchesRuntimeContract(t *testing.T) {
	path := os.Getenv("PTAH_ADMISSION_RENDER")
	if path == "" {
		t.Skip("PTAH_ADMISSION_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var mutating *admissionregistrationv1.MutatingWebhookConfiguration
	var validating *admissionregistrationv1.ValidatingWebhookConfiguration
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(rendered))
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var typeMeta metav1.TypeMeta
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			t.Fatal(err)
		}
		switch typeMeta.Kind {
		case "MutatingWebhookConfiguration":
			if mutating != nil {
				t.Fatal("rendered admission singleton contains multiple MutatingWebhookConfigurations")
			}
			mutating = &admissionregistrationv1.MutatingWebhookConfiguration{}
			if err := json.Unmarshal(raw, mutating); err != nil {
				t.Fatal(err)
			}
		case "ValidatingWebhookConfiguration":
			if validating != nil {
				t.Fatal("rendered admission singleton contains multiple ValidatingWebhookConfigurations")
			}
			validating = &admissionregistrationv1.ValidatingWebhookConfiguration{}
			if err := json.Unmarshal(raw, validating); err != nil {
				t.Fatal(err)
			}
		}
	}
	if mutating == nil || validating == nil {
		t.Fatalf("rendered admission singleton is incomplete: mutating=%t validating=%t", mutating != nil, validating != nil)
	}
	if mutating.Name != AdmissionConfigurationName || validating.Name != AdmissionConfigurationName {
		t.Fatalf("rendered admission singleton names = %q and %q, want %q", mutating.Name, validating.Name, AdmissionConfigurationName)
	}

	expected := runtimeInvariantsFromRenderedAnnotations(t, validating.Annotations)
	if err := expected.validate(); err != nil {
		t.Fatalf("rendered runtime invariants are invalid: %v", err)
	}
	if err := verifyMutatingWebhookContract(mutating, expected); err != nil {
		t.Fatalf("rendered mutating admission contract differs from runtime verification: %v", err)
	}
	if err := verifyValidatingWebhookContract(validating, expected); err != nil {
		t.Fatalf("rendered validating admission contract differs from runtime verification: %v", err)
	}
}

func runtimeInvariantsFromRenderedAnnotations(t *testing.T, annotations map[string]string) RuntimeInvariants {
	t.Helper()
	required := func(key string) string {
		value := annotations[key]
		if value == "" {
			t.Fatalf("rendered admission singleton lacks nonempty annotation %s", key)
		}
		return value
	}
	parseBool := func(key string) bool {
		value, err := strconv.ParseBool(required(key))
		if err != nil {
			t.Fatalf("rendered admission annotation %s is not a boolean: %v", key, err)
		}
		return value
	}
	parseInt32 := func(key string) int32 {
		value, err := strconv.ParseInt(required(key), 10, 32)
		if err != nil {
			t.Fatalf("rendered admission annotation %s is not an int32: %v", key, err)
		}
		return int32(value)
	}

	return RuntimeInvariants{
		ReleaseName:                  required(ReleaseNameAnnotation),
		ReleaseNamespace:             required(ReleaseNamespaceAnnotation),
		CoordinationNamespace:        required(CoordinationAnnotation),
		LeaderElection:               parseBool(LeaderElectionAnnotation),
		LeaderElectionID:             required(LeaderElectionIDAnnotation),
		WebhookServiceName:           required(WebhookServiceAnnotation),
		WebhookTimeoutSeconds:        5,
		HookServiceAccountName:       required(HookServiceAccountAnnotation),
		ControllerServiceAccountName: required(ControllerServiceAccountAnnotation),
		ControllerDeploymentName:     required(ControllerDeploymentAnnotation),
		CertificateDeploymentName:    required(CertificateDeploymentAnnotation),
		ControllerStateVersion:       parseInt32(ControllerStateVersionAnnotation),
		AdmissionContractVersion:     parseInt32(AdmissionContractVersionAnnotation),
		ReleaseSequence:              parseInt32(ReleaseSequenceAnnotation),
	}
}
