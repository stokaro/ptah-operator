package main

import (
	"bytes"
	"context"
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

	"github.com/stokaro/ptah-operator/internal/controllerstate"
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
			name:   "migrations",
			client: clients.Migrations,
			want:   schema.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahmigrations"},
		},
		{
			name:   "migration plans",
			client: clients.MigrationPlans,
			want:   schema.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahmigrationplans"},
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

func TestModeFlagAllowlistsRejectIgnoredInputs(t *testing.T) {
	tests := []struct {
		mode string
		flag string
	}{
		{mode: "verify", flag: "--release-name=ignored"},
		{mode: "verify", flag: "--controller-state-version=2"},
		{mode: "reconcile", flag: "--verify-controller-state=true"},
		{mode: "reconcile", flag: "--hook-service-account-name=ignored"},
		{mode: "runtime-verify", flag: "--manager-image=ignored"},
		{mode: "runtime-verify", flag: "--controller-state-version=2"},
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

// The release guards, the preflights that dry-ran them, the uninstall hook and
// the runtime contract the guards pinned are gone. A hook or an init container
// left over from an older chart is refused rather than run as something else.
func TestRemovedModesAndFlagsAreRefused(t *testing.T) {
	for _, mode := range []string{
		"image-check",
		"identity-probe",
		"preflight",
		"teardown-quiesce",
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
		"--webhook-secret-name=ptah-webhook-cert",
		"--webhook-port=9443",
		"--certificate-health-port=8081",
		"--controller-replicas=1",
		"--controller-runtime-args-b64=W10=",
		"--certificate-runtime-args-b64=W10=",
		"--runtime-deployment-config-expressions-b64=W10=",
		"--runtime-pod-config-expressions-b64=W10=",
		"--runtime-admission-contract-b64=e30=",
		"--verify-certificate-recovery=true",
		"--controller-service-account-managed=true",
		"--previous-controller-service-account-name=ptah-operator-v1",
		"--previous-controller-release-sequence=1",
		"--release-sequence=1",
	} {
		name := strings.TrimPrefix(strings.SplitN(flag, "=", 2)[0], "--")
		for _, mode := range []string{"reconcile", "runtime-verify"} {
			err := run(context.Background(), []string{mode, flag}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("%s with %s error = %v, want refusal", mode, flag, err)
			}
		}
	}
}

// The reconcile hook runs the image the values name with the arguments the
// chart renders, and a --reuse-values upgrade keeps the old image under a new
// chart. The hook refuses a chart of another release before it reads the
// cluster, when the controller-state version differs. A pair that matches
// reaches the cluster, which a test process has none of, so the in-cluster
// error is what shows the check let it through.
func TestReconcileRefusesAChartFromAnotherRelease(t *testing.T) {
	state := int64(controllerstate.CurrentVersion)
	for _, test := range []struct {
		name    string
		state   string
		refusal string
	}{
		{name: "the chart published with the image", state: strconv.FormatInt(state, 10)},
		{name: "a chart of a later state contract", state: strconv.FormatInt(state+1, 10), refusal: "controller-state version"},
		{name: "a chart of an earlier state contract", state: strconv.FormatInt(state-1, 10), refusal: "controller-state version"},
		{name: "a chart that names no state contract", refusal: "controller-state version"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"reconcile"}
			if test.state != "" {
				args = append(args, "--controller-state-version="+test.state)
			}
			err := run(context.Background(), args, &bytes.Buffer{})
			if test.refusal == "" {
				if err == nil || !strings.Contains(err.Error(), "load in-cluster configuration") {
					t.Fatalf("run error = %v, want the matching pair to reach the cluster", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.refusal) ||
				!strings.Contains(err.Error(), "published with the image") || strings.Contains(err.Error(), "in-cluster") {
				t.Fatalf("run error = %v, want the %s refusal before the cluster is read", err, test.refusal)
			}
		})
	}
}

// A hook that refuses prints its reason to stderr, which stays inside the Pod.
// Helm reports only that the Job failed, so the reason reaches the person who
// ran the upgrade through the termination message or not at all.
func TestReportTerminationMessageWritesTheRefusal(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "termination-log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	original := terminationMessageFile
	terminationMessageFile = path
	t.Cleanup(func() { terminationMessageFile = original })

	reportTerminationMessage(errors.New("controller downgrade refused: PtahSchema team-a/orders stores controller state version 3"))

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "ptah-crd-manager: controller downgrade refused: PtahSchema team-a/orders stores controller state version 3\n"
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
