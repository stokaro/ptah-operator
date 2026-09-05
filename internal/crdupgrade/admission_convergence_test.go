package crdupgrade

// These are intentionally white-box tests because the exact denial envelope,
// metadata shape, and marker-retirement preconditions are private parts of one
// fail-closed admission protocol.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestRenderedAdmissionConvergenceSentinelMatchesCompiledContract(t *testing.T) {
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
	guard.CertificateServiceAccountName = renderedDeploymentServiceAccount(t, rendered, rollout.CertificateDeploymentName)
	name := AdmissionConvergencePolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	markerName := AdmissionConvergenceMarkerName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence)
	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	var binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
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
		switch typeMeta.Kind {
		case "ValidatingAdmissionPolicy":
			var object admissionregistrationv1.ValidatingAdmissionPolicy
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if object.Name == name {
				policy = &object
			}
		case "ValidatingAdmissionPolicyBinding":
			var object admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if object.Name == name {
				binding = &object
			}
		case "ConfigMap":
			var object corev1.ConfigMap
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if object.Name == markerName {
				marker = &object
			}
		}
	}
	if err := guard.verifyPolicy(policy); err != nil {
		t.Fatalf("rendered admission convergence policy: %v: %s", err, admissionConvergencePolicyDifference(policy.Spec, guard.policy().Spec))
	}
	if err := guard.verifyBinding(binding); err != nil {
		t.Fatalf("rendered admission convergence binding: %v", err)
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

// This white-box test evaluates the sentinel without installing the earlier
// ServiceAccountOriginGuard. That models one API server whose independent
// guard cache is stale or missing and proves the directly fenced sentinel is
// itself sufficient for the credential-draining safety boundary.
func TestAdmissionConvergenceSentinelIndependentlyFencesControllerCredentials(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionConvergenceFixture(t)
	guard := fixture.guard
	guard.PreviousControllerServiceAccountName = "previous-controller"
	policy := guard.policy()
	if policy.Spec.MatchConstraints == nil || len(policy.Spec.MatchConstraints.ResourceRules) != 1 {
		t.Fatal("admission convergence sentinel lacks its all-resource credential-fence rule")
	}
	rule := policy.Spec.MatchConstraints.ResourceRules[0].RuleWithOperations
	if !reflect.DeepEqual(rule.Operations, []admissionregistrationv1.OperationType{
		admissionregistrationv1.Create,
		admissionregistrationv1.Update,
		admissionregistrationv1.Delete,
		admissionregistrationv1.Connect,
	}) || !reflect.DeepEqual(rule.APIGroups, []string{"*"}) ||
		!reflect.DeepEqual(rule.APIVersions, []string{"*"}) ||
		!reflect.DeepEqual(rule.Resources, []string{"*/*"}) ||
		rule.Scope == nil || *rule.Scope != admissionregistrationv1.AllScopes {
		t.Fatalf("admission convergence credential-fence rule = %#v", rule)
	}
	if fixture.guard.binding().Spec.MatchResources != nil {
		t.Fatal("admission convergence binding narrows the policy away from controller credential requests")
	}

	activation := func(active int64) map[string]any {
		return rolloutActivationCELObject(
			&RolloutGuard{
				ReleaseName:              guard.ReleaseName,
				ReleaseNamespace:         guard.ReleaseNamespace,
				ControllerStateVersion:   guard.ControllerStateVersion,
				AdmissionContractVersion: guard.AdmissionContractVersion,
				ReleaseSequence:          guard.ReleaseSequence,
			},
			active,
			int64(guard.ControllerStateVersion),
			int64(guard.AdmissionContractVersion),
			int64(guard.ReleaseSequence),
			guard.ManagerImage,
		)
	}
	drainingParams := activation(0)
	drainingParams["data"] = map[string]any{
		activeReleaseDataKey:               "0",
		controllerCredentialsDataKey:       string(ControllerCredentialsDraining),
		controllerCredentialsTargetDataKey: strconv.FormatInt(int64(guard.ReleaseSequence), 10),
		controllerCredentialsAttemptDataKey: hookIdentityDigest(
			guard.ReleaseNamespace,
			guard.ReleaseName,
			guard.ReleaseSequence,
			guard.ManagerImage,
		),
	}
	activeCandidateParams := activation(int64(guard.ReleaseSequence))
	activePredecessorParams := activation(int64(guard.PreviousControllerReleaseSequence))
	validPodName := guard.ControllerDeploymentName + "-abcdef1234-abcde"
	validCertificatePodName := guard.CertificateServiceAccountName + "-abcdef1234-abcde"
	validHookPodName := guard.HookServiceAccountName + "-abcde"
	validCleanupPodName := guard.CleanupServiceAccountName + "-abcde"
	controllerRequest := func(serviceAccount, podName string, includeExtras bool) map[string]any {
		userInfo := map[string]any{"username": guard.serviceAccountUsername(serviceAccount)}
		if includeExtras {
			userInfo["extra"] = map[string]any{
				serviceAccountPodNameExtra: []any{podName},
				serviceAccountPodUIDExtra:  []any{"pod-uid"},
			}
		}
		return map[string]any{
			"operation": "UPDATE",
			"namespace": guard.ReleaseNamespace,
			"name":      "unrelated",
			"dryRun":    false,
			"resource":  map[string]any{"group": "apps", "version": "v1", "resource": "deployments"},
			"userInfo":  userInfo,
		}
	}
	tokenRequest := func(serviceAccount, username, podName string, bound bool) (map[string]any, map[string]any) {
		object := map[string]any{"spec": map[string]any{}}
		if bound {
			object["spec"] = map[string]any{"boundObjectRef": map[string]any{
				"apiVersion": "v1", "kind": "Pod", "name": podName, "uid": "pod-uid",
			}}
		}
		return object, map[string]any{
			"operation":   "CREATE",
			"namespace":   guard.ReleaseNamespace,
			"name":        serviceAccount,
			"subResource": "token",
			"dryRun":      false,
			"resource":    map[string]any{"group": "", "version": "v1", "resource": "serviceaccounts"},
			"userInfo":    map[string]any{"username": username, "groups": []any{"system:nodes"}},
		}
	}
	candidateTokenObject, candidateTokenRequest := tokenRequest(guard.ControllerServiceAccountName, "system:node:worker", validPodName, true)
	previousTokenObject, previousTokenRequest := tokenRequest(guard.PreviousControllerServiceAccountName, "system:node:worker", validPodName, true)
	unboundTokenObject, unboundTokenRequest := tokenRequest(guard.ControllerServiceAccountName, "system:node:worker", validPodName, false)
	nonNodeTokenObject, nonNodeTokenRequest := tokenRequest(guard.ControllerServiceAccountName, "system:serviceaccount:other:issuer", validPodName, true)
	certificateTokenObject, certificateTokenRequest := tokenRequest(guard.CertificateServiceAccountName, "system:node:worker", validCertificatePodName, true)
	unboundCertificateTokenObject, unboundCertificateTokenRequest := tokenRequest(guard.CertificateServiceAccountName, "system:node:worker", validCertificatePodName, false)
	nonNodeCertificateTokenObject, nonNodeCertificateTokenRequest := tokenRequest(guard.CertificateServiceAccountName, "system:serviceaccount:other:issuer", validCertificatePodName, true)
	hookTokenObject, hookTokenRequest := tokenRequest(guard.HookServiceAccountName, "system:node:worker", validHookPodName, true)
	unboundHookTokenObject, unboundHookTokenRequest := tokenRequest(guard.HookServiceAccountName, "system:node:worker", validHookPodName, false)
	cleanupTokenObject, cleanupTokenRequest := tokenRequest(guard.CleanupServiceAccountName, "system:node:worker", validCleanupPodName, true)
	unboundCleanupTokenObject, unboundCleanupTokenRequest := tokenRequest(guard.CleanupServiceAccountName, "system:node:worker", validCleanupPodName, false)

	tests := []struct {
		name          string
		object        map[string]any
		request       map[string]any
		params        map[string]any
		wantMatch     bool
		wantDenial    string
		wantDenyCount int
	}{
		{
			name: "draining denies exact candidate controller workload",
			request: controllerRequest(
				guard.ControllerServiceAccountName,
				validPodName,
				true,
			),
			params: drainingParams, wantMatch: true,
			wantDenial: controllerPrincipalGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name:   "draining denies exact candidate bound TokenRequest",
			object: candidateTokenObject, request: candidateTokenRequest,
			params: drainingParams, wantMatch: true,
			wantDenial: controllerPrincipalGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name: "draining denies exact predecessor controller workload",
			request: controllerRequest(
				guard.PreviousControllerServiceAccountName,
				validPodName,
				true,
			),
			params: drainingParams, wantMatch: true,
			wantDenial: controllerPrincipalGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name:   "draining denies exact predecessor bound TokenRequest",
			object: previousTokenObject, request: previousTokenRequest,
			params: drainingParams, wantMatch: true,
			wantDenial: controllerPrincipalGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name: "active candidate exact controller workload is admitted",
			request: controllerRequest(
				guard.ControllerServiceAccountName,
				validPodName,
				true,
			),
			params: activeCandidateParams, wantMatch: true,
		},
		{
			name: "active predecessor exact controller workload is admitted",
			request: controllerRequest(
				guard.PreviousControllerServiceAccountName,
				validPodName,
				true,
			),
			params: activePredecessorParams, wantMatch: true,
		},
		{
			name: "active controller workload without authenticator extras is denied",
			request: controllerRequest(
				guard.ControllerServiceAccountName,
				validPodName,
				false,
			),
			params: activeCandidateParams, wantMatch: true,
			wantDenial: serviceAccountOriginGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name: "active controller workload with foreign Pod name is denied",
			request: controllerRequest(
				guard.ControllerServiceAccountName,
				"foreign-abcdef1234-abcde",
				true,
			),
			params: activeCandidateParams, wantMatch: true,
			wantDenial: serviceAccountOriginGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name:   "active exact bound controller TokenRequest is admitted",
			object: candidateTokenObject, request: candidateTokenRequest,
			params: activeCandidateParams, wantMatch: true,
		},
		{
			name:   "active unbound controller TokenRequest is denied",
			object: unboundTokenObject, request: unboundTokenRequest,
			params: activeCandidateParams, wantMatch: true,
			wantDenial: serviceAccountOriginGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name:   "active non-node controller TokenRequest is denied",
			object: nonNodeTokenObject, request: nonNodeTokenRequest,
			params: activeCandidateParams, wantMatch: true,
			wantDenial: serviceAccountOriginGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name: "active exact certificate workload is admitted",
			request: controllerRequest(
				guard.CertificateServiceAccountName,
				validCertificatePodName,
				true,
			),
			params: activeCandidateParams, wantMatch: true,
		},
		{
			name: "draining exact certificate workload is admitted",
			request: controllerRequest(
				guard.CertificateServiceAccountName,
				validCertificatePodName,
				true,
			),
			params: drainingParams, wantMatch: true,
		},
		{
			name: "active certificate workload without authenticator extras is denied",
			request: controllerRequest(
				guard.CertificateServiceAccountName,
				validCertificatePodName,
				false,
			),
			params: activeCandidateParams, wantMatch: true,
			wantDenial: serviceAccountOriginGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name: "draining certificate workload without authenticator extras is denied",
			request: controllerRequest(
				guard.CertificateServiceAccountName,
				validCertificatePodName,
				false,
			),
			params: drainingParams, wantMatch: true,
			wantDenial: serviceAccountOriginGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name:   "active exact bound certificate TokenRequest is admitted",
			object: certificateTokenObject, request: certificateTokenRequest,
			params: activeCandidateParams, wantMatch: true,
		},
		{
			name:   "draining exact bound certificate TokenRequest is admitted",
			object: certificateTokenObject, request: certificateTokenRequest,
			params: drainingParams, wantMatch: true,
		},
		{
			name:   "active unbound certificate TokenRequest is denied",
			object: unboundCertificateTokenObject, request: unboundCertificateTokenRequest,
			params: activeCandidateParams, wantMatch: true,
			wantDenial: serviceAccountOriginGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name:   "draining unbound certificate TokenRequest is denied",
			object: unboundCertificateTokenObject, request: unboundCertificateTokenRequest,
			params: drainingParams, wantMatch: true,
			wantDenial: serviceAccountOriginGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name:   "active non-node certificate TokenRequest is denied",
			object: nonNodeCertificateTokenObject, request: nonNodeCertificateTokenRequest,
			params: activeCandidateParams, wantMatch: true,
			wantDenial: serviceAccountOriginGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name:   "draining non-node certificate TokenRequest is denied",
			object: nonNodeCertificateTokenObject, request: nonNodeCertificateTokenRequest,
			params: drainingParams, wantMatch: true,
			wantDenial: serviceAccountOriginGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name: "exact hook workload is admitted",
			request: controllerRequest(
				guard.HookServiceAccountName,
				validHookPodName,
				true,
			),
			params: drainingParams, wantMatch: true,
		},
		{
			name: "hook workload without authenticator extras is denied",
			request: controllerRequest(
				guard.HookServiceAccountName,
				validHookPodName,
				false,
			),
			params: drainingParams, wantMatch: true,
			wantDenial: serviceAccountOriginGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name:   "exact bound hook TokenRequest is admitted",
			object: hookTokenObject, request: hookTokenRequest,
			params: drainingParams, wantMatch: true,
		},
		{
			name:   "unbound hook TokenRequest is denied",
			object: unboundHookTokenObject, request: unboundHookTokenRequest,
			params: drainingParams, wantMatch: true,
			wantDenial: serviceAccountOriginGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name: "exact cleanup workload is admitted",
			request: controllerRequest(
				guard.CleanupServiceAccountName,
				validCleanupPodName,
				true,
			),
			params: drainingParams, wantMatch: true,
		},
		{
			name: "cleanup workload without authenticator extras is denied",
			request: controllerRequest(
				guard.CleanupServiceAccountName,
				validCleanupPodName,
				false,
			),
			params: drainingParams, wantMatch: true,
			wantDenial: serviceAccountOriginGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name:   "exact bound cleanup TokenRequest is admitted",
			object: cleanupTokenObject, request: cleanupTokenRequest,
			params: drainingParams, wantMatch: true,
		},
		{
			name:   "unbound cleanup TokenRequest is denied",
			object: unboundCleanupTokenObject, request: unboundCleanupTokenRequest,
			params: drainingParams, wantMatch: true,
			wantDenial: serviceAccountOriginGuardDenialMessage(), wantDenyCount: 1,
		},
		{
			name: "unrelated identity stays outside the sentinel",
			request: map[string]any{
				"operation": "UPDATE",
				"namespace": guard.ReleaseNamespace,
				"name":      "unrelated",
				"dryRun":    false,
				"resource":  map[string]any{"group": "apps", "version": "v1", "resource": "deployments"},
				"userInfo":  map[string]any{"username": "system:serviceaccount:other:other"},
			},
			params: activeCandidateParams,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := test.object
			if object == nil {
				object = map[string]any{}
			}
			if got := evaluatePolicyMatchConditions(t, policy, object, object, test.request, test.params); got != test.wantMatch {
				t.Fatalf("sentinel match = %t, want %t", got, test.wantMatch)
			}
			if !test.wantMatch {
				return
			}
			results := evaluatePolicyValidations(t, policy, object, object, test.request, test.params)
			denied := 0
			for index, allowed := range results {
				if allowed {
					continue
				}
				denied++
				if got := policy.Spec.Validations[index].Message; got != test.wantDenial {
					t.Fatalf("denial %d message = %q, want %q", index, got, test.wantDenial)
				}
			}
			if denied != test.wantDenyCount {
				t.Fatalf("sentinel denial count = %d, want %d; results=%v", denied, test.wantDenyCount, results)
			}
		})
	}
}

func TestAdmissionConvergenceMarkerProofRemainsOneExactCause(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionConvergenceFixture(t)
	guard := fixture.guard
	policy := guard.policy()
	marker := guard.marker()
	marker.UID = "marker-uid"
	marker.ResourceVersion = "marker-rv"
	object, ok := rolloutCELClone(t, marker).(map[string]any)
	if !ok {
		t.Fatal("marker JSON did not decode to an object")
	}
	oldObject, ok := rolloutCELClone(t, marker).(map[string]any)
	if !ok {
		t.Fatal("old marker JSON did not decode to an object")
	}
	params := rolloutActivationCELObject(
		&RolloutGuard{
			ReleaseName:              guard.ReleaseName,
			ReleaseNamespace:         guard.ReleaseNamespace,
			ControllerStateVersion:   guard.ControllerStateVersion,
			AdmissionContractVersion: guard.AdmissionContractVersion,
			ReleaseSequence:          guard.ReleaseSequence,
		},
		0,
		int64(guard.ControllerStateVersion),
		int64(guard.AdmissionContractVersion),
		int64(guard.ReleaseSequence),
		guard.ManagerImage,
	)
	request := map[string]any{
		"operation": "UPDATE",
		"namespace": guard.ReleaseNamespace,
		"name":      marker.Name,
		"dryRun":    true,
		"resource":  map[string]any{"group": "", "version": "v1", "resource": "configmaps"},
		"options": map[string]any{
			"fieldManager": guard.sentinelProbe(ReleaseActivationState{
				ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsActive,
			}).FieldManager,
		},
		"userInfo": map[string]any{"username": guard.serviceAccountUsername(guard.HookServiceAccountName)},
	}
	if !evaluatePolicyMatchConditions(t, policy, object, oldObject, request, params) {
		t.Fatal("exact marker proof does not match the expanded sentinel")
	}
	results := evaluatePolicyValidations(t, policy, object, oldObject, request, params)
	denied := 0
	for index, allowed := range results {
		if allowed {
			continue
		}
		denied++
		want := admissionConvergenceDenialMessage(ReleaseActivationState{
			ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsActive,
		})
		if got := policy.Spec.Validations[index].Message; got != want {
			t.Fatalf("marker denial message = %q, want %q", got, want)
		}
	}
	if denied != 1 {
		t.Fatalf("marker proof denial count = %d, want one; results=%v", denied, results)
	}
}

func TestAdmissionConvergenceDependencyProofIsSingleCauseAndSentinelExclusive(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionConvergenceFixture(t)
	guard := fixture.guard
	rollout, err := guard.rolloutForDependencies()
	if err != nil {
		t.Fatal(err)
	}
	dependencyPolicy := rollout.hookIdentityPolicy()
	probe := newAdmissionConvergenceDependencyProbe(
		dependencyPolicy.Name,
		hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage),
	)
	marker := guard.marker()
	marker.UID = "marker-uid"
	marker.ResourceVersion = "marker-rv"
	object, ok := rolloutCELClone(t, marker).(map[string]any)
	if !ok {
		t.Fatal("marker JSON did not decode to an object")
	}
	params := rolloutActivationCELObject(
		&RolloutGuard{
			ReleaseName:              guard.ReleaseName,
			ReleaseNamespace:         guard.ReleaseNamespace,
			ControllerStateVersion:   guard.ControllerStateVersion,
			AdmissionContractVersion: guard.AdmissionContractVersion,
			ReleaseSequence:          guard.ReleaseSequence,
		},
		0,
		int64(guard.ControllerStateVersion),
		int64(guard.AdmissionContractVersion),
		int64(guard.ReleaseSequence),
		guard.ManagerImage,
	)
	request := map[string]any{
		"operation": "UPDATE",
		"namespace": guard.ReleaseNamespace,
		"name":      marker.Name,
		"dryRun":    true,
		"resource":  map[string]any{"group": "", "version": "v1", "resource": "configmaps"},
		"options":   map[string]any{"fieldManager": probe.FieldManager},
		"userInfo":  map[string]any{"username": guard.serviceAccountUsername(guard.HookServiceAccountName)},
	}
	if !evaluatePolicyMatchConditions(t, dependencyPolicy, object, object, request, nil) {
		t.Fatal("exact dependency marker request does not match its workload policy")
	}
	results := evaluatePolicyValidations(t, dependencyPolicy, object, object, request, nil)
	denied := 0
	for index, allowed := range results {
		if allowed {
			continue
		}
		denied++
		if got := dependencyPolicy.Spec.Validations[index].Message; got != probe.Message {
			t.Fatalf("dependency denial message = %q, want %q", got, probe.Message)
		}
	}
	if denied != 1 {
		t.Fatalf("dependency proof denial count = %d, want one; results=%v", denied, results)
	}

	sentinel := guard.policy()
	if !evaluatePolicyMatchConditions(t, sentinel, object, object, request, params) {
		t.Fatal("dependency marker request does not enter the sentinel's reserved-probe branch")
	}
	for index, allowed := range evaluatePolicyValidations(t, sentinel, object, object, request, params) {
		if !allowed {
			t.Fatalf("sentinel added a second denial to dependency probe at validation %d: %q", index, sentinel.Spec.Validations[index].Message)
		}
	}

	request["dryRun"] = false
	results = evaluatePolicyValidations(t, dependencyPolicy, object, object, request, nil)
	denialMessages := []string{}
	for index, allowed := range results {
		if !allowed {
			denialMessages = append(denialMessages, dependencyPolicy.Spec.Validations[index].Message)
		}
	}
	if !slices.Contains(denialMessages, admissionConvergenceProbePersistenceMessage) || !slices.Contains(denialMessages, probe.Message) {
		t.Fatalf("persistent reserved field manager denials = %q, want mutation and proof denials", denialMessages)
	}
}

func TestAdmissionConvergenceProbeBundleSelectsExactlyOnePolicyCause(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionConvergenceFixture(t)
	guard := fixture.guard
	rollout, err := guard.rolloutForDependencies()
	if err != nil {
		t.Fatal(err)
	}
	state := ReleaseActivationState{ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsActive}
	policies := map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}
	blueprints, err := predecessorRetirementPairBlueprints(rollout)
	if err != nil {
		t.Fatal(err)
	}
	for _, blueprint := range blueprints {
		policies[blueprint.name] = blueprint.policy
	}
	probes := guard.dependencyProbes()
	if len(policies) != len(probes) {
		t.Fatalf("compiled dependency policies = %d, probes = %d", len(policies), len(probes))
	}
	commonAnyExpression := guard.anyConvergenceProbeRequestExpression()
	seenFieldManagers := map[string]string{}
	for _, probe := range probes {
		policy := policies[probe.PolicyName]
		if policy == nil {
			t.Fatalf("probe %s has no compiled dependency policy", probe.PolicyName)
		}
		stripAdmissionConvergenceDependencyProbe(t, policy)
		if got := policy.Spec.Variables[0].Expression; got != commonAnyExpression {
			t.Fatalf("policy %s any-probe selector = %q, want %q", probe.PolicyName, got, commonAnyExpression)
		}
		wantExact := admissionConvergenceProbeRequestExpression(
			guard.ReleaseNamespace,
			AdmissionConvergenceMarkerName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence),
			probe.FieldManager,
		)
		if got := policy.Spec.Variables[1].Expression; got != wantExact {
			t.Fatalf("policy %s exact selector = %q, want %q", probe.PolicyName, got, wantExact)
		}
		if got := policy.Spec.Validations[len(policy.Spec.Validations)-1]; got.Expression != `!variables.isAdmissionConvergenceProbe` || got.Message != probe.Message {
			t.Fatalf("policy %s proof validation = %#v, want unique exact denial %q", probe.PolicyName, got, probe.Message)
		}
		if previous := seenFieldManagers[probe.FieldManager]; previous != "" {
			t.Fatalf("policies %s and %s share field manager %q", previous, probe.PolicyName, probe.FieldManager)
		}
		seenFieldManagers[probe.FieldManager] = probe.PolicyName
	}

	sentinel := guard.policy()
	sentinelVariables := make(map[string]string, len(sentinel.Spec.Variables))
	for _, variable := range sentinel.Spec.Variables {
		sentinelVariables[variable.Name] = variable.Expression
	}
	if got := sentinelVariables["isAnyConvergenceProbe"]; got != commonAnyExpression {
		t.Fatalf("sentinel any-probe selector = %q, want %q", got, commonAnyExpression)
	}
	if got := sentinelVariables["isMarkerProbe"]; got != guard.markerRequestExpression() {
		t.Fatalf("sentinel exact selector = %q, want %q", got, guard.markerRequestExpression())
	}
	sentinelProbe := guard.sentinelProbe(state)
	if previous := seenFieldManagers[sentinelProbe.FieldManager]; previous != "" {
		t.Fatalf("sentinel and %s share field manager %q", previous, sentinelProbe.FieldManager)
	}
	for _, probe := range probes {
		if strings.Contains(sentinelVariables["isMarkerProbe"], probe.FieldManager) {
			t.Fatalf("sentinel exact selector also selects dependency %s", probe.PolicyName)
		}
		for _, policy := range policies {
			if policy.Name == probe.PolicyName {
				continue
			}
			if strings.Contains(policy.Spec.Variables[1].Expression, probe.FieldManager) {
				t.Fatalf("non-target policy %s exact selector also selects %s", policy.Name, probe.PolicyName)
			}
		}
	}
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

func TestAdmissionConvergenceGuardAcceptsOnlyExactStoredContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*admissionConvergenceFixture)
		want   string
	}{
		{name: "exact"},
		{
			name: "byte-exact dependency replacement",
			mutate: func(f *admissionConvergenceFixture) {
				name := HookIdentityGuardPolicyName(
					f.guard.ReleaseNamespace,
					f.guard.ReleaseName,
					f.guard.ReleaseSequence,
					f.guard.ManagerImage,
				)
				f.policies.objects[name].UID = "replacement-uid"
				f.policies.objects[name].ResourceVersion = "replacement-rv"
			},
		},
		{
			name: "policy namespace",
			mutate: func(f *admissionConvergenceFixture) {
				f.policies.objects[f.name].Namespace = "foreign"
			},
			want: "foreign or incomplete ownership",
		},
		{
			name: "policy owner",
			mutate: func(f *admissionConvergenceFixture) {
				f.policies.objects[f.name].OwnerReferences = []metav1.OwnerReference{{Name: "foreign"}}
			},
			want: "foreign or incomplete ownership",
		},
		{
			name: "binding finalizer",
			mutate: func(f *admissionConvergenceFixture) {
				f.bindings.objects[f.name].Finalizers = []string{"foreign.example/finalizer"}
			},
			want: "foreign or incomplete ownership",
		},
		{
			name: "binding deletion",
			mutate: func(f *admissionConvergenceFixture) {
				now := metav1.Now()
				f.bindings.objects[f.name].DeletionTimestamp = &now
			},
			want: "foreign or incomplete ownership",
		},
		{
			name: "marker missing UID",
			mutate: func(f *admissionConvergenceFixture) {
				f.configMaps.objects[f.markerName].UID = ""
			},
			want: "foreign or incomplete ownership",
		},
		{
			name: "marker missing resource version",
			mutate: func(f *admissionConvergenceFixture) {
				f.configMaps.objects[f.markerName].ResourceVersion = ""
			},
			want: "foreign or incomplete ownership",
		},
		{
			name: "marker owner",
			mutate: func(f *admissionConvergenceFixture) {
				f.configMaps.objects[f.markerName].OwnerReferences = []metav1.OwnerReference{{Name: "foreign"}}
			},
			want: "exact marker contract",
		},
		{
			name: "marker full attempt mismatch",
			mutate: func(f *admissionConvergenceFixture) {
				attempt := f.configMaps.objects[f.markerName].Data[admissionConvergenceAttemptDataKey]
				replacement := "0"
				if strings.HasSuffix(attempt, replacement) {
					replacement = "1"
				}
				f.configMaps.objects[f.markerName].Data[admissionConvergenceAttemptDataKey] = attempt[:63] + replacement
			},
			want: "exact marker contract",
		},
		{
			name: "cleanup identity annotation",
			mutate: func(f *admissionConvergenceFixture) {
				f.configMaps.objects[f.markerName].Annotations[admissionConvergenceCleanupAnnotation] += "-old"
			},
			want: "foreign or incomplete ownership",
		},
		{
			name: "dependency policy spec",
			mutate: func(f *admissionConvergenceFixture) {
				name := HookIdentityGuardPolicyName(
					f.guard.ReleaseNamespace,
					f.guard.ReleaseName,
					f.guard.ReleaseSequence,
					f.guard.ManagerImage,
				)
				f.policies.objects[name].Spec.Validations[0].Expression = "true"
			},
			want: "verify admission convergence dependency policy",
		},
		{
			name: "dependency policy lifecycle owner",
			mutate: func(f *admissionConvergenceFixture) {
				name := RuntimePodGuardPolicyName(f.guard.ReleaseSequence)
				f.policies.objects[name].OwnerReferences = []metav1.OwnerReference{{Name: "foreign"}}
			},
			want: "foreign or incomplete ownership",
		},
		{
			name: "dependency binding metadata",
			mutate: func(f *admissionConvergenceFixture) {
				name := ParentReplicaSetGuardPolicyName(f.guard.ReleaseNamespace, f.guard.ReleaseName, f.guard.ReleaseSequence, f.guard.ManagerImage)
				f.bindings.objects[name].Labels["foreign.example/owner"] = "true"
			},
			want: "foreign or incomplete ownership",
		},
		{
			name: "dependency binding missing",
			mutate: func(f *admissionConvergenceFixture) {
				name := ParentHookJobContractPolicyName(f.guard.ReleaseNamespace, f.guard.ReleaseName, f.guard.ReleaseSequence, f.guard.ManagerImage)
				delete(f.bindings.objects, name)
			},
			want: "get admission convergence dependency binding",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newAdmissionConvergenceFixture(t)
			if test.mutate != nil {
				test.mutate(fixture)
			}
			state, err := fixture.guard.VerifyPreCutover(context.Background())
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				want := ReleaseActivationState{ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsActive}
				if state != want {
					t.Fatalf("VerifyPreCutover() = %#v, want %#v", state, want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyPreCutover() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestAdmissionConvergencePolicyPinsExactCurrentCallers(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionConvergenceFixture(t)
	conditions := fixture.guard.policy().Spec.MatchConditions
	if len(conditions) != 1 {
		t.Fatalf("match conditions = %d, want one explicit branch union", len(conditions))
	}
	for description, want := range map[string]string{
		"content-versioned marker probe": fixture.guard.anyConvergenceProbeRequestExpression(),
		"controller caller fence":        controllerPrincipalMatchExpression(fixture.guard.ReleaseNamespace, fixture.guard.ControllerServiceAccountName, fixture.guard.PreviousControllerServiceAccountName),
		"certificate caller fence": fixture.guard.serviceAccountUsername(
			fixture.guard.CertificateServiceAccountName,
		),
	} {
		if !strings.Contains(conditions[0].Expression, want) {
			t.Fatalf("match condition lacks %s %q: %q", description, want, conditions[0].Expression)
		}
	}
	variables := make(map[string]string, len(fixture.guard.policy().Spec.Variables))
	for _, variable := range fixture.guard.policy().Spec.Variables {
		variables[variable.Name] = variable.Expression
	}
	if got, want := variables["isMarkerProbe"], fixture.guard.markerRequestExpression(); got != want {
		t.Fatalf("sentinel marker variable = %q, want exact caller-bound probe %q", got, want)
	}
	if got, want := variables["isControllerTokenRequest"], fmt.Sprintf(
		`variables.isTokenRequest && request.namespace == %q && (request.name in [%q])`,
		fixture.guard.ReleaseNamespace,
		fixture.guard.ControllerServiceAccountName,
	); got != want {
		t.Fatalf("controller TokenRequest variable = %q, want %q", got, want)
	}
	oldCleanup := strings.Replace(fixture.guard.CleanupServiceAccountName, "-v1-", "-v0-", 1)
	if strings.Contains(conditions[0].Expression, oldCleanup) {
		t.Fatalf("caller condition admits older cleanup identity %q", oldCleanup)
	}
}

func TestAdmissionConvergenceProbeRequiresExactTupleDenial(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionConvergenceFixture(t)
	sealAdmissionConvergenceMarkerForTest(t, fixture.guard, fixture.configMaps.objects[fixture.markerName])
	state := ReleaseActivationState{ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsActive}
	exact := admissionConvergenceExactProbeErrors(fixture.guard, state)
	probes := append([]admissionConvergenceDependencyProbe{fixture.guard.sentinelProbe(state)}, fixture.guard.dependencyProbes()...)
	targetDenial := validatingAdmissionPolicyDenialCauseMessage(probes[0].PolicyName, probes[0].PolicyName, probes[0].Message)
	knownParamNotFound := validatingAdmissionPolicyDenialCauseMessage(
		probes[1].PolicyName,
		probes[1].PolicyName,
		admissionConvergenceParamNotFound,
	)
	knownParamKindNotSynced := validatingAdmissionPolicyDenialCauseMessage(
		probes[len(probes)-1].PolicyName,
		probes[len(probes)-1].PolicyName,
		admissionConvergenceParamKindNotSynced,
	)
	staleState := ReleaseActivationState{
		ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsDraining,
		DrainTargetReleaseSequence: fixture.guard.ReleaseSequence,
		DrainAttempt:               hookIdentityDigest(fixture.guard.ReleaseNamespace, fixture.guard.ReleaseName, fixture.guard.ReleaseSequence, fixture.guard.ManagerImage),
	}
	tests := []struct {
		name        string
		updates     []error
		mutate      func(*admissionConvergenceFixture)
		want        bool
		wantErr     string
		wantUpdates int
	}{
		{name: "all exact denials", updates: exact, want: true, wantUpdates: len(exact)},
		{name: "admitted is inconclusive", updates: []error{nil}, wantUpdates: 1},
		{name: "stale exact tuple resets", updates: []error{exactPolicyDenialError(fixture.name, fixture.name, admissionConvergenceDenialMessage(staleState))}, wantUpdates: 1},
		{name: "server timeout retries", updates: []error{apierrors.NewServerTimeout(schema.GroupResource{Resource: "configmaps"}, "update", 1)}, wantUpdates: 1},
		{
			name:        "known missing parameter cache retries",
			updates:     []error{admissionPolicyCausesError(knownParamNotFound)},
			wantUpdates: 1,
		},
		{
			name:        "known unsynced parameter kind retries",
			updates:     []error{admissionPolicyCausesError(knownParamKindNotSynced)},
			wantUpdates: 1,
		},
		{
			name:        "target denial mixed with known parameter transition retries",
			updates:     []error{admissionPolicyCausesError(targetDenial, knownParamNotFound)},
			wantUpdates: 1,
		},
		{
			name: "unknown policy parameter transition fails closed",
			updates: []error{admissionPolicyCausesError(validatingAdmissionPolicyDenialCauseMessage(
				"foreign-policy",
				"foreign-binding",
				admissionConvergenceParamNotFound,
			))},
			wantErr:     "unexpected response",
			wantUpdates: 1,
		},
		{
			name: "wrong binding parameter transition fails closed",
			updates: []error{admissionPolicyCausesError(validatingAdmissionPolicyDenialCauseMessage(
				probes[1].PolicyName,
				"foreign-binding",
				admissionConvergenceParamNotFound,
			))},
			wantErr:     "unexpected response",
			wantUpdates: 1,
		},
		{
			name: "other policy configuration error fails closed",
			updates: []error{admissionPolicyCausesError(validatingAdmissionPolicyDenialCauseMessage(
				probes[1].PolicyName,
				probes[1].PolicyName,
				"failed to configure binding: foreign policy error",
			))},
			wantErr:     "unexpected response",
			wantUpdates: 1,
		},
		{
			name: "known transition mixed with unknown cause fails closed",
			updates: []error{admissionPolicyCausesError(
				knownParamNotFound,
				validatingAdmissionPolicyDenialCauseMessage(probes[1].PolicyName, probes[1].PolicyName, "foreign policy error"),
			)},
			wantErr:     "unexpected response",
			wantUpdates: 1,
		},
		{
			name: "PolicyError typed cause fails closed",
			updates: []error{admissionPolicyStatusCausesError(metav1.StatusCause{
				Type:    metav1.CauseTypeUnexpectedServerResponse,
				Message: knownParamNotFound,
			})},
			wantErr:     "unexpected response",
			wantUpdates: 1,
		},
		{name: "wrong denial", updates: []error{exactPolicyDenialError(fixture.name, fixture.name, "foreign denial")}, wantErr: "unexpected response", wantUpdates: 1},
		{
			name: "foreign marker",
			mutate: func(f *admissionConvergenceFixture) {
				f.configMaps.objects[f.markerName].Annotations[ManagerImageAnnotation] = "foreign"
			},
			updates: exact,
			wantErr: "foreign or incomplete ownership",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			current := newAdmissionConvergenceFixture(t)
			if test.mutate != nil {
				test.mutate(current)
			}
			current.configMaps.updateErrors = append([]error(nil), test.updates...)
			got, err := current.guard.Probe(context.Background(), current.configMaps, state)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Probe() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("Probe() = %t, want %t", got, test.want)
			}
			if len(current.configMaps.updates) != test.wantUpdates {
				t.Fatalf("dry-run updates = %d, want %d", len(current.configMaps.updates), test.wantUpdates)
			}
			for index := range current.configMaps.updates {
				if !reflect.DeepEqual(current.configMaps.updateOptions[index].DryRun, []string{metav1.DryRunAll}) {
					t.Fatalf("update %d DryRun = %v, want All", index, current.configMaps.updateOptions[index].DryRun)
				}
				if current.configMaps.updateOptions[index].FieldManager == "" {
					t.Fatalf("update %d lacks an exact field manager", index)
				}
				if current.configMaps.updates[index].UID == "" || current.configMaps.updates[index].ResourceVersion == "" {
					t.Fatalf("probe %d discarded marker UID/resourceVersion", index)
				}
			}
		})
	}

	t.Run("one missing dependency denial resets", func(t *testing.T) {
		t.Parallel()
		current := newAdmissionConvergenceFixture(t)
		updates := admissionConvergenceExactProbeErrors(current.guard, state)
		missing := 1 + len(current.guard.dependencyProbes())/2
		updates[missing] = nil
		current.configMaps.updateErrors = updates
		proven, err := current.guard.Probe(context.Background(), current.configMaps, state)
		if err != nil {
			t.Fatal(err)
		}
		if proven {
			t.Fatal("Probe() accepted a bundle with one missing workload-policy denial")
		}
		if got, want := len(current.configMaps.updates), missing+1; got != want {
			t.Fatalf("dry-run updates = %d, want stop at missing member %d", got, want)
		}
	})

	t.Run("cancellation wins over a known parameter transition", func(t *testing.T) {
		t.Parallel()
		current := newAdmissionConvergenceFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		current.configMaps.beforeUpdate = cancel
		current.configMaps.updateErrors = []error{admissionPolicyCausesError(knownParamNotFound)}
		proven, err := current.guard.Probe(ctx, current.configMaps, state)
		if proven || !errors.Is(err, context.Canceled) {
			t.Fatalf("Probe() = %t, %v, want cancellation to win", proven, err)
		}
	})
}

func admissionPolicyCausesError(messages ...string) error {
	causes := make([]metav1.StatusCause, len(messages))
	for index, message := range messages {
		causes[index].Message = message
	}
	return admissionPolicyStatusCausesError(causes...)
}

func admissionPolicyStatusCausesError(causes ...metav1.StatusCause) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure,
		Reason: metav1.StatusReasonInvalid,
		Code:   422,
		Details: &metav1.StatusDetails{
			Causes: causes,
		},
	}}
}

func admissionConvergenceExactProbeErrors(g *AdmissionConvergenceGuard, state ReleaseActivationState) []error {
	probes := append([]admissionConvergenceDependencyProbe{g.sentinelProbe(state)}, g.dependencyProbes()...)
	denials := make([]error, len(probes))
	for index, probe := range probes {
		denials[index] = exactPolicyDenialError(probe.PolicyName, probe.PolicyName, probe.Message)
	}
	return denials
}

func TestAdmissionConvergenceProbeAbsentRequiresExactObjectsAndAdmission(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionConvergenceFixture(t)
	sealAdmissionConvergenceMarkerForTest(t, fixture.guard, fixture.configMaps.objects[fixture.markerName])
	state := ReleaseActivationState{ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsActive}
	proven, err := fixture.guard.ProbeAbsent(context.Background(), fixture.configMaps, state)
	if err != nil || !proven {
		t.Fatalf("ProbeAbsent() = %t, %v, want true", proven, err)
	}
	if got := len(fixture.configMaps.gets); got != 2 || fixture.configMaps.gets[0] != ReleaseActivationName || fixture.configMaps.gets[1] != fixture.markerName {
		t.Fatalf("direct GET order = %v, want activation then marker", fixture.configMaps.gets)
	}

	fixture = newAdmissionConvergenceFixture(t)
	sealAdmissionConvergenceMarkerForTest(t, fixture.guard, fixture.configMaps.objects[fixture.markerName])
	fixture.configMaps.updateErrors = []error{exactPolicyDenialError(fixture.name, fixture.name, admissionConvergenceDenialMessage(state))}
	proven, err = fixture.guard.ProbeAbsent(context.Background(), fixture.configMaps, state)
	if err != nil || proven {
		t.Fatalf("ProbeAbsent() with stale binding = %t, %v, want inconclusive", proven, err)
	}

	fixture = newAdmissionConvergenceFixture(t)
	sealAdmissionConvergenceMarkerForTest(t, fixture.guard, fixture.configMaps.objects[fixture.markerName])
	fixture.configMaps.updateErrors = []error{exactPolicyDenialError(fixture.name, fixture.name, "foreign denial")}
	proven, err = fixture.guard.ProbeAbsent(context.Background(), fixture.configMaps, state)
	if err == nil || proven || !strings.Contains(err.Error(), "unexpected response") {
		t.Fatalf("ProbeAbsent() with foreign denial = %t, %v, want fail closed", proven, err)
	}

	fixture = newAdmissionConvergenceFixture(t)
	sealAdmissionConvergenceMarkerForTest(t, fixture.guard, fixture.configMaps.objects[fixture.markerName])
	fixture.configMaps.updateErrors = []error{admissionPolicyCausesError(validatingAdmissionPolicyDenialCauseMessage(
		fixture.name,
		fixture.name,
		admissionConvergenceParamNotFound,
	))}
	proven, err = fixture.guard.ProbeAbsent(context.Background(), fixture.configMaps, state)
	if err != nil || proven {
		t.Fatalf("ProbeAbsent() during expected parameter-cache transition = %t, %v, want inconclusive", proven, err)
	}

	fixture = newAdmissionConvergenceFixture(t)
	sealAdmissionConvergenceMarkerForTest(t, fixture.guard, fixture.configMaps.objects[fixture.markerName])
	fixture.configMaps.objects[ReleaseActivationName].Data[activeReleaseDataKey] = "1"
	fixture.configMaps.objects[ReleaseActivationName].Annotations[ReleaseSequenceAnnotation] = "1"
	proven, err = fixture.guard.ProbeAbsent(context.Background(), fixture.configMaps, state)
	if err == nil || proven || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("ProbeAbsent() with changed activation = %t, %v, want fail closed", proven, err)
	}
}

func TestAdmissionConvergenceMarkerTargetUsesExactCurrentContract(t *testing.T) {
	t.Parallel()

	fixture := newAdmissionConvergenceFixture(t)
	sealAdmissionConvergenceMarkerForTest(t, fixture.guard, fixture.configMaps.objects[fixture.markerName])
	target, err := fixture.guard.MarkerTarget()
	if err != nil {
		t.Fatal(err)
	}
	if target.Name != fixture.markerName || target.Verify == nil {
		t.Fatalf("MarkerTarget() = %#v, want exact current marker %q", target, fixture.markerName)
	}
	marker := fixture.configMaps.objects[fixture.markerName].DeepCopy()
	if err := target.Verify(marker); err != nil {
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

func TestAdmissionConvergenceRetiresPreviousMarkerWithExactPreconditions(t *testing.T) {
	t.Parallel()

	rollout, policies, bindings, _ := readyRolloutGuard()
	previousImage := rollout.ManagerImage
	rollout.ReleaseSequence = 2
	rollout.PreviousControllerReleaseSequence = 1
	rollout.ManagerImage = "registry.example/ptah@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rollout.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)[:12]
	client := &admissionConvergenceConfigMapClient{objects: map[string]*corev1.ConfigMap{}}
	rollout.Policies = policies
	rollout.Bindings = bindings
	rollout.ConfigMaps = client
	rollout.ConfigMapDeleter = client
	guard := NewAdmissionConvergenceGuard(rollout)
	previous := *guard
	previous.ReleaseSequence = 1
	previous.ManagerImage = previousImage
	var err error
	previous.HookServiceAccountName, err = guard.hookServiceAccountFor(1, previousImage)
	if err != nil {
		t.Fatal(err)
	}
	previous.CleanupServiceAccountName, err = TeardownServiceAccountName(previous.HookServiceAccountName, 1)
	if err != nil {
		t.Fatal(err)
	}
	marker := previous.marker()
	marker.UID = "previous-uid"
	marker.ResourceVersion = "previous-rv"
	sealAdmissionConvergenceMarkerForTest(t, &previous, marker)
	client.objects[marker.Name] = marker

	if err := guard.RetirePreviousMarker(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.deletes) != 1 || client.deletes[0] != marker.Name {
		t.Fatalf("marker deletes = %v, want %q", client.deletes, marker.Name)
	}
	options := client.deleteOptions[0]
	if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != marker.UID ||
		options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != marker.ResourceVersion {
		t.Fatalf("marker delete preconditions = %#v", options.Preconditions)
	}
	if err := guard.RetirePreviousMarker(context.Background()); err != nil {
		t.Fatalf("lost-response retry: %v", err)
	}
}

func TestAdmissionConvergenceRefusesForeignPreviousMarker(t *testing.T) {
	t.Parallel()

	rollout, policies, bindings, _ := readyRolloutGuard()
	rollout.ReleaseSequence = 2
	rollout.PreviousControllerReleaseSequence = 1
	rollout.ManagerImage = "registry.example/ptah@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rollout.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)[:12]
	client := &admissionConvergenceConfigMapClient{objects: map[string]*corev1.ConfigMap{}}
	rollout.Policies, rollout.Bindings, rollout.ConfigMaps, rollout.ConfigMapDeleter = policies, bindings, client, client
	guard := NewAdmissionConvergenceGuard(rollout)
	name := AdmissionConvergenceMarkerName(rollout.ReleaseNamespace, rollout.ReleaseName, 1)
	client.objects[name] = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: rollout.ReleaseNamespace, UID: "foreign", ResourceVersion: "1",
		Annotations: map[string]string{ManagerImageAnnotation: "registry.example/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}}
	if err := guard.RetirePreviousMarker(context.Background()); err == nil || !strings.Contains(err.Error(), "previous admission convergence marker") {
		t.Fatalf("RetirePreviousMarker() error = %v, want foreign marker refusal", err)
	}
	if len(client.deletes) != 0 {
		t.Fatalf("foreign marker deletes = %v", client.deletes)
	}
}

func TestAdmissionConvergenceRefusesMalformedPreviousManagerIdentity(t *testing.T) {
	t.Parallel()

	for _, managerImage := range []string{
		" registry.example/ptah@sha256:" + strings.Repeat("a", 64),
		"registry.example/ptah image@sha256:" + strings.Repeat("a", 64),
		"registry.example/ptah@shadow@sha256:" + strings.Repeat("a", 64),
		"registry.example/ptah@sha256:" + strings.Repeat("A", 64),
	} {
		managerImage := managerImage
		t.Run(managerImage, func(t *testing.T) {
			t.Parallel()

			rollout, policies, bindings, _ := readyRolloutGuard()
			rollout.ReleaseSequence = 2
			rollout.PreviousControllerReleaseSequence = 1
			rollout.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("b", 64)
			rollout.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)[:12]
			client := &admissionConvergenceConfigMapClient{objects: map[string]*corev1.ConfigMap{}}
			rollout.Policies, rollout.Bindings, rollout.ConfigMaps, rollout.ConfigMapDeleter = policies, bindings, client, client
			guard := NewAdmissionConvergenceGuard(rollout)
			name := AdmissionConvergenceMarkerName(rollout.ReleaseNamespace, rollout.ReleaseName, 1)
			client.objects[name] = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: rollout.ReleaseNamespace, UID: "previous", ResourceVersion: "1",
				Annotations: map[string]string{ManagerImageAnnotation: managerImage},
			}}

			err := guard.RetirePreviousMarker(context.Background())
			if err == nil || !strings.Contains(err.Error(), "invalid manager identity") {
				t.Fatalf("RetirePreviousMarker() error = %v, want invalid manager identity", err)
			}
			if len(client.deletes) != 0 {
				t.Fatalf("malformed marker deletes = %v", client.deletes)
			}
		})
	}
}

type admissionConvergenceFixture struct {
	guard      *AdmissionConvergenceGuard
	policies   *rolloutPolicyClient
	bindings   *rolloutBindingClient
	configMaps *admissionConvergenceConfigMapClient
	name       string
	markerName string
}

func newAdmissionConvergenceFixture(t *testing.T) *admissionConvergenceFixture {
	t.Helper()
	rollout, policies, bindings, _ := readyRolloutGuard()
	configMaps := &admissionConvergenceConfigMapClient{objects: map[string]*corev1.ConfigMap{}}
	for name, object := range rollout.ConfigMaps.(*rolloutConfigMapClient).objects {
		configMaps.objects[name] = object.DeepCopy()
	}
	rollout.ConfigMaps = configMaps
	rollout.ConfigMapDeleter = configMaps
	guard := NewAdmissionConvergenceGuard(rollout)
	blueprints, err := predecessorRetirementPairBlueprints(rollout)
	if err != nil {
		t.Fatalf("build admission convergence fixture dependencies: %v", err)
	}
	for _, blueprint := range blueprints {
		policies.objects[blueprint.name] = readyPolicy(blueprint.policy)
		bindings.objects[blueprint.name] = blueprint.binding.DeepCopy()
	}
	name := AdmissionConvergencePolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	markerName := AdmissionConvergenceMarkerName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence)
	policies.objects[name] = readyPolicy(guard.policy())
	bindings.objects[name] = guard.binding()
	marker := guard.marker()
	marker.UID = types.UID("marker-uid")
	marker.ResourceVersion = "marker-rv"
	configMaps.objects[markerName] = marker
	return &admissionConvergenceFixture{
		guard: guard, policies: policies, bindings: bindings, configMaps: configMaps,
		name: name, markerName: markerName,
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

type admissionConvergenceConfigMapClient struct {
	objects       map[string]*corev1.ConfigMap
	updateErrors  []error
	gets          []string
	updates       []*corev1.ConfigMap
	updateOptions []metav1.UpdateOptions
	deletes       []string
	deleteOptions []metav1.DeleteOptions
	beforeUpdate  func()
}

func (c *admissionConvergenceConfigMapClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	c.gets = append(c.gets, name)
	object := c.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	return object.DeepCopy(), nil
}

func (c *admissionConvergenceConfigMapClient) Create(_ context.Context, object *corev1.ConfigMap, _ metav1.CreateOptions) (*corev1.ConfigMap, error) {
	c.objects[object.Name] = object.DeepCopy()
	return object.DeepCopy(), nil
}

func (c *admissionConvergenceConfigMapClient) Update(_ context.Context, object *corev1.ConfigMap, options metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	c.updates = append(c.updates, object.DeepCopy())
	c.updateOptions = append(c.updateOptions, options)
	if c.beforeUpdate != nil {
		c.beforeUpdate()
	}
	if len(c.updateErrors) != 0 {
		err := c.updateErrors[0]
		c.updateErrors = c.updateErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	return object.DeepCopy(), nil
}

func (c *admissionConvergenceConfigMapClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	c.deletes = append(c.deletes, name)
	c.deleteOptions = append(c.deleteOptions, options)
	if c.objects[name] == nil {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	delete(c.objects, name)
	return nil
}

var (
	_ ConfigMapWriter                  = (*admissionConvergenceConfigMapClient)(nil)
	_ ConfigMapDeleter                 = (*admissionConvergenceConfigMapClient)(nil)
	_ AdmissionConvergenceMarkerClient = (*admissionConvergenceConfigMapClient)(nil)
)
