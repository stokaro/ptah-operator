package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
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
			want:   schema.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahschemas"},
		},
		{
			name:   "plans",
			client: clients.Plans,
			want:   schema.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahschemaplans"},
		},
		{
			name:   "approvals",
			client: clients.Approvals,
			want:   schema.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahschemaapprovals"},
		},
		{
			name:   "migrations",
			client: clients.Migrations,
			want:   schema.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahmigrations"},
		},
		{
			name:   "migration plans",
			client: clients.MigrationPlans,
			want:   schema.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahmigrationplans"},
		},
		{
			name:   "migration approvals",
			client: clients.MigrationApprovals,
			want:   schema.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahmigrationapprovals"},
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
	// The complete set comes from the shipped CRDs, not from the rows above.
	// Counting those rows against themselves is what certified three kinds as
	// every kind that can store controller-written state.
	durable, err := crdupgrade.ControllerStateBearingResources()
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[schema.GroupVersionResource]struct{}, len(durable))
	for _, resource := range durable {
		want[schema.GroupVersionResource{Group: resource.Group, Version: resource.Version, Resource: resource.Resource}] = struct{}{}
	}
	got := make(map[schema.GroupVersionResource]struct{}, len(dynamicClient.resources))
	for _, resource := range dynamicClient.resources {
		if _, duplicate := got[resource.gvr]; duplicate {
			t.Fatalf("dynamic client asked for %s twice", resource.gvr)
		}
		got[resource.gvr] = struct{}{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state clients read %s, want one client per CRD that stores controller state: %s", sortedResources(got), sortedResources(want))
	}
}

func sortedResources(set map[schema.GroupVersionResource]struct{}) []string {
	names := make([]string, 0, len(set))
	for resource := range set {
		names = append(names, resource.String())
	}
	sort.Strings(names)
	return names
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

func TestModeFlagAllowlistsRejectIgnoredInputs(t *testing.T) {
	tests := []struct {
		mode string
		flag string
	}{
		{mode: "verify", flag: "--release-name=ignored"},
		{mode: "preflight", flag: "--verify-controller-state=true"},
		{mode: "identity-probe", flag: "--verify-controller-state=true"},
		{mode: "reconcile", flag: "--verify-controller-state=true"},
		{mode: "teardown-quiesce", flag: "--verify-controller-state=true"},
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

// The uninstall proof and cleanup modes, the certificate recovery proof and the
// predecessor controller identity are gone with the protocols that used them;
// a hook left over from an older chart must be refused rather than run as
// something else.
func TestRemovedProofModesAndFlagsAreRefused(t *testing.T) {
	for _, mode := range []string{
		"teardown-retirement-probe-a",
		"teardown-retirement-gate",
		"teardown",
		"teardown-retirement-final",
	} {
		err := run(context.Background(), []string{mode}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "unsupported mode") {
			t.Fatalf("run(%s) error = %v, want unsupported mode", mode, err)
		}
	}
	for _, flag := range []string{
		"--verify-certificate-recovery=true",
		"--controller-service-account-managed=true",
		"--previous-controller-service-account-name=ptah-operator-v1",
		"--previous-controller-release-sequence=1",
	} {
		name := strings.TrimPrefix(strings.SplitN(flag, "=", 2)[0], "--")
		err := run(context.Background(), []string{"runtime-verify", flag}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("runtime-verify with %s error = %v, want refusal", flag, err)
		}
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
			ReleaseNamespace:             "ptah-system",
			ControllerServiceAccountName: "ptah-controller",
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
	if guard.ControllerServiceAccountName != "ptah-controller" {
		t.Fatalf("rollout controller ServiceAccount = %q, want the expected invariant", guard.ControllerServiceAccountName)
	}
}

const validRuntimeAdmissionContractJSON = `{"version":1,"namespace":"ptah-system","commonInitContainerResources":{},"controllerContainerResources":{},"certificateContainerResources":{},"imagePullSecrets":[],"priorityClassName":"","priorityClassValue":0,"priorityClassPreemptionPolicy":"PreemptLowerPriority","controllerServiceAccountName":"ptah-controller","certificateServiceAccountName":"ptah-certificate","controllerServiceAccountCreate":false,"controllerServiceAccountEnforceMountableSecrets":false,"controllerSecretNames":["ptah-webhook"],"certificateSecretNames":[],"certificateRuntimeEnabled":false}`

// A hook that refuses prints its reason to stderr, which stays inside the Pod.
// Helm reports only that the Job failed, so the reason reaches the person who
// ran the uninstall through the termination message or not at all.
func TestReportTerminationMessageWritesTheRefusal(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "termination-log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	original := terminationMessageFile
	terminationMessageFile = path
	t.Cleanup(func() { terminationMessageFile = original })

	reportTerminationMessage(errors.New("foreign ClusterRoleBinding/example names a protected ServiceAccount"))

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "ptah-crd-manager: foreign ClusterRoleBinding/example names a protected ServiceAccount\n"
	if string(written) != want {
		t.Fatalf("termination message = %q, want %q", written, want)
	}
}

func TestReportTerminationMessageBoundsAndSurvivesAMissingFile(t *testing.T) {
	original := terminationMessageFile
	terminationMessageFile = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { terminationMessageFile = original })
	reportTerminationMessage(errors.New("no file here"))

	path := filepath.Join(t.TempDir(), "termination-log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	terminationMessageFile = path
	reportTerminationMessage(errors.New(strings.Repeat("x", terminationMessageLimit*2)))
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != terminationMessageLimit {
		t.Fatalf("termination message length = %d, want %d", len(written), terminationMessageLimit)
	}
}
