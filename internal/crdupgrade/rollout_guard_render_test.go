package crdupgrade

// White-box testing required: the rollout guard family is verified against
// the live cluster through RolloutGuard's unexported verify methods, and this
// test has to hand the rendered chart to exactly those.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
)

// TestRenderedRolloutGuardFamilyMatchesCompiledContracts builds a RolloutGuard
// from the preflight hook Job the chart renders, the way ptah-crd-manager
// builds it from that Job's arguments, and requires the rollout, runtime, hook
// identity and hook identity probe guards the chart renders to pass the same
// verification the reconcile hook applies to the live ones. A live guard is
// created from the manifest, so a render the compiled contract refuses is a
// release cutover that refuses itself.
func TestRenderedRolloutGuardFamilyMatchesCompiledContracts(t *testing.T) {
	if os.Getenv("PTAH_PRIVILEGE_RENDER") == "" {
		t.Skip("PTAH_PRIVILEGE_RENDER is set by the chart contract gate")
	}
	// The recovery render carries certificateRotation.recreateMissingSecret,
	// which the e2e values set and which changes the runtime-verify contract.
	for _, variable := range []string{"PTAH_PRIVILEGE_RENDER", "PTAH_PRIVILEGE_RECOVERY_RENDER"} {
		path := os.Getenv(variable)
		if path == "" {
			t.Fatalf("%s is set by the chart contract gate", variable)
		}
		t.Run(variable, func(t *testing.T) {
			verifyRenderedRolloutGuardFamily(t, path)
		})
	}
}

func verifyRenderedRolloutGuardFamily(t *testing.T, path string) {
	t.Helper()
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	policies := map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}
	bindings := map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	var preflight *batchv1.Job
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
		// Retirement hooks reuse the guards' names on pre-delete. They are not
		// the guards the reconcile hook verifies.
		var partial metav1.PartialObjectMetadata
		if err := json.Unmarshal(raw, &partial); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(partial.Annotations["helm.sh/hook"], "pre-delete") {
			continue
		}
		switch typeMeta.Kind {
		case "ValidatingAdmissionPolicy":
			var object admissionregistrationv1.ValidatingAdmissionPolicy
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			policies[object.Name] = &object
		case "ValidatingAdmissionPolicyBinding":
			var object admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			bindings[object.Name] = &object
		case "Job":
			var object batchv1.Job
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			containers := object.Spec.Template.Spec.Containers
			if len(containers) == 1 && len(containers[0].Args) > 0 && containers[0].Args[0] == "preflight" {
				preflight = &object
			}
		}
	}
	if preflight == nil {
		t.Fatal("rendered chart has no preflight hook Job")
	}
	guard := renderedRolloutGuard(t, preflight.Spec.Template.Spec.Containers[0].Args, preflight.Spec.Template.Spec.PriorityClassName)

	rolloutName := RolloutGuardPolicyName(guard.ReleaseSequence)
	runtimeName := RuntimeGuardPolicyName(guard.ReleaseSequence)
	identityName := HookIdentityGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	probeName := HookIdentityProbeGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)

	t.Run("rollout guard policy", func(t *testing.T) {
		policy := policies[rolloutName]
		if _, _, err := guard.verifyPolicy(policy); err != nil {
			t.Fatalf("%v\n%s", err, renderedSpecDifference(policy, guard.policy(guard.ControllerStateVersion, guard.AdmissionContractVersion)))
		}
	})
	t.Run("runtime guard policy", func(t *testing.T) {
		policy := policies[runtimeName]
		if _, _, _, err := guard.verifyRuntimePolicy(policy); err != nil {
			t.Fatalf("%v\n%s", err, renderedSpecDifference(policy, guard.runtimePolicy(guard.ControllerStateVersion, guard.ReleaseSequence, guard.ManagerImage)))
		}
	})
	t.Run("hook identity guard policy", func(t *testing.T) {
		policy := policies[identityName]
		if err := guard.verifyHookIdentityPolicy(policy); err != nil {
			t.Fatalf("%v\n%s", err, renderedSpecDifference(policy, guard.hookIdentityPolicy()))
		}
	})
	t.Run("hook identity probe guard policy", func(t *testing.T) {
		policy := policies[probeName]
		if err := guard.verifyHookIdentityProbePolicy(policy); err != nil {
			t.Fatalf("%v\n%s", err, renderedSpecDifference(policy, guard.hookIdentityProbePolicy()))
		}
	})
	for _, name := range []string{rolloutName, runtimeName, identityName, probeName} {
		t.Run("binding "+name, func(t *testing.T) {
			if err := guard.verifyBinding(bindings[name], name); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// renderedRolloutGuard reads the manager arguments of the rendered preflight
// Job the way ptah-crd-manager reads its flags. The versions the manager takes
// from its own build are taken from the same constants.
func renderedRolloutGuard(t *testing.T, args []string, priorityClassName string) *RolloutGuard {
	t.Helper()
	values := map[string]string{}
	for _, argument := range args[1:] {
		flag, value, found := strings.Cut(strings.TrimPrefix(argument, "--"), "=")
		if !found {
			t.Fatalf("manager argument %q is not --flag=value", argument)
		}
		values[flag] = value
	}
	integer := func(flag string) int32 {
		parsed, err := strconv.ParseInt(values[flag], 10, 32)
		if err != nil {
			t.Fatalf("manager argument --%s=%q: %v", flag, values[flag], err)
		}
		return int32(parsed)
	}
	boolean := func(flag string) bool {
		parsed, err := strconv.ParseBool(values[flag])
		if err != nil {
			t.Fatalf("manager argument --%s=%q: %v", flag, values[flag], err)
		}
		return parsed
	}
	list := func(flag string) []string {
		decoded, err := base64.StdEncoding.DecodeString(values[flag])
		if err != nil {
			t.Fatalf("manager argument --%s: %v", flag, err)
		}
		var items []string
		if err := json.Unmarshal(decoded, &items); err != nil {
			t.Fatalf("manager argument --%s: %v", flag, err)
		}
		return items
	}
	var contract RuntimeAdmissionContract
	contractJSON, err := base64.StdEncoding.DecodeString(values["runtime-admission-contract-b64"])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(contractJSON, &contract); err != nil {
		t.Fatal(err)
	}
	previousSequence := int32(0)
	if values["previous-controller-release-sequence"] != "" {
		previousSequence = integer("previous-controller-release-sequence")
	}
	return &RolloutGuard{
		ReleaseName:                             values["release-name"],
		ReleaseNamespace:                        values["release-namespace"],
		CoordinationNamespace:                   values["coordination-namespace"],
		LeaderElection:                          boolean("leader-election"),
		LeaderElectionID:                        values["leader-election-id"],
		WebhookServiceName:                      values["webhook-service-name"],
		WebhookTimeoutSeconds:                   integer("webhook-timeout-seconds"),
		WebhookSecretName:                       values["webhook-secret-name"],
		WebhookPort:                             integer("webhook-port"),
		CertificateHealthPort:                   integer("certificate-health-port"),
		CertificateRuntimeEnabled:               contract.CertificateRuntimeEnabled,
		HookServiceAccountName:                  values["hook-service-account-name"],
		ControllerServiceAccountName:            values["controller-service-account-name"],
		ControllerServiceAccountManaged:         boolean("controller-service-account-managed"),
		PreviousControllerServiceAccountName:    values["previous-controller-service-account-name"],
		PreviousControllerServiceAccountUID:     types.UID(values["previous-controller-service-account-uid"]),
		PreviousControllerServiceAccountManaged: boolean("previous-controller-service-account-managed"),
		PreviousControllerReleaseSequence:       previousSequence,
		ControllerDeploymentName:                values["controller-deployment-name"],
		ControllerReplicas:                      integer("controller-replicas"),
		CertificateDeploymentName:               values["certificate-deployment-name"],
		ControllerStateVersion:                  controllerstate.CurrentVersion,
		AdmissionContractVersion:                CurrentAdmissionContractVersion,
		ReleaseSequence:                         integer("release-sequence"),
		ManagerImage:                            values["manager-image"],
		ControllerArgs:                          list("controller-runtime-args-b64"),
		CertificateArgs:                         list("certificate-runtime-args-b64"),
		RuntimeDeploymentConfigExpressions:      list("runtime-deployment-config-expressions-b64"),
		RuntimePodConfigExpressions:             list("runtime-pod-config-expressions-b64"),
		RuntimeAdmissionContractB64:             values["runtime-admission-contract-b64"],
		PriorityClassName:                       priorityClassName,
	}
}

// renderedSpecDifference reports the compiled contract's first disagreement
// with a rendered policy through the same helper the reconcile hook uses.
func renderedSpecDifference(rendered, compiled *admissionregistrationv1.ValidatingAdmissionPolicy) string {
	if rendered == nil {
		return "the chart renders no such policy"
	}
	return policySpecDifference(rendered.Spec, compiled.Spec)
}
