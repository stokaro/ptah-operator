package crdupgrade

// This file keeps the exact RBAC a release issues as a compiled contract the
// rendered chart is held to. It is test code: the uninstall no longer reads
// it, and nothing else in the binary needs the whole inventory.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// releaseRBACContract is one ClusterRole or Role a release renders, with the
// exact rules it must carry.
type releaseRBACContract struct {
	name      string
	namespace string
	component string
	cluster   bool
	rules     []rbacv1.PolicyRule
}

// releaseRBACInventory compiles every ClusterRole and Role the release
// installs for its controller, certificate rotator and CRD hooks.
type releaseRBACInventory struct {
	rollout  *RolloutGuard
	contract RuntimeAdmissionContract
	// predecessorManagerImage is the manager image the sealed admission
	// inventory of the sequence before this one records. It is empty when a
	// render finds no such inventory, which is also every offline render.
	predecessorManagerImage string
}

func (t releaseRBACInventory) contracts() []releaseRBACContract {
	controller := t.rollout.ControllerDeploymentName
	hook := t.rollout.HookServiceAccountName
	bootstrap := privilegeHookBindingName(hook, 53, "-bootstrap")
	probe := privilegeHookBindingName(hook, 57, "-probe")
	crdNames := releaseCRDNames()
	guardNames := t.releaseAdmissionGuardNames()
	predecessorSequence := t.rollout.ReleaseSequence - 1
	contracts := []releaseRBACContract{
		{
			name: controller, cluster: true,
			rules: controllerClusterRoleRules(guardNames),
		},
		{
			name: controller + "-runtime-admission", namespace: t.rollout.ReleaseNamespace,
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule(
					[]string{""},
					[]string{"serviceaccounts"},
					[]string{t.contract.ControllerServiceAccountName, t.contract.CertificateServiceAccountName},
					[]string{"get"},
				),
				privilegePolicyRule([]string{""}, []string{"limitranges"}, nil, []string{"list"}),
			},
		},
		{
			name: controller, namespace: t.rollout.CoordinationNamespace,
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule([]string{"coordination.k8s.io"}, []string{"leases"}, nil, []string{"get", "create", "update"}),
			},
		},
		{
			name: hook, component: "crd-manager", cluster: true,
			rules: func() []rbacv1.PolicyRule {
				rules := []rbacv1.PolicyRule{
					privilegePolicyRule([]string{"apiextensions.k8s.io"}, []string{"customresourcedefinitions"}, crdNames, []string{"get", "update"}),
					privilegePolicyRule(
						[]string{"operator.ptah.run"},
						[]string{"ptahschemas", "ptahschemaplans", "ptahschemaapprovals", "ptahmigrations", "ptahmigrationplans", "ptahmigrationapprovals"},
						nil,
						[]string{"list"},
					),
					privilegePolicyRule(
						[]string{"admissionregistration.k8s.io"},
						[]string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"},
						[]string{AdmissionConfigurationName},
						[]string{"get", "update"},
					),
					privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"validatingadmissionpolicies"}, guardNames, []string{"get"}),
					privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"validatingadmissionpolicybindings"}, guardNames, []string{"get"}),
				}
				// Retiring a predecessor means reading and then deleting exactly
				// the objects it sealed, so the grant appears only where a sealed
				// inventory names them, and names nothing wider.
				if t.predecessorManagerImage != "" {
					rules = append(rules, privilegePolicyRule(
						[]string{"admissionregistration.k8s.io"},
						[]string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"},
						predecessorRetiredAdmissionGuardNames(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, predecessorSequence, t.predecessorManagerImage),
						[]string{"get", "delete"},
					))
				}
				return append(rules,
					privilegePolicyRule([]string{"scheduling.k8s.io"}, []string{"priorityclasses"}, nil, []string{"get", "list"}),
				)
			}(),
		},
		{
			name: hook, namespace: t.rollout.ReleaseNamespace, component: "crd-manager",
			rules: func() []rbacv1.PolicyRule {
				rules := []rbacv1.PolicyRule{
					privilegePolicyRule(
						[]string{"apps"}, []string{"deployments"},
						[]string{t.rollout.ControllerDeploymentName, t.rollout.CertificateDeploymentName},
						[]string{"get", "update"},
					),
					privilegePolicyRule([]string{"apps"}, []string{"replicasets"}, nil, []string{"list"}),
					privilegePolicyRule([]string{""}, []string{"pods"}, nil, []string{"list"}),
					privilegePolicyRule(
						[]string{""}, []string{"serviceaccounts"},
						[]string{t.contract.ControllerServiceAccountName, t.contract.CertificateServiceAccountName},
						[]string{"get"},
					),
					privilegePolicyRule([]string{""}, []string{"limitranges"}, nil, []string{"list"}),
					privilegePolicyRule([]string{""}, []string{"resourcequotas"}, nil, []string{"list"}),
					privilegePolicyRule(
						[]string{""}, []string{"configmaps"},
						[]string{ReleaseActivationName, AdmissionConvergenceMarkerName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence)},
						[]string{"get", "update"},
					),
				}
				if predecessorSequence > 0 {
					rules = append(rules, privilegePolicyRule(
						[]string{""}, []string{"configmaps"},
						[]string{AdmissionConvergenceMarkerName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, predecessorSequence)},
						[]string{"get", "delete"},
					))
					if t.predecessorManagerImage != "" {
						rules = append(rules, privilegePolicyRule(
							[]string{""}, []string{"configmaps"},
							[]string{HookIdentityProbeObjectName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, predecessorSequence, t.predecessorManagerImage)},
							[]string{"get", "delete"},
						))
					}
				}
				return rules
			}(),
		},
		{
			name: bootstrap, component: "hook-identity-bootstrap", cluster: true,
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"validatingadmissionpolicies"}, t.bootstrapAdmissionGuardNames(), []string{"get"}),
				privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"validatingadmissionpolicybindings"}, t.bootstrapAdmissionGuardNames(), []string{"get"}),
			},
		},
		{
			name: bootstrap, namespace: t.rollout.ReleaseNamespace, component: "hook-identity-bootstrap",
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule([]string{""}, []string{"configmaps"}, []string{HookIdentityProbeObjectName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage)}, []string{"get", "update"}),
				privilegePolicyRule([]string{"batch"}, []string{"jobs"}, nil, []string{"list"}),
				privilegePolicyRule([]string{""}, []string{"pods"}, nil, []string{"list"}),
			},
		},
		{
			name: probe, namespace: t.rollout.ReleaseNamespace, component: "crd-manager",
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule([]string{"apps"}, []string{"deployments"}, nil, []string{"create"}),
			},
		},
	}

	if t.contract.CertificateRuntimeEnabled {
		certificate := t.contract.CertificateServiceAccountName
		certificateClusterRules := []rbacv1.PolicyRule{
			privilegePolicyRule([]string{"apiextensions.k8s.io"}, []string{"customresourcedefinitions"}, crdNames, []string{"get"}),
			privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"mutatingwebhookconfigurations"}, []string{AdmissionConfigurationName}, []string{"get", "update"}),
			privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"validatingwebhookconfigurations"}, []string{AdmissionConfigurationName}, []string{"get", "update"}),
			privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"}, guardNames, []string{"get"}),
			privilegePolicyRule([]string{"scheduling.k8s.io"}, []string{"priorityclasses"}, nil, []string{"get", "list"}),
		}
		certificateRoleRules := []rbacv1.PolicyRule{
			privilegePolicyRule(
				[]string{""},
				[]string{"secrets"},
				[]string{t.rollout.WebhookSecretName, t.certificateStagingSecretName()},
				[]string{"get", "update"},
			),
		}
		if t.certificateRecreatesMissingSecret() {
			certificateRoleRules = append(certificateRoleRules, privilegePolicyRule([]string{""}, []string{"secrets"}, nil, []string{"create"}))
			certificateClusterRules = append(certificateClusterRules, privilegePolicyRule(
				[]string{"admissionregistration.k8s.io"},
				[]string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"},
				[]string{certificate},
				[]string{"get"},
			))
		}
		certificateRoleRules = append(certificateRoleRules,
			privilegePolicyRule([]string{"coordination.k8s.io"}, []string{"leases"}, []string{t.certificateLeaseName()}, []string{"get", "update"}),
			privilegePolicyRule(
				[]string{""}, []string{"serviceaccounts"},
				[]string{t.contract.ControllerServiceAccountName, certificate},
				[]string{"get"},
			),
			privilegePolicyRule([]string{""}, []string{"limitranges"}, nil, []string{"list"}),
		)
		if t.rollout.AdmissionContractVersion >= 2 {
			certificateRoleRules = append(certificateRoleRules, privilegePolicyRule(
				[]string{""},
				[]string{"configmaps"},
				[]string{t.certificateCanaryConfigMapName()},
				[]string{"get", "update"},
			))
		}
		certificateRoleRules = append(certificateRoleRules,
			privilegePolicyRule([]string{"discovery.k8s.io"}, []string{"endpointslices"}, nil, []string{"list"}),
		)
		contracts = append(contracts,
			releaseRBACContract{name: certificate, component: "certificate-rotation", cluster: true, rules: certificateClusterRules},
			releaseRBACContract{name: certificate, namespace: t.rollout.ReleaseNamespace, component: "certificate-rotation", rules: certificateRoleRules},
		)
		if t.rollout.ReleaseNamespace != corev1.NamespaceDefault {
			certificateDiscovery, _ := CertificateDiscoveryRoleName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName)
			contracts = append(contracts, releaseRBACContract{
				name: certificateDiscovery, namespace: corev1.NamespaceDefault, component: "certificate-rotation",
				rules: []rbacv1.PolicyRule{
					privilegePolicyRule([]string{"discovery.k8s.io"}, []string{"endpointslices"}, nil, []string{"list"}),
				},
			})
		}
	}
	return contracts
}

func releaseCRDNames() []string {
	return []string{
		"ptahmigrationapprovals.operator.ptah.run",
		"ptahmigrationplans.operator.ptah.run",
		"ptahmigrations.operator.ptah.run",
		"ptahrealms.operator.ptah.run",
		"ptahschemaapprovals.operator.ptah.run",
		"ptahschemaplans.operator.ptah.run",
		"ptahschemas.operator.ptah.run",
	}
}

// controllerClusterRoleRules is the controller ClusterRole the chart renders.
func controllerClusterRoleRules(guardNames []string) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		privilegePolicyRule([]string{"apiextensions.k8s.io"}, []string{"customresourcedefinitions"}, releaseCRDNames(), []string{"get"}),
		privilegePolicyRule(
			[]string{"admissionregistration.k8s.io"},
			[]string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"},
			[]string{AdmissionConfigurationName},
			[]string{"get"},
		),
		privilegePolicyRule(
			[]string{"admissionregistration.k8s.io"},
			[]string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"},
			guardNames,
			[]string{"get"},
		),
		privilegePolicyRule([]string{"operator.ptah.run"}, []string{"ptahschemas"}, nil, []string{"get", "list", "watch", "patch"}),
		privilegePolicyRule([]string{"operator.ptah.run"}, []string{"ptahschemas/finalizers", "ptahschemaplans/finalizers"}, nil, []string{"update"}),
		privilegePolicyRule([]string{"operator.ptah.run"}, []string{"ptahschemas/status", "ptahschemaplans/status", "ptahschemaapprovals/status"}, nil, []string{"get", "update", "patch"}),
		privilegePolicyRule([]string{"operator.ptah.run"}, []string{"ptahschemaplans"}, nil, []string{"get", "list", "watch", "create"}),
		privilegePolicyRule([]string{"operator.ptah.run"}, []string{"ptahschemaapprovals"}, nil, []string{"get", "list", "watch"}),
		privilegePolicyRule([]string{"operator.ptah.run"}, []string{"ptahmigrations"}, nil, []string{"get", "list", "watch", "patch"}),
		privilegePolicyRule([]string{"operator.ptah.run"}, []string{"ptahmigrations/finalizers"}, nil, []string{"update"}),
		privilegePolicyRule([]string{"operator.ptah.run"}, []string{"ptahmigrations/status"}, nil, []string{"get", "update", "patch"}),
		privilegePolicyRule([]string{"operator.ptah.run"}, []string{"ptahmigrationplans"}, nil, []string{"get", "list", "watch", "create"}),
		privilegePolicyRule([]string{"operator.ptah.run"}, []string{"ptahmigrationapprovals"}, nil, []string{"get", "list", "watch"}),
		privilegePolicyRule([]string{"operator.ptah.run"}, []string{"ptahmigrationapprovals/status"}, nil, []string{"get", "update", "patch"}),
		privilegePolicyRule([]string{"operator.ptah.run"}, []string{"ptahrealms"}, nil, []string{"get", "list", "watch"}),
		privilegePolicyRule([]string{"batch"}, []string{"jobs"}, nil, []string{"get", "list", "watch", "create", "patch"}),
		privilegePolicyRule([]string{""}, []string{"pods"}, nil, []string{"get", "list", "watch"}),
		privilegePolicyRule([]string{""}, []string{"pods/log"}, nil, []string{"get"}),
		privilegePolicyRule([]string{""}, []string{"serviceaccounts"}, nil, []string{"get"}),
		privilegePolicyRule([]string{""}, []string{"limitranges"}, nil, []string{"list"}),
		privilegePolicyRule([]string{"node.k8s.io"}, []string{"runtimeclasses"}, nil, []string{"get"}),
		privilegePolicyRule([]string{"scheduling.k8s.io"}, []string{"priorityclasses"}, nil, []string{"get", "list"}),
		privilegePolicyRule([]string{""}, []string{"configmaps"}, nil, []string{"get", "list", "watch", "create"}),
		privilegePolicyRule([]string{""}, []string{"events"}, nil, []string{"create", "patch", "update"}),
	}
}

func privilegePolicyRule(apiGroups, resources, resourceNames, verbs []string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{
		APIGroups:     apiGroups,
		Resources:     resources,
		ResourceNames: resourceNames,
		Verbs:         verbs,
	}
}

// releaseAdmissionGuardNames are the retained admission guards the release
// keeps, in the order the chart lists them.
func (t releaseRBACInventory) releaseAdmissionGuardNames() []string {
	namespace, name, sequence, image := t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage
	names := []string{
		RolloutGuardPolicyName(sequence),
		RuntimeGuardPolicyName(sequence),
		RuntimePodGuardPolicyName(sequence),
		HookIdentityGuardPolicyName(namespace, name, sequence, image),
		HookIdentityProbeGuardPolicyName(namespace, name, sequence, image),
		ReleaseActivationGuardPolicyName(namespace, name),
		ControllerWriteGuardPolicyName(namespace, name, sequence, image),
		ControllerJobWriteGuardPolicyName(namespace, name, sequence, image),
		ControllerChunkWriteGuardPolicyName(namespace, name, sequence, image),
		ControllerPlanWriteGuardPolicyName(namespace, name, sequence, image),
		ControllerMigrationPlanWriteGuardPolicyName(namespace, name, sequence, image),
		CertificateMutatingWriteGuardPolicyName(namespace, name),
		CertificateValidatingWriteGuardPolicyName(namespace, name),
		NamespaceDeletionGuardPolicyName(namespace, name),
		ParentReplicaSetGuardPolicyName(namespace, name, sequence, image),
		ParentHookPodOriginGuardPolicyName(namespace, name),
		ParentHookJobOriginGuardPolicyName(namespace, name),
		ParentHookJobContractPolicyName(namespace, name, sequence, image),
	}
	if t.contract.CertificateRuntimeEnabled {
		names = append(names, StagingSecretGuardPolicyName(namespace, name))
	}
	return names
}

func (t releaseRBACInventory) bootstrapAdmissionGuardNames() []string {
	names := []string{
		HookIdentityGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		HookIdentityProbeGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		ControllerWriteGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		ControllerJobWriteGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		ControllerChunkWriteGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		ControllerPlanWriteGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		ControllerMigrationPlanWriteGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		CertificateMutatingWriteGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName),
		CertificateValidatingWriteGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName),
		NamespaceDeletionGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName),
		ParentReplicaSetGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
		ParentHookPodOriginGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName),
		ParentHookJobOriginGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName),
		ParentHookJobContractPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName, t.rollout.ReleaseSequence, t.rollout.ManagerImage),
	}
	if t.contract.CertificateRuntimeEnabled {
		names = append(names, StagingSecretGuardPolicyName(t.rollout.ReleaseNamespace, t.rollout.ReleaseName))
	}
	return names
}

func (t releaseRBACInventory) certificateLeaseName() string {
	const prefix = "--lease-name="
	for _, argument := range t.rollout.CertificateArgs {
		if strings.HasPrefix(argument, prefix) {
			return strings.TrimPrefix(argument, prefix)
		}
	}
	return ""
}

func (t releaseRBACInventory) certificateStagingSecretName() string {
	const prefix = "--staging-secret-name="
	for _, argument := range t.rollout.CertificateArgs {
		if strings.HasPrefix(argument, prefix) {
			return strings.TrimPrefix(argument, prefix)
		}
	}
	return ""
}

func (t releaseRBACInventory) certificateCanaryConfigMapName() string {
	const prefix = "--candidate-probe-config-map-name="
	for _, argument := range t.rollout.CertificateArgs {
		if strings.HasPrefix(argument, prefix) {
			return strings.TrimPrefix(argument, prefix)
		}
	}
	return ""
}

func (t releaseRBACInventory) certificateRecreatesMissingSecret() bool {
	for _, argument := range t.rollout.CertificateArgs {
		if argument == "--recreate-missing-secret=true" {
			return true
		}
	}
	return false
}

func privilegeHookBindingName(name string, limit int, suffix string) string {
	if len(name) > limit {
		name = name[:limit]
	}
	return strings.TrimSuffix(name, "-") + suffix
}

// TestRenderedReleaseRBACMatchesCompiledContract holds every ClusterRole and
// Role the chart renders for the controller, the certificate rotator and the
// CRD hooks to the exact rules compiled above.
func TestRenderedReleaseRBACMatchesCompiledContract(t *testing.T) {
	path := os.Getenv("PTAH_PRIVILEGE_RENDER")
	if path == "" {
		t.Skip("PTAH_PRIVILEGE_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inventory := renderedReleaseRBACInventory(t)
	expected := make(map[string]releaseRBACContract)
	for _, contract := range inventory.contracts() {
		expected[renderedRBACKey(contract.cluster, contract.namespace, contract.name)] = contract
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
			key = renderedRBACKey(true, "", object.Name)
			rules = object.Rules
			if object.Name == "ptah-e2e-ptah-operator" {
				managerCanSetPlanOwner = permitsResourceUpdate(object.Rules, "operator.ptah.run", "ptahschemaplans/finalizers")
			}
		case "Role":
			var object rbacv1.Role
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			key = renderedRBACKey(false, object.Namespace, object.Name)
			rules = object.Rules
		default:
			continue
		}
		contract, matched := expected[key]
		if !matched {
			continue
		}
		if seen[key] {
			t.Fatalf("rendered authorization object %s appears more than once", key)
		}
		seen[key] = true
		if !reflect.DeepEqual(rules, contract.rules) {
			t.Fatalf("rendered authorization object %s rules = %#v, want %#v", key, rules, contract.rules)
		}
	}
	if len(expected) == 0 {
		t.Fatal("the compiled release RBAC contract is empty")
	}
	for key := range expected {
		if !seen[key] {
			t.Errorf("rendered authorization object %s is missing", key)
		}
	}
	// Plan chunks use blockOwnerDeletion on their PtahSchemaPlan owner. The API
	// server therefore requires this permission when the manager creates a
	// chunk, independently of the contract compiled above.
	if !managerCanSetPlanOwner {
		t.Fatal("rendered manager ClusterRole cannot set a blocking PtahSchemaPlan owner reference")
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

const (
	renderedRBACReleaseName  = "ptah-e2e"
	renderedRBACManagerImage = "ghcr.io/stokaro/ptah-operator@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

type renderedRBACSettings struct {
	releaseNamespace               string
	coordinationNamespace          string
	controllerServiceAccountName   string
	controllerServiceAccountCreate bool
	certificateRuntimeEnabled      bool
}

func renderedReleaseRBACInventory(t *testing.T) releaseRBACInventory {
	t.Helper()
	settings, err := renderedRBACSettingsFromEnvironment(os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	const controllerName = "ptah-e2e-ptah-operator"
	attempt := hookIdentityDigest(settings.releaseNamespace, renderedRBACReleaseName, 1, renderedRBACManagerImage)
	controllerServiceAccountName := settings.controllerServiceAccountName
	admissionContractVersion := int32(1)
	certificateArgs := []string{
		"--lease-name=" + controllerName + "-cert-rotation",
		"--staging-secret-name=" + controllerName + "-cert-rotation-stage",
	}
	if settings.certificateRuntimeEnabled {
		admissionContractVersion = CurrentAdmissionContractVersion
		certificateArgs = append(certificateArgs,
			"--candidate-bind-address=:9444",
			"--candidate-probe-config-map-name="+controllerName+"-cert-canary",
		)
	}
	return releaseRBACInventory{
		rollout: &RolloutGuard{
			ReleaseName:                  renderedRBACReleaseName,
			ReleaseNamespace:             settings.releaseNamespace,
			CoordinationNamespace:        settings.coordinationNamespace,
			ReleaseSequence:              1,
			ManagerImage:                 renderedRBACManagerImage,
			HookServiceAccountName:       "ptah-e2e-ptah-operator-crd-v1-" + attempt[:12],
			ControllerServiceAccountName: controllerServiceAccountName,
			ControllerDeploymentName:     controllerName,
			CertificateDeploymentName:    controllerName + "-cert-rotator",
			CertificateRuntimeEnabled:    settings.certificateRuntimeEnabled,
			AdmissionContractVersion:     admissionContractVersion,
			WebhookSecretName:            controllerName + "-webhook-cert",
			CertificateArgs:              certificateArgs,
		},
		contract: RuntimeAdmissionContract{
			Namespace:                      settings.releaseNamespace,
			ControllerServiceAccountName:   controllerServiceAccountName,
			CertificateServiceAccountName:  controllerName + "-cert-rotator",
			ControllerServiceAccountCreate: settings.controllerServiceAccountCreate,
			CertificateRuntimeEnabled:      settings.certificateRuntimeEnabled,
		},
	}
}

func renderedRBACSettingsFromEnvironment(lookup func(string) (string, bool)) (renderedRBACSettings, error) {
	const (
		defaultReleaseNamespace = "ptah-e2e"
		defaultControllerName   = "ptah-e2e-ptah-operator"
	)
	settings := renderedRBACSettings{
		releaseNamespace:               defaultReleaseNamespace,
		coordinationNamespace:          defaultReleaseNamespace,
		controllerServiceAccountName:   defaultControllerName,
		controllerServiceAccountCreate: true,
		certificateRuntimeEnabled:      true,
	}
	if value, found := lookup("PTAH_RBAC_RELEASE_NAMESPACE"); found {
		if value == "" || value != strings.TrimSpace(value) {
			return renderedRBACSettings{}, errors.New("PTAH_RBAC_RELEASE_NAMESPACE must be non-empty without surrounding whitespace")
		}
		settings.releaseNamespace = value
		settings.coordinationNamespace = value
	}
	if value, found := lookup("PTAH_RBAC_COORDINATION_NAMESPACE"); found && value != "" {
		if value != strings.TrimSpace(value) {
			return renderedRBACSettings{}, errors.New("PTAH_RBAC_COORDINATION_NAMESPACE must not contain surrounding whitespace")
		}
		settings.coordinationNamespace = value
	}
	if value, found := lookup("PTAH_RBAC_CERTIFICATE_RUNTIME_ENABLED"); found {
		switch value {
		case "true":
			settings.certificateRuntimeEnabled = true
		case "false":
			settings.certificateRuntimeEnabled = false
		default:
			return renderedRBACSettings{}, errors.New("PTAH_RBAC_CERTIFICATE_RUNTIME_ENABLED must be exactly true or false")
		}
	}
	return settings, nil
}

// releaseTeardownRBACContracts compiles the ClusterRole and Role the uninstall
// hook runs with: delete authority over exactly the guards the teardown
// deletes, and in the release namespace the runtime stop and the retained
// objects it deletes by name.
func releaseTeardownRBACContracts(t *testing.T, rollout *RolloutGuard) []releaseRBACContract {
	t.Helper()
	quiesce, err := TeardownQuiesceJobName(rollout.HookServiceAccountName)
	if err != nil {
		t.Fatal(err)
	}
	role := []rbacv1.PolicyRule{
		privilegePolicyRule([]string{"apps"}, []string{"deployments"}, []string{rollout.ControllerDeploymentName, rollout.CertificateDeploymentName}, []string{"get", "update"}),
		privilegePolicyRule([]string{""}, []string{"pods"}, nil, []string{"list"}),
		privilegePolicyRule([]string{""}, []string{"configmaps"}, []string{ReleaseActivationName}, []string{"get", "update", "delete"}),
		privilegePolicyRule([]string{""}, []string{"configmaps"}, []string{
			AdmissionConvergenceMarkerName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence),
			HookIdentityProbeObjectName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage),
			ParentOriginReadyMarkerName(rollout.ReleaseNamespace, rollout.ReleaseName),
		}, []string{"get", "delete"}),
	}
	if rollout.CertificateRuntimeEnabled {
		role = append(role, privilegePolicyRule([]string{""}, []string{"secrets"}, []string{rollout.ControllerDeploymentName + "-cert-rotation-stage"}, []string{"delete"}))
	}
	return []releaseRBACContract{
		{
			name: quiesce, cluster: true,
			rules: []rbacv1.PolicyRule{privilegePolicyRule(
				[]string{"admissionregistration.k8s.io"},
				[]string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"},
				releaseTeardownGuardNames(rollout),
				[]string{"get", "delete"},
			)},
		},
		{name: quiesce, namespace: rollout.ReleaseNamespace, rules: role},
	}
}

// TestRenderedTeardownRBACMatchesCompiledContract holds the uninstall hook's
// rendered ClusterRole and Role to the rules compiled above, with resource
// names compared as sets: the chart may list them in any order, but not one
// more or one fewer than the teardown deletes.
func TestRenderedTeardownRBACMatchesCompiledContract(t *testing.T) {
	path := os.Getenv("PTAH_TEARDOWN_RENDER")
	if path == "" {
		t.Skip("PTAH_TEARDOWN_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inventory := renderedReleaseRBACInventory(t)
	expected := make(map[string]releaseRBACContract)
	for _, contract := range releaseTeardownRBACContracts(t, inventory.rollout) {
		expected[renderedRBACKey(contract.cluster, contract.namespace, contract.name)] = contract
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
			key, rules = renderedRBACKey(true, "", object.Name), object.Rules
		case "Role":
			var object rbacv1.Role
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			key, rules = renderedRBACKey(false, object.Namespace, object.Name), object.Rules
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
		if got, want := sortedRuleNames(rules), sortedRuleNames(contract.rules); !reflect.DeepEqual(got, want) {
			t.Fatalf("rendered authorization object %s rules = %#v, want %#v", key, got, want)
		}
	}
	if len(expected) != 2 {
		t.Fatalf("the compiled teardown RBAC contract has %d objects, want a ClusterRole and a Role", len(expected))
	}
	for key := range expected {
		if !seen[key] {
			t.Errorf("rendered authorization object %s is missing", key)
		}
	}
}

func sortedRuleNames(rules []rbacv1.PolicyRule) []rbacv1.PolicyRule {
	sorted := make([]rbacv1.PolicyRule, len(rules))
	for index, rule := range rules {
		copied := *rule.DeepCopy()
		slices.Sort(copied.ResourceNames)
		sorted[index] = copied
	}
	return sorted
}

func TestRenderedRBACSettingsFromEnvironment(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		values  map[string]string
		want    renderedRBACSettings
		wantErr string
	}{
		{
			name: "defaults",
			want: renderedRBACSettings{
				releaseNamespace: "ptah-e2e", coordinationNamespace: "ptah-e2e",
				controllerServiceAccountName: "ptah-e2e-ptah-operator", controllerServiceAccountCreate: true, certificateRuntimeEnabled: true,
			},
		},
		{
			name:   "separate coordination namespace",
			values: map[string]string{"PTAH_RBAC_RELEASE_NAMESPACE": "ptah-system", "PTAH_RBAC_COORDINATION_NAMESPACE": "ptah-coordination"},
			want: renderedRBACSettings{
				releaseNamespace: "ptah-system", coordinationNamespace: "ptah-coordination",
				controllerServiceAccountName: "ptah-e2e-ptah-operator", controllerServiceAccountCreate: true, certificateRuntimeEnabled: true,
			},
		},
		{
			name:   "empty coordination namespace uses the release namespace",
			values: map[string]string{"PTAH_RBAC_RELEASE_NAMESPACE": "ptah-system", "PTAH_RBAC_COORDINATION_NAMESPACE": ""},
			want: renderedRBACSettings{
				releaseNamespace: "ptah-system", coordinationNamespace: "ptah-system",
				controllerServiceAccountName: "ptah-e2e-ptah-operator", controllerServiceAccountCreate: true, certificateRuntimeEnabled: true,
			},
		},
		{
			name:   "an external certificate",
			values: map[string]string{"PTAH_RBAC_CERTIFICATE_RUNTIME_ENABLED": "false"},
			want: renderedRBACSettings{
				releaseNamespace: "ptah-e2e", coordinationNamespace: "ptah-e2e",
				controllerServiceAccountName: "ptah-e2e-ptah-operator", controllerServiceAccountCreate: true, certificateRuntimeEnabled: false,
			},
		},
		{name: "empty release namespace", values: map[string]string{"PTAH_RBAC_RELEASE_NAMESPACE": ""}, wantErr: "must be non-empty"},
		{name: "a certificate setting that is not a boolean", values: map[string]string{"PTAH_RBAC_CERTIFICATE_RUNTIME_ENABLED": "1"}, wantErr: "exactly true or false"},
		{name: "padded coordination namespace", values: map[string]string{"PTAH_RBAC_COORDINATION_NAMESPACE": " x"}, wantErr: "surrounding whitespace"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := renderedRBACSettingsFromEnvironment(func(name string) (string, bool) {
				value, found := test.values[name]
				return value, found
			})
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("renderedRBACSettingsFromEnvironment() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("renderedRBACSettingsFromEnvironment() = %#v, %v; want %#v", got, err, test.want)
			}
		})
	}
}

func renderedRBACKey(cluster bool, namespace, name string) string {
	if cluster {
		return "ClusterRole/" + name
	}
	return "Role/" + namespace + "/" + name
}
