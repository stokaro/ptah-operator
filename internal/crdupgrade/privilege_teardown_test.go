package crdupgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestRenderedPrivilegeTeardownRulesMatchCompiledContract(t *testing.T) {
	path := os.Getenv("PTAH_TEARDOWN_RENDER")
	if path == "" {
		t.Skip("PTAH_TEARDOWN_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	teardown := renderedPrivilegeTeardownContract(t)
	expected := make(map[string]privilegeAuthorizationContract)
	for _, contract := range teardown.teardownAuthorizationContracts() {
		key := renderedPrivilegeAuthorizationKey(contract.cluster, contract.namespace, contract.name)
		expected[key] = contract
	}
	seen := make(map[string]bool, len(expected))
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
		var (
			key   string
			rules []rbacv1.PolicyRule
		)
		switch typeMeta.Kind {
		case "ClusterRole":
			var object rbacv1.ClusterRole
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			key = renderedPrivilegeAuthorizationKey(true, "", object.Name)
			rules = object.Rules
		case "Role":
			var object rbacv1.Role
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			key = renderedPrivilegeAuthorizationKey(false, object.Namespace, object.Name)
			rules = object.Rules
		default:
			continue
		}
		contract, matched := expected[key]
		if !matched {
			t.Fatalf("rendered authorization object %s is not part of the compiled teardown contract", key)
		}
		if seen[key] {
			t.Fatalf("rendered authorization object %s appears more than once", key)
		}
		seen[key] = true
		if !reflect.DeepEqual(rules, contract.rules) {
			t.Fatalf("rendered authorization object %s rules = %#v, want %#v", key, rules, contract.rules)
		}
	}
	for key := range expected {
		if !seen[key] {
			t.Errorf("rendered authorization object %s is missing", key)
		}
	}
}

func permitsResourceUpdate(rules []rbacv1.PolicyRule, apiGroup, resource string) bool {
	for _, rule := range rules {
		if containsString(rule.APIGroups, apiGroup) && containsString(rule.Resources, resource) &&
			containsString(rule.Verbs, "update") {
			return true
		}
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestRenderedRetiredPrivilegeRulesMatchCompiledContract(t *testing.T) {
	path := os.Getenv("PTAH_PRIVILEGE_RENDER")
	if path == "" {
		t.Skip("PTAH_PRIVILEGE_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	teardown := renderedPrivilegeTeardownContract(t)
	expected := make(map[string]privilegeAuthorizationContract)
	for _, contract := range teardown.retiredAuthorizationContracts() {
		key := renderedPrivilegeAuthorizationKey(contract.cluster, contract.namespace, contract.name)
		expected[key] = contract
	}
	seen := make(map[string]bool, len(expected))
	managerCanSetPlanOwner := false
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
		var (
			key   string
			rules []rbacv1.PolicyRule
		)
		switch typeMeta.Kind {
		case "ClusterRole":
			var object rbacv1.ClusterRole
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			key = renderedPrivilegeAuthorizationKey(true, "", object.Name)
			rules = object.Rules
			if object.Name == "ptah-e2e-ptah-operator" {
				managerCanSetPlanOwner = permitsResourceUpdate(
					object.Rules,
					"operator.ptah.dev",
					"ptahschemaplans/finalizers",
				)
			}
		case "Role":
			var object rbacv1.Role
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			key = renderedPrivilegeAuthorizationKey(false, object.Namespace, object.Name)
			rules = object.Rules
		default:
			continue
		}
		contract, matched := expected[key]
		if !matched {
			continue
		}
		if seen[key] {
			t.Fatalf("rendered retired authorization object %s appears more than once", key)
		}
		seen[key] = true
		if !reflect.DeepEqual(rules, contract.rules) {
			t.Fatalf("rendered retired authorization object %s rules = %#v, want %#v", key, rules, contract.rules)
		}
	}
	for key := range expected {
		if !seen[key] {
			t.Errorf("rendered retired authorization object %s is missing", key)
		}
	}
	// Plan chunks use blockOwnerDeletion on their PtahSchemaPlan owner. The API
	// server therefore requires this permission when the manager creates a
	// chunk, independently of the teardown contract mirrored above.
	if !managerCanSetPlanOwner {
		t.Fatal("rendered manager ClusterRole cannot set a blocking PtahSchemaPlan owner reference")
	}
}

const (
	renderedPrivilegeReleaseName  = "ptah-e2e"
	renderedPrivilegeManagerImage = "ghcr.io/stokaro/ptah-operator@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

type renderedPrivilegeTeardownSettings struct {
	releaseNamespace               string
	coordinationNamespace          string
	controllerServiceAccountName   string
	controllerServiceAccountCreate bool
	certificateRuntimeEnabled      bool
}

func renderedPrivilegeTeardownContract(t *testing.T) *PrivilegeTeardown {
	t.Helper()
	settings, err := renderedPrivilegeTeardownSettingsFromEnvironment(os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	const controllerName = "ptah-e2e-ptah-operator"
	attempt := hookIdentityDigest(
		settings.releaseNamespace,
		renderedPrivilegeReleaseName,
		1,
		renderedPrivilegeManagerImage,
	)
	digest := attempt[:12]
	controllerServiceAccountName := settings.controllerServiceAccountName
	if settings.controllerServiceAccountCreate {
		base := strings.TrimSuffix(controllerServiceAccountName[:min(len(controllerServiceAccountName), 38)], "-")
		principalDigest := sha256.Sum256([]byte(strings.Join([]string{
			controllerServiceAccountName,
			"1",
			attempt,
		}, "\n")))
		controllerServiceAccountName = fmt.Sprintf("%s-v1-%x", base, principalDigest)[:len(base)+4+12]
	}
	guard := &RolloutGuard{
		ReleaseName:                  renderedPrivilegeReleaseName,
		ReleaseNamespace:             settings.releaseNamespace,
		CoordinationNamespace:        settings.coordinationNamespace,
		ReleaseSequence:              1,
		ManagerImage:                 renderedPrivilegeManagerImage,
		HookServiceAccountName:       "ptah-e2e-ptah-operator-crd-v1-" + digest,
		ControllerServiceAccountName: controllerServiceAccountName,
		ControllerDeploymentName:     controllerName,
		CertificateDeploymentName:    controllerName + "-cert-rotator",
		WebhookSecretName:            controllerName + "-webhook-cert",
		CertificateArgs: []string{
			"--lease-name=" + controllerName + "-cert-rotation",
			"--staging-secret-name=" + controllerName + "-cert-rotation-stage",
		},
	}
	cleanup, err := TeardownServiceAccountName(guard.HookServiceAccountName, guard.ReleaseSequence)
	if err != nil {
		t.Fatal(err)
	}
	privilege, err := TeardownPrivilegeRoleName(guard.HookServiceAccountName)
	if err != nil {
		t.Fatal(err)
	}
	residual, err := TeardownGuardRoleName(guard.HookServiceAccountName)
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := TeardownDiscoveryRoleName(guard.HookServiceAccountName)
	if err != nil {
		t.Fatal(err)
	}
	return NewPrivilegeTeardown(
		guard,
		RuntimeAdmissionContract{
			Namespace:                      settings.releaseNamespace,
			ControllerServiceAccountName:   guard.ControllerServiceAccountName,
			CertificateServiceAccountName:  guard.CertificateDeploymentName,
			ControllerServiceAccountCreate: settings.controllerServiceAccountCreate,
			CertificateRuntimeEnabled:      settings.certificateRuntimeEnabled,
		},
		PrivilegeTeardownConfig{
			CleanupServiceAccountName: cleanup,
			CleanupPrivilegeName:      privilege,
			ResidualGuardName:         residual,
			ResidualReleaseRoleName:   residual,
			ResidualDiscoveryRoleName: discovery,
			DiscoveryNamespace:        corev1.NamespaceDefault,
		},
		nil,
		nil,
		nil,
		nil,
		nil,
	)
}

func renderedPrivilegeTeardownSettingsFromEnvironment(
	lookup func(string) (string, bool),
) (renderedPrivilegeTeardownSettings, error) {
	const (
		defaultReleaseNamespace = "ptah-e2e"
		defaultControllerName   = "ptah-e2e-ptah-operator"
	)
	settings := renderedPrivilegeTeardownSettings{
		releaseNamespace:               defaultReleaseNamespace,
		coordinationNamespace:          defaultReleaseNamespace,
		controllerServiceAccountName:   defaultControllerName,
		controllerServiceAccountCreate: true,
		certificateRuntimeEnabled:      true,
	}
	if value, found := lookup("PTAH_TEARDOWN_RELEASE_NAMESPACE"); found {
		if value == "" || value != strings.TrimSpace(value) {
			return renderedPrivilegeTeardownSettings{}, errors.New("PTAH_TEARDOWN_RELEASE_NAMESPACE must be non-empty without surrounding whitespace")
		}
		settings.releaseNamespace = value
		settings.coordinationNamespace = value
	}
	if value, found := lookup("PTAH_TEARDOWN_COORDINATION_NAMESPACE"); found && value != "" {
		if value != strings.TrimSpace(value) {
			return renderedPrivilegeTeardownSettings{}, errors.New("PTAH_TEARDOWN_COORDINATION_NAMESPACE must not contain surrounding whitespace")
		}
		settings.coordinationNamespace = value
	}
	if value, found := lookup("PTAH_TEARDOWN_CONTROLLER_SERVICE_ACCOUNT_NAME"); found {
		if value == "" || value != strings.TrimSpace(value) {
			return renderedPrivilegeTeardownSettings{}, errors.New("PTAH_TEARDOWN_CONTROLLER_SERVICE_ACCOUNT_NAME must be non-empty without surrounding whitespace")
		}
		settings.controllerServiceAccountName = value
	}
	var err error
	settings.controllerServiceAccountCreate, err = renderedPrivilegeBooleanSetting(
		lookup,
		"PTAH_TEARDOWN_CONTROLLER_SERVICE_ACCOUNT_CREATE",
		settings.controllerServiceAccountCreate,
	)
	if err != nil {
		return renderedPrivilegeTeardownSettings{}, err
	}
	settings.certificateRuntimeEnabled, err = renderedPrivilegeBooleanSetting(
		lookup,
		"PTAH_TEARDOWN_CERTIFICATE_RUNTIME_ENABLED",
		settings.certificateRuntimeEnabled,
	)
	if err != nil {
		return renderedPrivilegeTeardownSettings{}, err
	}
	if !settings.controllerServiceAccountCreate {
		if _, found := lookup("PTAH_TEARDOWN_CONTROLLER_SERVICE_ACCOUNT_NAME"); !found {
			return renderedPrivilegeTeardownSettings{}, errors.New("PTAH_TEARDOWN_CONTROLLER_SERVICE_ACCOUNT_NAME is required when PTAH_TEARDOWN_CONTROLLER_SERVICE_ACCOUNT_CREATE=false")
		}
	}
	return settings, nil
}

func renderedPrivilegeBooleanSetting(
	lookup func(string) (string, bool),
	name string,
	fallback bool,
) (bool, error) {
	value, found := lookup(name)
	if !found {
		return fallback, nil
	}
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be exactly true or false", name)
	}
}

func TestRenderedPrivilegeTeardownSettingsFromEnvironment(t *testing.T) {
	for _, test := range []struct {
		name   string
		values map[string]string
		want   renderedPrivilegeTeardownSettings
	}{
		{
			name: "defaults",
			want: renderedPrivilegeTeardownSettings{
				releaseNamespace:               "ptah-e2e",
				coordinationNamespace:          "ptah-e2e",
				controllerServiceAccountName:   "ptah-e2e-ptah-operator",
				controllerServiceAccountCreate: true,
				certificateRuntimeEnabled:      true,
			},
		},
		{
			name: "all overrides",
			values: map[string]string{
				"PTAH_TEARDOWN_RELEASE_NAMESPACE":                 "ptah-system",
				"PTAH_TEARDOWN_COORDINATION_NAMESPACE":            "ptah-coordination",
				"PTAH_TEARDOWN_CONTROLLER_SERVICE_ACCOUNT_NAME":   "external-controller",
				"PTAH_TEARDOWN_CONTROLLER_SERVICE_ACCOUNT_CREATE": "false",
				"PTAH_TEARDOWN_CERTIFICATE_RUNTIME_ENABLED":       "false",
			},
			want: renderedPrivilegeTeardownSettings{
				releaseNamespace:               "ptah-system",
				coordinationNamespace:          "ptah-coordination",
				controllerServiceAccountName:   "external-controller",
				controllerServiceAccountCreate: false,
				certificateRuntimeEnabled:      false,
			},
		},
		{
			name: "empty coordination namespace uses release namespace",
			values: map[string]string{
				"PTAH_TEARDOWN_RELEASE_NAMESPACE":      "ptah-system",
				"PTAH_TEARDOWN_COORDINATION_NAMESPACE": "",
			},
			want: renderedPrivilegeTeardownSettings{
				releaseNamespace:               "ptah-system",
				coordinationNamespace:          "ptah-system",
				controllerServiceAccountName:   "ptah-e2e-ptah-operator",
				controllerServiceAccountCreate: true,
				certificateRuntimeEnabled:      true,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookup := func(name string) (string, bool) {
				value, found := test.values[name]
				return value, found
			}
			got, err := renderedPrivilegeTeardownSettingsFromEnvironment(lookup)
			if err != nil {
				t.Fatalf("renderedPrivilegeTeardownSettingsFromEnvironment() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("renderedPrivilegeTeardownSettingsFromEnvironment() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestRenderedPrivilegeTeardownSettingsRejectInvalidEnvironment(t *testing.T) {
	for _, test := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{
			name:   "empty release namespace",
			values: map[string]string{"PTAH_TEARDOWN_RELEASE_NAMESPACE": ""},
			want:   "PTAH_TEARDOWN_RELEASE_NAMESPACE must be non-empty",
		},
		{
			name:   "invalid boolean",
			values: map[string]string{"PTAH_TEARDOWN_CERTIFICATE_RUNTIME_ENABLED": "1"},
			want:   "must be exactly true or false",
		},
		{
			name: "external controller name missing",
			values: map[string]string{
				"PTAH_TEARDOWN_CONTROLLER_SERVICE_ACCOUNT_CREATE": "false",
			},
			want: "PTAH_TEARDOWN_CONTROLLER_SERVICE_ACCOUNT_NAME is required",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookup := func(name string) (string, bool) {
				value, found := test.values[name]
				return value, found
			}
			_, err := renderedPrivilegeTeardownSettingsFromEnvironment(lookup)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("renderedPrivilegeTeardownSettingsFromEnvironment() error = %v, want %q", err, test.want)
			}
		})
	}
}

func renderedPrivilegeAuthorizationKey(cluster bool, namespace, name string) string {
	if cluster {
		return "ClusterRole/" + name
	}
	return "Role/" + namespace + "/" + name
}

func TestPrivilegeTeardownDeletesExactPrivilegesBeforeServiceAccounts(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	if err := fixture.teardown.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight() error = %v", err)
	}
	if len(fixture.events) != 0 {
		t.Fatalf("Preflight() mutated resources: %v", fixture.events)
	}

	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}

	hook := fixture.guard.HookServiceAccountName
	bootstrap := privilegeHookBindingName(hook, 53, "-bootstrap")
	probe := privilegeHookBindingName(hook, 57, "-probe")
	quiesce, err := TeardownQuiesceJobName(hook)
	if err != nil {
		t.Fatalf("derive teardown quiesce name: %v", err)
	}
	want := []string{
		"RoleBinding/" + fixture.guard.ReleaseNamespace + "/" + fixture.guard.ControllerDeploymentName + "-runtime-admission",
		"RoleBinding/" + fixture.guard.CoordinationNamespace + "/" + fixture.guard.ControllerDeploymentName,
		"RoleBinding/" + fixture.guard.ReleaseNamespace + "/" + fixture.contract.CertificateServiceAccountName,
		"RoleBinding/" + fixture.guard.ReleaseNamespace + "/" + hook,
		"RoleBinding/" + fixture.guard.ReleaseNamespace + "/" + bootstrap,
		"RoleBinding/" + fixture.guard.ReleaseNamespace + "/" + probe,
		"RoleBinding/" + fixture.guard.ReleaseNamespace + "/" + quiesce,
		"ClusterRoleBinding/" + fixture.guard.ControllerDeploymentName,
		"ClusterRoleBinding/" + fixture.contract.CertificateServiceAccountName,
		"ClusterRoleBinding/" + hook,
		"ClusterRoleBinding/" + bootstrap,
		"ClusterRoleBinding/" + quiesce,
		"ServiceAccount/" + fixture.contract.ControllerServiceAccountName,
		"ServiceAccount/" + fixture.contract.CertificateServiceAccountName,
		"ServiceAccount/" + hook,
		"RoleBinding/" + fixture.guard.ReleaseNamespace + "/" + fixture.cleanupPrivilege,
		"RoleBinding/" + fixture.guard.CoordinationNamespace + "/" + fixture.cleanupPrivilege,
		"ClusterRoleBinding/" + fixture.cleanupPrivilege,
		"ClusterRole/" + fixture.cleanupPrivilege,
	}
	if !reflect.DeepEqual(fixture.events, want) {
		t.Fatalf("deletion order = %#v, want %#v", fixture.events, want)
	}
	fixture.assertOnlyCleanupAccessRemains(t)
	fixture.assertDeletePreconditions(t)
}

func TestPrivilegeTeardownRetiresCleanupServiceAccountFromExactResidualState(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	eventsBefore := len(fixture.events)
	callsBefore := len(fixture.serviceAccounts.calls)

	if err := fixture.teardown.RetireCleanupServiceAccount(context.Background()); err != nil {
		t.Fatalf("RetireCleanupServiceAccount() error = %v", err)
	}
	if fixture.serviceAccounts.objects[fixture.cleanupName] != nil {
		t.Fatalf("cleanup ServiceAccount/%s remains", fixture.cleanupName)
	}
	if got := fixture.events[eventsBefore:]; !reflect.DeepEqual(got, []string{"ServiceAccount/" + fixture.cleanupName}) {
		t.Fatalf("cleanup retirement events = %#v", got)
	}
	if got := fixture.serviceAccounts.calls[callsBefore:]; len(got) != 1 || got[0].name != fixture.cleanupName {
		t.Fatalf("cleanup retirement delete calls = %#v", got)
	}
	fixture.assertDeletePreconditions(t)
}

func TestPrivilegeTeardownRejectsCleanupServiceAccountRetirementBeforeResidualState(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)

	err := fixture.teardown.RetireCleanupServiceAccount(context.Background())
	if err == nil || !strings.Contains(err.Error(), "non-residual privilege remains") {
		t.Fatalf("RetireCleanupServiceAccount() error = %v, want non-residual privilege rejection", err)
	}
	if len(fixture.events) != 0 {
		t.Fatalf("early cleanup retirement mutated resources: %v", fixture.events)
	}
}

func TestPrivilegeTeardownRejectsDriftedResidualStateBeforeCleanupServiceAccountRetirement(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	eventsBefore := len(fixture.events)
	roleKey := privilegeBindingKey(fixture.guard.ReleaseNamespace, fixture.residualGuard)
	fixture.roles.objects[roleKey].Rules = append(
		fixture.roles.objects[roleKey].Rules,
		privilegePolicyRule([]string{""}, []string{"secrets"}, nil, []string{"get"}),
	)

	err := fixture.teardown.RetireCleanupServiceAccount(context.Background())
	if err == nil || !strings.Contains(err.Error(), "policy rules differ from the exact ordered privilege contract") {
		t.Fatalf("RetireCleanupServiceAccount() error = %v, want exact residual contract rejection", err)
	}
	if fixture.serviceAccounts.objects[fixture.cleanupName] == nil {
		t.Fatalf("drifted residual state removed cleanup ServiceAccount/%s", fixture.cleanupName)
	}
	if got := fixture.events[eventsBefore:]; len(got) != 0 {
		t.Fatalf("drifted residual state caused mutations: %v", got)
	}
}

func TestPrivilegeTeardownCleanupServiceAccountRetirementUsesFreshPreconditions(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	eventsBefore := len(fixture.events)
	fixture.serviceAccounts.beforeDelete = func(name string, object *corev1.ServiceAccount) {
		if name == fixture.cleanupName {
			object.ResourceVersion = "raced"
		}
	}

	err := fixture.teardown.RetireCleanupServiceAccount(context.Background())
	if err == nil || !apierrors.IsConflict(err) {
		t.Fatalf("RetireCleanupServiceAccount() error = %v, want delete precondition conflict", err)
	}
	if fixture.serviceAccounts.objects[fixture.cleanupName] == nil {
		t.Fatalf("precondition conflict removed cleanup ServiceAccount/%s", fixture.cleanupName)
	}
	if got := fixture.events[eventsBefore:]; len(got) != 0 {
		t.Fatalf("precondition conflict recorded deletion events: %v", got)
	}
}

func TestPrivilegeTeardownCleanupServiceAccountRetirementRejectsRecreation(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	recreated := fixture.serviceAccounts.objects[fixture.cleanupName].DeepCopy()
	recreated.UID = "uid-recreated-cleanup"
	recreated.ResourceVersion = "2"
	fixture.serviceAccounts.afterDelete = func(name string) {
		if name == fixture.cleanupName {
			fixture.serviceAccounts.objects[name] = recreated.DeepCopy()
		}
	}

	err := fixture.teardown.RetireCleanupServiceAccount(context.Background())
	if err == nil || !strings.Contains(err.Error(), "remains after exact retirement") {
		t.Fatalf("RetireCleanupServiceAccount() error = %v, want recreation rejection", err)
	}
	if got := fixture.serviceAccounts.objects[fixture.cleanupName]; got == nil || got.UID != recreated.UID {
		t.Fatalf("recreated cleanup ServiceAccount = %#v", got)
	}
}

func TestPrivilegeTeardownCleanupServiceAccountRetirementAcceptsPostDeleteUnauthorized(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	fixture.serviceAccounts.afterDelete = func(name string) {
		if name == fixture.cleanupName {
			fixture.serviceAccounts.getErr[name] = apierrors.NewUnauthorized("bound token retired")
		}
	}

	if err := fixture.teardown.RetireCleanupServiceAccount(context.Background()); err != nil {
		t.Fatalf("RetireCleanupServiceAccount() error = %v, want successful self-revocation", err)
	}
	if fixture.serviceAccounts.objects[fixture.cleanupName] != nil {
		t.Fatalf("cleanup ServiceAccount/%s remains", fixture.cleanupName)
	}
}

func TestPrivilegeTeardownCleanupServiceAccountRetirementRejectsPreDeleteUnauthorized(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	fixture.serviceAccounts.getErr[fixture.cleanupName] = apierrors.NewUnauthorized("credential rejected before retirement")
	callsBefore := len(fixture.serviceAccounts.calls)

	err := fixture.teardown.RetireCleanupServiceAccount(context.Background())
	if err == nil || !apierrors.IsUnauthorized(err) {
		t.Fatalf("RetireCleanupServiceAccount() error = %v, want preflight Unauthorized", err)
	}
	if got := len(fixture.serviceAccounts.calls); got != callsBefore {
		t.Fatalf("preflight Unauthorized caused %d cleanup deletes, want %d", got, callsBefore)
	}
}

func TestPrivilegeTeardownCleanupServiceAccountRetirementRejectsPostDeleteReadFailure(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	readFailure := errors.New("temporary cleanup identity read failure")
	fixture.serviceAccounts.afterDelete = func(name string) {
		if name == fixture.cleanupName {
			fixture.serviceAccounts.getErr[name] = readFailure
		}
	}

	err := fixture.teardown.RetireCleanupServiceAccount(context.Background())
	if err == nil || !errors.Is(err, readFailure) {
		t.Fatalf("RetireCleanupServiceAccount() error = %v, want post-delete read failure", err)
	}
}

func TestPrivilegeTeardownRejectsForeignBindingsBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name string
		add  func(*privilegeTeardownFixture)
		want string
	}{
		{
			name: "RoleBinding",
			add: func(f *privilegeTeardownFixture) {
				f.roleBindings.objects[privilegeBindingKey("foreign", "dangerous")] = &rbacv1.RoleBinding{
					ObjectMeta: privilegeObjectMeta("dangerous", "foreign", f.guard, "", "foreign-role"),
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
					Subjects:   []rbacv1.Subject{privilegeServiceAccountSubject(f.guard.ReleaseNamespace, f.contract.ControllerServiceAccountName)},
				}
			},
			want: "foreign RoleBinding",
		},
		{
			name: "ClusterRoleBinding",
			add: func(f *privilegeTeardownFixture) {
				f.clusterBindings.objects["dangerous"] = &rbacv1.ClusterRoleBinding{
					ObjectMeta: privilegeObjectMeta("dangerous", "", f.guard, "", "foreign-cluster"),
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
					Subjects:   []rbacv1.Subject{privilegeServiceAccountSubject(f.guard.ReleaseNamespace, f.cleanupName)},
				}
			},
			want: "foreign ClusterRoleBinding",
		},
		{
			name: "previous controller RoleBinding",
			add: func(f *privilegeTeardownFixture) {
				f.guard.PreviousControllerServiceAccountName = "previous-controller"
				f.guard.PreviousControllerServiceAccountUID = "previous-controller-uid"
				f.guard.PreviousControllerReleaseSequence = 0
				f.roleBindings.objects[privilegeBindingKey("foreign", "previous-controller-extra")] = &rbacv1.RoleBinding{
					ObjectMeta: privilegeObjectMeta("previous-controller-extra", "foreign", f.guard, "", "previous-controller-extra"),
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "edit"},
					Subjects:   []rbacv1.Subject{privilegeServiceAccountSubject(f.guard.ReleaseNamespace, f.guard.PreviousControllerServiceAccountName)},
				}
			},
			want: "foreign RoleBinding",
		},
		{
			name: "encoded ServiceAccount user",
			add: func(f *privilegeTeardownFixture) {
				f.roleBindings.objects[privilegeBindingKey("foreign", "encoded-user")] = &rbacv1.RoleBinding{
					ObjectMeta: privilegeObjectMeta("encoded-user", "foreign", f.guard, "", "encoded-user"),
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "admin"},
					Subjects: []rbacv1.Subject{{
						Kind: rbacv1.UserKind,
						Name: "system:serviceaccount:" + f.guard.ReleaseNamespace + ":" + f.guard.HookServiceAccountName,
					}},
				}
			},
			want: "foreign RoleBinding",
		},
		{
			name: "release ServiceAccount group",
			add: func(f *privilegeTeardownFixture) {
				f.clusterBindings.objects["release-serviceaccounts"] = &rbacv1.ClusterRoleBinding{
					ObjectMeta: privilegeObjectMeta("release-serviceaccounts", "", f.guard, "", "release-serviceaccounts"),
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "admin"},
					Subjects: []rbacv1.Subject{{
						Kind: rbacv1.GroupKind,
						Name: "system:serviceaccounts:" + f.guard.ReleaseNamespace,
					}},
				}
			},
			want: "foreign ClusterRoleBinding",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPrivilegeTeardownFixture(t, true, true)
			test.add(fixture)
			err := fixture.teardown.Teardown(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Teardown() error = %v, want %q rejection", err, test.want)
			}
			if len(fixture.events) != 0 {
				t.Fatalf("foreign binding caused mutations: %v", fixture.events)
			}
		})
	}
}

func TestPrivilegeTeardownScopesLegacyControllerGuardAuthorityToFullCleanup(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	privilegeNames := fixture.teardown.privilegeAdmissionGuardNames()
	runtimeNames := fixture.teardown.runtimeAdmissionGuardNames()
	bootstrapNames := fixture.teardown.bootstrapAdmissionGuardNames()
	for _, name := range legacyControllerGuardNames(fixture.guard.ReleaseNamespace, fixture.guard.ReleaseName) {
		if !stringSliceContains(privilegeNames, name) {
			t.Fatalf("full cleanup admission inventory is missing %s", name)
		}
		if stringSliceContains(runtimeNames, name) {
			t.Fatalf("runtime authority unexpectedly includes legacy guard %s", name)
		}
		if stringSliceContains(bootstrapNames, name) {
			t.Fatalf("bootstrap authority unexpectedly includes legacy guard %s", name)
		}
	}
}

func TestPrivilegeTeardownRetainsExternalControllerServiceAccount(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, false, true)
	fixture.serviceAccounts.objects[fixture.contract.ControllerServiceAccountName] = &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fixture.contract.ControllerServiceAccountName,
			Namespace:       fixture.guard.ReleaseNamespace,
			UID:             "external-controller",
			ResourceVersion: "1",
		},
	}

	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	if fixture.serviceAccounts.objects[fixture.contract.ControllerServiceAccountName] == nil {
		t.Fatal("external controller ServiceAccount was deleted")
	}
	for _, event := range fixture.events {
		if event == "ServiceAccount/"+fixture.contract.ControllerServiceAccountName {
			t.Fatalf("external controller ServiceAccount deletion was attempted: %v", fixture.events)
		}
	}
	for _, contract := range fixture.teardown.teardownAuthorizationContracts() {
		for _, rule := range contract.rules {
			if (reflect.DeepEqual(rule.Resources, []string{"serviceaccounts"})) && stringSliceContains(rule.ResourceNames, fixture.contract.ControllerServiceAccountName) {
				t.Fatalf("external controller ServiceAccount appears in %s/%s delete or read authority", contract.namespace, contract.name)
			}
		}
	}
}

func TestPrivilegeTeardownExternalControllerServiceAccountMustBeDedicated(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, false, true)
	fixture.roleBindings.objects[privilegeBindingKey("foreign", "external-controller-extra")] = &rbacv1.RoleBinding{
		ObjectMeta: privilegeObjectMeta("external-controller-extra", "foreign", fixture.guard, "", "external-controller-extra"),
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "edit"},
		Subjects:   []rbacv1.Subject{privilegeServiceAccountSubject(fixture.guard.ReleaseNamespace, fixture.contract.ControllerServiceAccountName)},
	}

	err := fixture.teardown.Teardown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "foreign RoleBinding") {
		t.Fatalf("Teardown() error = %v, want dedicated external controller identity rejection", err)
	}
	if len(fixture.events) != 0 {
		t.Fatalf("foreign external-controller binding caused mutations: %v", fixture.events)
	}
}

func TestPrivilegeTeardownOmitsDisabledCertificateRuntime(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, false)
	certificate := fixture.contract.CertificateServiceAccountName
	fixture.serviceAccounts.objects[certificate] = &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: certificate, Namespace: fixture.guard.ReleaseNamespace, UID: "external-cert", ResourceVersion: "1"},
	}
	fixture.clusterBindings.objects[certificate] = &rbacv1.ClusterRoleBinding{
		ObjectMeta: privilegeObjectMeta(certificate, "", fixture.guard, "certificate-rotation", "disabled-cert-binding"),
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: certificate},
		Subjects:   []rbacv1.Subject{privilegeServiceAccountSubject(fixture.guard.ReleaseNamespace, certificate)},
	}

	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	if fixture.clusterBindings.objects[certificate] == nil || fixture.serviceAccounts.objects[certificate] == nil {
		t.Fatal("disabled certificate runtime resources were changed")
	}
	for _, contract := range fixture.teardown.teardownAuthorizationContracts() {
		for _, rule := range contract.rules {
			if !reflect.DeepEqual(rule.Resources, []string{"deployments"}) && stringSliceContains(rule.ResourceNames, certificate) {
				t.Fatalf("disabled certificate identity appears in %s/%s authorization contract: %#v", contract.namespace, contract.name, rule)
			}
		}
	}
}

func TestPrivilegeTeardownSharesReleaseScopedPrivilegeWhenCoordinationMatches(t *testing.T) {
	fixture := newPrivilegeTeardownFixtureWithCoordination(t, true, true, "ptah-system")
	cleanupBindingKey := privilegeBindingKey(fixture.guard.ReleaseNamespace, fixture.cleanupPrivilege)
	cleanupBindingCount := 0
	for key, binding := range fixture.roleBindings.objects {
		if binding.Name == fixture.cleanupPrivilege {
			cleanupBindingCount++
			if key != cleanupBindingKey {
				t.Fatalf("cleanup privilege RoleBinding key = %q, want %q", key, cleanupBindingKey)
			}
		}
	}
	if cleanupBindingCount != 1 {
		t.Fatalf("cleanup privilege RoleBinding count = %d, want 1", cleanupBindingCount)
	}
	role := fixture.roles.objects[cleanupBindingKey]
	if role == nil || len(role.Rules) == 0 || !stringSliceContains(role.Rules[0].ResourceNames, fixture.guard.ControllerDeploymentName) {
		t.Fatalf("shared release cleanup Role does not include coordination RoleBinding deletion: %#v", role)
	}

	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	fixture.assertOnlyCleanupAccessRemains(t)
}

func TestPrivilegeTeardownPropagatesUIDAndResourceVersionPreconditions(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	fixture.clusterBindings.beforeDelete = func(name string, object *rbacv1.ClusterRoleBinding) {
		if name == fixture.guard.ControllerDeploymentName {
			object.ResourceVersion = "replaced"
		}
	}

	err := fixture.teardown.Teardown(context.Background())
	if err == nil || !apierrors.IsConflict(errors.Unwrap(err)) {
		t.Fatalf("Teardown() error = %v, want concurrent replacement conflict", err)
	}
	if fixture.clusterBindings.objects[fixture.guard.ControllerDeploymentName] == nil {
		t.Fatal("concurrently replaced binding was deleted")
	}
	if len(fixture.clusterBindings.calls) != 1 {
		t.Fatalf("delete calls = %d, want 1", len(fixture.clusterBindings.calls))
	}
	call := fixture.clusterBindings.calls[0]
	if call.options.Preconditions == nil || call.options.Preconditions.UID == nil || call.options.Preconditions.ResourceVersion == nil ||
		*call.options.Preconditions.UID == "" || *call.options.Preconditions.ResourceVersion != "1" {
		t.Fatalf("delete preconditions = %#v", call.options.Preconditions)
	}
}

func TestPrivilegeTeardownRejectsUnsafeDeletionMetadataBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*privilegeTeardownFixture)
		want   string
	}{
		{
			name: "binding finalizer",
			mutate: func(f *privilegeTeardownFixture) {
				f.clusterBindings.objects[f.guard.ControllerDeploymentName].Finalizers = []string{"hold.example"}
			},
			want: "has finalizers",
		},
		{
			name: "retained binding terminating",
			mutate: func(f *privilegeTeardownFixture) {
				now := metav1.Now()
				f.clusterBindings.objects[f.residualGuard].DeletionTimestamp = &now
			},
			want: "deletion is already in progress",
		},
		{
			name: "ServiceAccount owner",
			mutate: func(f *privilegeTeardownFixture) {
				f.serviceAccounts.objects[f.guard.HookServiceAccountName].OwnerReferences = []metav1.OwnerReference{{Name: "unexpected"}}
			},
			want: "unexpected owner references",
		},
		{
			name: "retained Role finalizer",
			mutate: func(f *privilegeTeardownFixture) {
				key := privilegeBindingKey(f.guard.ReleaseNamespace, f.residualGuard)
				f.roles.objects[key].Finalizers = []string{"hold.example"}
			},
			want: "has finalizers",
		},
		{
			name: "quiesce ClusterRole terminating",
			mutate: func(f *privilegeTeardownFixture) {
				quiesce, err := TeardownQuiesceJobName(f.guard.HookServiceAccountName)
				if err != nil {
					panic(err)
				}
				now := metav1.Now()
				f.clusterRoles.objects[quiesce].DeletionTimestamp = &now
			},
			want: "deletion is already in progress",
		},
		{
			name: "cleanup privilege Role owner",
			mutate: func(f *privilegeTeardownFixture) {
				key := privilegeBindingKey(f.guard.ReleaseNamespace, f.cleanupPrivilege)
				f.roles.objects[key].OwnerReferences = []metav1.OwnerReference{{Name: "unexpected"}}
			},
			want: "unexpected owner references",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPrivilegeTeardownFixture(t, true, true)
			test.mutate(fixture)
			err := fixture.teardown.Teardown(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Teardown() error = %v, want %q", err, test.want)
			}
			if len(fixture.events) != 0 {
				t.Fatalf("invalid deletion metadata caused mutations: %v", fixture.events)
			}
		})
	}
}

func TestPrivilegeTeardownRetryContinuesAfterPartialDeletion(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	failingKey := privilegeBindingKey(fixture.guard.ReleaseNamespace, fixture.guard.ControllerDeploymentName+"-runtime-admission")
	fixture.roleBindings.deleteErrors[failingKey] = errors.New("temporary API failure")

	if err := fixture.teardown.Teardown(context.Background()); err == nil || !strings.Contains(err.Error(), "temporary API failure") {
		t.Fatalf("first Teardown() error = %v", err)
	}
	delete(fixture.roleBindings.deleteErrors, failingKey)
	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("retry Teardown() error = %v", err)
	}
	fixture.assertOnlyCleanupAccessRemains(t)
}

func TestPrivilegeTeardownRetryCompletesAfterPartialSelfRevocation(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	fixture.clusterRoles.deleteErrors[fixture.cleanupPrivilege] = errors.New("temporary self-revocation failure")

	err := fixture.teardown.Teardown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "temporary self-revocation failure") {
		t.Fatalf("first Teardown() error = %v", err)
	}
	if fixture.clusterBindings.objects[fixture.cleanupPrivilege] != nil {
		t.Fatal("cleanup privilege ClusterRoleBinding remains after successful self-revocation step")
	}
	if fixture.clusterRoles.objects[fixture.cleanupPrivilege] == nil {
		t.Fatal("failed cleanup privilege ClusterRole deletion unexpectedly removed the object")
	}
	delete(fixture.clusterRoles.deleteErrors, fixture.cleanupPrivilege)
	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("retry Teardown() error = %v", err)
	}
	fixture.assertOnlyCleanupAccessRemains(t)
}

func TestPrivilegeTeardownRetryCompletesAfterNamespacedSelfRevocation(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	coordinationKey := privilegeBindingKey(fixture.guard.CoordinationNamespace, fixture.cleanupPrivilege)
	fixture.roleBindings.deleteErrors[coordinationKey] = errors.New("temporary coordination self-revocation failure")

	err := fixture.teardown.Teardown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "temporary coordination self-revocation failure") {
		t.Fatalf("first Teardown() error = %v", err)
	}
	releaseKey := privilegeBindingKey(fixture.guard.ReleaseNamespace, fixture.cleanupPrivilege)
	if fixture.roleBindings.objects[releaseKey] != nil {
		t.Fatal("release cleanup privilege RoleBinding remains after successful self-revocation step")
	}
	if fixture.roleBindings.objects[coordinationKey] == nil {
		t.Fatal("failed coordination cleanup privilege RoleBinding deletion unexpectedly removed the object")
	}
	delete(fixture.roleBindings.deleteErrors, coordinationKey)
	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("retry Teardown() error = %v", err)
	}
	fixture.assertOnlyCleanupAccessRemains(t)
}

func TestPrivilegeTeardownPaginatesCompleteBindingInventory(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	wantRoleBindings := privilegeRoleBindingItems(fixture.roleBindings.objects)
	pages := map[string]*rbacv1.RoleBindingList{}
	continueToken := ""
	for pageIndex := 0; pageIndex < 5; pageIndex++ {
		next := fmt.Sprintf("page-%d", pageIndex+1)
		items := make([]rbacv1.RoleBinding, 0, privilegeTeardownBindingPageSize)
		for itemIndex := 0; itemIndex < privilegeTeardownBindingPageSize; itemIndex++ {
			name := fmt.Sprintf("unrelated-%d-%d", pageIndex, itemIndex)
			items = append(items, rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "other"}})
		}
		pages[continueToken] = &rbacv1.RoleBindingList{ListMeta: metav1.ListMeta{Continue: next}, Items: items}
		continueToken = next
	}
	pages[continueToken] = &rbacv1.RoleBindingList{Items: wantRoleBindings}
	fixture.roleBindings.pages = pages

	if err := fixture.teardown.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight() error = %v", err)
	}
	wantTokens := []string{"", "page-1", "page-2", "page-3", "page-4", "page-5"}
	if !reflect.DeepEqual(fixture.roleBindings.listTokens, wantTokens) {
		t.Fatalf("RoleBinding continuation requests = %#v, want %#v", fixture.roleBindings.listTokens, wantTokens)
	}
	if len(fixture.events) != 0 {
		t.Fatalf("Preflight() mutated resources: %v", fixture.events)
	}
}

func TestPrivilegeTeardownRejectsInvalidBindingPagination(t *testing.T) {
	unrelated := func(name string) rbacv1.RoleBinding {
		return rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "other"}}
	}
	for _, test := range []struct {
		name   string
		mutate func(*privilegeTeardownFixture)
		want   string
	}{
		{
			name: "nil page",
			mutate: func(f *privilegeTeardownFixture) {
				f.roleBindings.pages = map[string]*rbacv1.RoleBindingList{"": nil}
			},
			want: "nil page",
		},
		{
			name: "repeated continuation token",
			mutate: func(f *privilegeTeardownFixture) {
				f.roleBindings.pages = map[string]*rbacv1.RoleBindingList{
					"":      {ListMeta: metav1.ListMeta{Continue: "again"}, Items: []rbacv1.RoleBinding{unrelated("one")}},
					"again": {ListMeta: metav1.ListMeta{Continue: "again"}, Items: []rbacv1.RoleBinding{unrelated("two")}},
				}
			},
			want: "repeated its current continuation token",
		},
		{
			name: "empty continued page",
			mutate: func(f *privilegeTeardownFixture) {
				f.roleBindings.pages = map[string]*rbacv1.RoleBindingList{
					"":      {ListMeta: metav1.ListMeta{Continue: "empty"}, Items: []rbacv1.RoleBinding{unrelated("one")}},
					"empty": {ListMeta: metav1.ListMeta{Continue: "later"}},
				}
			},
			want: "empty page with a continuation token",
		},
		{
			name: "duplicate object across pages",
			mutate: func(f *privilegeTeardownFixture) {
				duplicate := unrelated("duplicate")
				f.roleBindings.pages = map[string]*rbacv1.RoleBindingList{
					"":          {ListMeta: metav1.ListMeta{Continue: "duplicate"}, Items: []rbacv1.RoleBinding{duplicate}},
					"duplicate": {Items: []rbacv1.RoleBinding{duplicate}},
				}
			},
			want: "duplicate RoleBinding",
		},
		{
			name: "oversized page",
			mutate: func(f *privilegeTeardownFixture) {
				items := make([]rbacv1.RoleBinding, 0, privilegeTeardownBindingPageSize+1)
				for index := 0; index <= privilegeTeardownBindingPageSize; index++ {
					items = append(items, unrelated(fmt.Sprintf("oversized-%d", index)))
				}
				f.roleBindings.pages = map[string]*rbacv1.RoleBindingList{"": {Items: items}}
			},
			want: "oversized page",
		},
		{
			name: "inconsistent remaining count",
			mutate: func(f *privilegeTeardownFixture) {
				remaining := int64(1)
				f.clusterBindings.listMeta.RemainingItemCount = &remaining
			},
			want: "unreturned objects",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPrivilegeTeardownFixture(t, true, true)
			test.mutate(fixture)
			err := fixture.teardown.Teardown(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Teardown() error = %v, want %q", err, test.want)
			}
			if len(fixture.events) != 0 {
				t.Fatalf("invalid inventory pagination caused mutations: %v", fixture.events)
			}
		})
	}
}

func TestPrivilegeTeardownPaginationHonorsContextCancellation(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.roleBindings.pages = map[string]*rbacv1.RoleBindingList{
		"": {ListMeta: metav1.ListMeta{Continue: "next"}, Items: []rbacv1.RoleBinding{{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "other"}}}},
	}
	fixture.roleBindings.afterList = func(options metav1.ListOptions) {
		if options.Continue == "" {
			cancel()
		}
	}
	err := fixture.teardown.Preflight(ctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Preflight() error = %v, want context cancellation", err)
	}
	if !reflect.DeepEqual(fixture.roleBindings.listTokens, []string{""}) {
		t.Fatalf("RoleBinding requests after cancellation = %#v", fixture.roleBindings.listTokens)
	}
}

func TestPrivilegeTeardownRejectsInexactExpectedBinding(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*rbacv1.ClusterRoleBinding)
	}{
		{
			name:   "role reference",
			mutate: func(binding *rbacv1.ClusterRoleBinding) { binding.RoleRef.Name = "cluster-admin" },
		},
		{
			name: "additional subject",
			mutate: func(binding *rbacv1.ClusterRoleBinding) {
				binding.Subjects = append(binding.Subjects, rbacv1.Subject{Kind: rbacv1.UserKind, Name: "extra"})
			},
		},
		{
			name:   "ownership",
			mutate: func(binding *rbacv1.ClusterRoleBinding) { binding.Annotations[helmReleaseNameAnnotation] = "other" },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPrivilegeTeardownFixture(t, true, true)
			test.mutate(fixture.clusterBindings.objects[fixture.guard.ControllerDeploymentName])
			if err := fixture.teardown.Teardown(context.Background()); err == nil {
				t.Fatal("Teardown() unexpectedly accepted an inexact binding")
			}
			if len(fixture.events) != 0 {
				t.Fatalf("inexact binding caused mutations: %v", fixture.events)
			}
		})
	}
}

func TestPrivilegeTeardownRejectsDriftedAuthorizationRulesBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*privilegeTeardownFixture)
	}{
		{
			name: "ClusterRole extra rule",
			mutate: func(f *privilegeTeardownFixture) {
				role := f.clusterRoles.objects[f.residualGuard]
				role.Rules = append(role.Rules, privilegePolicyRule([]string{""}, []string{"secrets"}, nil, []string{"get"}))
			},
		},
		{
			name: "retired controller ClusterRole extra mutation",
			mutate: func(f *privilegeTeardownFixture) {
				role := f.clusterRoles.objects[f.guard.ControllerDeploymentName]
				role.Rules = append(role.Rules, privilegePolicyRule([]string{""}, []string{"secrets"}, nil, []string{"update"}))
			},
		},
		{
			name: "retired hook Role extra mutation",
			mutate: func(f *privilegeTeardownFixture) {
				key := privilegeBindingKey(f.guard.ReleaseNamespace, f.guard.HookServiceAccountName)
				role := f.roles.objects[key]
				role.Rules = append(role.Rules, privilegePolicyRule([]string{""}, []string{"secrets"}, nil, []string{"delete"}))
			},
		},
		{
			name: "retired certificate Role extra mutation",
			mutate: func(f *privilegeTeardownFixture) {
				key := privilegeBindingKey(f.guard.ReleaseNamespace, f.guard.CertificateDeploymentName)
				role := f.roles.objects[key]
				role.Rules = append(role.Rules, privilegePolicyRule([]string{""}, []string{"configmaps"}, nil, []string{"patch"}))
			},
		},
		{
			name: "ClusterRole missing rule",
			mutate: func(f *privilegeTeardownFixture) {
				role := f.clusterRoles.objects[f.residualGuard]
				role.Rules = role.Rules[:len(role.Rules)-1]
			},
		},
		{
			name: "ClusterRole reordered rules",
			mutate: func(f *privilegeTeardownFixture) {
				role := f.clusterRoles.objects[f.residualGuard]
				role.Rules[0], role.Rules[1] = role.Rules[1], role.Rules[0]
			},
		},
		{
			name: "ClusterRole altered rule",
			mutate: func(f *privilegeTeardownFixture) {
				f.clusterRoles.objects[f.cleanupPrivilege].Rules[0].Verbs = []string{"get", "delete"}
			},
		},
		{
			name: "Role extra rule",
			mutate: func(f *privilegeTeardownFixture) {
				key := privilegeBindingKey(f.guard.ReleaseNamespace, f.residualGuard)
				role := f.roles.objects[key]
				role.Rules = append(role.Rules, privilegePolicyRule([]string{""}, []string{"secrets"}, nil, []string{"get"}))
			},
		},
		{
			name: "Role missing rule",
			mutate: func(f *privilegeTeardownFixture) {
				key := privilegeBindingKey(f.guard.ReleaseNamespace, f.residualGuard)
				role := f.roles.objects[key]
				role.Rules = role.Rules[:len(role.Rules)-1]
			},
		},
		{
			name: "Role reordered rules",
			mutate: func(f *privilegeTeardownFixture) {
				quiesce, err := TeardownQuiesceJobName(f.guard.HookServiceAccountName)
				if err != nil {
					panic(err)
				}
				role := f.roles.objects[privilegeBindingKey(f.guard.ReleaseNamespace, quiesce)]
				role.Rules[0], role.Rules[1] = role.Rules[1], role.Rules[0]
			},
		},
		{
			name: "Role altered rule",
			mutate: func(f *privilegeTeardownFixture) {
				key := privilegeBindingKey(corev1.NamespaceDefault, f.discoveryName)
				f.roles.objects[key].Rules[0].Resources = []string{"services"}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPrivilegeTeardownFixture(t, true, true)
			test.mutate(fixture)
			err := fixture.teardown.Teardown(context.Background())
			if err == nil || !strings.Contains(err.Error(), "policy rules differ from the exact ordered privilege contract") {
				t.Fatalf("Teardown() error = %v, want exact policy rule rejection", err)
			}
			if len(fixture.events) != 0 {
				t.Fatalf("drifted authorization rules caused mutations: %v", fixture.events)
			}
		})
	}
}

func TestPrivilegeTeardownRequiresCleanupAccessAndIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*privilegeTeardownFixture)
		want   string
	}{
		{
			name: "cleanup privilege ClusterRoleBinding",
			mutate: func(f *privilegeTeardownFixture) {
				delete(f.clusterBindings.objects, f.cleanupPrivilege)
			},
			want: "cleanup privilege ClusterRoleBinding is required",
		},
		{
			name:   "cleanup privilege ClusterRole",
			mutate: func(f *privilegeTeardownFixture) { delete(f.clusterRoles.objects, f.cleanupPrivilege) },
			want:   "required ClusterRole",
		},
		{
			name:   "retired controller ClusterRole",
			mutate: func(f *privilegeTeardownFixture) { delete(f.clusterRoles.objects, f.guard.ControllerDeploymentName) },
			want:   "required ClusterRole",
		},
		{
			name: "retired hook Role",
			mutate: func(f *privilegeTeardownFixture) {
				delete(f.roles.objects, privilegeBindingKey(f.guard.ReleaseNamespace, f.guard.HookServiceAccountName))
			},
			want: "required Role",
		},
		{
			name: "cleanup privilege release RoleBinding",
			mutate: func(f *privilegeTeardownFixture) {
				delete(f.roleBindings.objects, privilegeBindingKey(f.guard.ReleaseNamespace, f.cleanupPrivilege))
			},
			want: "cleanup privilege RoleBinding",
		},
		{
			name: "cleanup privilege coordination RoleBinding",
			mutate: func(f *privilegeTeardownFixture) {
				delete(f.roleBindings.objects, privilegeBindingKey(f.guard.CoordinationNamespace, f.cleanupPrivilege))
			},
			want: "cleanup privilege RoleBinding",
		},
		{
			name:   "residual ClusterRoleBinding",
			mutate: func(f *privilegeTeardownFixture) { delete(f.clusterBindings.objects, f.residualGuard) },
			want:   "required ClusterRoleBinding",
		},
		{
			name:   "residual ClusterRole",
			mutate: func(f *privilegeTeardownFixture) { delete(f.clusterRoles.objects, f.residualGuard) },
			want:   "required ClusterRole",
		},
		{
			name: "residual release RoleBinding",
			mutate: func(f *privilegeTeardownFixture) {
				delete(f.roleBindings.objects, privilegeBindingKey(f.guard.ReleaseNamespace, f.residualGuard))
			},
			want: "required RoleBinding",
		},
		{
			name: "residual discovery RoleBinding",
			mutate: func(f *privilegeTeardownFixture) {
				delete(f.roleBindings.objects, privilegeBindingKey(corev1.NamespaceDefault, f.discoveryName))
			},
			want: "required RoleBinding",
		},
		{
			name: "residual release Role",
			mutate: func(f *privilegeTeardownFixture) {
				delete(f.roles.objects, privilegeBindingKey(f.guard.ReleaseNamespace, f.residualGuard))
			},
			want: "required Role",
		},
		{
			name: "residual discovery Role",
			mutate: func(f *privilegeTeardownFixture) {
				delete(f.roles.objects, privilegeBindingKey(corev1.NamespaceDefault, f.discoveryName))
			},
			want: "required Role",
		},
		{
			name:   "ServiceAccount",
			mutate: func(f *privilegeTeardownFixture) { delete(f.serviceAccounts.objects, f.cleanupName) },
			want:   "required ServiceAccount",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPrivilegeTeardownFixture(t, true, true)
			test.mutate(fixture)
			err := fixture.teardown.Preflight(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Preflight() error = %v, want %q", err, test.want)
			}
			if len(fixture.events) != 0 {
				t.Fatalf("missing cleanup resource caused mutations: %v", fixture.events)
			}
		})
	}
}

func TestPrivilegeTeardownRejectsInvalidCertificateStagingSecretIdentity(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing",
			args: []string{"--namespace=ptah-system", "--lease-name=ptah-certificate-rotation"},
			want: "requires exactly one --staging-secret-name argument, found 0",
		},
		{
			name: "duplicate",
			args: []string{
				"--namespace=ptah-system",
				"--lease-name=ptah-certificate-rotation",
				"--staging-secret-name=ptah-certificate-rotation-stage",
				"--staging-secret-name=ptah-certificate-rotation-stage-2",
			},
			want: "requires exactly one --staging-secret-name argument, found 2",
		},
		{
			name: "empty",
			args: []string{
				"--namespace=ptah-system",
				"--lease-name=ptah-certificate-rotation",
				"--staging-secret-name=",
			},
			want: "must have a nonempty, unpadded value",
		},
		{
			name: "padded",
			args: []string{
				"--namespace=ptah-system",
				"--lease-name=ptah-certificate-rotation",
				"--staging-secret-name= ptah-certificate-rotation-stage",
			},
			want: "must have a nonempty, unpadded value",
		},
		{
			name: "serving Secret alias",
			args: []string{
				"--namespace=ptah-system",
				"--lease-name=ptah-certificate-rotation",
				"--staging-secret-name=ptah-e2e-webhook-cert",
			},
			want: "staging and serving Secret names must differ",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPrivilegeTeardownFixture(t, true, true)
			fixture.guard.CertificateArgs = append([]string(nil), test.args...)
			err := fixture.teardown.Preflight(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Preflight() error = %v, want %q", err, test.want)
			}
			if len(fixture.events) != 0 {
				t.Fatalf("invalid certificate staging Secret identity caused mutations: %v", fixture.events)
			}
		})
	}
}

func TestPrivilegeTeardownFailsClosedWithoutPredecessorInventory(t *testing.T) {
	fixture := newPrivilegeTeardownFixture(t, true, true)
	fixture.guard.ReleaseSequence = 2
	fixture.guard.HookServiceAccountName = "ptah-e2e-operator-crd-v2-0123456789ab"
	err := fixture.teardown.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requires an explicit predecessor privilege inventory") {
		t.Fatalf("Preflight() error = %v, want predecessor inventory refusal", err)
	}
	if len(fixture.events) != 0 {
		t.Fatalf("missing predecessor inventory caused mutations: %v", fixture.events)
	}
}

func TestTeardownDiscoveryRoleNamePreservesMaximumSequenceIdentity(t *testing.T) {
	const hook = "abcdefghijklmnopqrstuvwxyz1234-crd-v2147483647-0123456789ab"
	name, err := TeardownDiscoveryRoleName(hook)
	if err != nil {
		t.Fatalf("TeardownDiscoveryRoleName() error = %v", err)
	}
	const want = "abcdefghijklmnopqrst-cleanup-discovery-v2147483647-0123456789ab"
	if name != want {
		t.Fatalf("TeardownDiscoveryRoleName() = %q, want %q", name, want)
	}
	if len(name) > 63 {
		t.Fatalf("TeardownDiscoveryRoleName() length = %d, want at most 63", len(name))
	}
}

type privilegeTeardownFixture struct {
	guard              *RolloutGuard
	contract           RuntimeAdmissionContract
	cleanupName        string
	cleanupPrivilege   string
	residualGuard      string
	discoveryName      string
	teardown           *PrivilegeTeardown
	roleBindings       *fakePrivilegeRoleBindings
	clusterBindings    *fakePrivilegeClusterRoleBindings
	roles              *fakePrivilegeRoles
	clusterRoles       *fakePrivilegeClusterRoles
	serviceAccounts    *fakePrivilegeServiceAccounts
	events             []string
	nextObjectIdentity int
}

func newPrivilegeTeardownFixture(t *testing.T, controllerAccountCreate, certificateEnabled bool) *privilegeTeardownFixture {
	return newPrivilegeTeardownFixtureWithCoordination(t, controllerAccountCreate, certificateEnabled, "ptah-coordination")
}

func newPrivilegeTeardownFixtureWithCoordination(
	t *testing.T,
	controllerAccountCreate bool,
	certificateEnabled bool,
	coordinationNamespace string,
) *privilegeTeardownFixture {
	t.Helper()
	guard := &RolloutGuard{
		Policies:                     privilegePolicyReader{},
		Bindings:                     privilegePolicyBindingReader{},
		ReleaseName:                  "ptah-e2e",
		ReleaseNamespace:             "ptah-system",
		CoordinationNamespace:        coordinationNamespace,
		LeaderElection:               true,
		LeaderElectionID:             "ptah-operator.operator.ptah.dev",
		WebhookServiceName:           "ptah-e2e-webhook",
		WebhookTimeoutSeconds:        5,
		WebhookSecretName:            "ptah-e2e-webhook-cert",
		WebhookPort:                  9443,
		CertificateHealthPort:        8081,
		HookServiceAccountName:       "ptah-e2e-operator-crd-v1-0123456789ab",
		ControllerServiceAccountName: "ptah-e2e-operator",
		ControllerDeploymentName:     "ptah-e2e-operator",
		ControllerReplicas:           1,
		CertificateDeploymentName:    "ptah-e2e-operator-cert-rotator",
		ControllerStateVersion:       1,
		AdmissionContractVersion:     1,
		ReleaseSequence:              1,
		ManagerImage:                 "ghcr.io/stokaro/ptah-operator@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ControllerArgs:               []string{"--leader-elect=true"},
		CertificateArgs: []string{
			"--namespace=ptah-system",
			"--lease-name=ptah-certificate-rotation",
			"--staging-secret-name=ptah-certificate-rotation-stage",
		},
		RuntimeDeploymentConfigExpressions: []string{"true"},
		RuntimePodConfigExpressions:        []string{"true"},
		RuntimeAdmissionContractB64:        "e30=",
		PollEvery:                          time.Millisecond,
	}
	contract := RuntimeAdmissionContract{
		Namespace:                      guard.ReleaseNamespace,
		ControllerServiceAccountName:   guard.ControllerServiceAccountName,
		CertificateServiceAccountName:  guard.CertificateDeploymentName,
		ControllerServiceAccountCreate: controllerAccountCreate,
		CertificateRuntimeEnabled:      certificateEnabled,
	}
	fixture := &privilegeTeardownFixture{guard: guard, contract: contract}
	fixture.roleBindings = &fakePrivilegeRoleBindings{objects: map[string]*rbacv1.RoleBinding{}, deleteErrors: map[string]error{}, events: &fixture.events}
	fixture.clusterBindings = &fakePrivilegeClusterRoleBindings{objects: map[string]*rbacv1.ClusterRoleBinding{}, deleteErrors: map[string]error{}, events: &fixture.events}
	fixture.roles = &fakePrivilegeRoles{objects: map[string]*rbacv1.Role{}, getErr: map[string]error{}}
	fixture.clusterRoles = &fakePrivilegeClusterRoles{objects: map[string]*rbacv1.ClusterRole{}, deleteErrors: map[string]error{}, events: &fixture.events}
	fixture.serviceAccounts = &fakePrivilegeServiceAccounts{objects: map[string]*corev1.ServiceAccount{}, getErr: map[string]error{}, deleteErrors: map[string]error{}, events: &fixture.events}
	cleanupName, err := TeardownServiceAccountName(guard.HookServiceAccountName, guard.ReleaseSequence)
	if err != nil {
		t.Fatalf("derive cleanup ServiceAccount: %v", err)
	}
	fixture.cleanupName = cleanupName
	fixture.cleanupPrivilege, err = TeardownPrivilegeRoleName(guard.HookServiceAccountName)
	if err != nil {
		t.Fatalf("derive cleanup privilege: %v", err)
	}
	fixture.residualGuard, err = TeardownGuardRoleName(guard.HookServiceAccountName)
	if err != nil {
		t.Fatalf("derive residual guard: %v", err)
	}
	fixture.discoveryName, err = TeardownDiscoveryRoleName(guard.HookServiceAccountName)
	if err != nil {
		t.Fatalf("derive residual discovery Role: %v", err)
	}
	fixture.teardown = NewPrivilegeTeardown(
		guard,
		contract,
		PrivilegeTeardownConfig{
			CleanupServiceAccountName: fixture.cleanupName,
			CleanupPrivilegeName:      fixture.cleanupPrivilege,
			ResidualGuardName:         fixture.residualGuard,
			ResidualReleaseRoleName:   fixture.residualGuard,
			ResidualDiscoveryRoleName: fixture.discoveryName,
			DiscoveryNamespace:        corev1.NamespaceDefault,
		},
		fixture.roleBindings,
		fixture.clusterBindings,
		fixture.roles,
		fixture.clusterRoles,
		fixture.serviceAccounts,
	)

	for _, binding := range fixture.teardown.bindingContracts() {
		fixture.nextObjectIdentity++
		identity := fmt.Sprintf("binding-%02d", fixture.nextObjectIdentity)
		metadata := privilegeObjectMeta(binding.name, binding.namespace, guard, binding.component, identity)
		subjects := append([]rbacv1.Subject{binding.subject}, binding.fixedSubjects...)
		if binding.cluster {
			fixture.clusterBindings.objects[binding.name] = &rbacv1.ClusterRoleBinding{
				ObjectMeta: metadata,
				RoleRef:    binding.roleRef,
				Subjects:   subjects,
			}
		} else {
			fixture.roleBindings.objects[privilegeBindingKey(binding.namespace, binding.name)] = &rbacv1.RoleBinding{
				ObjectMeta: metadata,
				RoleRef:    binding.roleRef,
				Subjects:   subjects,
			}
		}
	}
	for _, account := range fixture.teardown.serviceAccountContracts() {
		fixture.nextObjectIdentity++
		identity := fmt.Sprintf("account-%02d", fixture.nextObjectIdentity)
		fixture.serviceAccounts.objects[account.name] = &corev1.ServiceAccount{
			ObjectMeta: privilegeObjectMeta(account.name, guard.ReleaseNamespace, guard, account.component, identity),
		}
	}
	for _, contract := range fixture.teardown.authorizationContracts() {
		fixture.nextObjectIdentity++
		identity := fmt.Sprintf("authorization-%02d", fixture.nextObjectIdentity)
		metadata := privilegeObjectMeta(contract.name, contract.namespace, guard, contract.component, identity)
		if contract.cluster {
			fixture.clusterRoles.objects[contract.name] = &rbacv1.ClusterRole{
				ObjectMeta: metadata,
				Rules:      append([]rbacv1.PolicyRule(nil), contract.rules...),
			}
		} else {
			fixture.roles.objects[privilegeBindingKey(contract.namespace, contract.name)] = &rbacv1.Role{
				ObjectMeta: metadata,
				Rules:      append([]rbacv1.PolicyRule(nil), contract.rules...),
			}
		}
	}
	return fixture
}

func (f *privilegeTeardownFixture) assertOnlyCleanupAccessRemains(t *testing.T) {
	t.Helper()
	wantRoleBindings := []string{
		privilegeBindingKey(corev1.NamespaceDefault, f.discoveryName),
		privilegeBindingKey(f.guard.ReleaseNamespace, f.residualGuard),
	}
	sort.Strings(wantRoleBindings)
	if got := sortedPrivilegeRoleBindingKeys(f.roleBindings.objects); !reflect.DeepEqual(got, wantRoleBindings) {
		t.Fatalf("remaining RoleBindings = %#v", sortedPrivilegeRoleBindingKeys(f.roleBindings.objects))
	}
	if len(f.clusterBindings.objects) != 1 || f.clusterBindings.objects[f.residualGuard] == nil {
		t.Fatalf("remaining ClusterRoleBindings = %#v", sortedPrivilegeClusterBindingKeys(f.clusterBindings.objects))
	}
	wantClusterRoles := make([]string, 0)
	for _, contract := range f.teardown.authorizationContracts() {
		if contract.cluster && contract.name != f.cleanupPrivilege {
			wantClusterRoles = append(wantClusterRoles, contract.name)
		}
	}
	sort.Strings(wantClusterRoles)
	if got := sortedPrivilegeClusterRoleKeys(f.clusterRoles.objects); !reflect.DeepEqual(got, wantClusterRoles) {
		t.Fatalf("remaining ClusterRoles = %#v", sortedPrivilegeClusterRoleKeys(f.clusterRoles.objects))
	}
	wantRoles := make([]string, 0, 5)
	for _, contract := range f.teardown.authorizationContracts() {
		if !contract.cluster {
			wantRoles = append(wantRoles, privilegeBindingKey(contract.namespace, contract.name))
		}
	}
	sort.Strings(wantRoles)
	if got := sortedPrivilegeRoleKeys(f.roles.objects); !reflect.DeepEqual(got, wantRoles) {
		t.Fatalf("remaining Roles = %#v, want %#v", got, wantRoles)
	}
	if len(f.serviceAccounts.objects) != 1 || f.serviceAccounts.objects[f.cleanupName] == nil {
		t.Fatalf("remaining ServiceAccounts = %#v", sortedPrivilegeServiceAccountKeys(f.serviceAccounts.objects))
	}
}

func (f *privilegeTeardownFixture) assertDeletePreconditions(t *testing.T) {
	t.Helper()
	check := func(kind, name string, options metav1.DeleteOptions) {
		t.Helper()
		if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil ||
			*options.Preconditions.UID == "" || *options.Preconditions.ResourceVersion == "" {
			t.Fatalf("%s/%s delete preconditions = %#v", kind, name, options.Preconditions)
		}
	}
	for _, call := range f.clusterBindings.calls {
		check("ClusterRoleBinding", call.name, call.options)
	}
	for _, call := range f.roleBindings.calls {
		check("RoleBinding", call.namespace+"/"+call.name, call.options)
	}
	for _, call := range f.serviceAccounts.calls {
		check("ServiceAccount", call.name, call.options)
	}
	for _, call := range f.clusterRoles.calls {
		check("ClusterRole", call.name, call.options)
	}
}

func privilegeObjectMeta(name, namespace string, guard *RolloutGuard, component, identity string) metav1.ObjectMeta {
	labels := map[string]string{managedByLabel: "Helm", instanceLabel: guard.ReleaseName}
	if component != "" {
		labels["app.kubernetes.io/component"] = component
	}
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: namespace,
		Annotations: map[string]string{
			helmReleaseNameAnnotation:      guard.ReleaseName,
			helmReleaseNamespaceAnnotation: guard.ReleaseNamespace,
		},
		Labels:          labels,
		UID:             types.UID("uid-" + identity),
		ResourceVersion: "1",
	}
}

func privilegeServiceAccountSubject(namespace, name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: namespace}
}

type privilegePolicyReader struct{}

func (privilegePolicyReader) Get(context.Context, string, metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	return nil, errors.New("unexpected policy read")
}

type privilegePolicyBindingReader struct{}

func (privilegePolicyBindingReader) Get(context.Context, string, metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	return nil, errors.New("unexpected policy binding read")
}

type privilegeRoleBindingDeleteCall struct {
	namespace string
	name      string
	options   metav1.DeleteOptions
}

type fakePrivilegeRoleBindings struct {
	objects      map[string]*rbacv1.RoleBinding
	pages        map[string]*rbacv1.RoleBindingList
	listMeta     metav1.ListMeta
	listErr      error
	nilList      bool
	afterList    func(metav1.ListOptions)
	listTokens   []string
	deleteErrors map[string]error
	beforeDelete func(string, string, *rbacv1.RoleBinding)
	calls        []privilegeRoleBindingDeleteCall
	events       *[]string
}

func (f *fakePrivilegeRoleBindings) List(_ context.Context, options metav1.ListOptions) (*rbacv1.RoleBindingList, error) {
	if options.Limit != privilegeTeardownBindingPageSize {
		return nil, fmt.Errorf("list limit = %d", options.Limit)
	}
	f.listTokens = append(f.listTokens, options.Continue)
	if f.afterList != nil {
		defer f.afterList(options)
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.nilList {
		return nil, nil
	}
	if f.pages != nil {
		page, found := f.pages[options.Continue]
		if !found {
			return nil, fmt.Errorf("unexpected continuation token %q", options.Continue)
		}
		if page == nil {
			return nil, nil
		}
		return page.DeepCopy(), nil
	}
	keys := sortedPrivilegeRoleBindingKeys(f.objects)
	items := make([]rbacv1.RoleBinding, 0, len(keys))
	for _, key := range keys {
		items = append(items, *f.objects[key].DeepCopy())
	}
	return &rbacv1.RoleBindingList{ListMeta: f.listMeta, Items: items}, nil
}

func (f *fakePrivilegeRoleBindings) Delete(_ context.Context, namespace, name string, options metav1.DeleteOptions) error {
	key := privilegeBindingKey(namespace, name)
	f.calls = append(f.calls, privilegeRoleBindingDeleteCall{namespace: namespace, name: name, options: options})
	object := f.objects[key]
	if object == nil {
		return apierrors.NewNotFound(schema.GroupResource{Group: rbacv1.GroupName, Resource: "rolebindings"}, name)
	}
	if f.beforeDelete != nil {
		f.beforeDelete(namespace, name, object)
	}
	if err := privilegeCheckDeletePreconditions(object, options); err != nil {
		return err
	}
	if err := f.deleteErrors[key]; err != nil {
		return err
	}
	delete(f.objects, key)
	*f.events = append(*f.events, "RoleBinding/"+namespace+"/"+name)
	return nil
}

type privilegeClusterBindingDeleteCall struct {
	name    string
	options metav1.DeleteOptions
}

type fakePrivilegeClusterRoleBindings struct {
	objects      map[string]*rbacv1.ClusterRoleBinding
	pages        map[string]*rbacv1.ClusterRoleBindingList
	listMeta     metav1.ListMeta
	listErr      error
	nilList      bool
	afterList    func(metav1.ListOptions)
	listTokens   []string
	deleteErrors map[string]error
	beforeDelete func(string, *rbacv1.ClusterRoleBinding)
	calls        []privilegeClusterBindingDeleteCall
	events       *[]string
}

type privilegeClusterRoleDeleteCall struct {
	name    string
	options metav1.DeleteOptions
}

type fakePrivilegeRoles struct {
	objects map[string]*rbacv1.Role
	getErr  map[string]error
}

func (f *fakePrivilegeRoles) Get(_ context.Context, namespace, name string, _ metav1.GetOptions) (*rbacv1.Role, error) {
	key := privilegeBindingKey(namespace, name)
	if err := f.getErr[key]; err != nil {
		return nil, err
	}
	object := f.objects[key]
	if object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: rbacv1.GroupName, Resource: "roles"}, name)
	}
	return object.DeepCopy(), nil
}

type fakePrivilegeClusterRoles struct {
	objects      map[string]*rbacv1.ClusterRole
	getErr       map[string]error
	deleteErrors map[string]error
	beforeDelete func(string, *rbacv1.ClusterRole)
	calls        []privilegeClusterRoleDeleteCall
	events       *[]string
}

func (f *fakePrivilegeClusterRoles) Get(_ context.Context, name string, _ metav1.GetOptions) (*rbacv1.ClusterRole, error) {
	if err := f.getErr[name]; err != nil {
		return nil, err
	}
	object := f.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: rbacv1.GroupName, Resource: "clusterroles"}, name)
	}
	return object.DeepCopy(), nil
}

func (f *fakePrivilegeClusterRoles) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	f.calls = append(f.calls, privilegeClusterRoleDeleteCall{name: name, options: options})
	object := f.objects[name]
	if object == nil {
		return apierrors.NewNotFound(schema.GroupResource{Group: rbacv1.GroupName, Resource: "clusterroles"}, name)
	}
	if f.beforeDelete != nil {
		f.beforeDelete(name, object)
	}
	if err := privilegeCheckDeletePreconditions(object, options); err != nil {
		return err
	}
	if err := f.deleteErrors[name]; err != nil {
		return err
	}
	delete(f.objects, name)
	*f.events = append(*f.events, "ClusterRole/"+name)
	return nil
}

func (f *fakePrivilegeClusterRoleBindings) List(_ context.Context, options metav1.ListOptions) (*rbacv1.ClusterRoleBindingList, error) {
	if options.Limit != privilegeTeardownBindingPageSize {
		return nil, fmt.Errorf("list limit = %d", options.Limit)
	}
	f.listTokens = append(f.listTokens, options.Continue)
	if f.afterList != nil {
		defer f.afterList(options)
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.nilList {
		return nil, nil
	}
	if f.pages != nil {
		page, found := f.pages[options.Continue]
		if !found {
			return nil, fmt.Errorf("unexpected continuation token %q", options.Continue)
		}
		if page == nil {
			return nil, nil
		}
		return page.DeepCopy(), nil
	}
	keys := sortedPrivilegeClusterBindingKeys(f.objects)
	items := make([]rbacv1.ClusterRoleBinding, 0, len(keys))
	for _, key := range keys {
		items = append(items, *f.objects[key].DeepCopy())
	}
	return &rbacv1.ClusterRoleBindingList{ListMeta: f.listMeta, Items: items}, nil
}

func (f *fakePrivilegeClusterRoleBindings) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	f.calls = append(f.calls, privilegeClusterBindingDeleteCall{name: name, options: options})
	object := f.objects[name]
	if object == nil {
		return apierrors.NewNotFound(schema.GroupResource{Group: rbacv1.GroupName, Resource: "clusterrolebindings"}, name)
	}
	if f.beforeDelete != nil {
		f.beforeDelete(name, object)
	}
	if err := privilegeCheckDeletePreconditions(object, options); err != nil {
		return err
	}
	if err := f.deleteErrors[name]; err != nil {
		return err
	}
	delete(f.objects, name)
	*f.events = append(*f.events, "ClusterRoleBinding/"+name)
	return nil
}

type privilegeServiceAccountDeleteCall struct {
	name    string
	options metav1.DeleteOptions
}

type fakePrivilegeServiceAccounts struct {
	objects      map[string]*corev1.ServiceAccount
	getErr       map[string]error
	deleteErrors map[string]error
	beforeDelete func(string, *corev1.ServiceAccount)
	afterDelete  func(string)
	calls        []privilegeServiceAccountDeleteCall
	events       *[]string
}

func (f *fakePrivilegeServiceAccounts) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ServiceAccount, error) {
	if err := f.getErr[name]; err != nil {
		return nil, err
	}
	object := f.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "serviceaccounts"}, name)
	}
	return object.DeepCopy(), nil
}

func (f *fakePrivilegeServiceAccounts) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	f.calls = append(f.calls, privilegeServiceAccountDeleteCall{name: name, options: options})
	object := f.objects[name]
	if object == nil {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "serviceaccounts"}, name)
	}
	if f.beforeDelete != nil {
		f.beforeDelete(name, object)
	}
	if err := privilegeCheckDeletePreconditions(object, options); err != nil {
		return err
	}
	if err := f.deleteErrors[name]; err != nil {
		return err
	}
	delete(f.objects, name)
	*f.events = append(*f.events, "ServiceAccount/"+name)
	if f.afterDelete != nil {
		f.afterDelete(name)
	}
	return nil
}

func privilegeCheckDeletePreconditions(object metav1.Object, options metav1.DeleteOptions) error {
	if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil ||
		*options.Preconditions.UID != object.GetUID() || *options.Preconditions.ResourceVersion != object.GetResourceVersion() {
		return apierrors.NewConflict(schema.GroupResource{Resource: "objects"}, object.GetName(), errors.New("delete precondition mismatch"))
	}
	return nil
}

func sortedPrivilegeRoleBindingKeys(objects map[string]*rbacv1.RoleBinding) []string {
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func privilegeRoleBindingItems(objects map[string]*rbacv1.RoleBinding) []rbacv1.RoleBinding {
	keys := sortedPrivilegeRoleBindingKeys(objects)
	items := make([]rbacv1.RoleBinding, 0, len(keys))
	for _, key := range keys {
		items = append(items, *objects[key].DeepCopy())
	}
	return items
}

func sortedPrivilegeClusterBindingKeys(objects map[string]*rbacv1.ClusterRoleBinding) []string {
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedPrivilegeRoleKeys(objects map[string]*rbacv1.Role) []string {
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedPrivilegeClusterRoleKeys(objects map[string]*rbacv1.ClusterRole) []string {
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedPrivilegeServiceAccountKeys(objects map[string]*corev1.ServiceAccount) []string {
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
