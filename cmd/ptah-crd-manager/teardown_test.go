package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

// The uninstall hook checks what it is about to delete before it stops
// anything: an inventory it would refuse to delete must not cost the release
// its runtime first.
func TestRunTeardownModeRefusesAForeignInventoryBeforeStoppingTheRuntime(t *testing.T) {
	replicas := int32(2)
	clientset := fake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "ptah-controller", Namespace: "ptah-system"},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		},
		&admissionregistrationv1.ValidatingAdmissionPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: crdupgrade.RolloutGuardPolicyName(1), UID: "foreign-uid", ResourceVersion: "1"},
		},
	)
	err := runTeardownMode(context.Background(), clientset, teardownTestRollout(clientset))
	if err == nil || !strings.Contains(err.Error(), "preflight retained release inventory") ||
		!strings.Contains(err.Error(), crdupgrade.RolloutGuardPolicyName(1)) {
		t.Fatalf("runTeardownMode() error = %v, want a preflight refusal of the foreign rollout guard", err)
	}
	for _, action := range clientset.Actions() {
		if action.GetVerb() != "get" && action.GetVerb() != "list" {
			t.Fatalf("a refused teardown wrote to the cluster: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
}

// A release whose retained inventory and runtime are already gone, as after
// an uninstall that stopped past its last deletion, finishes without error.
func TestRunTeardownModeFinishesWhenNothingIsLeft(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	if err := runTeardownMode(context.Background(), clientset, teardownTestRollout(clientset)); err != nil {
		t.Fatalf("runTeardownMode() error = %v", err)
	}
}

func TestRunTeardownModeRequiresItsDependencies(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	rollout := teardownTestRollout(clientset)
	for name, call := range map[string]func() error{
		"nil client":  func() error { return runTeardownMode(context.Background(), nil, rollout) },
		"nil rollout": func() error { return runTeardownMode(context.Background(), clientset, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil || !strings.Contains(err.Error(), "dependencies are required") {
				t.Fatalf("runTeardownMode() error = %v, want a dependency refusal", err)
			}
		})
	}
}

func teardownTestRollout(clientset *fake.Clientset) *crdupgrade.RolloutGuard {
	const (
		releaseNamespace = "ptah-system"
		releaseName      = "ptah"
	)
	managerImage := "registry.example/ptah@sha256:" + strings.Repeat("a", 64)
	identity := sha256.Sum256([]byte(releaseNamespace + "\n" + releaseName + "\n1\n" + managerImage))
	return &crdupgrade.RolloutGuard{
		Policies:                     clientset.AdmissionregistrationV1().ValidatingAdmissionPolicies(),
		Bindings:                     clientset.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings(),
		Deployments:                  clientset.AppsV1().Deployments(releaseNamespace),
		Pods:                         clientset.CoreV1().Pods(releaseNamespace),
		ConfigMaps:                   clientset.CoreV1().ConfigMaps(releaseNamespace),
		ConfigMapDeleter:             clientset.CoreV1().ConfigMaps(releaseNamespace),
		ReleaseName:                  releaseName,
		ReleaseNamespace:             releaseNamespace,
		CoordinationNamespace:        releaseNamespace,
		LeaderElectionID:             "ptah-operator.operator.ptah.run",
		WebhookServiceName:           "ptah-webhook",
		WebhookTimeoutSeconds:        10,
		WebhookSecretName:            "ptah-webhook-cert",
		WebhookPort:                  9443,
		CertificateHealthPort:        8081,
		HookServiceAccountName:       fmt.Sprintf("ptah-crd-v1-%x", identity)[:24],
		ControllerServiceAccountName: "ptah-controller-v1",
		ControllerDeploymentName:     "ptah-controller",
		ControllerReplicas:           2,
		CertificateDeploymentName:    "ptah-cert-rotator",
		ControllerStateVersion:       controllerstate.CurrentVersion,
		AdmissionContractVersion:     1,
		ReleaseSequence:              1,
		ManagerImage:                 managerImage,
		ControllerArgs: []string{
			"--executor-image=registry.example/executor@sha256:" + strings.Repeat("b", 64),
			"--webhook-port=9443",
		},
		CertificateArgs: []string{
			"--namespace=ptah-system",
			"--secret-name=ptah-webhook-cert",
			"--staging-secret-name=ptah-webhook-cert-stage",
			"--candidate-service-name=ptah-cert-transition",
			"--health-bind-address=:8081",
		},
		RuntimeDeploymentConfigExpressions: []string{`object.spec.replicas == 2`},
		RuntimePodConfigExpressions:        []string{`object.spec.restartPolicy == "Always"`},
		RuntimeAdmissionContractB64:        "e30=",
		PollEvery:                          time.Millisecond,
	}
}
