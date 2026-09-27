package main

// These tests are white-box because the final deletion order is a
// package-local orchestration boundary of the manager binary, rather than a
// reusable public API.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

func TestTeardownRetirementFinalizerDeletesMarkersBeforeActivation(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	dedicated, err := guard.MarkerTarget()
	if err != nil {
		t.Fatal(err)
	}
	secondary := crdupgrade.TeardownRetirementMarkerTarget{
		Name: "ptah-admission-convergence-v1-1-deadbeefcafe",
		Verify: func(object *corev1.ConfigMap) error {
			if object.Data["contract"] != "exact" {
				return errors.New("secondary marker differs")
			}
			return nil
		},
	}
	dedicatedObject, err := guard.Marker()
	if err != nil {
		t.Fatal(err)
	}
	dedicatedObject.UID = "dedicated-uid"
	dedicatedObject.ResourceVersion = "11"
	secondaryObject := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: secondary.Name, Namespace: dedicatedObject.Namespace, UID: "secondary-uid", ResourceVersion: "12"},
		Data:       map[string]string{"contract": "exact"},
	}
	activation := teardownRetirementManagerTestActivation(t, guard)
	client := &teardownRetirementFinalizerClient{objects: map[string]*corev1.ConfigMap{
		dedicated.Name:                   dedicatedObject,
		secondary.Name:                   secondaryObject,
		crdupgrade.ReleaseActivationName: activation,
	}}
	finalizer, err := newTeardownRetirementFinalizer(client, guard, secondary)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizer.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{secondary.Name, crdupgrade.ReleaseActivationName}
	if !reflect.DeepEqual(client.deletes, wantOrder) {
		t.Fatalf("deletion order = %v, want %v", client.deletes, wantOrder)
	}
	for _, name := range wantOrder {
		if client.deleteOptions[name].Preconditions == nil || client.deleteOptions[name].Preconditions.UID == nil ||
			client.deleteOptions[name].Preconditions.ResourceVersion == nil {
			t.Errorf("ConfigMap/%s deletion lacks UID/resourceVersion preconditions", name)
		}
	}
	if err := finalizer.Finalize(context.Background()); err != nil {
		t.Fatalf("completed retry: %v", err)
	}
	if len(client.deletes) != len(wantOrder) {
		t.Fatalf("completed retry issued more mutations: %v", client.deletes)
	}
	if client.objects[dedicated.Name] == nil {
		t.Fatal("finalizer deleted the Helm-owned dedicated marker")
	}
	for _, name := range client.gets {
		if name == dedicated.Name {
			t.Fatal("finalizer inspected the Helm-owned dedicated marker")
		}
	}
}

func TestTeardownRetirementFinalizerAcceptsOnlyContiguousRetryPrefixes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		presentMarkers  []bool
		activation      bool
		wantDeleteCount int
		wantError       string
	}{
		{name: "fresh", presentMarkers: []bool{true, true}, activation: true, wantDeleteCount: 3},
		{name: "first marker already deleted", presentMarkers: []bool{false, true}, activation: true, wantDeleteCount: 2},
		{name: "all markers already deleted", presentMarkers: []bool{false, false}, activation: true, wantDeleteCount: 1},
		{name: "complete", presentMarkers: []bool{false, false}, activation: false},
		{name: "non-contiguous marker prefix", presentMarkers: []bool{true, false}, activation: true, wantError: "non-contiguous"},
		{name: "activation absent before retained secondary marker", presentMarkers: []bool{true, false}, activation: false, wantError: "retains"},
		{name: "activation absent before all retained markers", presentMarkers: []bool{true, true}, activation: false, wantError: "retains"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			guard := teardownRetirementManagerTestGuard()
			first := crdupgrade.TeardownRetirementMarkerTarget{Name: "first", Verify: func(*corev1.ConfigMap) error { return nil }}
			second := crdupgrade.TeardownRetirementMarkerTarget{Name: "second", Verify: func(*corev1.ConfigMap) error { return nil }}
			markers := []crdupgrade.TeardownRetirementMarkerTarget{first, second}
			objects := map[string]*corev1.ConfigMap{}
			for index, present := range test.presentMarkers {
				if present {
					objects[markers[index].Name] = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
						Name: markers[index].Name, UID: types.UID(strconv.Itoa(index + 1)), ResourceVersion: strconv.Itoa(index + 1),
					}}
				}
			}
			if test.activation {
				objects[crdupgrade.ReleaseActivationName] = teardownRetirementManagerTestActivation(t, guard)
			}
			client := &teardownRetirementFinalizerClient{objects: objects}
			finalizer, err := newTeardownRetirementFinalizer(client, guard, markers...)
			if err != nil {
				t.Fatal(err)
			}
			err = finalizer.Finalize(context.Background())
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Finalize() error = %v, want containing %q", err, test.wantError)
				}
				if len(client.deletes) != 0 {
					t.Fatalf("unsafe retry mutated %v", client.deletes)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(client.deletes) != test.wantDeleteCount {
				t.Fatalf("delete count = %d, want %d", len(client.deletes), test.wantDeleteCount)
			}
		})
	}
}

func TestTeardownRetirementFinalizerRejectsForeignObjectWithoutMutation(t *testing.T) {
	t.Parallel()

	guard := teardownRetirementManagerTestGuard()
	secondary := crdupgrade.TeardownRetirementMarkerTarget{
		Name: "secondary",
		Verify: func(object *corev1.ConfigMap) error {
			if object.Data["contract"] != "exact" {
				return errors.New("secondary marker differs")
			}
			return nil
		},
	}
	marker := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: secondary.Name, UID: "marker-uid", ResourceVersion: "1"},
		Data:       map[string]string{"contract": "foreign"},
	}
	client := &teardownRetirementFinalizerClient{objects: map[string]*corev1.ConfigMap{
		secondary.Name:                   marker,
		crdupgrade.ReleaseActivationName: teardownRetirementManagerTestActivation(t, guard),
	}}
	finalizer, err := newTeardownRetirementFinalizer(client, guard, secondary)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizer.Finalize(context.Background()); err == nil || !strings.Contains(err.Error(), "secondary marker differs") {
		t.Fatalf("Finalize() error = %v, want secondary marker rejection", err)
	}
	if len(client.deletes) != 0 {
		t.Fatalf("foreign marker caused mutations: %v", client.deletes)
	}
}

func TestTeardownRetirementFinalizerResumesAfterEveryDelete(t *testing.T) {
	t.Parallel()

	for crashAfter := 1; crashAfter <= 2; crashAfter++ {
		t.Run(strconv.Itoa(crashAfter), func(t *testing.T) {
			t.Parallel()
			guard := teardownRetirementManagerTestGuard()
			dedicated, err := guard.MarkerTarget()
			if err != nil {
				t.Fatal(err)
			}
			secondary := crdupgrade.TeardownRetirementMarkerTarget{
				Name: "secondary",
				Verify: func(object *corev1.ConfigMap) error {
					if object.Data["contract"] != "exact" {
						return errors.New("secondary marker differs")
					}
					return nil
				},
			}
			dedicatedObject, markerErr := guard.Marker()
			if markerErr != nil {
				t.Fatal(markerErr)
			}
			dedicatedObject.UID, dedicatedObject.ResourceVersion = "dedicated-uid", "1"
			client := &teardownRetirementFinalizerClient{
				objects: map[string]*corev1.ConfigMap{
					secondary.Name: {
						ObjectMeta: metav1.ObjectMeta{Name: secondary.Name, UID: "secondary-uid", ResourceVersion: "1"},
						Data:       map[string]string{"contract": "exact"},
					},
					dedicated.Name:                   dedicatedObject,
					crdupgrade.ReleaseActivationName: teardownRetirementManagerTestActivation(t, guard),
				},
				failAfterDelete: crashAfter,
			}
			finalizer, finalizerErr := newTeardownRetirementFinalizer(client, guard, secondary)
			if finalizerErr != nil {
				t.Fatal(finalizerErr)
			}
			if err := finalizer.Finalize(context.Background()); err == nil || !strings.Contains(err.Error(), "simulated crash") {
				t.Fatalf("first Finalize() error = %v, want simulated crash", err)
			}

			client.failAfterDelete = 0
			if err := finalizer.Finalize(context.Background()); err != nil {
				t.Fatalf("retry Finalize() error = %v", err)
			}
			if len(client.objects) != 1 || client.objects[dedicated.Name] == nil {
				t.Fatalf("retry retained %d ConfigMaps; dedicated marker present = %t", len(client.objects), client.objects[dedicated.Name] != nil)
			}
		})
	}
}

func TestConfiguredTeardownRetirementGuardAddsOnlyExactCertificateRecoveryPair(t *testing.T) {
	t.Parallel()

	baseRollout := teardownRetirementManagerTestRollout()
	if _, err := newConfiguredTeardownRetirementGuard(baseRollout, crdupgrade.RuntimeAdmissionContract{}); err != nil {
		t.Fatal(err)
	}

	rollout := teardownRetirementManagerTestRollout()
	rollout.CertificateRuntimeEnabled = true
	rollout.CertificateArgs = append(rollout.CertificateArgs,
		"--staging-secret-name=ptah-webhook-cert-stage",
		"--recreate-missing-secret=true",
		"--secret-create-policy-name="+rollout.CertificateDeploymentName,
		"--secret-create-policy-binding-name="+rollout.CertificateDeploymentName,
		"--secret-create-service-account-name="+rollout.CertificateDeploymentName,
	)
	contract := crdupgrade.RuntimeAdmissionContract{
		CertificateRuntimeEnabled:     true,
		CertificateServiceAccountName: rollout.CertificateDeploymentName,
	}
	configured, err := newConfiguredTeardownRetirementGuard(rollout, contract)
	if err != nil {
		t.Fatal(err)
	}
	recoveryConfig := certificateRecoveryRetirementConfig(
		rollout,
		rollout.CertificateDeploymentName,
		rollout.CertificateDeploymentName,
		rollout.CertificateDeploymentName,
	)
	if recoveryConfig.ReleaseName != rollout.ReleaseName {
		t.Fatalf(
			"certificate recovery retirement release name = %q, want %q",
			recoveryConfig.ReleaseName,
			rollout.ReleaseName,
		)
	}
	_, err = configured.WithOriginalPairs(crdupgrade.TeardownOriginalPairVerifier{
		Name: rollout.CertificateDeploymentName,
		VerifyPolicy: func(*admissionregistrationv1.ValidatingAdmissionPolicy) error {
			return nil
		},
		VerifyBinding: func(*admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("configured certificate pair was not registered exactly once: %v", err)
	}

	disabledContract := contract
	disabledContract.CertificateRuntimeEnabled = false
	if _, err := newConfiguredTeardownRetirementGuard(rollout, disabledContract); err == nil || !strings.Contains(err.Error(), "runtime is disabled") {
		t.Fatalf("disabled certificate runtime error = %v", err)
	}
	rollout.CertificateArgs = []string{"--recreate-missing-secret=true"}
	if _, err := newConfiguredTeardownRetirementGuard(rollout, contract); err == nil || !strings.Contains(err.Error(), "policy-name") {
		t.Fatalf("incomplete certificate recovery error = %v", err)
	}
}

func TestCertificateRecoveryRetirementMetadataIsExact(t *testing.T) {
	t.Parallel()

	rollout := teardownRetirementManagerTestRollout()
	name := rollout.CertificateDeploymentName
	object := &admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{
		Name:            name,
		UID:             "policy-uid",
		ResourceVersion: "7",
		Annotations: map[string]string{
			"meta.helm.sh/release-name":      rollout.ReleaseName,
			"meta.helm.sh/release-namespace": rollout.ReleaseNamespace,
		},
		Labels: map[string]string{
			"helm.sh/chart":                "ptah-operator-0.1.0",
			"app.kubernetes.io/name":       "ptah-operator",
			"app.kubernetes.io/instance":   rollout.ReleaseName,
			"app.kubernetes.io/version":    "0.1.0",
			"app.kubernetes.io/managed-by": "Helm",
			"app.kubernetes.io/component":  "certificate-rotation",
		},
	}}
	if err := verifyCertificateRecoveryRetirementMetadata("ValidatingAdmissionPolicy", name, rollout, object); err != nil {
		t.Fatal(err)
	}
	object.Labels["foreign"] = "true"
	if err := verifyCertificateRecoveryRetirementMetadata("ValidatingAdmissionPolicy", name, rollout, object); err == nil {
		t.Fatal("foreign metadata was accepted")
	}
}

func teardownRetirementManagerTestGuard() *crdupgrade.TeardownRetirementGuard {
	return crdupgrade.NewTeardownRetirementGuard(teardownRetirementManagerTestRollout())
}

func teardownRetirementManagerTestRollout() *crdupgrade.RolloutGuard {
	releaseNamespace := "ptah-system"
	releaseName := "ptah"
	managerImage := "registry.example/ptah@sha256:" + strings.Repeat("a", 64)
	identity := sha256.Sum256([]byte(releaseNamespace + "\n" + releaseName + "\n1\n" + managerImage))
	return &crdupgrade.RolloutGuard{
		Policies:                           emptyTeardownRetirementPolicyReader{},
		Bindings:                           emptyTeardownRetirementBindingReader{},
		ReleaseName:                        releaseName,
		ReleaseNamespace:                   releaseNamespace,
		CoordinationNamespace:              releaseNamespace,
		LeaderElectionID:                   "ptah-operator.operator.ptah.run",
		WebhookServiceName:                 "ptah-webhook",
		WebhookTimeoutSeconds:              10,
		WebhookSecretName:                  "ptah-webhook-cert",
		WebhookPort:                        9443,
		CertificateHealthPort:              8081,
		HookServiceAccountName:             fmt.Sprintf("ptah-crd-v1-%x", identity)[:24],
		ControllerServiceAccountName:       "ptah-controller",
		ControllerDeploymentName:           "ptah-controller",
		ControllerReplicas:                 2,
		CertificateDeploymentName:          "ptah-cert-rotator",
		ControllerStateVersion:             1,
		AdmissionContractVersion:           1,
		ReleaseSequence:                    1,
		ManagerImage:                       managerImage,
		ControllerArgs:                     []string{"--webhook-port=9443"},
		CertificateArgs:                    []string{"--namespace=ptah-system"},
		RuntimeDeploymentConfigExpressions: []string{`object.spec.replicas == 2`},
		RuntimePodConfigExpressions:        []string{`object.spec.restartPolicy == "Always"`},
		RuntimeAdmissionContractB64:        "e30=",
		PollEvery:                          time.Millisecond,
	}
}

func teardownRetirementManagerTestActivation(t *testing.T, guard *crdupgrade.TeardownRetirementGuard) *corev1.ConfigMap {
	t.Helper()
	marker, err := guard.Marker()
	if err != nil {
		t.Fatal(err)
	}
	attempt := marker.Data["release-attempt"]
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            crdupgrade.ReleaseActivationName,
			Namespace:       marker.Namespace,
			UID:             "activation-uid",
			ResourceVersion: "13",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "ptah-operator",
				"app.kubernetes.io/instance":   "ptah",
				"app.kubernetes.io/component":  "rollout-guard",
			},
			Annotations: map[string]string{
				"helm.sh/hook":                                "pre-install,pre-upgrade",
				"helm.sh/hook-weight":                         "-166",
				"helm.sh/resource-policy":                     "keep",
				"operator.ptah.run/rollout-guard-version":     "1",
				"operator.ptah.run/release-name":              "ptah",
				"operator.ptah.run/release-namespace":         "ptah-system",
				crdupgrade.ControllerStateVersionAnnotation:   "1",
				crdupgrade.AdmissionContractVersionAnnotation: "1",
				crdupgrade.ReleaseSequenceAnnotation:          "1",
				crdupgrade.ManagerImageAnnotation:             marker.Annotations[crdupgrade.ManagerImageAnnotation],
			},
		},
		Data: map[string]string{
			"active-release-sequence":                        "1",
			"controller-credentials":                         "draining",
			"controller-credentials-target-release-sequence": "1",
			"controller-credentials-attempt":                 attempt,
		},
	}
}

type teardownRetirementFinalizerClient struct {
	objects         map[string]*corev1.ConfigMap
	gets            []string
	updates         []*corev1.ConfigMap
	deletes         []string
	deleteOptions   map[string]metav1.DeleteOptions
	failAfterDelete int
}

func (c *teardownRetirementFinalizerClient) Update(_ context.Context, object *corev1.ConfigMap, _ metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	if object == nil {
		return nil, apierrors.NewBadRequest("nil ConfigMap")
	}
	stored := c.objects[object.Name]
	if stored == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, object.Name)
	}
	updated := object.DeepCopy()
	updated.ResourceVersion = stored.ResourceVersion + "-updated"
	c.objects[object.Name] = updated
	c.updates = append(c.updates, updated.DeepCopy())
	return updated.DeepCopy(), nil
}

func (c *teardownRetirementFinalizerClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	c.gets = append(c.gets, name)
	object := c.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	return object.DeepCopy(), nil
}

func (c *teardownRetirementFinalizerClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	object := c.objects[name]
	if object == nil {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil ||
		*options.Preconditions.UID != object.UID || *options.Preconditions.ResourceVersion != object.ResourceVersion {
		return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, name, errors.New("precondition differs"))
	}
	delete(c.objects, name)
	c.deletes = append(c.deletes, name)
	if c.deleteOptions == nil {
		c.deleteOptions = map[string]metav1.DeleteOptions{}
	}
	c.deleteOptions[name] = options
	if c.failAfterDelete > 0 && len(c.deletes) == c.failAfterDelete {
		return errors.New("simulated crash after delete")
	}
	return nil
}

type emptyTeardownRetirementPolicyReader struct{}

func (emptyTeardownRetirementPolicyReader) Get(context.Context, string, metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	return nil, errors.New("unexpected policy read")
}

type emptyTeardownRetirementBindingReader struct{}

func (emptyTeardownRetirementBindingReader) Get(context.Context, string, metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	return nil, errors.New("unexpected binding read")
}
