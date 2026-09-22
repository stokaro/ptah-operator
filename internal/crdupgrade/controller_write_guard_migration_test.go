package crdupgrade

// The controller write guard confines the controller identity's main-resource
// patches to the one finalizer that identity owns. The manager holds patch on
// both families, so the boundary has to cover both: a guard that names only
// PtahSchema leaves migration policy, artifact selection, target selection and
// suspension writable by the controller (stokaro/ptah-operator#233).
//
// The proof decides real admission requests against the expression the chart
// renders, rather than reading the template for a literal. A rule that selects
// nothing and an expression that admits everything both read correctly, and
// this repository has already paid for a test that could not tell.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/predicates/rules"
	"k8s.io/apiserver/pkg/authentication/user"
)

const (
	controllerWriteProofNamespace = "team-a"
	controllerWriteProofName      = "write-guard-proof"
	controllerWriteProofRelease   = "ptah-e2e"
)

// TestRenderedControllerWriteGuardSelectsBothFamilies hands Kubernetes' own
// rule matcher the resource rules the chart renders. An expression that would
// refuse a migration patch decides nothing while the rule selects schemas
// alone.
func TestRenderedControllerWriteGuardSelectsBothFamilies(t *testing.T) {
	t.Parallel()

	policy, binding, _ := renderedControllerWriteGuard(t)
	for _, resource := range []string{"ptahschemas", "ptahmigrations"} {
		attributes := controllerWriteAdmissionAttributes(resource)
		if !controllerWriteRulesMatch(policy.Spec.MatchConstraints.ResourceRules, attributes) {
			t.Fatalf("rendered controller write policy does not select an UPDATE on %s", resource)
		}
		if !controllerWriteRulesMatch(binding.Spec.MatchResources.ResourceRules, attributes) {
			t.Fatalf("rendered controller write binding does not select an UPDATE on %s", resource)
		}
	}
}

// TestRenderedControllerWriteGuardConfinesMigrationPatches evaluates the
// rendered CEL over the requests the controller identity actually makes, and
// over the ones it must never make.
func TestRenderedControllerWriteGuardConfinesMigrationPatches(t *testing.T) {
	t.Parallel()

	policy, _, serviceAccount := renderedControllerWriteGuard(t)
	migrationFinalizer := "operator.ptah.run/migration-operation"
	schemaFinalizer := "operator.ptah.run/active-operation"

	tests := []struct {
		name     string
		resource string
		before   []string
		after    []string
		mutate   func(object map[string]any)
		admitted bool
	}{
		{
			name:     "a migration patch that adds its own finalizer",
			resource: "ptahmigrations", before: nil, after: []string{migrationFinalizer}, admitted: true,
		},
		{
			name:     "a migration patch that removes its own finalizer",
			resource: "ptahmigrations", before: []string{migrationFinalizer}, after: nil, admitted: true,
		},
		{
			name:     "a migration patch that changes spec.policy.apply",
			resource: "ptahmigrations",
			mutate: func(object map[string]any) {
				object["spec"].(map[string]any)["policy"].(map[string]any)["apply"] = "Always"
			},
		},
		{
			name:     "a migration patch that changes spec.suspend",
			resource: "ptahmigrations",
			mutate: func(object map[string]any) {
				object["spec"].(map[string]any)["suspend"] = true
			},
		},
		{
			name:     "a migration patch that changes the artifact",
			resource: "ptahmigrations",
			mutate: func(object map[string]any) {
				object["spec"].(map[string]any)["artifact"].(map[string]any)["ociRef"] = "oci://example.invalid/other:v2"
			},
		},
		{
			name:     "a migration patch that changes the target",
			resource: "ptahmigrations",
			mutate: func(object map[string]any) {
				object["spec"].(map[string]any)["target"].(map[string]any)["coordinationKey"] = "production/other"
			},
		},
		{
			name:     "a migration patch that adds a foreign finalizer",
			resource: "ptahmigrations", before: nil, after: []string{"example.invalid/foreign"},
		},
		{
			name:     "a migration patch that adds the schema family's finalizer",
			resource: "ptahmigrations", before: nil, after: []string{schemaFinalizer},
		},
		{
			name:     "a migration patch that changes labels",
			resource: "ptahmigrations",
			mutate: func(object map[string]any) {
				object["metadata"].(map[string]any)["labels"].(map[string]any)["team"] = "b"
			},
		},
		{
			name:     "a migration patch that changes annotations",
			resource: "ptahmigrations",
			mutate: func(object map[string]any) {
				object["metadata"].(map[string]any)["annotations"].(map[string]any)["note"] = "rewritten"
			},
		},
		{
			name:     "a migration patch that adds an owner reference",
			resource: "ptahmigrations",
			mutate: func(object map[string]any) {
				object["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap", "name": "owner", "uid": "owner-uid",
				}}
			},
		},
		{
			name:     "a migration patch that changes status through the main resource",
			resource: "ptahmigrations",
			mutate: func(object map[string]any) {
				object["status"].(map[string]any)["phase"] = "Applying"
			},
		},
		{
			name:     "a schema patch that adds its own finalizer",
			resource: "ptahschemas", before: nil, after: []string{schemaFinalizer}, admitted: true,
		},
		{
			name:     "a schema patch that adds the migration family's finalizer",
			resource: "ptahschemas", before: nil, after: []string{migrationFinalizer},
		},
		{
			name:     "a schema patch that changes spec.suspend",
			resource: "ptahschemas",
			mutate: func(object map[string]any) {
				object["spec"].(map[string]any)["suspend"] = true
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			oldObject := controllerWriteSubject(test.resource, test.before)
			object := controllerWriteSubject(test.resource, test.after)
			if test.mutate != nil {
				object = controllerWriteSubject(test.resource, test.before)
				test.mutate(object)
			}
			request := controllerWriteRequest(test.resource, serviceAccount)
			params := controllerWriteActivationParams()
			if !evaluatePolicyMatchConditions(t, policy, object, oldObject, request, params) {
				t.Fatalf("the rendered guard does not decide a controller %s patch at all", test.resource)
			}
			results := evaluatePolicyValidations(t, policy, object, oldObject, request, params)
			admitted := true
			refused := -1
			for index, allowed := range results {
				if !allowed {
					admitted = false
					refused = index
					break
				}
			}
			if admitted != test.admitted {
				if test.admitted {
					t.Fatalf("the rendered guard refused a legitimate patch at validation %d: %s",
						refused, policy.Spec.Validations[refused].Message)
				}
				t.Fatal("the rendered guard admitted a controller patch to user-authored configuration")
			}
			if !test.admitted && policy.Spec.Validations[refused].Message != controllerWriteGuardDenialMessage() {
				t.Fatalf("validation %d refused with %q, want the write-guard denial",
					refused, policy.Spec.Validations[refused].Message)
			}
		})
	}
}

func renderedControllerWriteGuard(t *testing.T) (
	*admissionregistrationv1.ValidatingAdmissionPolicy,
	*admissionregistrationv1.ValidatingAdmissionPolicyBinding,
	string,
) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for rendered controller write guard tests")
	}
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve controller write guard test path")
	}
	chart := filepath.Join(filepath.Dir(filename), "..", "..", "charts", "ptah-operator")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, helm,
		"template", controllerWriteProofRelease, chart,
		"--namespace", controllerWriteProofRelease,
		"--show-only", "templates/controller-write-guard.yaml",
		"--show-only", "templates/deployment.yaml",
		"--set-string", "image.digest=sha256:"+strings.Repeat("2", 64),
		"--set-string", "execution.executorImage=e2e.invalid/executor@sha256:"+strings.Repeat("0", 64),
		"--set-string", "execution.runnerImage=e2e.invalid/runner@sha256:"+strings.Repeat("1", 64),
		"--set-string", "execution.ptahVersion=e2e-explicit-version",
	)
	temporaryHome := t.TempDir()
	command.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(temporaryHome, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(temporaryHome, "config"),
		"HELM_DATA_HOME="+filepath.Join(temporaryHome, "data"),
	)
	rendered, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("render controller write guard: %v\n%s", err, rendered)
	}

	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	var binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
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
		switch typeMeta.Kind {
		case "ValidatingAdmissionPolicy":
			var object admissionregistrationv1.ValidatingAdmissionPolicy
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(object.Name, controllerWriteGuardNamePrefix) {
				policy = &object
			}
		case "ValidatingAdmissionPolicyBinding":
			var object admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(object.Name, controllerWriteGuardNamePrefix) {
				binding = &object
			}
		}
	}
	if policy == nil || binding == nil {
		t.Fatalf("rendered chart carries no controller write guard:\n%s", rendered)
	}
	return policy, binding, renderedDeploymentServiceAccount(t, rendered, controllerWriteProofRelease+"-ptah-operator")
}

func controllerWriteRulesMatch(
	resourceRules []admissionregistrationv1.NamedRuleWithOperations,
	attributes admission.Attributes,
) bool {
	for _, rule := range resourceRules {
		if len(rule.ResourceNames) != 0 {
			continue
		}
		matcher := rules.Matcher{Rule: rule.RuleWithOperations, Attr: attributes}
		if matcher.Matches() {
			return true
		}
	}
	return false
}

func controllerWriteAdmissionAttributes(resource string) admission.Attributes {
	kinds := map[string]string{"ptahschemas": "PtahSchema", "ptahmigrations": "PtahMigration"}
	return admission.NewAttributesRecord(
		nil,
		nil,
		schema.GroupVersionKind{Group: "operator.ptah.run", Version: "v1alpha1", Kind: kinds[resource]},
		controllerWriteProofNamespace,
		controllerWriteProofName,
		schema.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: resource},
		"",
		admission.Update,
		nil,
		false,
		&user.DefaultInfo{Name: "system:serviceaccount:ptah-e2e:controller"},
	)
}

func controllerWriteSubject(resource string, finalizers []string) map[string]any {
	kinds := map[string]string{"ptahschemas": "PtahSchema", "ptahmigrations": "PtahMigration"}
	metadata := map[string]any{
		"name":            controllerWriteProofName,
		"namespace":       controllerWriteProofNamespace,
		"uid":             "write-guard-proof-uid",
		"resourceVersion": "1001",
		"generation":      int64(1),
		"labels":          map[string]any{"team": "a"},
		"annotations":     map[string]any{"note": "author"},
	}
	if len(finalizers) != 0 {
		carried := make([]any, 0, len(finalizers))
		for _, finalizer := range finalizers {
			carried = append(carried, finalizer)
		}
		metadata["finalizers"] = carried
	}
	artifact := map[string]any{
		"ociRef":                 "oci://example.invalid/orders:v1",
		"verificationPolicyFrom": map[string]any{"name": "verification", "key": "policy.yaml"},
	}
	spec := map[string]any{
		"suspend": false,
		"policy":  map[string]any{"apply": "OnApproval"},
		"target": map[string]any{
			"engine":          "PostgreSQL",
			"coordinationKey": "production/orders",
			"urlFrom":         map[string]any{"name": "orders-database", "key": "url"},
		},
	}
	if resource == "ptahmigrations" {
		spec["artifact"] = artifact
	} else {
		spec["desired"] = artifact
	}
	return map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1",
		"kind":       kinds[resource],
		"metadata":   metadata,
		"spec":       spec,
		"status":     map[string]any{"phase": "InSync"},
	}
}

func controllerWriteRequest(resource, serviceAccount string) map[string]any {
	return map[string]any{
		"operation":   "UPDATE",
		"namespace":   controllerWriteProofNamespace,
		"name":        controllerWriteProofName,
		"dryRun":      false,
		"subResource": "",
		"options":     map[string]any{"fieldManager": "ptah-operator"},
		"resource": map[string]any{
			"group":    "operator.ptah.run",
			"version":  "v1alpha1",
			"resource": resource,
		},
		"userInfo": map[string]any{
			"username": "system:serviceaccount:" + controllerWriteProofRelease + ":" + serviceAccount,
		},
	}
}

func controllerWriteActivationParams() map[string]any {
	sequence := strconv.Itoa(1)
	return map[string]any{
		"metadata": map[string]any{
			"name":            ReleaseActivationName,
			"namespace":       controllerWriteProofRelease,
			"uid":             "release-activation-uid",
			"resourceVersion": "101",
			"annotations": map[string]any{
				"helm.sh/hook":                     "pre-install,pre-upgrade",
				"helm.sh/hook-weight":              releaseActivationHookWeight,
				"helm.sh/resource-policy":          "keep",
				rolloutGuardVersionAnnotation:      rolloutGuardVersion,
				ReleaseNameAnnotation:              controllerWriteProofRelease,
				ReleaseNamespaceAnnotation:         controllerWriteProofRelease,
				ControllerStateVersionAnnotation:   ourStateVersionString(),
				AdmissionContractVersionAnnotation: "1",
				ReleaseSequenceAnnotation:          sequence,
				ManagerImageAnnotation:             renderedGuardManagerImage,
			},
			"labels": map[string]any{
				managedByLabel:                rolloutGuardManagedBy,
				instanceLabel:                 controllerWriteProofRelease,
				"app.kubernetes.io/component": rolloutGuardComponent,
			},
		},
		"data": map[string]any{
			activeReleaseDataKey:         sequence,
			controllerCredentialsDataKey: string(ControllerCredentialsActive),
		},
	}
}
