package crdupgrade

// These are intentionally white-box tests because the marker shape and its
// sealed inventory are private parts of predecessor retirement.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestRenderedAdmissionConvergenceMarkerMatchesCompiledContract(t *testing.T) {
	path := os.Getenv("PTAH_ROLLOUT_GUARD_RENDER")
	if path == "" {
		t.Skip("PTAH_ROLLOUT_GUARD_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rollout, _, _, _ := readyRolloutGuard()
	rollout.ReleaseName = "ptah-e2e"
	rollout.ReleaseNamespace = "ptah-e2e"
	rollout.ManagerImage = renderedGuardManagerImage
	rollout.HookServiceAccountName = "ptah-e2e-ptah-operator-crd-v1-" + hookIdentityDigest(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)[:12]
	rollout.ControllerServiceAccountName = renderedDeploymentServiceAccount(t, rendered, "ptah-e2e-ptah-operator")
	rollout.ControllerDeploymentName = "ptah-e2e-ptah-operator"
	rollout.CertificateDeploymentName = "ptah-e2e-ptah-operator-cert-rotator"
	guard := NewAdmissionConvergenceGuard(rollout)
	markerName := AdmissionConvergenceMarkerName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence)
	var marker *corev1.ConfigMap
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
		if typeMeta.Kind != "ConfigMap" {
			continue
		}
		var object corev1.ConfigMap
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		if object.Name == markerName {
			marker = &object
		}
	}
	if marker == nil {
		t.Fatal("rendered admission convergence marker is missing")
	}
	marker.UID = "rendered-marker"
	marker.ResourceVersion = "1"
	if err := guard.verifyMarker(marker); err != nil {
		t.Fatalf("rendered admission convergence marker: %v", err)
	}
}

func admissionConvergencePolicyDifference(got, want admissionregistrationv1.ValidatingAdmissionPolicySpec) string {
	if !reflect.DeepEqual(got.ParamKind, want.ParamKind) {
		return fmt.Sprintf("paramKind got %#v, want %#v", got.ParamKind, want.ParamKind)
	}
	if !reflect.DeepEqual(got.MatchConstraints, want.MatchConstraints) {
		return fmt.Sprintf("matchConstraints got %#v, want %#v", got.MatchConstraints, want.MatchConstraints)
	}
	if !reflect.DeepEqual(got.FailurePolicy, want.FailurePolicy) {
		return fmt.Sprintf("failurePolicy got %#v, want %#v", got.FailurePolicy, want.FailurePolicy)
	}
	if !reflect.DeepEqual(got.MatchConditions, want.MatchConditions) {
		return fmt.Sprintf("matchConditions got %#v, want %#v", got.MatchConditions, want.MatchConditions)
	}
	if !reflect.DeepEqual(got.Variables, want.Variables) {
		return fmt.Sprintf("variables got %#v, want %#v", got.Variables, want.Variables)
	}
	if len(got.Validations) != len(want.Validations) {
		return fmt.Sprintf("validations length got %d, want %d", len(got.Validations), len(want.Validations))
	}
	for index := range got.Validations {
		if got.Validations[index] == want.Validations[index] {
			continue
		}
		position := firstStringDifference(got.Validations[index].Expression, want.Validations[index].Expression)
		return fmt.Sprintf(
			"validation %d differs at byte %d: got %q, want %q",
			index,
			position,
			stringDifferenceWindow(got.Validations[index].Expression, position),
			stringDifferenceWindow(want.Validations[index].Expression, position),
		)
	}
	return "unclassified difference"
}

func firstStringDifference(left, right string) int {
	limit := min(len(left), len(right))
	for index := range limit {
		if left[index] != right[index] {
			return index
		}
	}
	return limit
}

func stringDifferenceWindow(value string, position int) string {
	start := max(0, position-80)
	end := min(len(value), position+80)
	return value[start:end]
}

func TestAdmissionConvergenceMarkerTargetUsesExactCurrentContract(t *testing.T) {
	t.Parallel()

	rollout, _, _, _ := readyRolloutGuard()
	guard := NewAdmissionConvergenceGuard(rollout)
	markerName := AdmissionConvergenceMarkerName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence)
	marker := guard.unsealedMarker()
	marker.UID = types.UID("marker-uid")
	marker.ResourceVersion = "marker-rv"
	sealAdmissionConvergenceMarkerForTest(t, guard, marker)
	target, err := guard.MarkerTarget()
	if err != nil {
		t.Fatal(err)
	}
	if target.Name != markerName || target.Verify == nil {
		t.Fatalf("MarkerTarget() = %#v, want exact current marker %q", target, markerName)
	}
	if err := target.Verify(marker.DeepCopy()); err != nil {
		t.Fatalf("verify exact current marker: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*corev1.ConfigMap)
	}{
		{
			name: "foreign attempt",
			mutate: func(object *corev1.ConfigMap) {
				object.Data[admissionConvergenceAttemptDataKey] = strings.Repeat("f", 64)
			},
		},
		{
			name: "foreign lifecycle owner",
			mutate: func(object *corev1.ConfigMap) {
				object.OwnerReferences = []metav1.OwnerReference{{Name: "foreign"}}
			},
		},
		{
			name: "missing deletion precondition identity",
			mutate: func(object *corev1.ConfigMap) {
				object.UID = ""
			},
		},
		{
			name: "unsealed",
			mutate: func(object *corev1.ConfigMap) {
				object.Immutable = nil
				delete(object.Data, PredecessorRetirementInventoryDataKey)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			foreign := marker.DeepCopy()
			test.mutate(foreign)
			if err := target.Verify(foreign); err == nil {
				t.Fatalf("marker verifier accepted foreign object: %#v", foreign)
			}
		})
	}
}

func TestAdmissionConvergenceMarkerTargetRefusesAnIncompleteIdentity(t *testing.T) {
	t.Parallel()

	rollout, _, _, _ := readyRolloutGuard()
	for _, test := range []struct {
		name   string
		mutate func(*AdmissionConvergenceGuard)
	}{
		{name: "nil", mutate: nil},
		{name: "empty manager image", mutate: func(guard *AdmissionConvergenceGuard) { guard.ManagerImage = "" }},
		{name: "foreign cleanup identity", mutate: func(guard *AdmissionConvergenceGuard) { guard.CleanupServiceAccountName = "foreign" }},
		{name: "shared ServiceAccount", mutate: func(guard *AdmissionConvergenceGuard) {
			guard.CertificateServiceAccountName = guard.ControllerServiceAccountName
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var guard *AdmissionConvergenceGuard
			if test.mutate != nil {
				guard = NewAdmissionConvergenceGuard(rollout)
				test.mutate(guard)
			}
			if _, err := guard.MarkerTarget(); err == nil {
				t.Fatal("MarkerTarget() accepted an incomplete identity")
			}
		})
	}
}

func sealAdmissionConvergenceMarkerForTest(t *testing.T, guard *AdmissionConvergenceGuard, marker *corev1.ConfigMap) {
	t.Helper()
	if guard == nil || marker == nil {
		t.Fatal("admission convergence guard and marker are required")
	}
	entries := predecessorRetirementExpectedEntries(
		guard.ReleaseNamespace,
		guard.ReleaseName,
		guard.ReleaseSequence,
		guard.ManagerImage,
	)
	for index := range entries {
		entries[index].UID = types.UID(fmt.Sprintf("test-inventory-%d", index))
		entries[index].Digest = strings.Repeat("a", 64)
	}
	encoded, err := encodePredecessorRetirementInventory(predecessorRetirementInventory{
		Version: PredecessorRetirementInventoryVersion,
		Entries: entries,
	})
	if err != nil {
		t.Fatalf("encode sealed test inventory: %v", err)
	}
	marker.Data = cloneStringMap(marker.Data)
	marker.Data[PredecessorRetirementInventoryDataKey] = encoded
	immutable := true
	marker.Immutable = &immutable
}
