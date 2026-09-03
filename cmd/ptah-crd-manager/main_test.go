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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
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
		{mode: "teardown", flag: "--verify-controller-state=true"},
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

const validRuntimeAdmissionContractJSON = `{"version":1,"namespace":"ptah-system","commonInitContainerResources":{},"controllerContainerResources":{},"certificateContainerResources":{},"imagePullSecrets":[],"priorityClassName":"","priorityClassValue":0,"priorityClassPreemptionPolicy":"PreemptLowerPriority","controllerServiceAccountName":"ptah-controller","certificateServiceAccountName":"ptah-certificate","controllerServiceAccountCreate":false,"controllerServiceAccountEnforceMountableSecrets":false,"controllerSecretNames":["ptah-webhook"],"certificateSecretNames":[],"certificateRuntimeEnabled":false}`
