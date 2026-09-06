package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/fake"
)

func TestStoredControllerStateClientsUseEveryDurableResource(t *testing.T) {
	dynamicClient := &recordingDynamicClient{}
	clients := storedControllerStateClients(dynamicClient)
	tests := []struct {
		name   string
		client crdupgrade.ControllerStateListClient
		want   schema.GroupVersionResource
	}{
		{
			name:   "schemas",
			client: clients.Schemas,
			want:   schema.GroupVersionResource{Group: "operator.ptah.dev", Version: "v1alpha1", Resource: "ptahschemas"},
		},
		{
			name:   "plans",
			client: clients.Plans,
			want:   schema.GroupVersionResource{Group: "operator.ptah.dev", Version: "v1alpha1", Resource: "ptahschemaplans"},
		},
		{
			name:   "approvals",
			client: clients.Approvals,
			want:   schema.GroupVersionResource{Group: "operator.ptah.dev", Version: "v1alpha1", Resource: "ptahschemaapprovals"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resource, ok := test.client.(*recordedDynamicResource)
			if !ok || resource.gvr != test.want {
				t.Fatalf("state client = %#v, want resource %s", test.client, test.want)
			}
		})
	}
	if len(dynamicClient.resources) != len(tests) {
		t.Fatalf("dynamic Resource calls = %d, want %d", len(dynamicClient.resources), len(tests))
	}
}

type recordingDynamicClient struct {
	resources []*recordedDynamicResource
}

func (c *recordingDynamicClient) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	resource := &recordedDynamicResource{gvr: gvr}
	c.resources = append(c.resources, resource)
	return resource
}

type recordedDynamicResource struct {
	dynamic.NamespaceableResourceInterface
	gvr schema.GroupVersionResource
}

func TestImageCheckProvesCompiledSequenceWithoutClusterAccess(t *testing.T) {
	var output bytes.Buffer
	image := "registry.example/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	err := run(context.Background(), []string{
		"image-check",
		"--release-sequence=" + strconv.FormatInt(int64(crdupgrade.CurrentReleaseSequence), 10),
		"--manager-image=" + image,
	}, &output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), image) {
		t.Fatalf("image-check output = %q, want exact image identity", output.String())
	}
}

func TestWaitForRetiredBoundCredentialRevocation(t *testing.T) {
	if err := waitForRetiredBoundCredentialRevocation(context.Background(), time.Nanosecond); err != nil {
		t.Fatalf("waitForRetiredBoundCredentialRevocation() error = %v", err)
	}
	if err := waitForRetiredBoundCredentialRevocation(context.Background(), 0); err == nil ||
		!strings.Contains(err.Error(), "delay must be positive") {
		t.Fatalf("zero delay error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForRetiredBoundCredentialRevocation(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled wait error = %v, want context.Canceled", err)
	}
}

func TestImageCheckRejectsMismatchedSequenceAndAPIFlags(t *testing.T) {
	tests := [][]string{
		{"image-check", "--release-sequence=" + strconv.FormatInt(int64(crdupgrade.CurrentReleaseSequence+1), 10), "--manager-image=image"},
		{"image-check", "--release-sequence=" + strconv.FormatInt(int64(crdupgrade.CurrentReleaseSequence), 10), "--manager-image=image", "--release-name=unexpected"},
	}
	for _, args := range tests {
		if err := run(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatalf("run(%v) succeeded, want refusal", args)
		}
	}
}

func TestRuntimeInvariantsRejectSameCandidateAndPredecessorServiceAccount(t *testing.T) {
	t.Parallel()

	_, err := runtimeInvariants(
		"release", "ptah-system", "ptah-system", "true",
		"leader", "webhook", 10,
		"hook", "controller", true, "controller", "previous-uid", false, "controller",
		"certificate", crdupgrade.CurrentReleaseSequence, 0,
	)
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("runtimeInvariants error = %v, want distinct-principal refusal", err)
	}
}

func TestModeFlagAllowlistsRejectIgnoredInputs(t *testing.T) {
	tests := []struct {
		mode string
		flag string
	}{
		{mode: "verify", flag: "--release-name=ignored"},
		{mode: "preflight", flag: "--verify-controller-state=true"},
		{mode: "identity-probe", flag: "--verify-controller-state=true"},
		{mode: "reconcile", flag: "--verify-controller-state=true"},
		{mode: "teardown-retirement-probe-a", flag: "--verify-controller-state=true"},
		{mode: "teardown-retirement-gate", flag: "--verify-controller-state=true"},
		{mode: "teardown-quiesce", flag: "--verify-controller-state=true"},
		{mode: "teardown", flag: "--verify-controller-state=true"},
		{mode: "teardown-retirement-final", flag: "--verify-controller-state=true"},
		{mode: "image-check", flag: "--timeout=1s"},
	}
	for _, test := range tests {
		t.Run(test.mode+test.flag, func(t *testing.T) {
			err := run(context.Background(), []string{test.mode, test.flag}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "is not valid in "+test.mode+" mode") {
				t.Fatalf("run error = %v, want mode allowlist refusal", err)
			}
		})
	}
}

func TestTeardownRetirementBarrierRechecksInitialPhaseBeforeEveryStoredSweep(t *testing.T) {
	guard := teardownRetirementManagerTestGuard()
	activation := teardownRetirementManagerTestActivation(t, guard)
	clientset := fake.NewSimpleClientset(activation)
	configMaps := clientset.CoreV1().ConfigMaps(activation.Namespace)
	baseCalls := 0
	additionalCalls := 0
	barrier := &admissionConvergenceBarrier{
		verifyStored: func(context.Context) error {
			baseCalls++
			return nil
		},
	}
	if err := bindTeardownRetirementPhase(
		barrier,
		guard,
		configMaps,
		crdupgrade.TeardownRetirementActive,
		func(context.Context) error {
			additionalCalls++
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := barrier.verifyStored(context.Background()); err != nil {
		t.Fatalf("active stored sweep failed: %v", err)
	}
	if baseCalls != 1 || additionalCalls != 1 {
		t.Fatalf("active stored sweep calls = base %d, additional %d, want 1/1", baseCalls, additionalCalls)
	}
	if err := configMaps.Delete(context.Background(), crdupgrade.ReleaseActivationName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := barrier.verifyStored(context.Background()); err == nil || !strings.Contains(err.Error(), "phase changed") {
		t.Fatalf("terminal stored sweep error = %v, want phase-change refusal", err)
	}
	if baseCalls != 1 || additionalCalls != 1 {
		t.Fatalf("phase-changing sweep called wrapped verifiers: base %d, additional %d", baseCalls, additionalCalls)
	}
}

func TestTeardownRetirementBarrierClosesPhaseAroundStoredSweep(t *testing.T) {
	guard := teardownRetirementManagerTestGuard()
	activation := teardownRetirementManagerTestActivation(t, guard)
	clientset := fake.NewSimpleClientset(activation)
	configMaps := clientset.CoreV1().ConfigMaps(activation.Namespace)
	barrier := &admissionConvergenceBarrier{verifyStored: func(context.Context) error { return nil }}
	if err := bindTeardownRetirementPhase(
		barrier,
		guard,
		configMaps,
		crdupgrade.TeardownRetirementActive,
		func(ctx context.Context) error {
			return configMaps.Delete(ctx, crdupgrade.ReleaseActivationName, metav1.DeleteOptions{})
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := barrier.verifyStored(context.Background()); err == nil || !strings.Contains(err.Error(), "phase changed") {
		t.Fatalf("phase-changing stored sweep error = %v, want closing phase refusal", err)
	}
}

func TestTeardownRetirementDrainAuthorizationDistinguishesActiveAndTerminal(t *testing.T) {
	guard := teardownRetirementManagerTestGuard()
	draining := teardownRetirementManagerTestActivation(t, guard)
	drainingClient := fake.NewSimpleClientset(draining).CoreV1().ConfigMaps(draining.Namespace)
	if err := verifyTeardownRetirementDrainAuthorization(
		context.Background(),
		guard,
		drainingClient,
		crdupgrade.TeardownRetirementActive,
	); err != nil {
		t.Fatalf("exact draining authorization failed: %v", err)
	}

	active := draining.DeepCopy()
	active.Data = map[string]string{
		"active-release-sequence": "1",
		"controller-credentials":  "active",
	}
	activeClient := fake.NewSimpleClientset(active).CoreV1().ConfigMaps(active.Namespace)
	if err := verifyTeardownRetirementDrainAuthorization(
		context.Background(),
		guard,
		activeClient,
		crdupgrade.TeardownRetirementActive,
	); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("credential-active authorization error = %v, want draining refusal", err)
	}

	terminalClient := fake.NewSimpleClientset().CoreV1().ConfigMaps(draining.Namespace)
	if err := verifyTeardownRetirementDrainAuthorization(
		context.Background(),
		guard,
		terminalClient,
		crdupgrade.TeardownRetirementTerminal,
	); err != nil {
		t.Fatalf("terminal authorization failed: %v", err)
	}
	if err := verifyTeardownRetirementDrainAuthorization(
		context.Background(),
		guard,
		terminalClient,
		crdupgrade.TeardownRetirementPhase("unknown"),
	); err == nil || !strings.Contains(err.Error(), "unknown teardown retirement phase") {
		t.Fatalf("unknown phase error = %v, want refusal", err)
	}
}

func TestDecodeRuntimeAdmissionContractRequiresEveryWireField(t *testing.T) {
	var fields map[string]any
	if err := json.Unmarshal([]byte(validRuntimeAdmissionContractJSON), &fields); err != nil {
		t.Fatal(err)
	}
	for field := range fields {
		t.Run(field, func(t *testing.T) {
			candidate := make(map[string]any, len(fields)-1)
			for key, value := range fields {
				if key != field {
					candidate[key] = value
				}
			}
			raw, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			_, err = decodeRuntimeAdmissionContract(base64.StdEncoding.EncodeToString(raw))
			if err == nil || !strings.Contains(err.Error(), "required field \""+field+"\" is missing") {
				t.Fatalf("decode error = %v, want missing field %q", err, field)
			}
		})
	}
}

func TestDecodeRuntimeAdmissionContractRejectsAmbiguousJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "duplicate top-level key",
			raw:  strings.Replace(validRuntimeAdmissionContractJSON, `"version":1`, `"version":1,"version":1`, 1),
			want: `duplicate JSON key "version" at $`,
		},
		{
			name: "duplicate nested key",
			raw:  strings.Replace(validRuntimeAdmissionContractJSON, `"commonInitContainerResources":{}`, `"commonInitContainerResources":{"requests":{"cpu":"1m","cpu":"2m"}}`, 1),
			want: `duplicate JSON key "cpu" at $.commonInitContainerResources.requests`,
		},
		{
			name: "unknown key",
			raw:  strings.Replace(validRuntimeAdmissionContractJSON, `"version":1`, `"version":1,"unexpected":true`, 1),
			want: `unknown field "unexpected"`,
		},
		{
			name: "unsupported version",
			raw:  strings.Replace(validRuntimeAdmissionContractJSON, `"version":1`, `"version":2`, 1),
			want: "unsupported version 2",
		},
		{
			name: "trailing value",
			raw:  validRuntimeAdmissionContractJSON + ` {}`,
			want: "trailing JSON value",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeRuntimeAdmissionContract(base64.StdEncoding.EncodeToString([]byte(test.raw)))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decode error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDecodeRuntimeAdmissionContractPreservesFalseValues(t *testing.T) {
	contract, err := decodeRuntimeAdmissionContract(base64.StdEncoding.EncodeToString([]byte(validRuntimeAdmissionContractJSON)))
	if err != nil {
		t.Fatal(err)
	}
	if contract.ControllerServiceAccountCreate || contract.CertificateRuntimeEnabled {
		t.Fatalf("decoded false values changed: %#v", contract)
	}
	if contract.Namespace != "ptah-system" || contract.ControllerServiceAccountName != "ptah-controller" || contract.CertificateServiceAccountName != "ptah-certificate" {
		t.Fatalf("decoded identity = %#v", contract)
	}
}

func TestNewRolloutGuardUsesDecodedPriorityClassContract(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte(strings.Replace(
		validRuntimeAdmissionContractJSON,
		`"priorityClassName":""`,
		`"priorityClassName":"runtime-critical"`,
		1,
	)))
	contract, err := decodeRuntimeAdmissionContract(encoded)
	if err != nil {
		t.Fatal(err)
	}

	guard := newRolloutGuard(
		fake.NewSimpleClientset(),
		crdupgrade.RuntimeInvariants{
			ReleaseNamespace:                     "ptah-system",
			PreviousControllerServiceAccountName: "previous-controller",
			PreviousControllerReleaseSequence:    4,
		},
		"manager-image", "webhook-secret",
		9443, 8081, 1,
		nil, nil, nil, nil,
		contract,
		encoded,
	)
	if guard.PriorityClassName != "runtime-critical" {
		t.Fatalf("rollout priority class = %q, want decoded runtime-critical class", guard.PriorityClassName)
	}
	if guard.RuntimeAdmissionContractB64 != encoded {
		t.Fatal("rollout lost the encoded runtime admission contract")
	}
	if guard.PreviousControllerServiceAccountName != "previous-controller" || guard.PreviousControllerReleaseSequence != 4 {
		t.Fatalf("rollout predecessor identity = %q/%d, want expected invariant", guard.PreviousControllerServiceAccountName, guard.PreviousControllerReleaseSequence)
	}
}

const validRuntimeAdmissionContractJSON = `{"version":1,"namespace":"ptah-system","commonInitContainerResources":{},"controllerContainerResources":{},"certificateContainerResources":{},"imagePullSecrets":[],"priorityClassName":"","priorityClassValue":0,"priorityClassPreemptionPolicy":"PreemptLowerPriority","controllerServiceAccountName":"ptah-controller","certificateServiceAccountName":"ptah-certificate","controllerServiceAccountCreate":false,"controllerServiceAccountEnforceMountableSecrets":false,"controllerSecretNames":["ptah-webhook"],"certificateSecretNames":[],"certificateRuntimeEnabled":false}`
