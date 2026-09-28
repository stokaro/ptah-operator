package crdupgrade

// This file keeps the exact RBAC a release issues as a compiled contract the
// rendered chart is held to. It is test code: nothing in the binary needs the
// whole inventory.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// releaseRBACContract is one ClusterRole or Role a release renders, with the
// exact rules it must carry.
type releaseRBACContract struct {
	name      string
	namespace string
	cluster   bool
	rules     []rbacv1.PolicyRule
}

// releaseRBACInventory names what a release's RBAC refers to: its identities,
// its Deployments and the objects its certificate rotator writes.
type releaseRBACInventory struct {
	releaseNamespace             string
	coordinationNamespace        string
	hookServiceAccountName       string
	controllerName               string
	controllerServiceAccountName string
	certificateName              string
	certificateRuntimeEnabled    bool
}

// contracts compiles every ClusterRole and Role the release installs for its
// controller, its certificate rotator and its CRD hook.
func (t releaseRBACInventory) contracts() []releaseRBACContract {
	crdNames := releaseCRDNames()
	contracts := []releaseRBACContract{
		{name: t.controllerName, cluster: true, rules: controllerClusterRoleRules()},
		{
			name: t.controllerName, namespace: t.coordinationNamespace,
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule([]string{"coordination.k8s.io"}, []string{"leases"}, nil, []string{"get", "create", "update"}),
			},
		},
		{
			name: t.hookServiceAccountName, cluster: true,
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule([]string{"apiextensions.k8s.io"}, []string{"customresourcedefinitions"}, crdNames, []string{"get", "update"}),
				privilegePolicyRule(
					[]string{"operator.ptah.run"},
					[]string{"ptahschemas", "ptahschemaplans", "ptahschemaapprovals", "ptahmigrations", "ptahmigrationplans", "ptahmigrationapprovals"},
					nil,
					[]string{"list"},
				),
			},
		},
		{
			name: t.hookServiceAccountName, namespace: t.releaseNamespace,
			rules: []rbacv1.PolicyRule{
				privilegePolicyRule(
					[]string{"apps"}, []string{"deployments"},
					[]string{t.controllerName, t.certificateName},
					[]string{"get", "update"},
				),
				privilegePolicyRule([]string{""}, []string{"pods"}, nil, []string{"list"}),
			},
		},
	}
	if t.certificateRuntimeEnabled {
		contracts = append(contracts,
			releaseRBACContract{
				name: t.certificateName, cluster: true,
				rules: []rbacv1.PolicyRule{
					privilegePolicyRule([]string{"apiextensions.k8s.io"}, []string{"customresourcedefinitions"}, crdNames, []string{"get"}),
					privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"mutatingwebhookconfigurations"}, []string{AdmissionConfigurationName}, []string{"get", "update"}),
					privilegePolicyRule([]string{"admissionregistration.k8s.io"}, []string{"validatingwebhookconfigurations"}, []string{AdmissionConfigurationName}, []string{"get", "update"}),
				},
			},
			releaseRBACContract{
				name: t.certificateName, namespace: t.releaseNamespace,
				rules: []rbacv1.PolicyRule{
					privilegePolicyRule(
						[]string{""}, []string{"secrets"},
						[]string{t.controllerName + "-webhook-cert", t.controllerName + "-cert-rotation-stage"},
						[]string{"get", "update"},
					),
					privilegePolicyRule([]string{"coordination.k8s.io"}, []string{"leases"}, []string{t.controllerName + "-cert-rotation"}, []string{"get", "update"}),
					privilegePolicyRule([]string{"discovery.k8s.io"}, []string{"endpointslices"}, nil, []string{"list"}),
				},
			},
		)
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
func controllerClusterRoleRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		privilegePolicyRule([]string{"apiextensions.k8s.io"}, []string{"customresourcedefinitions"}, releaseCRDNames(), []string{"get"}),
		privilegePolicyRule(
			[]string{"admissionregistration.k8s.io"},
			[]string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"},
			[]string{AdmissionConfigurationName},
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
	// The chart names the hook after the release alone; see
	// ptah-operator.crdManagerServiceAccountName.
	return releaseRBACInventory{
		releaseNamespace:             settings.releaseNamespace,
		coordinationNamespace:        settings.coordinationNamespace,
		hookServiceAccountName:       controllerName + "-crd-manager",
		controllerName:               controllerName,
		controllerServiceAccountName: settings.controllerServiceAccountName,
		certificateName:              controllerName + "-cert-rotator",
		certificateRuntimeEnabled:    settings.certificateRuntimeEnabled,
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
