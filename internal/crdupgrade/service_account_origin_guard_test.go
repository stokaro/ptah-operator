package crdupgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestRenderedServiceAccountOriginGuardMatchesCompiledContract(t *testing.T) {
	path := os.Getenv("PTAH_ROLLOUT_GUARD_RENDER")
	if path == "" {
		t.Skip("PTAH_ROLLOUT_GUARD_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	guard := testServiceAccountOriginGuard()
	guard.ReleaseName = "ptah-e2e"
	guard.ReleaseNamespace = "ptah-e2e"
	guard.ControllerServiceAccountName = "ptah-e2e-ptah-operator"
	guard.ControllerDeploymentName = "ptah-e2e-ptah-operator"
	guard.CertificateServiceAccountName = "ptah-e2e-ptah-operator-cert-rotator"
	guard.CertificateDeploymentName = "ptah-e2e-ptah-operator-cert-rotator"
	guard.ManagerImage = "ghcr.io/stokaro/ptah-operator@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	guard.HookServiceAccountName = "ptah-e2e-ptah-operator-crd-v1-" + hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)[:12]
	name := ServiceAccountOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	var binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
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
		}
	}
	if err := guard.verifyPolicy(policy); err != nil {
		t.Fatalf("rendered service account origin policy: %v", err)
	}
	if err := guard.verifyBinding(binding); err != nil {
		t.Fatalf("rendered service account origin binding: %v", err)
	}
	if policy.Annotations["helm.sh/hook-weight"] != serviceAccountOriginPolicyWeight || binding.Annotations["helm.sh/hook-weight"] != serviceAccountOriginBindingWeight {
		t.Fatal("service account origin policy and binding must precede hook identity and privileged RBAC")
	}
}

func TestServiceAccountOriginGuardCoversCallerAndTokenRequestBypasses(t *testing.T) {
	guard := testServiceAccountOriginGuard()
	policy, err := guard.policy()
	if err != nil {
		t.Fatal(err)
	}
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail ||
		policy.Spec.MatchConstraints == nil || policy.Spec.MatchConstraints.MatchPolicy == nil ||
		*policy.Spec.MatchConstraints.MatchPolicy != admissionregistrationv1.Exact {
		t.Fatal("service account origin policy is not explicitly fail-closed with exact matching")
	}
	rules := policy.Spec.MatchConstraints.ResourceRules
	if len(rules) != 1 {
		t.Fatalf("resource rules = %d, want one all-resource rule", len(rules))
	}
	rule := rules[0].RuleWithOperations
	if !reflect.DeepEqual(rule.Operations, []admissionregistrationv1.OperationType{
		admissionregistrationv1.Create,
		admissionregistrationv1.Update,
		admissionregistrationv1.Delete,
		admissionregistrationv1.Connect,
	}) || !reflect.DeepEqual(rule.APIGroups, []string{"*"}) ||
		!reflect.DeepEqual(rule.APIVersions, []string{"*"}) ||
		!reflect.DeepEqual(rule.Resources, []string{"*/*"}) ||
		rule.Scope == nil || *rule.Scope != admissionregistrationv1.AllScopes {
		t.Fatalf("all-resource origin rule differs from the required operation boundary: %#v", rule)
	}
	serialized, err := json.Marshal(policy.Spec)
	if err != nil {
		t.Fatal(err)
	}
	contract := string(serialized)
	for _, required := range []string{
		`serviceaccounts`,
		`request.subResource == \"token\"`,
		`authentication.kubernetes.io/pod-name`,
		`authentication.kubernetes.io/pod-uid`,
		`.size() == 1`,
		`[0-9a-f]{12}$`,
		`request.userInfo.username.matches(\"^system:node:.+$\")`,
		`group == \"system:nodes\"`,
		`object.spec.boundObjectRef.apiVersion == \"v1\"`,
		`object.spec.boundObjectRef.kind == \"Pod\"`,
		`object.spec.boundObjectRef.uid != \"\"`,
		`ptah-hook-identity-v[1-9][0-9]*-[0-9a-f]{12}-`,
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("service account origin contract does not contain %q", required)
		}
	}
}

func TestServiceAccountOriginGuardPolicyIsStableAcrossReleaseSequences(t *testing.T) {
	guard := testServiceAccountOriginGuard()
	policy, err := guard.policy()
	if err != nil {
		t.Fatal(err)
	}
	other := *guard
	other.ReleaseSequence = 2
	other.ManagerImage = "registry.example/ptah@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	other.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(other.ReleaseNamespace, other.ReleaseName, other.ReleaseSequence, other.ManagerImage)[:12]
	otherPolicy, err := other.policy()
	if err != nil {
		t.Fatal(err)
	}
	if policy.Name != otherPolicy.Name || !reflect.DeepEqual(policy.Spec, otherPolicy.Spec) {
		t.Fatal("retained service account origin policy changed across release sequences")
	}
}

func TestServiceAccountOriginGuardPrepareWaitsForLiveDryRunDenial(t *testing.T) {
	guard := testServiceAccountOriginGuard()
	policy, err := guard.policy()
	if err != nil {
		t.Fatal(err)
	}
	name := ServiceAccountOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	guard.Policies = &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{name: readyPolicy(policy)}}
	guard.Bindings = &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{name: guard.binding()}}
	requests := &serviceAccountOriginTokenClient{acceptedBeforeDenial: 1}
	guard.TokenRequests = requests
	if err := guard.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.calls != 2 {
		t.Fatalf("TokenRequest probes = %d, want one accepted propagation probe and one denied proof", requests.calls)
	}
	if requests.serviceAccountName != guard.HookServiceAccountName {
		t.Fatalf("TokenRequest ServiceAccount = %q, want %q", requests.serviceAccountName, guard.HookServiceAccountName)
	}
	if requests.request == nil || requests.request.Spec.BoundObjectRef != nil {
		t.Fatal("origin enforcement proof must use an unbound TokenRequest")
	}
	if !reflect.DeepEqual(requests.options.DryRun, []string{metav1.DryRunAll}) {
		t.Fatalf("TokenRequest dry-run = %v, want All", requests.options.DryRun)
	}
	if requests.acceptedResponse.Status.Token != "" {
		t.Fatal("accepted propagation response retained an opaque credential")
	}
}

func TestServiceAccountOriginGuardPrepareRejectsUnexpectedTokenRequestError(t *testing.T) {
	guard := testServiceAccountOriginGuard()
	policy, err := guard.policy()
	if err != nil {
		t.Fatal(err)
	}
	name := ServiceAccountOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	guard.Policies = &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{name: readyPolicy(policy)}}
	guard.Bindings = &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{name: guard.binding()}}
	guard.TokenRequests = &serviceAccountOriginTokenClient{err: errors.New("forbidden by unrelated authorization")}
	err = guard.Prepare(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unrelated authorization") {
		t.Fatalf("Prepare error = %v, want unexpected TokenRequest failure", err)
	}
}

func TestServiceAccountOriginGuardRejectsForeignSpecAndHookIdentity(t *testing.T) {
	guard := testServiceAccountOriginGuard()
	policy, err := guard.policy()
	if err != nil {
		t.Fatal(err)
	}
	mutated := policy.DeepCopy()
	ignore := admissionregistrationv1.Ignore
	mutated.Spec.FailurePolicy = &ignore
	if err := guard.verifyPolicy(mutated); err == nil || !strings.Contains(err.Error(), "immutable contract") {
		t.Fatalf("verifyPolicy error = %v, want immutable-contract refusal", err)
	}
	guard.HookServiceAccountName = "ptah-crd-v1-000000000000"
	if _, err := guard.policy(); err == nil || !strings.Contains(err.Error(), "candidate release identity") {
		t.Fatalf("policy error = %v, want hook identity refusal", err)
	}
}

func testServiceAccountOriginGuard() *ServiceAccountOriginGuard {
	managerImage := "registry.example/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	guard := &ServiceAccountOriginGuard{
		Policies:                      &rolloutPolicyClient{},
		Bindings:                      &rolloutBindingClient{},
		TokenRequests:                 &serviceAccountOriginTokenClient{},
		ReleaseName:                   "ptah",
		ReleaseNamespace:              "ptah-system",
		ControllerServiceAccountName:  "ptah-controller",
		CertificateServiceAccountName: "ptah-cert-rotator",
		ControllerDeploymentName:      "ptah-controller",
		CertificateDeploymentName:     "ptah-cert-rotator",
		ReleaseSequence:               1,
		ManagerImage:                  managerImage,
		PollEvery:                     time.Nanosecond,
	}
	guard.HookServiceAccountName = "ptah-crd-v1-" + hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)[:12]
	return guard
}

type serviceAccountOriginTokenClient struct {
	acceptedBeforeDenial int
	calls                int
	serviceAccountName   string
	request              *authenticationv1.TokenRequest
	options              metav1.CreateOptions
	acceptedResponse     *authenticationv1.TokenRequest
	err                  error
}

func (c *serviceAccountOriginTokenClient) CreateToken(
	_ context.Context,
	serviceAccountName string,
	request *authenticationv1.TokenRequest,
	options metav1.CreateOptions,
) (*authenticationv1.TokenRequest, error) {
	c.calls++
	c.serviceAccountName = serviceAccountName
	c.request = request.DeepCopy()
	c.options = options
	if c.err != nil {
		return nil, c.err
	}
	if c.calls <= c.acceptedBeforeDenial {
		c.acceptedResponse = &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: "must-not-be-retained"}}
		return c.acceptedResponse, nil
	}
	return nil, fmt.Errorf("admission denied: %s", serviceAccountOriginGuardDenialMessage())
}
