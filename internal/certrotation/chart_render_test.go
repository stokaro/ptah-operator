package certrotation_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/stokaro/ptah-operator/internal/certrotation"
)

const (
	releaseName      = "rotation-test"
	releaseNamespace = "ptah-system"
	managerDigest    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	executorDigest   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	runnerDigest     = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	ptahVersion      = "e2e-explicit-version"
)

func TestGeneratedCertificateLifecycleRender(t *testing.T) {
	t.Parallel()
	renderStarted := time.Now()
	objects := renderChart(t, "--set", "webhook.timeoutSeconds=7")
	renderFinished := time.Now()
	managerName := releaseName + "-ptah-operator"
	rotatorName := releaseName + "-ptah-operator-cert-rotator"
	secretName := releaseName + "-ptah-operator-webhook-cert"
	stagingSecretName := releaseName + "-ptah-operator-cert-rotation-stage"
	leaseName := releaseName + "-ptah-operator-cert-rotation"
	configurationName := "ptah-operator-admission"

	secret := mustObject(t, objects, "Secret", secretName)
	if !maps.Equal(secret.GetLabels(), map[string]string{
		certrotation.GeneratedSecretLabel: certrotation.GeneratedSecretLabelValue,
		certrotation.HelmManagedByLabel:   certrotation.HelmManagedByLabelValue,
	}) || !maps.Equal(secret.GetAnnotations(), map[string]string{
		certrotation.HelmReleaseNameAnnotation:      releaseName,
		certrotation.HelmReleaseNamespaceAnnotation: releaseNamespace,
	}) {
		t.Fatalf("generated Secret ownership metadata = labels=%v annotations=%v", secret.GetLabels(), secret.GetAnnotations())
	}
	for _, key := range []string{"ca.crt", "ca.key", "tls.crt", "tls.key"} {
		if _, found, err := unstructured.NestedString(secret.Object, "data", key); err != nil || !found {
			t.Errorf("generated Secret data is missing %q", key)
		}
	}
	ca := mustCertificateFromSecret(t, secret, "ca.crt")
	serving := mustCertificateFromSecret(t, secret, "tls.crt")
	assertBootstrapCertificateExpiry(t, ca, renderStarted, renderFinished, 2*24*time.Hour)
	assertBootstrapCertificateExpiry(t, serving, renderStarted, renderFinished, 24*time.Hour)
	// The rotator replaces a CA that was issued already inside the renewal
	// threshold in its first pass rather than after the switch delay, so that
	// the install's `helm install --wait` covers the whole transition. That
	// holds only while the bootstrap CA lives no longer than the threshold.
	if lifetime, threshold := ca.NotAfter.Sub(ca.NotBefore), 720*time.Hour; lifetime > threshold {
		t.Fatalf("bootstrap CA lives %s, longer than the default renewal threshold %s", lifetime, threshold)
	}
	if !serving.NotAfter.Before(ca.NotAfter) {
		t.Fatalf("bootstrap serving certificate expires at %s, CA expires at %s", serving.NotAfter, ca.NotAfter)
	}
	stagingSecret := mustObject(t, objects, "Secret", stagingSecretName)
	if !maps.Equal(stagingSecret.GetLabels(), map[string]string{
		certrotation.StagingSecretLabel: certrotation.StagingSecretLabelValue,
		certrotation.HelmManagedByLabel: certrotation.HelmManagedByLabelValue,
	}) || !maps.Equal(stagingSecret.GetAnnotations(), map[string]string{
		certrotation.HelmReleaseNameAnnotation:      releaseName,
		certrotation.HelmReleaseNamespaceAnnotation: releaseNamespace,
	}) {
		t.Fatalf("staging Secret ownership metadata = labels=%v annotations=%v", stagingSecret.GetLabels(), stagingSecret.GetAnnotations())
	}
	if len(stagingSecret.GetOwnerReferences()) != 0 || len(stagingSecret.GetFinalizers()) != 0 {
		t.Fatalf("staging Secret contains unsupported metadata: %#v", stagingSecret.Object["metadata"])
	}
	if secretType, _, _ := unstructured.NestedString(stagingSecret.Object, "type"); secretType != "Opaque" {
		t.Fatalf("staging Secret type = %q, want Opaque", secretType)
	}
	if data, found, err := unstructured.NestedMap(stagingSecret.Object, "data"); err != nil || found || len(data) != 0 {
		t.Fatalf("chart owns staging Secret data: found=%v data=%v err=%v", found, data, err)
	}
	if stringData, found, err := unstructured.NestedMap(stagingSecret.Object, "stringData"); err != nil || found || len(stringData) != 0 {
		t.Fatalf("chart owns staging Secret stringData: found=%v data=%v err=%v", found, stringData, err)
	}
	if immutable, found, err := unstructured.NestedBool(stagingSecret.Object, "immutable"); err != nil || found || immutable {
		t.Fatalf("chart sets staging Secret immutable: found=%v value=%v err=%v", found, immutable, err)
	}
	managerRole := mustObject(t, objects, "ClusterRole", managerName)
	for _, rule := range objectRules(t, managerRole) {
		if slices.Contains(stringSlice(rule["resources"]), "secrets") {
			t.Fatal("manager ClusterRole grants Secret access")
		}
	}

	role := mustNamespacedObject(t, objects, "Role", releaseNamespace, rotatorName)
	assertExactRule(t, role, "", "secrets", []string{secretName, stagingSecretName}, []string{"get", "update"})
	assertNoResourceVerb(t, role, "", "secrets", "create")
	assertExactRule(t, role, "coordination.k8s.io", "leases", []string{leaseName}, []string{"get", "update"})
	assertExactRule(t, role, "discovery.k8s.io", "endpointslices", nil, []string{"list"})
	// The rotator lists the webhook Service's EndpointSlices in the release
	// namespace and nowhere else, and nothing in the release reaches into
	// default.
	for _, object := range objects {
		if object.GetNamespace() == "default" {
			t.Fatalf("the release renders %s/%s into the default namespace", object.GetKind(), object.GetName())
		}
	}
	// No runtime Pod checks its own admission any more, so no Role grants it
	// the reads that check needed.
	assertObjectAbsent(t, objects, "Role", managerName+"-runtime-admission")
	assertObjectAbsent(t, objects, "RoleBinding", managerName+"-runtime-admission")

	clusterRole := mustObject(t, objects, "ClusterRole", rotatorName)
	assertNoResourceVerb(t, clusterRole, "discovery.k8s.io", "endpointslices", "list")
	assertExactRule(t, clusterRole, "admissionregistration.k8s.io", "mutatingwebhookconfigurations", []string{configurationName}, []string{"get", "update"})
	assertExactRule(t, clusterRole, "admissionregistration.k8s.io", "validatingwebhookconfigurations", []string{configurationName}, []string{"get", "update"})
	// The rotator reads no admission policy: the guards it once verified are
	// gone.
	assertNoResourceVerb(t, clusterRole, "admissionregistration.k8s.io", "validatingadmissionpolicies", "get")
	assertNoResourceVerb(t, clusterRole, "admissionregistration.k8s.io", "validatingadmissionpolicybindings", "get")
	assertNoResourceVerb(t, clusterRole, "scheduling.k8s.io", "priorityclasses", "get")
	// Helm deletes the staging Secret with the rest of the release, and no
	// uninstall hook runs before it.
	for _, object := range objects {
		if strings.Contains(object.GetAnnotations()["helm.sh/hook"], "pre-delete") {
			t.Fatalf("the release renders the uninstall hook object %s/%s", object.GetKind(), object.GetName())
		}
	}
	for _, object := range []*unstructured.Unstructured{stagingSecret, secret} {
		if policy := object.GetAnnotations()["helm.sh/resource-policy"]; policy != "" {
			t.Fatalf("Secret %s outlives the release with resource policy %q", object.GetName(), policy)
		}
	}
	assertObjectAbsent(t, objects, "ValidatingAdmissionPolicy", rotatorName)
	assertObjectAbsent(t, objects, "ValidatingAdmissionPolicyBinding", rotatorName)
	mustObject(t, objects, "Lease", leaseName)

	rotatorDeployment := mustObject(t, objects, "Deployment", rotatorName)
	if len(rotatorDeployment.GetName()) > 63 {
		t.Fatalf("rotator Deployment name length = %d, want at most 63", len(rotatorDeployment.GetName()))
	}
	if got, _, _ := unstructured.NestedString(rotatorDeployment.Object, "spec", "strategy", "type"); got != "Recreate" {
		t.Errorf("rotator Deployment strategy = %q, want Recreate", got)
	}
	containers, _, err := unstructured.NestedSlice(rotatorDeployment.Object, "spec", "template", "spec", "containers")
	if err != nil || len(containers) != 1 {
		t.Fatalf("rotator Deployment containers = %d, want 1", len(containers))
	}
	container := containers[0].(map[string]any)
	if got := stringSlice(container["command"]); !slices.Equal(got, []string{"/ptah-cert-rotator"}) {
		t.Errorf("rotator command = %v", got)
	}
	args := stringSlice(container["args"])
	for _, want := range []string{
		"--release-name=" + releaseName,
		"--staging-secret-name=" + stagingSecretName,
		"--mutating-webhook-names=mapproval.operator.ptah.run,mmigrationapproval.operator.ptah.run",
		"--validating-webhook-names=vapproval.operator.ptah.run,vmigrationapproval.operator.ptah.run,vpodintent.operator.ptah.run,vcontrollerwrite.operator.ptah.run",
		"--run-interval=6h",
		"--ca-switch-delay=6h",
		"--operation-timeout=15m",
		"--retry-initial=5s",
		"--retry-max=5m",
		"--health-bind-address=:8081",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("rotator args do not contain %q: %v", want, args)
		}
	}
	mutatingConfiguration := mustObject(t, objects, "MutatingWebhookConfiguration", configurationName)
	validatingConfiguration := mustObject(t, objects, "ValidatingWebhookConfiguration", configurationName)
	for _, configuration := range []*unstructured.Unstructured{mutatingConfiguration, validatingConfiguration} {
		if got := configuration.GetAnnotations()["operator.ptah.run/admission-contract-version"]; got != "2" {
			t.Fatalf("%s admission contract version = %q, want 2", configuration.GetKind(), got)
		}
	}
	if got, want := strings.Split(requiredArgumentValue(t, args, "--mutating-webhook-names="), ","), []string{
		"mapproval.operator.ptah.run", "mmigrationapproval.operator.ptah.run",
	}; !slices.Equal(got, want) {
		t.Fatalf("rotator mutating production webhook inventory = %v, want %v", got, want)
	}
	if got, want := strings.Split(requiredArgumentValue(t, args, "--validating-webhook-names="), ","), []string{
		"vapproval.operator.ptah.run", "vmigrationapproval.operator.ptah.run",
		"vpodintent.operator.ptah.run", "vcontrollerwrite.operator.ptah.run",
	}; !slices.Equal(got, want) {
		t.Fatalf("rotator validating production webhook inventory = %v, want %v", got, want)
	}
	for _, forbiddenPrefix := range []string{
		"--candidate-",
		"--recreate-missing-secret",
	} {
		if slices.ContainsFunc(args, func(arg string) bool { return strings.HasPrefix(arg, forbiddenPrefix) }) {
			t.Errorf("rotator args unexpectedly contain %q: %v", forbiddenPrefix, args)
		}
	}
	ports := container["ports"].([]any)
	if len(ports) != 1 {
		t.Fatalf("rotator ports = %d, want the health port alone", len(ports))
	}
	wantPorts := map[string]int64{"health": 8081}
	for _, value := range ports {
		port := value.(map[string]any)
		name, _ := port["name"].(string)
		if port["containerPort"] != wantPorts[name] || port["protocol"] != "TCP" {
			t.Errorf("rotator port = %#v", port)
		}
		delete(wantPorts, name)
	}
	if len(wantPorts) != 0 {
		t.Errorf("rotator ports omit %v", wantPorts)
	}
	assertHTTPProbe(t, container, "livenessProbe", "/healthz")
	assertHTTPProbe(t, container, "readinessProbe", "/readyz")
	// The admission canary is gone with the proof that used it, in either
	// certificate mode.
	assertObjectAbsent(t, objects, "ConfigMap", releaseName+"-ptah-operator-cert-canary")
	assertObjectAbsent(t, objects, "Service", releaseName+"-ptah-operator-cert-transition")
	for _, configuration := range []*unstructured.Unstructured{mutatingConfiguration, validatingConfiguration} {
		webhooks, _, err := unstructured.NestedSlice(configuration.Object, "webhooks")
		if err != nil {
			t.Fatal(err)
		}
		for _, webhook := range webhooks {
			if name, _ := webhook.(map[string]any)["name"].(string); strings.Contains(name, "canary") {
				t.Fatalf("%s carries the canary entry %s", configuration.GetKind(), name)
			}
		}
	}

	deployment := mustObject(t, objects, "Deployment", releaseName+"-ptah-operator")
	containers, _, err = unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if err != nil || len(containers) != 1 {
		t.Fatalf("manager Deployment containers = %d, want 1", len(containers))
	}
	managerArgs := stringSlice(containers[0].(map[string]any)["args"])
	if !slices.Contains(managerArgs, "--ptah-version="+ptahVersion) {
		t.Fatalf("manager args do not bind the explicit Ptah version: %v", managerArgs)
	}
	if component := deployment.GetLabels()["app.kubernetes.io/component"]; component != "controller" {
		t.Fatalf("manager Deployment component label = %q, want controller", component)
	}
	var controllerDeployments []string
	for _, object := range objects {
		labels := object.GetLabels()
		if object.GetKind() == "Deployment" &&
			labels["app.kubernetes.io/instance"] == releaseName &&
			labels["app.kubernetes.io/component"] == "controller" {
			controllerDeployments = append(controllerDeployments, object.GetName())
		}
	}
	if !slices.Equal(controllerDeployments, []string{managerName}) {
		t.Fatalf("controller-labeled Deployments = %v, want only %q", controllerDeployments, managerName)
	}
	assertManagerTLSProjection(t, deployment, secretName)
}

func mustCertificateFromSecret(t *testing.T, secret *unstructured.Unstructured, key string) *x509.Certificate {
	t.Helper()
	encoded, found, err := unstructured.NestedString(secret.Object, "data", key)
	if err != nil || !found {
		t.Fatalf("generated Secret data is missing %q", key)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode generated Secret %s: %v", key, err)
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("generated Secret %s is not exactly one PEM certificate", key)
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse generated Secret %s: %v", key, err)
	}
	return certificate
}

func assertBootstrapCertificateExpiry(
	t *testing.T,
	certificate *x509.Certificate,
	renderStarted time.Time,
	renderFinished time.Time,
	validity time.Duration,
) {
	t.Helper()
	const timestampTolerance = time.Minute
	earliest := renderStarted.Add(validity - timestampTolerance)
	latest := renderFinished.Add(validity + timestampTolerance)
	if certificate.NotAfter.Before(earliest) || certificate.NotAfter.After(latest) {
		t.Fatalf("bootstrap certificate expiry = %s, want between %s and %s", certificate.NotAfter, earliest, latest)
	}
}

func TestCertificateEndpointSliceRBACIsNamespaceBound(t *testing.T) {
	t.Parallel()
	for _, namespace := range []string{releaseNamespace, "default"} {
		namespace := namespace
		t.Run(namespace, func(t *testing.T) {
			t.Parallel()
			objects := renderChartInNamespace(t, namespace)
			rotatorName := releaseName + "-ptah-operator-cert-rotator"

			roles := 0
			bindings := 0
			for _, object := range objects {
				switch object.GetKind() {
				case "ClusterRole":
					for _, rule := range objectRules(t, object) {
						groups := stringSlice(rule["apiGroups"])
						resources := stringSlice(rule["resources"])
						if (slices.Contains(groups, "discovery.k8s.io") || slices.Contains(groups, "*")) &&
							(slices.Contains(resources, "endpointslices") || slices.Contains(resources, "*")) {
							t.Fatalf("ClusterRole/%s grants EndpointSlice authority: %v", object.GetName(), rule)
						}
					}
				case "Role":
					for _, rule := range objectRules(t, object) {
						if slices.Contains(stringSlice(rule["apiGroups"]), "discovery.k8s.io") {
							if object.GetNamespace() != namespace || object.GetName() != rotatorName {
								t.Fatalf("Role %s/%s grants EndpointSlice authority outside the rotator's Role", object.GetNamespace(), object.GetName())
							}
							roles++
						}
					}
				case "RoleBinding":
					if object.GetName() == rotatorName {
						if object.GetNamespace() != namespace {
							t.Fatalf("certificate RoleBinding rendered into %s, want %s", object.GetNamespace(), namespace)
						}
						assertExactCertificateRoleBinding(t, objects, namespace, rotatorName, rotatorName, namespace)
						bindings++
					}
				}
			}
			if roles != 1 || bindings != 1 {
				t.Fatalf("certificate EndpointSlice RBAC = %d rules and %d RoleBindings, want one of each", roles, bindings)
			}
		})
	}
}

func TestMissingSecretRecreationOptInRender(t *testing.T) {
	t.Parallel()
	objects := renderChart(t, "--set", "certificateRotation.recreateMissingSecret=true")
	rotatorName := releaseName + "-ptah-operator-cert-rotator"
	secretName := releaseName + "-ptah-operator-webhook-cert"

	role := mustObject(t, objects, "Role", rotatorName)
	assertExactRule(t, role, "", "secrets", nil, []string{"create"})
	clusterRole := mustObject(t, objects, "ClusterRole", rotatorName)
	assertExactRule(t, clusterRole, "admissionregistration.k8s.io", "validatingadmissionpolicies", []string{rotatorName}, []string{"get"})
	assertExactRule(t, clusterRole, "admissionregistration.k8s.io", "validatingadmissionpolicybindings", []string{rotatorName}, []string{"get"})
	assertSecretCreateGuard(t, objects, rotatorName, secretName)

	deployment := mustObject(t, objects, "Deployment", rotatorName)
	containers, _, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if err != nil || len(containers) != 1 {
		t.Fatalf("rotator Deployment containers = %d, want 1", len(containers))
	}
	args := stringSlice(containers[0].(map[string]any)["args"])
	if !slices.Contains(args, "--recreate-missing-secret=true") {
		t.Errorf("rotator args do not contain %q: %v", "--recreate-missing-secret=true", args)
	}
}

func TestGeneratedCertificateRequiresRotation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "schema validation"},
		{name: "template validation", args: []string{"--skip-schema-validation"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			args := append(slices.Clone(test.args), "--set", "certificateRotation.enabled=false")
			if _, err := renderChartCommand(t, args...); err == nil {
				t.Fatal("Helm accepted a generated webhook certificate without its rotator")
			}
		})
	}
}

func TestExistingSecretDisablesBuiltInLifecycle(t *testing.T) {
	t.Parallel()
	objects := renderChart(t,
		"--set-string", "webhook.existingSecret=external-webhook-cert",
		"--set-string", "webhook.caBundle=external-ca",
		"--set", "certificateRotation.recreateMissingSecret=true",
	)
	for _, object := range objects {
		if object.GetLabels()["app.kubernetes.io/component"] == "certificate-rotation" {
			t.Fatalf("external Secret render contains certificate lifecycle object %s/%s", object.GetKind(), object.GetName())
		}
		if object.GetKind() == "CronJob" || object.GetKind() == "Lease" {
			t.Fatalf("external Secret render contains %s %q", object.GetKind(), object.GetName())
		}
	}
	assertObjectAbsent(t, objects, "Secret", releaseName+"-ptah-operator-cert-rotation-stage")
	assertObjectAbsent(t, objects, "ConfigMap", releaseName+"-ptah-operator-cert-canary")
	assertObjectAbsent(t, objects, "Service", releaseName+"-ptah-operator-cert-transition")
	// The admission contract no longer depends on how certificates are
	// managed, so switching to an external Secret is not a contract downgrade
	// the singleton check would refuse.
	for _, kind := range []string{"MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"} {
		configuration := mustObject(t, objects, kind, "ptah-operator-admission")
		if got := configuration.GetAnnotations()["operator.ptah.run/admission-contract-version"]; got != "2" {
			t.Fatalf("%s external-certificate admission contract version = %q, want 2", kind, got)
		}
	}
	deployment := mustObject(t, objects, "Deployment", releaseName+"-ptah-operator")
	assertManagerTLSProjection(t, deployment, "external-webhook-cert")
}

func TestLongFullnameKeepsGeneratedNamesValid(t *testing.T) {
	t.Parallel()
	objects := renderChart(t,
		"--set-string", "fullnameOverride="+strings.Repeat("a", 120),
		"--set", "certificateRotation.recreateMissingSecret=true",
	)
	for _, object := range objects {
		limit := 253
		switch object.GetKind() {
		case "ConfigMap", "Deployment", "Lease", "PodDisruptionBudget", "Secret", "Service", "ServiceAccount":
			limit = 63
		}
		if len(object.GetName()) > limit {
			t.Errorf("%s name %q has length %d, limit %d", object.GetKind(), object.GetName(), len(object.GetName()), limit)
		}
	}
}

func TestCASwitchDelayRendersFromTheIntervalUnlessSet(t *testing.T) {
	t.Parallel()
	rotatorName := releaseName + "-ptah-operator-cert-rotator"
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "default", want: "--ca-switch-delay=6h"},
		{name: "interval only", args: []string{"--set-string", "certificateRotation.interval=3h"}, want: "--ca-switch-delay=3h"},
		{name: "explicit delay", args: []string{"--set-string", "certificateRotation.caSwitchDelay=90s"}, want: "--ca-switch-delay=90s"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			objects := renderChart(t, test.args...)
			rotatorDeployment := mustObject(t, objects, "Deployment", rotatorName)
			containers, _, err := unstructured.NestedSlice(rotatorDeployment.Object, "spec", "template", "spec", "containers")
			if err != nil || len(containers) != 1 {
				t.Fatalf("rotator Deployment containers = %d, want 1", len(containers))
			}
			args := stringSlice(containers[0].(map[string]any)["args"])
			var delays []string
			for _, arg := range args {
				if strings.HasPrefix(arg, "--ca-switch-delay=") {
					delays = append(delays, arg)
				}
			}
			if !slices.Equal(delays, []string{test.want}) {
				t.Fatalf("rotator CA switch delay arguments = %v, want exactly %q", delays, test.want)
			}
		})
	}
}

// renewalThresholdRefusal is what the chart says when the renewal threshold is
// shorter than the bootstrap CA it generates, which
// TestGeneratedCertificateLifecycleRender pins to two days.
const renewalThresholdRefusal = "certificateRotation.renewalThreshold must be at least 48h"

func TestCertificateRotationValueValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		flag    string
		setting string
		// want, when set, is the refusal the render must name, for a value
		// the schema admits and only a template refuses.
		want string
	}{
		{flag: "--set-string", setting: "certificateRotation.probeTimeout=0s"},
		{flag: "--set-string", setting: "certificateRotation.interval=0s"},
		{flag: "--set-string", setting: "certificateRotation.caSwitchDelay=0s"},
		{flag: "--set-string", setting: "certificateRotation.caSwitchDelay=soon"},
		{flag: "--set-string", setting: "certificateRotation.caSwitchDelay=59s"},
		{flag: "--set-string", setting: "certificateRotation.caSwitchDelay=59999ms"},
		{flag: "--set-string", setting: "certificateRotation.renewalThreshold=47h", want: renewalThresholdRefusal},
		{flag: "--set-string", setting: "certificateRotation.renewalThreshold=172799s", want: renewalThresholdRefusal},
		{flag: "--set-string", setting: "certificateRotation.operationTimeout=0s"},
		{flag: "--set-string", setting: "certificateRotation.retryInitial=0s"},
		{flag: "--set-string", setting: "certificateRotation.retryMax=0s"},
		{flag: "--set", setting: "certificateRotation.healthPort=0"},
		// The canary's settings are gone, and a values file that still sets
		// one is refused rather than silently ignored.
		{flag: "--set", setting: "certificateRotation.candidatePort=9444"},
		{flag: "--set-string", setting: "certificateRotation.admissionConvergence.stabilityDuration=10s"},
		{flag: "--set-string", setting: "certificateRotation.recreateMissingSecret=not-a-boolean"},
		{flag: "--set-string", setting: "webhook.existingSecret=Bad_Name"},
	} {
		t.Run(test.setting, func(t *testing.T) {
			_, err := renderChartCommand(t, test.flag, test.setting)
			if err == nil {
				t.Fatalf("Helm accepted invalid value %q", test.setting)
			}
			if test.want == "" {
				return
			}
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || !strings.Contains(string(exitError.Stderr), test.want) {
				t.Fatalf("Helm refused %q with %v, want a refusal naming %q", test.setting, renderRefusal(err), test.want)
			}
		})
	}
	for _, setting := range []string{
		"certificateRotation.caSwitchDelay=60s",
		"certificateRotation.caSwitchDelay=1m",
		"certificateRotation.renewalThreshold=48h",
		"certificateRotation.renewalThreshold=172800s",
	} {
		if _, err := renderChartCommand(t, "--set-string", setting); err != nil {
			t.Errorf("Helm rejected the boundary value %q: %v", setting, err)
		}
	}
	// With the built-in lifecycle off there is no bootstrap CA to replace,
	// so a short threshold is not the chart's concern.
	if _, err := renderChartCommand(t,
		"--set-string", "certificateRotation.renewalThreshold=24h",
		"--set-string", "webhook.existingSecret=provided-webhook-cert",
		"--set-string", "webhook.caBundle="+base64.StdEncoding.EncodeToString([]byte("provided-ca")),
	); err != nil {
		t.Errorf("Helm rejected a short threshold without the built-in lifecycle: %v", err)
	}
}

func assertObjectAbsent(t *testing.T, objects []*unstructured.Unstructured, kind, name string) {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind && object.GetName() == name {
			t.Fatalf("rendered object %s/%s must be absent", kind, name)
		}
	}
}

func assertSecretCreateGuard(
	t *testing.T,
	objects []*unstructured.Unstructured,
	guardName string,
	secretName string,
) {
	t.Helper()
	policy := mustObject(t, objects, "ValidatingAdmissionPolicy", guardName)
	if failurePolicy, _, _ := unstructured.NestedString(policy.Object, "spec", "failurePolicy"); failurePolicy != "Fail" {
		t.Fatalf("Secret CREATE guard failurePolicy = %q, want Fail", failurePolicy)
	}
	matchConditions, _, err := unstructured.NestedSlice(policy.Object, "spec", "matchConditions")
	if err != nil || len(matchConditions) != 1 {
		t.Fatalf("Secret CREATE guard matchConditions = %#v", matchConditions)
	}
	matchExpression := matchConditions[0].(map[string]any)["expression"].(string)
	wantUsername := "system:serviceaccount:" + releaseNamespace + ":" + guardName
	if !strings.Contains(matchExpression, wantUsername) {
		t.Fatalf("Secret CREATE guard does not bind exact ServiceAccount %q", wantUsername)
	}
	validations, _, err := unstructured.NestedSlice(policy.Object, "spec", "validations")
	if err != nil || len(validations) != 1 {
		t.Fatalf("Secret CREATE guard validations = %#v", validations)
	}
	validation := validations[0].(map[string]any)
	if validation["message"] != "certificate rotator Secret CREATE is outside its exact recovery contract" {
		t.Fatalf("Secret CREATE guard denial message = %v", validation["message"])
	}
	expression := validation["expression"].(string)
	for _, required := range []string{
		"object.metadata.name == '" + secretName + "'",
		"object.metadata.namespace == '" + releaseNamespace + "'",
		"(!has(object.metadata.generateName) || object.metadata.generateName == '')",
		"object.metadata.labels ==",
		"operator.ptah.run/generated-webhook-certificate",
		"object.metadata.annotations ==",
		"app.kubernetes.io/managed-by",
		"meta.helm.sh/release-name",
		"meta.helm.sh/release-namespace",
		"object.metadata.ownerReferences.size() == 0",
		"object.metadata.finalizers.size() == 0",
		"object.type == 'kubernetes.io/tls'",
		"!has(object.immutable)",
		"object.stringData.size() == 0",
		"object.data.size() == 4",
		"'ca.crt' in object.data",
		"'ca.key' in object.data",
		"'tls.crt' in object.data",
		"'tls.key' in object.data",
	} {
		if !strings.Contains(expression, required) {
			t.Errorf("Secret CREATE guard expression does not contain %q", required)
		}
	}

	binding := mustObject(t, objects, "ValidatingAdmissionPolicyBinding", guardName)
	if policyName, _, _ := unstructured.NestedString(binding.Object, "spec", "policyName"); policyName != guardName {
		t.Fatalf("Secret CREATE guard binding policyName = %q, want %q", policyName, guardName)
	}
	actions, _, err := unstructured.NestedStringSlice(binding.Object, "spec", "validationActions")
	if err != nil || !slices.Equal(actions, []string{"Deny"}) {
		t.Fatalf("Secret CREATE guard validationActions = %v, want [Deny]", actions)
	}
	namespace, _, _ := unstructured.NestedString(
		binding.Object,
		"spec", "matchResources", "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name",
	)
	if namespace != releaseNamespace {
		t.Fatalf("Secret CREATE guard namespace = %q, want %q", namespace, releaseNamespace)
	}
	contract := certrotation.Config{
		Namespace:   releaseNamespace,
		ReleaseName: releaseName,
		SecretName:  secretName,
	}
	typedPolicy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(policy.Object, typedPolicy); err != nil {
		t.Fatalf("decode Secret CREATE guard policy: %v", err)
	}
	if err := certrotation.VerifySecretCreatePolicyContract(typedPolicy, contract, guardName); err != nil {
		t.Fatalf("rendered Secret CREATE guard policy differs from runtime contract: %v", err)
	}
	typedBinding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(binding.Object, typedBinding); err != nil {
		t.Fatalf("decode Secret CREATE guard binding: %v", err)
	}
	if err := certrotation.VerifySecretCreateBindingContract(typedBinding, contract, guardName); err != nil {
		t.Fatalf("rendered Secret CREATE guard binding differs from runtime contract: %v", err)
	}
}

func assertHTTPProbe(t *testing.T, container map[string]any, field, path string) {
	t.Helper()
	probe, ok := container[field].(map[string]any)
	if !ok {
		t.Fatalf("rotator %s = %#v", field, container[field])
	}
	httpGet, ok := probe["httpGet"].(map[string]any)
	if !ok {
		t.Fatalf("rotator %s.httpGet = %#v", field, probe["httpGet"])
	}
	if httpGet["path"] != path || httpGet["port"] != "health" || httpGet["scheme"] != "HTTP" {
		t.Errorf("rotator %s.httpGet = %#v", field, httpGet)
	}
}

func TestChartRequiresBoundedExplicitPtahVersion(t *testing.T) {
	t.Parallel()
	for name, version := range map[string]string{
		"missing":     "",
		"over-bounds": strings.Repeat("v", 129),
	} {
		name, version := name, version
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := renderChartCommand(t,
				"--set-string", "execution.ptahVersion="+version,
			); err == nil {
				t.Fatalf("chart accepted invalid explicit Ptah version %q", version)
			}
		})
	}
}

func TestChartPreservesExplicitPtahVersionAsOneExactArgument(t *testing.T) {
	t.Parallel()

	const version = "v1 # exact-build"
	objects := renderChart(t, "--set-string", "execution.ptahVersion="+version)
	deployment := mustObject(t, objects, "Deployment", releaseName+"-ptah-operator")
	containers, _, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if err != nil || len(containers) != 1 {
		t.Fatalf("manager Deployment containers = %d, want 1", len(containers))
	}
	managerArgs := stringSlice(containers[0].(map[string]any)["args"])
	if !slices.Contains(managerArgs, "--ptah-version="+version) {
		t.Fatalf("manager args did not preserve the exact Ptah version: %v", managerArgs)
	}
}

// TestChartRequireDistinctApproverReachesManagerArgs proves the four-eyes
// switch travels from the chart value to the manager's own flag: it is the
// installer's control precisely because nothing on a PtahSchema or
// PtahMigration can set it, so the only path from a value to the running
// handlers is this one.
func TestChartRequireDistinctApproverReachesManagerArgs(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "default off", want: "--require-distinct-approver=false"},
		{
			name: "explicit on",
			args: []string{"--set", "approvals.requireDistinctApprover=true"},
			want: "--require-distinct-approver=true",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			objects := renderChart(t, test.args...)
			deployment := mustObject(t, objects, "Deployment", releaseName+"-ptah-operator")
			containers, _, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
			if err != nil || len(containers) != 1 {
				t.Fatalf("manager Deployment containers = %d, want 1", len(containers))
			}
			managerArgs := stringSlice(containers[0].(map[string]any)["args"])
			if !slices.Contains(managerArgs, test.want) {
				t.Fatalf("manager args = %v, want %s", managerArgs, test.want)
			}
		})
	}
}

// TestChartRequireDistinctApproverGatesSpecWriterWebhooks proves the two
// spec-writer mutating webhook entries -- mschemawriter and mmigrationwriter --
// exist only when approvals.requireDistinctApprover is on, and that the
// rotator's probed inventory and the CRD hook's runtime-verify flag move with
// them. Off is the default a plain PtahSchema or PtahMigration write must not
// pay the mutating webhook's failurePolicy: Fail coupling for.
func TestChartRequireDistinctApproverGatesSpecWriterWebhooks(t *testing.T) {
	t.Parallel()

	writerNames := []string{"mschemawriter.operator.ptah.run", "mmigrationwriter.operator.ptah.run"}

	for _, test := range []struct {
		name            string
		args            []string
		wantMutatingArg string
		wantWriterCount int
	}{
		{
			name:            "default off",
			wantMutatingArg: "--mutating-webhook-names=mapproval.operator.ptah.run,mmigrationapproval.operator.ptah.run",
			wantWriterCount: 0,
		},
		{
			name:            "explicit on",
			args:            []string{"--set", "approvals.requireDistinctApprover=true"},
			wantMutatingArg: "--mutating-webhook-names=mapproval.operator.ptah.run,mmigrationapproval.operator.ptah.run,mschemawriter.operator.ptah.run,mmigrationwriter.operator.ptah.run",
			wantWriterCount: 2,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			objects := renderChart(t, test.args...)

			mutatingConfiguration := mustObject(t, objects, "MutatingWebhookConfiguration", "ptah-operator-admission")
			webhooks, _, err := unstructured.NestedSlice(mutatingConfiguration.Object, "webhooks")
			if err != nil {
				t.Fatal(err)
			}
			present := 0
			for _, webhook := range webhooks {
				name, _ := webhook.(map[string]any)["name"].(string)
				if slices.Contains(writerNames, name) {
					present++
					clientConfig := webhook.(map[string]any)["clientConfig"].(map[string]any)
					service := clientConfig["service"].(map[string]any)
					wantPath := "/mutate-operator-ptah-run-v1alpha1-ptahschema"
					if name == "mmigrationwriter.operator.ptah.run" {
						wantPath = "/mutate-operator-ptah-run-v1alpha1-ptahmigration"
					}
					if service["path"] != wantPath {
						t.Errorf("%s clientConfig.service.path = %v, want %s", name, service["path"], wantPath)
					}
					if webhook.(map[string]any)["failurePolicy"] != "Fail" {
						t.Errorf("%s failurePolicy = %v, want Fail", name, webhook.(map[string]any)["failurePolicy"])
					}
				}
			}
			if present != test.wantWriterCount {
				t.Fatalf("MutatingWebhookConfiguration carries %d of the spec-writer entries, want %d (webhooks: %v)",
					present, test.wantWriterCount, webhooks)
			}

			rotatorDeployment := mustObject(t, objects, "Deployment", releaseName+"-ptah-operator-cert-rotator")
			rotatorContainers, _, err := unstructured.NestedSlice(rotatorDeployment.Object, "spec", "template", "spec", "containers")
			if err != nil || len(rotatorContainers) != 1 {
				t.Fatalf("rotator Deployment containers = %d, want 1", len(rotatorContainers))
			}
			rotatorArgs := stringSlice(rotatorContainers[0].(map[string]any)["args"])
			if !slices.Contains(rotatorArgs, test.wantMutatingArg) {
				t.Fatalf("rotator args = %v, want %s", rotatorArgs, test.wantMutatingArg)
			}

			for _, deploymentName := range []string{releaseName + "-ptah-operator", releaseName + "-ptah-operator-cert-rotator"} {
				deployment := mustObject(t, objects, "Deployment", deploymentName)
				initContainers, _, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "initContainers")
				if err != nil || len(initContainers) != 1 {
					t.Fatalf("%s init containers = %d, want 1", deploymentName, len(initContainers))
				}
				initArgs := stringSlice(initContainers[0].(map[string]any)["args"])
				wantVerifyArg := "--require-distinct-approver=false"
				if len(test.args) > 0 {
					wantVerifyArg = "--require-distinct-approver=true"
				}
				if !slices.Contains(initArgs, wantVerifyArg) {
					t.Fatalf("%s runtime-verify init container args = %v, want %s", deploymentName, initArgs, wantVerifyArg)
				}
			}
		})
	}
}

// The controller runs as one ServiceAccount in every release: the configured
// name, or the release's full name when none is configured. An external one is
// used as named and never rendered.
func TestControllerServiceAccountIsTheSameNameInEveryRelease(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		args     []string
		want     string
		external bool
	}{
		{name: "generated", want: releaseName + "-ptah-operator"},
		{name: "configured", args: []string{"--set-string", "serviceAccount.name=platform-controller"}, want: "platform-controller"},
		{
			name:     "external",
			args:     []string{"--set", "serviceAccount.create=false", "--set-string", "serviceAccount.name=platform-controller"},
			want:     "platform-controller",
			external: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			objects := renderChart(t, test.args...)
			deployment := mustObject(t, objects, "Deployment", releaseName+"-ptah-operator")
			name, found, err := unstructured.NestedString(deployment.Object, "spec", "template", "spec", "serviceAccountName")
			if err != nil || !found {
				t.Fatalf("controller Deployment serviceAccountName: found=%t err=%v", found, err)
			}
			if name != test.want {
				t.Fatalf("controller ServiceAccount name = %q, want %q", name, test.want)
			}
			rendered := false
			for _, object := range objects {
				if object.GetKind() == "ServiceAccount" && object.GetName() == name {
					rendered = true
				}
			}
			if rendered == test.external {
				t.Fatalf("controller ServiceAccount %s/%s rendered = %t, want %t", releaseNamespace, name, rendered, !test.external)
			}
		})
	}
}

// A ServiceAccount name is a DNS subdomain, so the whole Kubernetes range is
// available and nothing past it is.
func TestControllerServiceAccountNameKeepsTheKubernetesRange(t *testing.T) {
	t.Parallel()

	for _, create := range []string{"true", "false"} {
		if _, err := renderChartCommand(t,
			"--set", "serviceAccount.create="+create,
			"--set-string", "serviceAccount.name="+strings.Repeat("a", 253),
		); err != nil {
			t.Fatalf("serviceAccount.create=%s rejected a 253-character controller ServiceAccount: %v", create, err)
		}
		if _, err := renderChartCommand(t,
			"--set", "serviceAccount.create="+create,
			"--set-string", "serviceAccount.name="+strings.Repeat("a", 254),
		); err == nil {
			t.Fatalf("serviceAccount.create=%s accepted a 254-character controller ServiceAccount", create)
		}
	}
}

func TestConfiguredPriorityClassReachesTheReconcileHookJob(t *testing.T) {
	t.Parallel()

	const priorityClassName = "runtime-critical"
	objects := renderChart(t, "--set-string", "priorityClassName="+priorityClassName)
	jobs := 0
	for _, object := range objects {
		if object.GetKind() != "Job" {
			continue
		}
		jobs++
		if component := object.GetLabels()["app.kubernetes.io/component"]; component != "crd-manager" {
			t.Fatalf("the release renders the hook Job %s of component %q, want the CRD reconcile hook alone", object.GetName(), component)
		}
		priorityClass, found, err := unstructured.NestedString(object.Object, "spec", "template", "spec", "priorityClassName")
		if err != nil {
			t.Fatalf("Job/%s priorityClassName: %v", object.GetName(), err)
		}
		if !found || priorityClass != priorityClassName {
			t.Fatalf("reconcile Job priorityClassName = %q, found=%t; want %q", priorityClass, found, priorityClassName)
		}
		for _, field := range []string{"priority", "preemptionPolicy"} {
			if value, found, err := unstructured.NestedFieldNoCopy(object.Object, "spec", "template", "spec", field); err != nil {
				t.Fatalf("Job/%s %s: %v", object.GetName(), field, err)
			} else if found {
				t.Fatalf("Job/%s contains Pod-admission output field %s=%v", object.GetName(), field, value)
			}
		}
	}
	if jobs != 1 {
		t.Fatalf("the release renders %d hook Jobs, want the CRD reconcile hook alone", jobs)
	}
	// The class's value and preemption policy are the PriorityClass's own:
	// the chart no longer pins them, and refuses the values that did.
	for _, setting := range []string{"priorityClassValue=1000", "priorityClassPreemptionPolicy=Never"} {
		if _, err := renderChartCommand(t, "--set-string", "priorityClassName="+priorityClassName, "--set-string", setting); err == nil {
			t.Fatalf("Helm accepted the removed value %s", setting)
		}
	}
}

func renderChart(t *testing.T, additionalArgs ...string) []*unstructured.Unstructured {
	return renderChartInNamespace(t, releaseNamespace, additionalArgs...)
}

func renderChartInNamespace(t *testing.T, namespace string, additionalArgs ...string) []*unstructured.Unstructured {
	t.Helper()
	output, err := renderChartCommandInNamespace(t, namespace, additionalArgs...)
	if err != nil {
		// Never print renderer output: a generated chart render contains private
		// key material by design.
		t.Fatalf("helm template failed: %v", err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	var objects []*unstructured.Unstructured
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode Helm object: %v", err)
		}
		if object.Object == nil || object.GetKind() == "" {
			continue
		}
		objects = append(objects, object)
	}
	return objects
}

// renderRefusal returns what Helm printed when it refused a render, or the
// error itself when Helm did not run to a refusal.
func renderRefusal(err error) string {
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return strings.TrimSpace(string(exitError.Stderr))
	}
	return err.Error()
}

func renderChartCommand(t *testing.T, additionalArgs ...string) ([]byte, error) {
	return renderChartCommandInNamespace(t, releaseNamespace, additionalArgs...)
}

func renderChartCommandInNamespace(t *testing.T, namespace string, additionalArgs ...string) ([]byte, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for chart render tests")
	}
	_, filename, _, _ := runtime.Caller(0)
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	args := []string{
		"template", releaseName, filepath.Join(repositoryRoot, "charts", "ptah-operator"),
		"--namespace", namespace,
		"--set-string", "image.digest=sha256:" + managerDigest,
		"--set-string", "execution.executorImage=example.invalid/ptah@sha256:" + executorDigest,
		"--set-string", "execution.runnerImage=example.invalid/operator@sha256:" + runnerDigest,
		"--set-string", "execution.ptahVersion=" + ptahVersion,
	}
	args = append(args, additionalArgs...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, helm, args...)
	temporaryHome := t.TempDir()
	command.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(temporaryHome, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(temporaryHome, "config"),
		"HELM_DATA_HOME="+filepath.Join(temporaryHome, "data"),
	)
	return command.Output()
}

func mustObject(t *testing.T, objects []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind && object.GetName() == name {
			return object
		}
	}
	t.Fatalf("rendered object %s/%s was not found", kind, name)
	return nil
}

func mustNamespacedObject(
	t *testing.T,
	objects []*unstructured.Unstructured,
	kind string,
	namespace string,
	name string,
) *unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind && object.GetNamespace() == namespace && object.GetName() == name {
			return object
		}
	}
	t.Fatalf("rendered object %s/%s/%s was not found", kind, namespace, name)
	return nil
}

func assertExactCertificateRoleBinding(
	t *testing.T,
	objects []*unstructured.Unstructured,
	namespace string,
	name string,
	subjectName string,
	subjectNamespace string,
) {
	t.Helper()
	binding := mustNamespacedObject(t, objects, "RoleBinding", namespace, name)
	roleRef, found, err := unstructured.NestedStringMap(binding.Object, "roleRef")
	if err != nil || !found || !maps.Equal(roleRef, map[string]string{
		"apiGroup": "rbac.authorization.k8s.io",
		"kind":     "Role",
		"name":     name,
	}) {
		t.Fatalf("certificate RoleBinding %s/%s roleRef = %v, found=%t err=%v", namespace, name, roleRef, found, err)
	}
	subjects, found, err := unstructured.NestedSlice(binding.Object, "subjects")
	if err != nil || !found || len(subjects) != 1 {
		t.Fatalf("certificate RoleBinding %s/%s subjects = %#v, found=%t err=%v", namespace, name, subjects, found, err)
	}
	subject, ok := subjects[0].(map[string]any)
	if !ok || !reflect.DeepEqual(subject, map[string]any{
		"kind":      "ServiceAccount",
		"name":      subjectName,
		"namespace": subjectNamespace,
	}) {
		t.Fatalf("certificate RoleBinding %s/%s subject = %#v", namespace, name, subjects[0])
	}
}

func objectRules(t *testing.T, object *unstructured.Unstructured) []map[string]any {
	t.Helper()
	rawRules, found, err := unstructured.NestedSlice(object.Object, "rules")
	if err != nil || !found {
		t.Fatalf("%s/%s has no rules", object.GetKind(), object.GetName())
	}
	rules := make([]map[string]any, 0, len(rawRules))
	for _, rawRule := range rawRules {
		rules = append(rules, rawRule.(map[string]any))
	}
	return rules
}

func assertExactRule(
	t *testing.T,
	object *unstructured.Unstructured,
	apiGroup string,
	resource string,
	resourceNames []string,
	verbs []string,
) {
	t.Helper()
	for _, rule := range objectRules(t, object) {
		if !slices.Contains(stringSlice(rule["apiGroups"]), apiGroup) || !slices.Contains(stringSlice(rule["resources"]), resource) {
			continue
		}
		gotNames := stringSlice(rule["resourceNames"])
		gotVerbs := stringSlice(rule["verbs"])
		slices.Sort(gotNames)
		slices.Sort(resourceNames)
		slices.Sort(gotVerbs)
		slices.Sort(verbs)
		if !slices.Equal(gotNames, resourceNames) || !slices.Equal(gotVerbs, verbs) {
			continue
		}
		return
	}
	t.Fatalf("%s/%s has no exact rule for %s/%s with resourceNames=%v verbs=%v", object.GetKind(), object.GetName(), apiGroup, resource, resourceNames, verbs)
}

func assertNoResourceVerb(
	t *testing.T,
	object *unstructured.Unstructured,
	apiGroup string,
	resource string,
	verb string,
) {
	t.Helper()
	for _, rule := range objectRules(t, object) {
		if slices.Contains(stringSlice(rule["apiGroups"]), apiGroup) &&
			slices.Contains(stringSlice(rule["resources"]), resource) &&
			slices.Contains(stringSlice(rule["verbs"]), verb) {
			t.Fatalf("%s/%s unexpectedly grants %s on %s/%s", object.GetKind(), object.GetName(), verb, apiGroup, resource)
		}
	}
}

func assertManagerTLSProjection(t *testing.T, deployment *unstructured.Unstructured, secretName string) {
	t.Helper()
	volumes, found, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "volumes")
	if err != nil || !found {
		t.Fatal("manager Deployment has no volumes")
	}
	for _, rawVolume := range volumes {
		volume := rawVolume.(map[string]any)
		if volume["name"] != "webhook-cert" {
			continue
		}
		secret := volume["secret"].(map[string]any)
		if secret["secretName"] != secretName {
			t.Fatalf("manager certificate volume uses Secret %v, want %q", secret["secretName"], secretName)
		}
		items := secret["items"].([]any)
		keys := make([]string, 0, len(items))
		for _, rawItem := range items {
			keys = append(keys, rawItem.(map[string]any)["key"].(string))
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"tls.crt", "tls.key"}) {
			t.Fatalf("manager certificate projection keys = %v, want only tls.crt and tls.key", keys)
		}
		return
	}
	t.Fatal("manager Deployment has no webhook-cert volume")
}

func stringSlice(value any) []string {
	values, _ := value.([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.(string))
	}
	return result
}

func requiredArgumentValue(t *testing.T, args []string, prefix string) string {
	t.Helper()
	var value string
	for _, arg := range args {
		if !strings.HasPrefix(arg, prefix) {
			continue
		}
		if value != "" {
			t.Fatalf("rotator args contain duplicate %q arguments: %v", prefix, args)
		}
		value = strings.TrimPrefix(arg, prefix)
	}
	if value == "" {
		t.Fatalf("rotator args do not contain a nonempty %q argument: %v", prefix, args)
	}
	return value
}
