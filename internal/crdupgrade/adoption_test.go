package crdupgrade

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAdmissionAdopterStampsExactLegacySingleton(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
	mutating.object.Annotations["owner.example.test/preserved"] = "true"
	if err := adopter.Adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	for kind, annotations := range map[string]map[string]string{
		"mutating":   mutating.object.Annotations,
		"validating": validating.object.Annotations,
	} {
		for key, want := range adopter.Expected.annotations() {
			if got := annotations[key]; got != want {
				t.Fatalf("%s annotation %s = %q, want %q", kind, key, got, want)
			}
		}
	}
	if mutating.object.Annotations["owner.example.test/preserved"] != "true" {
		t.Fatal("adoption removed a foreign annotation")
	}
	if mutating.dryRunUpdates != 1 || validating.dryRunUpdates != 1 ||
		mutating.realUpdates != 1 || validating.realUpdates != 1 {
		t.Fatalf("updates mutating=%d/%d validating=%d/%d, want dry-run/real 1/1 each",
			mutating.dryRunUpdates, mutating.realUpdates, validating.dryRunUpdates, validating.realUpdates)
	}
}

func TestAdmissionAdopterPreflightDryRunsLegacyTupleWithoutPersisting(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
	mutatingBefore := mutating.object.DeepCopy()
	validatingBefore := validating.object.DeepCopy()
	if err := adopter.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mutating.dryRunUpdates != 1 || validating.dryRunUpdates != 1 ||
		mutating.realUpdates != 0 || validating.realUpdates != 0 {
		t.Fatalf("preflight updates mutating=%d/%d validating=%d/%d, want dry-run/real 1/0 each",
			mutating.dryRunUpdates, mutating.realUpdates, validating.dryRunUpdates, validating.realUpdates)
	}
	if !reflect.DeepEqual(mutating.object, mutatingBefore) || !reflect.DeepEqual(validating.object, validatingBefore) {
		t.Fatal("admission preflight persisted a legacy adoption")
	}
}

func TestAdmissionAdopterResumesPartialExactAdoption(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, false, true)
	if err := adopter.Adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mutating.dryRunUpdates != 0 || mutating.realUpdates != 0 ||
		validating.dryRunUpdates != 1 || validating.realUpdates != 1 {
		t.Fatalf("partial adoption updates mutating=%d/%d validating=%d/%d",
			mutating.dryRunUpdates, mutating.realUpdates, validating.dryRunUpdates, validating.realUpdates)
	}
}

func TestAdmissionAdopterAllowsOwnedContractToChangeAfterPreflight(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, false, false)
	adopter.Expected.WebhookTimeoutSeconds = 7
	mutating.object.Webhooks[0].ClientConfig.Service.Name = "old-service"
	validating.object.Webhooks[0].TimeoutSeconds = valuePointer(int32(4))
	if err := adopter.Adopt(context.Background()); err != nil {
		t.Fatalf("owned previous contract was rejected before Helm could update it: %v", err)
	}
	if mutating.dryRunUpdates != 0 || mutating.realUpdates != 0 ||
		validating.dryRunUpdates != 0 || validating.realUpdates != 0 {
		t.Fatal("unchanged owned versions unexpectedly mutated admission objects")
	}
}

func TestAdmissionAdopterLeavesOlderOwnedVersionForAtomicHelmUpdate(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, false, false)
	adopter.Expected.ControllerStateVersion = 2
	adopter.Expected.AdmissionContractVersion = 2
	if err := adopter.Adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	for kind, annotations := range map[string]map[string]string{
		"mutating":   mutating.object.Annotations,
		"validating": validating.object.Annotations,
	} {
		if annotations[ControllerStateVersionAnnotation] != "1" ||
			annotations[AdmissionContractVersionAnnotation] != "1" {
			t.Fatalf("%s version ratchets changed before the Helm contract update: %v", kind, annotations)
		}
	}
	if mutating.dryRunUpdates != 0 || mutating.realUpdates != 0 ||
		validating.dryRunUpdates != 0 || validating.realUpdates != 0 {
		t.Fatal("owned version transition mutated annotations before Helm")
	}
}

func TestAdmissionAdopterRecognizesLegacyContractWithPreviousTimeout(t *testing.T) {
	adopter, _, _ := readyAdmissionAdopter(t, true, true)
	adopter.Expected.WebhookTimeoutSeconds = 7
	if err := adopter.Adopt(context.Background()); err != nil {
		t.Fatalf("exact legacy contract with an older timeout was rejected: %v", err)
	}
}

func TestAdmissionAdopterAllowsBothObjectsAbsentOnInstall(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, false, false)
	mutating.object = nil
	validating.object = nil
	if err := adopter.Adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionAdopterRejectsIncompleteSingleton(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
	validating.object = nil
	err := adopter.Adopt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("Adopt error = %v, want incomplete singleton refusal", err)
	}
	if mutating.dryRunUpdates != 0 || mutating.realUpdates != 0 {
		t.Fatal("incomplete singleton was mutated")
	}
}

func TestAdmissionAdopterRejectsUnknownLegacyContractBeforeMutation(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
	mutating.object.Webhooks[0].ClientConfig.Service.Name = "foreign-service"
	err := adopter.Adopt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Service target does not match") {
		t.Fatalf("Adopt error = %v, want exact contract refusal", err)
	}
	if mutating.dryRunUpdates != 0 || validating.dryRunUpdates != 0 ||
		mutating.realUpdates != 0 || validating.realUpdates != 0 {
		t.Fatal("unknown legacy contract was mutated")
	}
}

func TestAdmissionAdopterRejectsForeignOrPartialOwnership(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AdmissionAdopter, *mutatingAdmissionClient)
		want   string
	}{
		{
			name: "foreign Helm release",
			mutate: func(_ *AdmissionAdopter, mutating *mutatingAdmissionClient) {
				mutating.object.Annotations[helmReleaseNameAnnotation] = "foreign"
			},
			want: "is not owned by Helm release",
		},
		{
			name: "partial owned tuple",
			mutate: func(adopter *AdmissionAdopter, mutating *mutatingAdmissionClient) {
				mutating.object.Annotations[ReleaseNameAnnotation] = adopter.Expected.ReleaseName
			},
			want: "incomplete owned annotation tuple",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
			test.mutate(adopter, mutating)
			err := adopter.Adopt(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Adopt error = %v, want %q", err, test.want)
			}
			if mutating.dryRunUpdates != 0 || validating.dryRunUpdates != 0 ||
				mutating.realUpdates != 0 || validating.realUpdates != 0 {
				t.Fatal("invalid singleton ownership was mutated")
			}
		})
	}
}

func TestAdmissionAdopterPreflightsBothObjectsBeforeMutation(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
	validating.dryRunError = errors.New("policy denied")
	err := adopter.Adopt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dry-run legacy ValidatingWebhookConfiguration adoption") {
		t.Fatalf("Adopt error = %v, want dry-run refusal", err)
	}
	if mutating.dryRunUpdates != 1 || validating.dryRunUpdates != 1 ||
		mutating.realUpdates != 0 || validating.realUpdates != 0 {
		t.Fatalf("failed preflight updates mutating=%d/%d validating=%d/%d",
			mutating.dryRunUpdates, mutating.realUpdates, validating.dryRunUpdates, validating.realUpdates)
	}
}

func readyAdmissionAdopter(t *testing.T, mutatingLegacy, validatingLegacy bool) (*AdmissionAdopter, *mutatingAdmissionClient, *validatingAdmissionClient) {
	t.Helper()
	verifier := readyRuntimeVerifier(t)
	mutating := verifier.Mutating.(*mutatingAdmissionClient)
	validating := verifier.Validating.(*validatingAdmissionClient)
	setHelmOwnership(&mutating.object.ObjectMeta, verifier.Expected)
	setHelmOwnership(&validating.object.ObjectMeta, verifier.Expected)
	if mutatingLegacy {
		removeOwnedAnnotations(&mutating.object.ObjectMeta, verifier.Expected)
	}
	if validatingLegacy {
		removeOwnedAnnotations(&validating.object.ObjectMeta, verifier.Expected)
	}
	return &AdmissionAdopter{Mutating: mutating, Validating: validating, Expected: verifier.Expected}, mutating, validating
}

func setHelmOwnership(metadata *metav1.ObjectMeta, expected RuntimeInvariants) {
	metadata.Annotations[helmReleaseNameAnnotation] = expected.ReleaseName
	metadata.Annotations[helmReleaseNamespaceAnnotation] = expected.ReleaseNamespace
	metadata.Labels = map[string]string{
		managedByLabel: "Helm",
		instanceLabel:  expected.ReleaseName,
	}
}

func removeOwnedAnnotations(metadata *metav1.ObjectMeta, expected RuntimeInvariants) {
	for key := range expected.annotations() {
		delete(metadata.Annotations, key)
	}
}
