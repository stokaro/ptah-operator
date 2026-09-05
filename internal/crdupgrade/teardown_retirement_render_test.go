package crdupgrade

// These tests intentionally use the package under test: uninstall hook
// ordering and exact stored admission objects are one private chart/binary
// contract, not a public API.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestRenderedTeardownRetirementMatchesCompiledContract(t *testing.T) {
	tests := []struct {
		name                string
		certificateRecovery bool
	}{
		{name: "base inventory"},
		{name: "generated certificate recovery", certificateRecovery: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			objects := renderTeardownRetirementChart(t, test.certificateRecovery)
			guard := teardownRetirementGuardFromRender(t, objects)
			if test.certificateRecovery {
				var err error
				guard, err = guard.WithOriginalPairs(TeardownOriginalPairVerifier{
					Name: guard.rollout.CertificateDeploymentName,
					VerifyPolicy: func(*admissionregistrationv1.ValidatingAdmissionPolicy) error {
						return nil
					},
					VerifyBinding: func(*admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
						return nil
					},
				})
				if err != nil {
					t.Fatal(err)
				}
			}

			marker, err := guard.Marker()
			if err != nil {
				t.Fatal(err)
			}
			var renderedMarker corev1.ConfigMap
			decodeTeardownRetirementObject(t, findTeardownRetirementObject(t, objects, "ConfigMap", marker.Name, teardownRetirementMarkerHookWeight), &renderedMarker)
			if !reflect.DeepEqual(&renderedMarker, marker) {
				t.Fatal("rendered teardown probe ConfigMap differs from the exact compiled contract")
			}

			for _, fence := range []TeardownFence{TeardownFenceA, TeardownFenceB} {
				dormantPolicy, dormantBinding, _, err := guard.DormantFencePair(fence)
				if err != nil {
					t.Fatal(err)
				}
				assertRenderedTeardownRetirementPolicy(t, objects, dormantPolicy)
				assertRenderedTeardownRetirementBinding(t, objects, dormantBinding)

				originalPolicy, originalBinding, _, err := guard.OriginalFencePair(fence)
				if err != nil {
					t.Fatal(err)
				}
				assertRenderedTeardownRetirementPolicy(t, objects, originalPolicy)
				assertRenderedTeardownRetirementBinding(t, objects, originalBinding)
			}

			pairs, err := guard.RetirementPairs()
			if err != nil {
				t.Fatal(err)
			}
			wantCount := 28
			if test.certificateRecovery {
				wantCount++
			}
			if len(pairs) != wantCount {
				t.Fatalf("compiled retirement inventory has %d pairs, want %d", len(pairs), wantCount)
			}
			for _, pair := range pairs {
				assertRenderedTeardownRetirementPolicy(t, objects, pair.Policy)
				assertRenderedTeardownRetirementBinding(t, objects, pair.Binding)
			}
			legacyParentNames := []string{
				legacyParentHookJobOriginGuardPolicyName(guard.rollout.ReleaseNamespace, guard.rollout.ReleaseName),
				legacyParentHookPodOriginGuardPolicyName(guard.rollout.ReleaseNamespace, guard.rollout.ReleaseName),
			}
			for _, name := range legacyParentNames {
				pairIndex := slices.IndexFunc(pairs, func(pair TeardownRetirementPair) bool {
					return pair.Original.Name == name
				})
				if pairIndex < 0 || pairs[pairIndex].Original.OptionalGroup != legacyParentWorkloadOriginTeardownGroup {
					t.Fatalf("rendered retirement inventory does not classify parent v1 pair %s as exact optional legacy", name)
				}
			}

			certificateName := guard.rollout.CertificateDeploymentName
			certificatePairRendered := countTeardownRetirementObjects(objects, "ValidatingAdmissionPolicy", certificateName, "") != 0
			if certificatePairRendered != test.certificateRecovery {
				t.Fatalf("conditional certificate-recovery retirement pair rendered=%t, want %t", certificatePairRendered, test.certificateRecovery)
			}

			assertTeardownRetirementFinalResources(t, objects, guard)
		})
	}
}

func TestRenderedTeardownRetirementStableAnchorsDoNotDriftAcrossImages(t *testing.T) {
	first := renderTeardownRetirementChartWithDigest(t, false, strings.Repeat("2", 64))
	second := renderTeardownRetirementChartWithDigest(t, false, strings.Repeat("3", 64))
	firstGuard := teardownRetirementGuardFromRender(t, first)
	secondGuard := teardownRetirementGuardFromRender(t, second)
	if firstGuard.attempt() == secondGuard.attempt() || firstGuard.markerName() == secondGuard.markerName() {
		t.Fatal("candidate-specific retirement identities did not change with the manager image")
	}
	for _, fence := range []TeardownFence{TeardownFenceA, TeardownFenceB} {
		if firstGuard.fenceName(fence) != secondGuard.fenceName(fence) {
			t.Fatalf("stable fence %s drifted across manager images", fence)
		}
		for _, objects := range [][]*unstructured.Unstructured{first, second} {
			name := firstGuard.fenceName(fence)
			if countTeardownRetirementObjects(objects, "ValidatingAdmissionPolicy", name, "") != 2 ||
				countTeardownRetirementObjects(objects, "ValidatingAdmissionPolicyBinding", name, "") != 2 {
				t.Fatalf("stable fence %s does not have exactly one ordinary anchor and one pre-delete replacement", fence)
			}
		}
	}
}

func renderTeardownRetirementChart(t *testing.T, certificateRecovery bool) []*unstructured.Unstructured {
	t.Helper()
	return renderTeardownRetirementChartWithDigest(t, certificateRecovery, strings.Repeat("2", 64))
}

func renderTeardownRetirementChartWithDigest(t *testing.T, certificateRecovery bool, digest string) []*unstructured.Unstructured {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for teardown retirement render tests")
	}
	_, filename, _, _ := goruntime.Caller(0)
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	args := []string{
		"template", "ptah-e2e", filepath.Join(repositoryRoot, "charts", "ptah-operator"),
		"--namespace", "ptah-e2e",
		"--show-only", "templates/teardown-retirement.yaml",
		"--set-string", "image.digest=sha256:" + digest,
		"--set-string", "execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"--set-string", "execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"--set-string", "execution.ptahVersion=e2e-explicit-version",
	}
	if certificateRecovery {
		args = append(args, "--set", "certificateRotation.recreateMissingSecret=true")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, helm, args...)
	temporaryHome := t.TempDir()
	command.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(temporaryHome, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(temporaryHome, "config"),
		"HELM_DATA_HOME="+filepath.Join(temporaryHome, "data"),
	)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("helm template teardown retirement failed: %v", err)
	}

	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	var objects []*unstructured.Unstructured
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode teardown retirement Helm object: %v", err)
		}
		if object.Object != nil && object.GetKind() != "" {
			objects = append(objects, object)
		}
	}
	return objects
}

func teardownRetirementGuardFromRender(t *testing.T, objects []*unstructured.Unstructured) *TeardownRetirementGuard {
	t.Helper()
	finalJobObject := findTeardownRetirementObjectByComponent(t, objects, "Job", teardownRetirementComponent, "105")
	var finalJob batchv1.Job
	decodeTeardownRetirementObject(t, finalJobObject, &finalJob)
	if len(finalJob.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("rendered final retirement Job has %d containers, want 1", len(finalJob.Spec.Template.Spec.Containers))
	}
	args := finalJob.Spec.Template.Spec.Containers[0].Args
	parseBool := func(name string) bool {
		value, err := strconv.ParseBool(teardownRetirementRenderArgument(t, args, name))
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return value
	}
	parseInt32 := func(name string) int32 {
		value, err := strconv.ParseInt(teardownRetirementRenderArgument(t, args, name), 10, 32)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return int32(value)
	}
	rollout := &RolloutGuard{
		Policies:                                &rolloutPolicyClient{},
		Bindings:                                &rolloutBindingClient{},
		ReleaseName:                             teardownRetirementRenderArgument(t, args, "--release-name="),
		ReleaseNamespace:                        teardownRetirementRenderArgument(t, args, "--release-namespace="),
		CoordinationNamespace:                   teardownRetirementRenderArgument(t, args, "--coordination-namespace="),
		LeaderElection:                          parseBool("--leader-election="),
		LeaderElectionID:                        teardownRetirementRenderArgument(t, args, "--leader-election-id="),
		WebhookServiceName:                      teardownRetirementRenderArgument(t, args, "--webhook-service-name="),
		WebhookTimeoutSeconds:                   parseInt32("--webhook-timeout-seconds="),
		WebhookSecretName:                       teardownRetirementRenderArgument(t, args, "--webhook-secret-name="),
		WebhookPort:                             parseInt32("--webhook-port="),
		CertificateHealthPort:                   parseInt32("--certificate-health-port="),
		HookServiceAccountName:                  teardownRetirementRenderArgument(t, args, "--hook-service-account-name="),
		ControllerServiceAccountName:            teardownRetirementRenderArgument(t, args, "--controller-service-account-name="),
		ControllerServiceAccountManaged:         parseBool("--controller-service-account-managed="),
		PreviousControllerServiceAccountName:    teardownRetirementRenderArgument(t, args, "--previous-controller-service-account-name="),
		PreviousControllerServiceAccountUID:     types.UID(teardownRetirementRenderArgument(t, args, "--previous-controller-service-account-uid=")),
		PreviousControllerServiceAccountManaged: parseBool("--previous-controller-service-account-managed="),
		PreviousControllerReleaseSequence:       parseInt32("--previous-controller-release-sequence="),
		ControllerDeploymentName:                teardownRetirementRenderArgument(t, args, "--controller-deployment-name="),
		ControllerReplicas:                      parseInt32("--controller-replicas="),
		CertificateDeploymentName:               teardownRetirementRenderArgument(t, args, "--certificate-deployment-name="),
		ControllerStateVersion:                  1,
		AdmissionContractVersion:                1,
		ReleaseSequence:                         parseInt32("--release-sequence="),
		ManagerImage:                            teardownRetirementRenderArgument(t, args, "--manager-image="),
		ControllerArgs:                          decodedManagerStringArrayArgument(t, args, "--controller-runtime-args-b64="),
		CertificateArgs:                         decodedManagerStringArrayArgument(t, args, "--certificate-runtime-args-b64="),
		RuntimeDeploymentConfigExpressions:      decodedManagerStringArrayArgument(t, args, "--runtime-deployment-config-expressions-b64="),
		RuntimePodConfigExpressions:             decodedManagerStringArrayArgument(t, args, "--runtime-pod-config-expressions-b64="),
		RuntimeAdmissionContractB64:             teardownRetirementRenderArgument(t, args, "--runtime-admission-contract-b64="),
		PollEvery:                               time.Millisecond,
	}
	guard := NewTeardownRetirementGuard(rollout)
	if err := guard.validate(); err != nil {
		t.Fatalf("rendered teardown retirement identity is invalid: %v", err)
	}
	return guard
}

func assertRenderedTeardownRetirementPolicy(t *testing.T, objects []*unstructured.Unstructured, want *admissionregistrationv1.ValidatingAdmissionPolicy) {
	t.Helper()
	object := findTeardownRetirementObject(t, objects, want.Kind, want.Name, want.Annotations["helm.sh/hook-weight"])
	var got admissionregistrationv1.ValidatingAdmissionPolicy
	decodeTeardownRetirementObject(t, object, &got)
	if !exactTeardownRetirementMetadata(got.ObjectMeta, want.ObjectMeta) {
		t.Fatalf("rendered ValidatingAdmissionPolicy/%s at weight %s metadata differs from the exact compiled contract", want.Name, want.Annotations["helm.sh/hook-weight"])
	}
	if difference := teardownRetirementPolicyDifference(&got, want); difference != "" {
		t.Fatalf("rendered ValidatingAdmissionPolicy/%s at weight %s differs from the exact compiled contract: %s", want.Name, want.Annotations["helm.sh/hook-weight"], difference)
	}
}

func teardownRetirementPolicyDifference(got, want *admissionregistrationv1.ValidatingAdmissionPolicy) string {
	if !reflect.DeepEqual(got.Spec.FailurePolicy, want.Spec.FailurePolicy) {
		return "failurePolicy"
	}
	if !reflect.DeepEqual(got.Spec.MatchConstraints, want.Spec.MatchConstraints) {
		return "matchConstraints"
	}
	if !reflect.DeepEqual(got.Spec.ParamKind, want.Spec.ParamKind) {
		return "paramKind"
	}
	if !reflect.DeepEqual(got.Spec.AuditAnnotations, want.Spec.AuditAnnotations) {
		return "auditAnnotations"
	}
	if !reflect.DeepEqual(got.Spec.MatchConditions, want.Spec.MatchConditions) {
		for index := 0; index < min(len(got.Spec.MatchConditions), len(want.Spec.MatchConditions)); index++ {
			if got.Spec.MatchConditions[index] != want.Spec.MatchConditions[index] {
				return "matchCondition " + strconv.Itoa(index)
			}
		}
		return "matchCondition count"
	}
	if !reflect.DeepEqual(got.Spec.Variables, want.Spec.Variables) {
		for index := 0; index < min(len(got.Spec.Variables), len(want.Spec.Variables)); index++ {
			if got.Spec.Variables[index] != want.Spec.Variables[index] {
				return "variable " + strconv.Itoa(index) + " got=" + got.Spec.Variables[index].Expression + " want=" + want.Spec.Variables[index].Expression
			}
		}
		return "variable count got=" + strconv.Itoa(len(got.Spec.Variables)) + " want=" + strconv.Itoa(len(want.Spec.Variables))
	}
	if !reflect.DeepEqual(got.Spec.Validations, want.Spec.Validations) {
		for index := 0; index < min(len(got.Spec.Validations), len(want.Spec.Validations)); index++ {
			if got.Spec.Validations[index] != want.Spec.Validations[index] {
				return "validation " + strconv.Itoa(index) + " got=" + got.Spec.Validations[index].Expression + " want=" + want.Spec.Validations[index].Expression
			}
		}
		return "validation count got=" + strconv.Itoa(len(got.Spec.Validations)) + " want=" + strconv.Itoa(len(want.Spec.Validations))
	}
	return ""
}

func assertRenderedTeardownRetirementBinding(t *testing.T, objects []*unstructured.Unstructured, want *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	t.Helper()
	object := findTeardownRetirementObject(t, objects, want.Kind, want.Name, want.Annotations["helm.sh/hook-weight"])
	var got admissionregistrationv1.ValidatingAdmissionPolicyBinding
	decodeTeardownRetirementObject(t, object, &got)
	if !exactTeardownRetirementMetadata(got.ObjectMeta, want.ObjectMeta) || !reflect.DeepEqual(got.Spec, want.Spec) {
		t.Fatalf("rendered ValidatingAdmissionPolicyBinding/%s at weight %s differs from the exact compiled contract", want.Name, want.Annotations["helm.sh/hook-weight"])
	}
}

func assertTeardownRetirementFinalResources(t *testing.T, objects []*unstructured.Unstructured, guard *TeardownRetirementGuard) {
	t.Helper()
	finalJobObject := findTeardownRetirementObjectByComponent(t, objects, "Job", teardownRetirementComponent, "105")
	var job batchv1.Job
	decodeTeardownRetirementObject(t, finalJobObject, &job)
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 270 || job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatal("rendered final retirement Job deadline, retry, or container contract differs")
	}
	container := job.Spec.Template.Spec.Containers[0]
	if job.Name != guard.finalJobName() || job.Spec.Template.Spec.ServiceAccountName != guard.cleanupServiceAccountName() ||
		container.Name != teardownRetirementComponent || container.Image != guard.rollout.ManagerImage ||
		!slices.Equal(container.Command, []string{"/ptah-crd-manager"}) || !slices.Equal(container.Args, guard.hookArgsWithTimeout("teardown-retirement-final", "240s")) {
		t.Fatal("rendered final retirement Job identity or command differs from the exact compiled contract")
	}

	for _, object := range objects {
		if object.GetAnnotations()["helm.sh/hook"] == "post-delete" {
			t.Fatalf("%s/%s is an essential non-retriable post-delete hook", object.GetKind(), object.GetName())
		}
		if weight := object.GetAnnotations()["helm.sh/hook-weight"]; weight == "100" || weight == "101" || weight == "102" || weight == "103" {
			t.Fatalf("%s/%s retains obsolete final-only RBAC at weight %s", object.GetKind(), object.GetName(), weight)
		}
		if object.GetKind() != "Role" && object.GetKind() != "ClusterRole" {
			continue
		}
		component := object.GetLabels()["app.kubernetes.io/component"]
		if component != teardownRetirementComponent && component != "teardown-retirement-bootstrap" {
			continue
		}
		var rules []rbacv1.PolicyRule
		if object.GetKind() == "Role" {
			var rendered rbacv1.Role
			decodeTeardownRetirementObject(t, object, &rendered)
			rules = rendered.Rules
		} else {
			var rendered rbacv1.ClusterRole
			decodeTeardownRetirementObject(t, object, &rendered)
			rules = rendered.Rules
		}
		for _, rule := range rules {
			if !slices.Contains(rule.APIGroups, admissionregistrationv1.GroupName) {
				continue
			}
			for _, verb := range rule.Verbs {
				if verb != "get" {
					t.Fatalf("%s/%s grants admission mutation verb %q", object.GetKind(), object.GetName(), verb)
				}
			}
		}
	}
}

func findTeardownRetirementObject(t *testing.T, objects []*unstructured.Unstructured, kind, name, weight string) *unstructured.Unstructured {
	t.Helper()
	var found *unstructured.Unstructured
	for _, object := range objects {
		if object.GetKind() != kind || object.GetName() != name {
			continue
		}
		objectWeight, hasWeight := object.GetAnnotations()["helm.sh/hook-weight"]
		if (weight == "" && hasWeight) || (weight != "" && objectWeight != weight) {
			continue
		}
		if found != nil {
			t.Fatalf("rendered %s/%s at weight %s is duplicated", kind, name, weight)
		}
		found = object
	}
	if found == nil {
		t.Fatalf("rendered %s/%s at weight %s is missing", kind, name, weight)
	}
	return found
}

func findTeardownRetirementObjectByComponent(t *testing.T, objects []*unstructured.Unstructured, kind, component, weight string) *unstructured.Unstructured {
	t.Helper()
	var found *unstructured.Unstructured
	for _, object := range objects {
		if object.GetKind() != kind || object.GetLabels()["app.kubernetes.io/component"] != component || object.GetAnnotations()["helm.sh/hook-weight"] != weight {
			continue
		}
		if found != nil {
			t.Fatalf("rendered %s component %s at weight %s is duplicated", kind, component, weight)
		}
		found = object
	}
	if found == nil {
		t.Fatalf("rendered %s component %s at weight %s is missing", kind, component, weight)
	}
	return found
}

func countTeardownRetirementObjects(objects []*unstructured.Unstructured, kind, name, weight string) int {
	count := 0
	for _, object := range objects {
		if object.GetKind() == kind && object.GetName() == name && (weight == "" || object.GetAnnotations()["helm.sh/hook-weight"] == weight) {
			count++
		}
	}
	return count
}

func decodeTeardownRetirementObject(t *testing.T, object *unstructured.Unstructured, into any) {
	t.Helper()
	raw, err := json.Marshal(object.Object)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

func teardownRetirementRenderArgument(t *testing.T, args []string, prefix string) string {
	t.Helper()
	for _, argument := range args {
		if strings.HasPrefix(argument, prefix) {
			return strings.TrimPrefix(argument, prefix)
		}
	}
	t.Fatalf("rendered teardown retirement argument %s is missing", prefix)
	return ""
}
