package crdupgrade

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	celgo "github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/ext"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const testRuntimeAdmissionContractB64 = "e30="

func TestRenderedRolloutGuardMatchesCompiledContract(t *testing.T) {
	path := os.Getenv("PTAH_ROLLOUT_GUARD_RENDER")
	if path == "" {
		t.Skip("PTAH_ROLLOUT_GUARD_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	policies := map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}
	bindings := map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	deployments := map[string]*appsv1.Deployment{}
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
		case "Deployment":
			var deployment appsv1.Deployment
			if err := json.Unmarshal(raw, &deployment); err != nil {
				t.Fatal(err)
			}
			deployments[deployment.Name] = &deployment
		case "ValidatingAdmissionPolicy":
			var policy admissionregistrationv1.ValidatingAdmissionPolicy
			if err := json.Unmarshal(raw, &policy); err != nil {
				t.Fatal(err)
			}
			policies[policy.Name] = &policy
		case "ValidatingAdmissionPolicyBinding":
			var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := json.Unmarshal(raw, &binding); err != nil {
				t.Fatal(err)
			}
			bindings[binding.Name] = &binding
		}
	}
	for name, policy := range policies {
		assertAdmissionPolicyCELHeadroom(t, "rendered "+name+" policy", policy)
	}

	managerImage := "ghcr.io/stokaro/ptah-operator@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	controllerDeployment := deployments["ptah-e2e-ptah-operator"]
	certificateDeployment := deployments["ptah-e2e-ptah-operator-cert-rotator"]
	if controllerDeployment == nil || certificateDeployment == nil {
		t.Fatal("rendered runtime Deployments are missing")
	}
	hookServiceAccount := "ptah-e2e-ptah-operator-crd-v1-" + hookIdentityDigest("ptah-e2e", "ptah-e2e", 1, managerImage)[:12]
	guard := &RolloutGuard{
		Policies:                           &rolloutPolicyClient{},
		Bindings:                           &rolloutBindingClient{},
		ReleaseName:                        "ptah-e2e",
		ReleaseNamespace:                   "ptah-e2e",
		CoordinationNamespace:              "ptah-e2e",
		LeaderElection:                     true,
		LeaderElectionID:                   "ptah-operator.operator.ptah.dev",
		WebhookServiceName:                 "ptah-e2e-ptah-operator-webhook",
		WebhookTimeoutSeconds:              5,
		WebhookSecretName:                  "ptah-e2e-ptah-operator-webhook-cert",
		WebhookPort:                        9443,
		CertificateHealthPort:              8081,
		HookServiceAccountName:             hookServiceAccount,
		ControllerServiceAccountName:       "ptah-e2e-ptah-operator",
		ControllerDeploymentName:           "ptah-e2e-ptah-operator",
		ControllerReplicas:                 *controllerDeployment.Spec.Replicas,
		CertificateDeploymentName:          "ptah-e2e-ptah-operator-cert-rotator",
		ControllerStateVersion:             1,
		AdmissionContractVersion:           1,
		ReleaseSequence:                    1,
		ManagerImage:                       managerImage,
		ControllerArgs:                     append([]string(nil), controllerDeployment.Spec.Template.Spec.Containers[0].Args...),
		CertificateArgs:                    append([]string(nil), certificateDeployment.Spec.Template.Spec.Containers[0].Args...),
		RuntimeDeploymentConfigExpressions: decodedManagerStringArrayArgument(t, controllerDeployment.Spec.Template.Spec.InitContainers[0].Args, "--runtime-deployment-config-expressions-b64="),
		RuntimePodConfigExpressions:        decodedManagerStringArrayArgument(t, controllerDeployment.Spec.Template.Spec.InitContainers[0].Args, "--runtime-pod-config-expressions-b64="),
		RuntimeAdmissionContractB64:        decodedManagerStringArgument(t, controllerDeployment.Spec.Template.Spec.InitContainers[0].Args, "--runtime-admission-contract-b64="),
		PollEvery:                          time.Millisecond,
	}
	rolloutName := RolloutGuardPolicyName(guard.ReleaseSequence)
	runtimeName := RuntimeGuardPolicyName(guard.ReleaseSequence)
	hookName := HookIdentityGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	hookProbeName := HookIdentityProbeGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	if _, _, err := guard.verifyPolicy(policies[rolloutName]); err != nil {
		t.Fatalf("rendered rollout policy: %v", err)
	}
	if _, _, _, err := guard.verifyRuntimePolicy(policies[runtimeName]); err != nil {
		t.Fatalf("rendered runtime policy: %v", err)
	}
	if err := guard.verifyHookIdentityPolicy(policies[hookName]); err != nil {
		t.Fatalf("rendered hook identity policy: %v", err)
	}
	if err := guard.verifyHookIdentityProbePolicy(policies[hookProbeName]); err != nil {
		t.Fatalf("rendered hook identity probe policy: %v", err)
	}
	for _, name := range []string{rolloutName, runtimeName, hookName, hookProbeName} {
		if err := guard.verifyBinding(bindings[name], name); err != nil {
			t.Fatalf("rendered %s binding: %v", name, err)
		}
	}
}

func TestRolloutGuardPrepareRequiresExactHelmCreatedRatchetsAndProvesEnforcement(t *testing.T) {
	guard, policies, bindings, deployments := readyRolloutGuard()
	rolloutName := RolloutGuardPolicyName(guard.ReleaseSequence)
	runtimeName := RuntimeGuardPolicyName(guard.ReleaseSequence)
	policies.objects[rolloutName] = readyPolicy(guard.policy(1, 1))
	policies.objects[runtimeName] = readyPolicy(guard.runtimePolicy(1, 1, guard.ManagerImage))
	bindings.objects[rolloutName] = guard.binding(rolloutName)
	bindings.objects[runtimeName] = guard.binding(runtimeName)

	if err := guard.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if policies.dryCreates != 0 || policies.realCreates != 0 || policies.dryUpdates != 0 || policies.realUpdates != 0 ||
		bindings.dryCreates != 0 || bindings.realCreates != 0 {
		t.Fatal("read-only rollout preparation attempted to author an admission policy")
	}
	if deployments.dryCreates != 2 {
		t.Fatalf("enforcement probe dry-run creates = %d, want 2", deployments.dryCreates)
	}
	want := guard.policy(1, 1)
	if !reflect.DeepEqual(policies.objects[rolloutName].Spec, want.Spec) {
		t.Fatal("verified rollout policy differs from candidate")
	}
	if policies.objects[rolloutName].Annotations[helmReleaseNameAnnotation] != "" {
		t.Fatal("persistent rollout policy unexpectedly became a Helm release resource")
	}
}

// This is intentionally a white-box test because the retained hook Pod policy
// is an internal upgrade contract rather than an exported API.
func TestHookIdentityPolicyScopesOptionalServiceAccount(t *testing.T) {
	t.Parallel()

	guard := runtimePodGuardFixture()
	teardownServiceAccount, err := TeardownServiceAccountName(guard.HookServiceAccountName, guard.ReleaseSequence)
	if err != nil {
		t.Fatal(err)
	}
	identityJob := HookIdentityProbeJobName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	preflightJob := guard.hookJobName("preflight")
	reconcileJob := guard.hookJobName("reconcile")
	quiesceJob := guard.hookJobName("teardown-quiesce")
	teardownJob := guard.hookJobName("teardown")
	policy := guard.hookIdentityPolicy()
	wantMatch := fmt.Sprintf(
		`request.namespace == %q && (((!has(request.subResource) || request.subResource == "") && ((has(object.spec.serviceAccountName) && object.spec.serviceAccountName in [%q, %q]) || (request.operation == "UPDATE" && has(oldObject.spec.serviceAccountName) && oldObject.spec.serviceAccountName in [%q, %q]))) || (has(request.subResource) && request.subResource != "" && (%s || %s || %s || %s || %s)))`,
		guard.ReleaseNamespace, guard.HookServiceAccountName, teardownServiceAccount, guard.HookServiceAccountName, teardownServiceAccount,
		generatedPodRequestNameExpression(identityJob), generatedPodRequestNameExpression(preflightJob), generatedPodRequestNameExpression(reconcileJob), generatedPodRequestNameExpression(quiesceJob), generatedPodRequestNameExpression(teardownJob),
	)
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatal("hook identity policy is not fail-closed")
	}
	if len(policy.Spec.MatchConditions) != 1 || policy.Spec.MatchConditions[0].Expression != wantMatch {
		t.Fatalf("optional ServiceAccount match condition\n got: %#v\nwant: %q", policy.Spec.MatchConditions, wantMatch)
	}
	wantValidation := fmt.Sprintf(
		`has(object.spec.serviceAccountName) && object.spec.serviceAccountName == (object.metadata.labels["batch.kubernetes.io/job-name"] == %q ? %q : %q)`,
		teardownJob, teardownServiceAccount, guard.HookServiceAccountName,
	)
	if len(policy.Spec.Validations) < 3 || policy.Spec.Validations[2].Expression != wantValidation {
		t.Fatalf("protected ServiceAccount validation\n got: %#v\nwant: %q", policy.Spec.Validations, wantValidation)
	}
}

func TestRolloutGuardPrepareRejectsMissingCandidatePolicy(t *testing.T) {
	guard, _, _, _ := readyRolloutGuard()
	err := guard.Prepare(context.Background())
	if err == nil || !strings.Contains(err.Error(), RolloutGuardPolicyName(guard.ReleaseSequence)) {
		t.Fatalf("Prepare error = %v, want missing candidate policy", err)
	}
}

func TestRolloutGuardEnforcementProbeCannotAcceptAnOlderGuardMessage(t *testing.T) {
	guard, _, _, _ := readyRolloutGuard()
	guard.ReleaseSequence = 2
	err := guard.waitEnforced(context.Background(), rolloutGuardDenialMessage(guard.ReleaseSequence), false)
	if err == nil || !strings.Contains(err.Error(), rolloutGuardDenialMessage(1)) {
		t.Fatalf("enforcement probe error = %v, want older guard message rejection", err)
	}
}

func TestRolloutGuardPrepareRejectsFutureOrTamperedPolicyBeforeMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RolloutGuard, *admissionregistrationv1.ValidatingAdmissionPolicy)
		want   string
	}{
		{
			name: "future floor",
			mutate: func(_ *RolloutGuard, policy *admissionregistrationv1.ValidatingAdmissionPolicy) {
				policy.Annotations[ControllerStateVersionAnnotation] = "2"
			},
			want: "not compatible",
		},
		{
			name: "tampered contract",
			mutate: func(_ *RolloutGuard, policy *admissionregistrationv1.ValidatingAdmissionPolicy) {
				policy.Spec.Validations[0].Expression = "true"
			},
			want: "spec differs",
		},
		{
			name: "foreign release",
			mutate: func(_ *RolloutGuard, policy *admissionregistrationv1.ValidatingAdmissionPolicy) {
				policy.Annotations[ReleaseNameAnnotation] = "foreign"
			},
			want: "foreign or incomplete ownership",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			guard, policies, bindings, _ := readyRolloutGuard()
			rolloutName := RolloutGuardPolicyName(guard.ReleaseSequence)
			runtimeName := RuntimeGuardPolicyName(guard.ReleaseSequence)
			policies.objects[rolloutName] = readyPolicy(guard.policy(1, 1))
			policies.objects[runtimeName] = readyPolicy(guard.runtimePolicy(1, 1, guard.ManagerImage))
			bindings.objects[rolloutName] = guard.binding(rolloutName)
			bindings.objects[runtimeName] = guard.binding(runtimeName)
			test.mutate(guard, policies.objects[rolloutName])

			err := guard.Prepare(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Prepare error = %v, want %q", err, test.want)
			}
			if policies.dryUpdates != 0 || policies.realUpdates != 0 {
				t.Fatal("invalid rollout policy was mutated")
			}
		})
	}
}

func TestRolloutGuardPrepareRejectsForeignBinding(t *testing.T) {
	guard, policies, bindings, _ := readyRolloutGuard()
	rolloutName := RolloutGuardPolicyName(guard.ReleaseSequence)
	runtimeName := RuntimeGuardPolicyName(guard.ReleaseSequence)
	policies.objects[rolloutName] = readyPolicy(guard.policy(1, 1))
	policies.objects[runtimeName] = readyPolicy(guard.runtimePolicy(1, 1, guard.ManagerImage))
	bindings.objects[rolloutName] = guard.binding(rolloutName)
	bindings.objects[runtimeName] = guard.binding(runtimeName)
	bindings.objects[rolloutName].Spec.PolicyName = "foreign"

	err := guard.Prepare(context.Background())
	if err == nil || !strings.Contains(err.Error(), "binding spec differs") {
		t.Fatalf("Prepare error = %v, want binding refusal", err)
	}
}

func TestRolloutGuardQuiescesLegacyDeploymentsAfterCompleteDryRun(t *testing.T) {
	guard, _, _, deployments := readyRolloutGuard()
	deployments.objects[guard.CertificateDeploymentName] = legacyDeployment(guard, guard.CertificateDeploymentName, "certificate-rotation")
	deployments.objects[guard.ControllerDeploymentName] = legacyDeployment(guard, guard.ControllerDeploymentName, "controller")

	if err := guard.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deployments.dryUpdates != 2 || deployments.realUpdates != 2 {
		t.Fatalf("Deployment updates dry/real=%d/%d, want 2/2", deployments.dryUpdates, deployments.realUpdates)
	}
	for _, name := range []string{guard.CertificateDeploymentName, guard.ControllerDeploymentName} {
		deployment := deployments.objects[name]
		if got := deployment.Annotations[ControllerStateVersionAnnotation]; got != "1" {
			t.Fatalf("Deployment %s state = %q, want 1", name, got)
		}
		if got := deployment.Annotations[ReleaseSequenceAnnotation]; got != "1" {
			t.Fatalf("Deployment %s release sequence = %q, want 1", name, got)
		}
		if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
			t.Fatalf("Deployment %s replicas were not quiesced", name)
		}
	}
	wantOrder := []string{guard.CertificateDeploymentName, guard.ControllerDeploymentName}
	if !reflect.DeepEqual(deployments.realScaleOrder, wantOrder) {
		t.Fatalf("scale order = %v, want %v", deployments.realScaleOrder, wantOrder)
	}
}

func TestRolloutGuardPreflightQuiesceDoesNotPersistStampOrScale(t *testing.T) {
	guard, _, _, deployments := readyRolloutGuard()
	for name, component := range map[string]string{
		guard.CertificateDeploymentName: "certificate-rotation",
		guard.ControllerDeploymentName:  "controller",
	} {
		deployments.objects[name] = legacyDeployment(guard, name, component)
	}
	before := map[string]*appsv1.Deployment{
		guard.CertificateDeploymentName: deployments.objects[guard.CertificateDeploymentName].DeepCopy(),
		guard.ControllerDeploymentName:  deployments.objects[guard.ControllerDeploymentName].DeepCopy(),
	}
	if err := guard.PreflightQuiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deployments.dryUpdates != 2 || deployments.realUpdates != 0 {
		t.Fatalf("preflight updates dry/real=%d/%d, want 2/0", deployments.dryUpdates, deployments.realUpdates)
	}
	for name, want := range before {
		if got := deployments.objects[name]; !reflect.DeepEqual(got, want) {
			t.Fatalf("preflight mutated Deployment %s", name)
		}
	}
}

func TestRolloutGuardQuiescePreflightsEveryAdoptionBeforeMutation(t *testing.T) {
	guard, _, _, deployments := readyRolloutGuard()
	deployments.objects[guard.CertificateDeploymentName] = legacyDeployment(guard, guard.CertificateDeploymentName, "certificate-rotation")
	deployments.objects[guard.ControllerDeploymentName] = legacyDeployment(guard, guard.ControllerDeploymentName, "controller")
	deployments.dryUpdateErrors[guard.ControllerDeploymentName] = errors.New("policy denied")

	err := guard.Quiesce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "quiescence") {
		t.Fatalf("Quiesce error = %v, want quiescence preflight refusal", err)
	}
	if deployments.realUpdates != 0 {
		t.Fatalf("real updates = %d, want 0", deployments.realUpdates)
	}
}

func TestRolloutGuardQuiesceRejectsForeignAndFutureDeployments(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*appsv1.Deployment)
		want   string
	}{
		{
			name: "foreign ownership",
			mutate: func(deployment *appsv1.Deployment) {
				deployment.Annotations[helmReleaseNameAnnotation] = "foreign"
			},
			want: "foreign or incomplete Helm ownership",
		},
		{
			name: "future state",
			mutate: func(deployment *appsv1.Deployment) {
				deployment.Annotations[ControllerStateVersionAnnotation] = "2"
			},
			want: "rollback refused",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			guard, _, _, deployments := readyRolloutGuard()
			deployment := legacyDeployment(guard, guard.ControllerDeploymentName, "controller")
			test.mutate(deployment)
			deployments.objects[deployment.Name] = deployment

			err := guard.Quiesce(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Quiesce error = %v, want %q", err, test.want)
			}
			if deployments.dryUpdates != 0 || deployments.realUpdates != 0 {
				t.Fatal("invalid Deployment was mutated")
			}
		})
	}
}

func TestRolloutGuardQuiesceWaitsForSelectedPodsToDisappear(t *testing.T) {
	guard, _, _, deployments := readyRolloutGuard()
	deployment := legacyDeployment(guard, guard.ControllerDeploymentName, "controller")
	deployment.Annotations[ControllerStateVersionAnnotation] = "1"
	deployments.objects[deployment.Name] = deployment
	guard.Pods = &rolloutPodClient{items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "still-running"}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	err := guard.Quiesce(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Quiesce error = %v, want deadline while selected Pod remains", err)
	}
}

func TestRolloutPolicyCoversScaleAndActivationAwareRecovery(t *testing.T) {
	guard, _, _, _ := readyRolloutGuard()
	policy := guard.policy(7, 5)
	serializedBytes, err := json.Marshal(policy.Spec)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(serializedBytes)
	for _, required := range []string{
		"deployments/scale",
		"mutatingwebhookconfigurations",
		"validatingwebhookconfigurations",
	} {
		if !strings.Contains(serialized, required) {
			t.Fatalf("policy does not contain %q", required)
		}
	}
	expressions := make([]string, 0, len(policy.Spec.Validations))
	for _, validation := range policy.Spec.Validations {
		expressions = append(expressions, validation.Expression, validation.Message)
	}
	joined := strings.Join(expressions, "\n")
	for _, required := range []string{
		"variables.activationValid",
		"variables.isActiveIdentity || variables.stopTransition",
		"variables.newRelease != 1 || variables.newState == 7",
		"variables.newAdmission == 5",
		ControllerDeploymentAnnotation,
		ControllerServiceAccountAnnotation,
		rolloutGuardDenialMessage(guard.ReleaseSequence),
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("policy validations do not contain %q", required)
		}
	}
}

func TestRolloutEnforcementProbeCannotUseBootstrapRecoveryException(t *testing.T) {
	t.Parallel()

	guard, _, _, _ := readyRolloutGuard()
	probe := guard.probeDeployment(false)
	if got := probe.Annotations[ReleaseSequenceAnnotation]; got != strconv.FormatInt(int64(guard.ReleaseSequence), 10) {
		t.Fatalf("probe release sequence = %q, want candidate sequence", got)
	}
	if _, found := probe.Annotations[ControllerStateVersionAnnotation]; found {
		t.Fatal("rollout enforcement probe unexpectedly has a valid controller-state marker")
	}
}

func TestRuntimePolicyBindsSingleTrustedContainerAndExactVerifierShape(t *testing.T) {
	guard, _, _, _ := readyRolloutGuard()
	policy := guard.runtimePolicy(guard.ControllerStateVersion, guard.ReleaseSequence, guard.ManagerImage)
	if policy.Spec.MatchConstraints.NamespaceSelector == nil || policy.Spec.MatchConstraints.ObjectSelector == nil {
		t.Fatal("runtime policy must render API-defaulted empty selectors explicitly")
	}
	serializedBytes, err := json.Marshal(policy.Spec)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(serializedBytes)
	for _, required := range []string{
		"containers.size() == 1",
		"initContainers.size() == 1",
		"serviceAccountName == variables.runtimeServiceAccount",
		`--executor-image=registry.example/executor@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb`,
		`--controller-runtime-args-b64=`,
		`--verify-controller-state=true`,
		"volumeMounts.size() == 3",
		`terminationMessagePath == \"/dev/termination-log\"`,
		`(!has(object.spec.template.spec.initContainers[0].restartPolicyRules) || object.spec.template.spec.initContainers[0].restartPolicyRules.size() == 0)`,
		`(!has(object.spec.template.spec.containers[0].restartPolicyRules) || object.spec.template.spec.containers[0].restartPolicyRules.size() == 0)`,
		`!has(object.spec.template.spec.containers[0].securityContext.seccompProfile)`,
		"!has(object.spec.template.spec.containers[0].lifecycle)",
	} {
		if !strings.Contains(serialized, required) {
			t.Fatalf("runtime policy does not contain %q", required)
		}
	}
}

// This is intentionally a white-box test because the activation-aware
// transition matrix is an internal retained-policy contract that is observable
// only while two release policies overlap in the API server.
func TestRolloutAndRuntimeDeploymentActivationTruthTable(t *testing.T) {
	t.Parallel()

	guard := runtimePodGuardFixture()
	guard.ReleaseSequence = 2
	guard.ControllerStateVersion = 2
	guard.AdmissionContractVersion = 2
	guard.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("2", 64)
	guard.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)[:12]
	rolloutPolicy := guard.policy(guard.ControllerStateVersion, guard.AdmissionContractVersion)
	runtimePolicy := guard.runtimePolicy(guard.ControllerStateVersion, guard.ReleaseSequence, guard.ManagerImage)
	assertAdmissionPolicyCELHeadroom(t, "activation-aware rollout policy", rolloutPolicy)
	assertAdmissionPolicyCELHeadroom(t, "activation-aware runtime policy", runtimePolicy)
	predecessorImage := "registry.example/ptah@sha256:" + strings.Repeat("1", 64)

	tests := []struct {
		name          string
		active        int64
		activeState   int64
		activeImage   string
		operation     string
		subresource   string
		actor         string
		oldMarker     int64
		newMarker     int64
		changeRuntime bool
		wantRollout   bool
		wantRuntime   bool
		malformed     bool
	}{
		{name: "bootstrap annotation-free create", operation: "CREATE", actor: "helm", activeImage: guard.ManagerImage, wantRollout: true, wantRuntime: true},
		{name: "bootstrap candidate create denied", operation: "CREATE", actor: rolloutHookUsername(guard), newMarker: 2, activeImage: guard.ManagerImage},
		{name: "bootstrap candidate stop", operation: "UPDATE", actor: rolloutHookUsername(guard), newMarker: 2, activeImage: guard.ManagerImage, wantRollout: true, wantRuntime: true},
		{name: "active predecessor create", active: 1, activeState: 1, activeImage: predecessorImage, operation: "CREATE", actor: "helm", newMarker: 1, wantRollout: true, wantRuntime: true},
		{name: "active predecessor update", active: 1, activeState: 1, activeImage: predecessorImage, operation: "UPDATE", actor: "helm", oldMarker: 1, newMarker: 1, wantRollout: true, wantRuntime: true},
		{name: "rollback staged stop to active predecessor", active: 1, activeState: 1, activeImage: predecessorImage, operation: "UPDATE", actor: "helm", oldMarker: 2, newMarker: 1, wantRollout: true, wantRuntime: true},
		{name: "candidate stop before activation", active: 1, activeState: 1, activeImage: predecessorImage, operation: "UPDATE", actor: rolloutHookUsername(guard), oldMarker: 1, newMarker: 2, wantRollout: true, wantRuntime: true},
		{name: "candidate stop requires exact preserved runtime", active: 1, activeState: 1, activeImage: predecessorImage, operation: "UPDATE", actor: rolloutHookUsername(guard), oldMarker: 1, newMarker: 2, changeRuntime: true},
		{name: "candidate stop requires hook", active: 1, activeState: 1, activeImage: predecessorImage, operation: "UPDATE", actor: "helm", oldMarker: 1, newMarker: 2},
		{name: "scale is always denied", active: 1, activeState: 1, activeImage: predecessorImage, operation: "UPDATE", subresource: "scale", actor: "helm", oldMarker: 1, newMarker: 1, wantRuntime: true},
		{name: "activated candidate update", active: 2, activeState: 2, activeImage: guard.ManagerImage, operation: "UPDATE", actor: "helm", oldMarker: 2, newMarker: 2, wantRollout: true, wantRuntime: true},
		{name: "activated predecessor denied", active: 2, activeState: 2, activeImage: guard.ManagerImage, operation: "UPDATE", actor: "helm", oldMarker: 1, newMarker: 1},
		{name: "malformed activation denied", active: 1, activeState: 1, activeImage: predecessorImage, operation: "UPDATE", actor: "helm", oldMarker: 1, newMarker: 1, malformed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			activeState := test.activeState
			if activeState == 0 {
				activeState = int64(guard.ControllerStateVersion)
			}
			paramsRelease := test.active
			if paramsRelease == 0 {
				paramsRelease = int64(guard.ReleaseSequence)
			}
			params := rolloutActivationCELObject(guard, test.active, activeState, activeState, paramsRelease, test.activeImage)
			if test.malformed {
				params["metadata"].(map[string]any)["labels"].(map[string]any)["app.kubernetes.io/component"] = "foreign"
			}
			oldObject := rolloutDeploymentCELObject(guard, test.oldMarker, test.activeState, test.activeImage)
			object := rolloutDeploymentCELObject(guard, test.newMarker, markerState(test.newMarker, guard.ControllerStateVersion), test.activeImage)
			if test.operation == "UPDATE" && test.newMarker == int64(guard.ReleaseSequence) && test.oldMarker != test.newMarker {
				object["spec"].(map[string]any)["replicas"] = int64(0)
				object["spec"].(map[string]any)["template"] = rolloutCELClone(t, oldObject["spec"].(map[string]any)["template"])
			}
			if test.changeRuntime {
				object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"] = guard.ManagerImage
			}
			request := rolloutRequestCELObject(guard, test.operation, test.subresource, test.actor)
			if got := evaluatePolicyValidationPrefix(t, rolloutPolicy, 7, object, oldObject, request, params); got != test.wantRollout {
				t.Fatalf("rollout activation decision = %t, want %t", got, test.wantRollout)
			}
			if test.subresource == "scale" {
				return
			}
			if got := evaluatePolicyValidationPrefix(t, runtimePolicy, 3, object, oldObject, request, params); got != test.wantRuntime {
				t.Fatalf("runtime activation decision = %t, want %t", got, test.wantRuntime)
			}
		})
	}
}

// This is intentionally a white-box test because an admission configuration
// must remain on the active identity until the retained activation parameter
// commits the candidate sequence.
func TestRolloutAdmissionActivationTruthTable(t *testing.T) {
	t.Parallel()

	guard := runtimePodGuardFixture()
	guard.ReleaseSequence = 2
	guard.ControllerStateVersion = 2
	guard.AdmissionContractVersion = 2
	guard.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("2", 64)
	guard.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)[:12]
	policy := guard.policy(guard.ControllerStateVersion, guard.AdmissionContractVersion)
	predecessorImage := "registry.example/ptah@sha256:" + strings.Repeat("1", 64)

	for _, test := range []struct {
		name        string
		active      int64
		activeState int64
		marker      int64
		markerState int64
		admission   int64
		image       string
		want        bool
	}{
		{name: "bootstrap annotation-free recovery", image: guard.ManagerImage, want: true},
		{name: "bootstrap candidate mutation denied", marker: 2, markerState: 2, admission: 2, image: guard.ManagerImage},
		{name: "active predecessor recovery", active: 1, activeState: 1, marker: 1, markerState: 1, admission: 1, image: predecessorImage, want: true},
		{name: "candidate mutation before activation denied", active: 1, activeState: 1, marker: 2, markerState: 2, admission: 2, image: predecessorImage},
		{name: "activated candidate exact identity", active: 2, activeState: 2, marker: 2, markerState: 2, admission: 2, image: guard.ManagerImage, want: true},
		{name: "activated predecessor denied", active: 2, activeState: 2, marker: 1, markerState: 1, admission: 1, image: guard.ManagerImage},
	} {
		t.Run(test.name, func(t *testing.T) {
			activeState := test.activeState
			if activeState == 0 {
				activeState = int64(guard.ControllerStateVersion)
			}
			paramsRelease := test.active
			if paramsRelease == 0 {
				paramsRelease = int64(guard.ReleaseSequence)
			}
			params := rolloutActivationCELObject(guard, test.active, activeState, activeState, paramsRelease, test.image)
			object := rolloutAdmissionCELObject(guard, test.marker, test.markerState, test.admission)
			request := map[string]any{
				"operation": "UPDATE",
				"name":      AdmissionConfigurationName,
				"dryRun":    false,
				"resource":  map[string]any{"group": "admissionregistration.k8s.io"},
				"userInfo":  map[string]any{"username": "helm"},
			}
			if got := evaluatePolicyValidationPrefix(t, policy, 7, object, object, request, params); got != test.want {
				t.Fatalf("admission activation decision = %t, want %t", got, test.want)
			}
		})
	}
}

func rolloutAdmissionCELObject(g *RolloutGuard, marker, state, admission int64) map[string]any {
	metadata := map[string]any{"name": AdmissionConfigurationName}
	if marker > 0 {
		metadata["annotations"] = map[string]any{
			ControllerStateVersionAnnotation:   strconv.FormatInt(state, 10),
			AdmissionContractVersionAnnotation: strconv.FormatInt(admission, 10),
			ReleaseSequenceAnnotation:          strconv.FormatInt(marker, 10),
			ReleaseNameAnnotation:              g.ReleaseName,
			ReleaseNamespaceAnnotation:         g.ReleaseNamespace,
			CoordinationAnnotation:             g.CoordinationNamespace,
			LeaderElectionAnnotation:           strconv.FormatBool(g.LeaderElection),
			LeaderElectionIDAnnotation:         g.LeaderElectionID,
			WebhookServiceAnnotation:           g.WebhookServiceName,
			HookServiceAccountAnnotation:       g.HookServiceAccountName,
			ControllerServiceAccountAnnotation: g.ControllerServiceAccountName,
			ControllerDeploymentAnnotation:     g.ControllerDeploymentName,
			CertificateDeploymentAnnotation:    g.CertificateDeploymentName,
		}
	}
	return map[string]any{"metadata": metadata}
}

func markerState(marker int64, candidate int32) int64 {
	if marker == 0 {
		return 0
	}
	if marker == int64(candidate) {
		return int64(candidate)
	}
	return marker
}

func rolloutHookUsername(g *RolloutGuard) string {
	return "system:serviceaccount:" + g.ReleaseNamespace + ":" + g.HookServiceAccountName
}

func rolloutActivationCELObject(g *RolloutGuard, active, state, admission, release int64, image string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"name":      ReleaseActivationName,
			"namespace": g.ReleaseNamespace,
			"annotations": map[string]any{
				"helm.sh/hook":                     "pre-install,pre-upgrade",
				"helm.sh/hook-weight":              releaseActivationHookWeight,
				"helm.sh/resource-policy":          "keep",
				rolloutGuardVersionAnnotation:      rolloutGuardVersion,
				ReleaseNameAnnotation:              g.ReleaseName,
				ReleaseNamespaceAnnotation:         g.ReleaseNamespace,
				ControllerStateVersionAnnotation:   strconv.FormatInt(state, 10),
				AdmissionContractVersionAnnotation: strconv.FormatInt(admission, 10),
				ReleaseSequenceAnnotation:          strconv.FormatInt(release, 10),
				ManagerImageAnnotation:             image,
			},
			"labels": map[string]any{
				managedByLabel:                rolloutGuardManagedBy,
				instanceLabel:                 g.ReleaseName,
				"app.kubernetes.io/component": rolloutGuardComponent,
			},
		},
		"data": map[string]any{activeReleaseDataKey: strconv.FormatInt(active, 10)},
	}
}

func rolloutDeploymentCELObject(g *RolloutGuard, marker, state int64, image string) map[string]any {
	metadata := map[string]any{
		"name":      g.ControllerDeploymentName,
		"namespace": g.ReleaseNamespace,
		"uid":       "deployment-uid",
		"labels":    map[string]any{instanceLabel: g.ReleaseName},
	}
	templateMetadata := map[string]any{"labels": map[string]any{instanceLabel: g.ReleaseName}}
	if marker > 0 {
		metadata["annotations"] = map[string]any{
			ControllerStateVersionAnnotation: strconv.FormatInt(state, 10),
			ReleaseSequenceAnnotation:        strconv.FormatInt(marker, 10),
		}
		templateMetadata["annotations"] = map[string]any{
			ControllerStateVersionAnnotation: strconv.FormatInt(state, 10),
			ReleaseSequenceAnnotation:        strconv.FormatInt(marker, 10),
		}
	}
	return map[string]any{
		"metadata": metadata,
		"spec": map[string]any{
			"replicas":        int64(1),
			"selector":        map[string]any{"matchLabels": map[string]any{instanceLabel: g.ReleaseName}},
			"strategy":        map[string]any{"type": "Recreate"},
			"minReadySeconds": int64(0),
			"template": map[string]any{
				"metadata": templateMetadata,
				"spec": map[string]any{
					"serviceAccountName": g.ControllerServiceAccountName,
					"containers":         []any{map[string]any{"name": "manager", "image": image}},
					"initContainers":     []any{map[string]any{"name": "verify-candidate-runtime", "image": image}},
				},
			},
		},
	}
}

func rolloutRequestCELObject(g *RolloutGuard, operation, subresource, actor string) map[string]any {
	request := map[string]any{
		"operation": operation,
		"namespace": g.ReleaseNamespace,
		"name":      g.ControllerDeploymentName,
		"dryRun":    false,
		"resource":  map[string]any{"group": "apps"},
		"userInfo":  map[string]any{"username": actor},
	}
	if subresource != "" {
		request["subResource"] = subresource
	}
	return request
}

func evaluatePolicyValidationPrefix(
	t *testing.T,
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
	validationCount int,
	object, oldObject, request map[string]any,
	params any,
) bool {
	t.Helper()
	values := map[string]any{
		"object":    object,
		"oldObject": oldObject,
		"request":   request,
		"params":    params,
	}
	variables := make(map[string]any, len(policy.Spec.Variables))
	for _, variable := range policy.Spec.Variables {
		variables[variable.Name] = evaluateRolloutCEL(t, variable.Expression, values, variables)
	}
	for index := 0; index < validationCount; index++ {
		result := evaluateRolloutCEL(t, policy.Spec.Validations[index].Expression, values, variables)
		allowed, ok := result.(bool)
		if !ok {
			t.Fatalf("validation %d result = %T(%v), want bool", index, result, result)
		}
		if !allowed {
			return false
		}
	}
	return true
}

func evaluatePolicyMatchConditions(
	t *testing.T,
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
	object, oldObject, request map[string]any,
	params any,
) bool {
	t.Helper()
	values := map[string]any{
		"object":    object,
		"oldObject": oldObject,
		"request":   request,
		"params":    params,
	}
	for index, condition := range policy.Spec.MatchConditions {
		result := evaluateRolloutCEL(t, condition.Expression, values, map[string]any{})
		matches, ok := result.(bool)
		if !ok {
			t.Fatalf("match condition %d result = %T(%v), want bool", index, result, result)
		}
		if !matches {
			return false
		}
	}
	return true
}

func evaluateRolloutCEL(t *testing.T, expression string, values, variables map[string]any) any {
	t.Helper()
	expression = strings.ReplaceAll(expression, " int(", " testInt(")
	expression = strings.ReplaceAll(expression, " string(", " testString(")
	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("oldObject", celgo.DynType),
		celgo.Variable("request", celgo.DynType),
		celgo.Variable("params", celgo.DynType),
		celgo.Variable("variables", celgo.DynType),
		celgo.Function("testInt", celgo.Overload(
			"ptah_test_dyn_to_int",
			[]*celgo.Type{celgo.DynType},
			celgo.IntType,
			celgo.UnaryBinding(func(value ref.Val) ref.Val {
				parsed, err := strconv.ParseInt(fmt.Sprint(value.Value()), 10, 64)
				if err != nil {
					return types.NewErr("parse integer: %v", err)
				}
				return types.Int(parsed)
			}),
		)),
		celgo.Function("testString", celgo.Overload(
			"ptah_test_dyn_to_string",
			[]*celgo.Type{celgo.DynType},
			celgo.StringType,
			celgo.UnaryBinding(func(value ref.Val) ref.Val {
				return types.String(fmt.Sprint(value.Value()))
			}),
		)),
		ext.Strings(),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := environment.Compile(expression)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile CEL %q: %v", expression, issues.Err())
	}
	program, err := environment.Program(ast)
	if err != nil {
		t.Fatalf("build CEL %q: %v", expression, err)
	}
	activation := make(map[string]any, len(values)+1)
	for name, value := range values {
		activation[name] = value
	}
	activation["variables"] = variables
	result, _, err := program.Eval(activation)
	if err != nil {
		t.Fatalf("evaluate CEL %q: %v", expression, err)
	}
	return result.Value()
}

func rolloutCELClone(t *testing.T, source any) any {
	t.Helper()
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var clone any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func readyRolloutGuard() (*RolloutGuard, *rolloutPolicyClient, *rolloutBindingClient, *rolloutDeploymentClient) {
	policies := &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}}
	bindings := &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}}
	deployments := &rolloutDeploymentClient{objects: map[string]*appsv1.Deployment{}, dryUpdateErrors: map[string]error{}}
	managerImage := "registry.example/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hookServiceAccount := "ptah-crd-v1-" + hookIdentityDigest("ptah-system", "ptah", 1, managerImage)[:12]
	guard := &RolloutGuard{
		Policies:                     policies,
		Bindings:                     bindings,
		Deployments:                  deployments,
		Pods:                         &rolloutPodClient{},
		ConfigMaps:                   &rolloutConfigMapClient{denyMalformedActivation: true},
		ReleaseName:                  "ptah",
		ReleaseNamespace:             "ptah-system",
		CoordinationNamespace:        "ptah-system",
		LeaderElection:               true,
		LeaderElectionID:             "ptah-operator.operator.ptah.dev",
		WebhookServiceName:           "ptah-webhook",
		WebhookTimeoutSeconds:        10,
		WebhookSecretName:            "ptah-webhook-cert",
		WebhookPort:                  9443,
		CertificateHealthPort:        8081,
		HookServiceAccountName:       hookServiceAccount,
		ControllerServiceAccountName: "ptah-controller",
		ControllerDeploymentName:     "ptah-controller",
		ControllerReplicas:           2,
		CertificateDeploymentName:    "ptah-cert-rotator",
		ControllerStateVersion:       1,
		AdmissionContractVersion:     1,
		ReleaseSequence:              1,
		ManagerImage:                 managerImage,
		ControllerArgs: []string{
			"--executor-image=registry.example/executor@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"--webhook-port=9443",
		},
		CertificateArgs: []string{
			"--namespace=ptah-system",
			"--secret-name=ptah-webhook-cert",
			"--health-bind-address=:8081",
		},
		RuntimeDeploymentConfigExpressions: []string{`object.spec.replicas == 2`},
		RuntimePodConfigExpressions:        []string{`object.spec.restartPolicy == "Always"`},
		RuntimeAdmissionContractB64:        testRuntimeAdmissionContractB64,
		PollEvery:                          time.Millisecond,
	}
	hookName := HookIdentityGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	policies.objects[hookName] = readyPolicy(guard.hookIdentityPolicy())
	bindings.objects[hookName] = guard.binding(hookName)
	hookProbeName := HookIdentityProbeGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	policies.objects[hookProbeName] = readyPolicy(guard.hookIdentityProbePolicy())
	bindings.objects[hookProbeName] = guard.binding(hookProbeName)
	activation := guard.releaseActivationGuard()
	activationName := ReleaseActivationGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	policies.objects[activationName] = readyPolicy(activation.policy())
	bindings.objects[activationName] = activation.binding()
	origin := NewServiceAccountOriginGuard(guard, nil)
	originName := ServiceAccountOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	originPolicy, err := origin.policy()
	if err != nil {
		panic(err)
	}
	policies.objects[originName] = readyPolicy(originPolicy)
	bindings.objects[originName] = origin.binding()
	namespaceGuard := NewNamespaceDeletionGuard(guard)
	namespaceGuardName := NamespaceDeletionGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	policies.objects[namespaceGuardName] = readyPolicy(namespaceGuard.policy())
	bindings.objects[namespaceGuardName] = namespaceGuard.binding()
	controllerWriteGuard := NewControllerWriteGuard(guard)
	controllerWriteGuardName := ControllerWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	policies.objects[controllerWriteGuardName] = readyPolicy(controllerWriteGuard.policy())
	bindings.objects[controllerWriteGuardName] = controllerWriteGuard.binding()
	certificateWriteGuard := NewCertificateWriteGuard(guard)
	for _, entry := range certificateWriteGuard.entries() {
		policies.objects[entry.name] = readyPolicy(certificateWriteGuard.policy(entry))
		bindings.objects[entry.name] = certificateWriteGuard.binding(entry)
	}
	controllerObjectGuard := NewControllerObjectGuard(guard)
	for _, entry := range controllerObjectGuard.entries() {
		policies.objects[entry.name] = readyPolicy(controllerObjectGuard.policy(entry))
		bindings.objects[entry.name] = controllerObjectGuard.binding(entry)
	}
	runtimePodName := RuntimePodGuardPolicyName(guard.ReleaseSequence)
	runtimePodPolicy, err := guard.runtimePodIdentityPolicy()
	if err != nil {
		panic(err)
	}
	runtimePodBinding, err := guard.runtimePodIdentityBinding()
	if err != nil {
		panic(err)
	}
	policies.objects[runtimePodName] = readyPolicy(runtimePodPolicy)
	bindings.objects[runtimePodName] = runtimePodBinding
	parent := NewParentWorkloadGuard(guard)
	for _, entry := range parent.entries() {
		policies.objects[entry.name] = readyPolicy(entry.policy)
		bindings.objects[entry.name] = entry.binding
	}
	guard.ConfigMaps.(*rolloutConfigMapClient).objects = map[string]*corev1.ConfigMap{
		ReleaseActivationName: activationObject(activation, 0),
	}
	return guard, policies, bindings, deployments
}

func decodedManagerStringArrayArgument(t *testing.T, args []string, prefix string) []string {
	t.Helper()
	for _, argument := range args {
		if !strings.HasPrefix(argument, prefix) {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(argument, prefix))
		if err != nil {
			t.Fatalf("decode %s: %v", prefix, err)
		}
		var values []string
		if err := json.Unmarshal(raw, &values); err != nil {
			t.Fatalf("parse %s: %v", prefix, err)
		}
		return values
	}
	t.Fatalf("manager argument %s is missing", prefix)
	return nil
}

func decodedManagerStringArgument(t *testing.T, args []string, prefix string) string {
	t.Helper()
	for _, argument := range args {
		if strings.HasPrefix(argument, prefix) {
			return strings.TrimPrefix(argument, prefix)
		}
	}
	t.Fatalf("manager argument %s is missing", prefix)
	return ""
}

func readyPolicy(policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	result := policy.DeepCopy()
	result.Generation = 1
	result.Status.ObservedGeneration = 1
	result.Status.TypeChecking = &admissionregistrationv1.TypeChecking{}
	return result
}

func legacyDeployment(guard *RolloutGuard, name, component string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: guard.ReleaseNamespace,
			Annotations: map[string]string{
				helmReleaseNameAnnotation:      guard.ReleaseName,
				helmReleaseNamespaceAnnotation: guard.ReleaseNamespace,
			},
			Labels: map[string]string{
				managedByLabel:                "Helm",
				instanceLabel:                 guard.ReleaseName,
				"app.kubernetes.io/component": component,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}}},
		},
		Status: appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1},
	}
}

type rolloutPolicyClient struct {
	objects     map[string]*admissionregistrationv1.ValidatingAdmissionPolicy
	dryCreates  int
	realCreates int
	dryUpdates  int
	realUpdates int
}

func (c *rolloutPolicyClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	object := c.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: admissionregistrationv1.GroupName, Resource: "validatingadmissionpolicies"}, name)
	}
	return object.DeepCopy(), nil
}

func (c *rolloutPolicyClient) Create(_ context.Context, policy *admissionregistrationv1.ValidatingAdmissionPolicy, options metav1.CreateOptions) (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	if len(options.DryRun) != 0 {
		c.dryCreates++
		return policy.DeepCopy(), nil
	}
	c.realCreates++
	c.objects[policy.Name] = readyPolicy(policy)
	return c.objects[policy.Name].DeepCopy(), nil
}

func (c *rolloutPolicyClient) Update(_ context.Context, policy *admissionregistrationv1.ValidatingAdmissionPolicy, options metav1.UpdateOptions) (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	if len(options.DryRun) != 0 {
		c.dryUpdates++
		return policy.DeepCopy(), nil
	}
	c.realUpdates++
	policy = policy.DeepCopy()
	policy.Generation++
	policy.Status.ObservedGeneration = policy.Generation
	policy.Status.TypeChecking = &admissionregistrationv1.TypeChecking{}
	c.objects[policy.Name] = policy
	return c.objects[policy.Name].DeepCopy(), nil
}

type rolloutBindingClient struct {
	objects     map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding
	dryCreates  int
	realCreates int
}

func (c *rolloutBindingClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	object := c.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: admissionregistrationv1.GroupName, Resource: "validatingadmissionpolicybindings"}, name)
	}
	return object.DeepCopy(), nil
}

func (c *rolloutBindingClient) Create(_ context.Context, binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding, options metav1.CreateOptions) (*admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	if len(options.DryRun) != 0 {
		c.dryCreates++
		return binding.DeepCopy(), nil
	}
	c.realCreates++
	c.objects[binding.Name] = binding.DeepCopy()
	return c.objects[binding.Name].DeepCopy(), nil
}

type rolloutDeploymentClient struct {
	objects         map[string]*appsv1.Deployment
	dryUpdateErrors map[string]error
	dryCreates      int
	dryUpdates      int
	realUpdates     int
	realScaleOrder  []string
}

func (c *rolloutDeploymentClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*appsv1.Deployment, error) {
	object, found := c.objects[name]
	if !found {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: appsv1.GroupName, Resource: "deployments"}, name)
	}
	return object.DeepCopy(), nil
}

func (c *rolloutDeploymentClient) Create(_ context.Context, deployment *appsv1.Deployment, options metav1.CreateOptions) (*appsv1.Deployment, error) {
	if len(options.DryRun) != 0 {
		c.dryCreates++
		if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas > 0 {
			return nil, errors.New(runtimeGuardDenialMessage(1))
		}
		return nil, errors.New(rolloutGuardDenialMessage(1))
	}
	return nil, errors.New("unexpected real Deployment create")
}

func (c *rolloutDeploymentClient) Update(_ context.Context, deployment *appsv1.Deployment, options metav1.UpdateOptions) (*appsv1.Deployment, error) {
	if len(options.DryRun) != 0 {
		c.dryUpdates++
		if deployment.Annotations == nil || deployment.Annotations[ControllerStateVersionAnnotation] == "" {
			return nil, errors.New(rolloutGuardDenialMessage(1))
		}
		if err := c.dryUpdateErrors[deployment.Name]; err != nil {
			return nil, err
		}
		return deployment.DeepCopy(), nil
	}
	c.realUpdates++
	updated := deployment.DeepCopy()
	if updated.Spec.Replicas != nil && *updated.Spec.Replicas == 0 {
		updated.Status = appsv1.DeploymentStatus{}
		c.realScaleOrder = append(c.realScaleOrder, updated.Name)
	}
	c.objects[updated.Name] = updated
	return updated.DeepCopy(), nil
}

type rolloutPodClient struct {
	items []corev1.Pod
}

type rolloutConfigMapClient struct {
	objects                 map[string]*corev1.ConfigMap
	denyMalformedActivation bool
}

func (c *rolloutConfigMapClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	if c.objects == nil || c.objects[name] == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	return c.objects[name].DeepCopy(), nil
}

func (c *rolloutConfigMapClient) Create(_ context.Context, object *corev1.ConfigMap, options metav1.CreateOptions) (*corev1.ConfigMap, error) {
	if len(options.DryRun) != 0 {
		return nil, errors.New(hookIdentityGuardDenialMessage(1))
	}
	if c.objects == nil {
		c.objects = map[string]*corev1.ConfigMap{}
	}
	c.objects[object.Name] = object.DeepCopy()
	return object.DeepCopy(), nil
}

func (c *rolloutConfigMapClient) Update(_ context.Context, object *corev1.ConfigMap, options metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	if len(options.DryRun) != 0 {
		if c.denyMalformedActivation && object.Name == ReleaseActivationName && len(object.Data) != 1 {
			return nil, errors.New(releaseActivationGuardDenialMessage())
		}
		if strings.HasPrefix(object.Name, "ptah-hook-probe-v") {
			return nil, errors.New(hookIdentityProbeGuardDenialMessage(1))
		}
		return object.DeepCopy(), nil
	}
	if c.objects == nil {
		c.objects = map[string]*corev1.ConfigMap{}
	}
	c.objects[object.Name] = object.DeepCopy()
	return object.DeepCopy(), nil
}

func (c *rolloutPodClient) List(_ context.Context, _ metav1.ListOptions) (*corev1.PodList, error) {
	return &corev1.PodList{Items: append([]corev1.Pod(nil), c.items...)}, nil
}
