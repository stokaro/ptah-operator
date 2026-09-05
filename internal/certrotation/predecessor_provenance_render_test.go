package certrotation_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const predecessorProvenanceProbeTemplate = `{{- $fixture := default (dict) .Values.fixture -}}
{{- $principal := include "ptah-operator.previousControllerPrincipalFromObjectsJSON" (dict
      "root" .
      "guardPolicy" (get $fixture "guardPolicy")
      "guardBinding" (get $fixture "guardBinding")
      "deployment" (get $fixture "deployment")
      "clusterRoleBinding" (get $fixture "clusterRoleBinding")
      "coordinationRoleBinding" (get $fixture "coordinationRoleBinding")
      "serviceAccount" (get $fixture "serviceAccount")) | fromJson -}}
apiVersion: v1
kind: ConfigMap
metadata:
  name: provenance-result
data:
  previousName: {{ $principal.name | quote }}
  previousSequence: {{ $principal.releaseSequence | quote }}
  previousUID: {{ $principal.uid | quote }}
  previousManaged: {{ $principal.managed | quote }}
`

func TestControllerPredecessorProvenanceRender(t *testing.T) {
	legacyName := releaseName + "-ptah-operator"
	candidateName := provenanceCandidateControllerServiceAccount()

	t.Run("fresh install", func(t *testing.T) {
		result, err := renderPredecessorProvenance(t, map[string]any{})
		if err != nil {
			t.Fatalf("fresh install provenance render: %v", err)
		}
		assertPredecessorResult(t, result, "", "0", "", "false")
	})

	t.Run("exact legacy predecessor", func(t *testing.T) {
		result, err := renderPredecessorProvenance(t, legacyControllerFixture(legacyName))
		if err != nil {
			t.Fatalf("legacy predecessor provenance render: %v", err)
		}
		assertPredecessorResult(t, result, legacyName, "0", "service-account-uid", "false")
	})

	t.Run("spoofed Helm ownership markers do not grant deletion authority", func(t *testing.T) {
		result, err := renderPredecessorProvenance(t, legacyControllerFixture(legacyName))
		if err != nil {
			t.Fatalf("spoofed ownership provenance render: %v", err)
		}
		assertPredecessorResult(t, result, legacyName, "0", "service-account-uid", "false")
	})

	t.Run("exact legacy predecessor with external ServiceAccount", func(t *testing.T) {
		fixture := legacyControllerFixture(legacyName)
		serviceAccountMetadata := metadataOf(fixtureObject(fixture, "serviceAccount"))
		serviceAccountMetadata["annotations"] = map[string]any{}
		serviceAccountMetadata["labels"] = map[string]any{"app.kubernetes.io/managed-by": "platform-team"}
		result, err := renderPredecessorProvenance(t, fixture)
		if err != nil {
			t.Fatalf("external legacy ServiceAccount provenance render: %v", err)
		}
		assertPredecessorResult(t, result, legacyName, "0", "service-account-uid", "false")
	})

	failureTests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "partial predecessor set",
			mutate: func(fixture map[string]any) {
				delete(fixture, "coordinationRoleBinding")
			},
		},
		{
			name: "foreign Deployment ownership",
			mutate: func(fixture map[string]any) {
				metadataOf(fixtureObject(fixture, "deployment"))["annotations"].(map[string]any)["meta.helm.sh/release-name"] = "foreign"
			},
		},
		{
			name: "mismatched binding subject",
			mutate: func(fixture map[string]any) {
				subjects := fixtureObject(fixture, "clusterRoleBinding")["subjects"].([]any)
				subjects[0].(map[string]any)["name"] = "different-controller"
			},
		},
		{
			name: "mismatched binding roleRef",
			mutate: func(fixture map[string]any) {
				fixtureObject(fixture, "coordinationRoleBinding")["roleRef"].(map[string]any)["name"] = "different-role"
			},
		},
		{
			name: "missing live ServiceAccount",
			mutate: func(fixture map[string]any) {
				delete(fixture, "serviceAccount")
			},
		},
		{
			name: "partial ServiceAccount Helm ownership",
			mutate: func(fixture map[string]any) {
				annotations := metadataOf(fixtureObject(fixture, "serviceAccount"))["annotations"].(map[string]any)
				delete(annotations, "meta.helm.sh/release-namespace")
			},
		},
		{
			name: "same-sequence candidate checkpoint",
			mutate: func(fixture map[string]any) {
				deployment := fixtureObject(fixture, "deployment")
				metadataOf(deployment)["annotations"].(map[string]any)["operator.ptah.dev/release-sequence"] = "1"
				metadataOf(deployment)["annotations"].(map[string]any)["operator.ptah.dev/controller-state-version"] = "1"
				deployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["serviceAccountName"] = candidateName
				for _, key := range []string{"clusterRoleBinding", "coordinationRoleBinding"} {
					subjects := fixtureObject(fixture, key)["subjects"].([]any)
					subjects[0].(map[string]any)["name"] = candidateName
				}
				serviceAccount := fixtureObject(fixture, "serviceAccount")
				metadataOf(serviceAccount)["name"] = candidateName
			},
		},
		{
			name: "predecessor collides with hook ServiceAccount",
			mutate: func(fixture map[string]any) {
				setLegacyPrincipalName(fixture, provenanceHookServiceAccount())
			},
		},
		{
			name: "predecessor collides with cleanup ServiceAccount",
			mutate: func(fixture map[string]any) {
				setLegacyPrincipalName(fixture, provenanceCleanupServiceAccount())
			},
		},
		{
			name: "predecessor collides with quiesce identity",
			mutate: func(fixture map[string]any) {
				setLegacyPrincipalName(fixture, provenanceQuiesceIdentity())
			},
		},
		{
			name: "predecessor collides with certificate ServiceAccount",
			mutate: func(fixture map[string]any) {
				setLegacyPrincipalName(fixture, provenanceCertificateServiceAccount())
			},
		},
	}
	for _, test := range failureTests {
		t.Run(test.name, func(t *testing.T) {
			fixture := legacyControllerFixture(legacyName)
			test.mutate(fixture)
			if _, err := renderPredecessorProvenance(t, fixture); err == nil {
				t.Fatal("Helm accepted invalid predecessor provenance")
			}
		})
	}
}

func TestRetainedControllerPrincipalProvenanceRender(t *testing.T) {
	legacyName := releaseName + "-ptah-operator"
	policy := retainedControllerPrincipalObject("ValidatingAdmissionPolicy", "-129", legacyName)
	binding := retainedControllerPrincipalObject("ValidatingAdmissionPolicyBinding", "-128", legacyName)

	for _, test := range []struct {
		name    string
		fixture map[string]any
	}{
		{name: "complete retry", fixture: map[string]any{"guardPolicy": policy, "guardBinding": binding}},
		{name: "retry after policy hook only", fixture: map[string]any{"guardPolicy": policy}},
		{name: "retry after binding hook only", fixture: map[string]any{"guardBinding": binding}},
		{name: "failed fresh install retry", fixture: map[string]any{
			"guardPolicy": retainedControllerPrincipalObject("ValidatingAdmissionPolicy", "-129", ""),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := renderPredecessorProvenance(t, test.fixture)
			if err != nil {
				t.Fatalf("retained principal provenance render: %v", err)
			}
			wantName := legacyName
			if test.name == "failed fresh install retry" {
				wantName = ""
			}
			wantUID := "service-account-uid"
			if wantName == "" {
				wantUID = ""
			}
			assertPredecessorResult(t, result, wantName, "0", wantUID, "false")
		})
	}

	t.Run("malformed immutable annotations", func(t *testing.T) {
		malformed := retainedControllerPrincipalObject("ValidatingAdmissionPolicy", "-129", legacyName)
		metadataOf(malformed)["annotations"].(map[string]any)["operator.ptah.dev/release-sequence"] = "01"
		if _, err := renderPredecessorProvenance(t, map[string]any{"guardPolicy": malformed}); err == nil {
			t.Fatal("Helm accepted malformed retained principal annotations")
		}
	})

	t.Run("same-sequence checkpoint without immutable tuple", func(t *testing.T) {
		checkpoint := retainedControllerPrincipalObject("ValidatingAdmissionPolicy", "-129", legacyName)
		annotations := metadataOf(checkpoint)["annotations"].(map[string]any)
		delete(annotations, "operator.ptah.dev/controller-service-account-name")
		delete(annotations, "operator.ptah.dev/manager-image")
		if _, err := renderPredecessorProvenance(t, map[string]any{"guardPolicy": checkpoint}); err == nil {
			t.Fatal("Helm accepted a retained same-sequence checkpoint without the immutable tuple")
		}
	})

	t.Run("policy and binding disagree", func(t *testing.T) {
		disagreeingBinding := retainedControllerPrincipalObject("ValidatingAdmissionPolicyBinding", "-128", "other-controller")
		if _, err := renderPredecessorProvenance(t, map[string]any{
			"guardPolicy":  policy,
			"guardBinding": disagreeingBinding,
		}); err == nil {
			t.Fatal("Helm accepted disagreeing retained principal annotations")
		}
	})
}

func TestRenderedControllerPrincipalGuardCarriesRetryTuple(t *testing.T) {
	objects := renderChart(t)
	guardName := "ptah-operator-service-account-origin-guard-v2-" + provenanceHookIdentityDigest()[:12]
	wantAnnotations := map[string]string{
		"operator.ptah.dev/controller-state-version":                    "1",
		"operator.ptah.dev/admission-contract-version":                  "1",
		"operator.ptah.dev/release-sequence":                            "1",
		"operator.ptah.dev/manager-image":                               provenanceManagerImage(),
		"operator.ptah.dev/hook-service-account-name":                   provenanceHookServiceAccount(),
		"operator.ptah.dev/controller-service-account-name":             provenanceCandidateControllerServiceAccount(),
		"operator.ptah.dev/controller-service-account-managed":          "true",
		"operator.ptah.dev/previous-controller-service-account-name":    "",
		"operator.ptah.dev/previous-controller-service-account-uid":     "",
		"operator.ptah.dev/previous-controller-service-account-managed": "false",
		"operator.ptah.dev/previous-controller-release-sequence":        "0",
	}
	for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
		object := mustObject(t, objects, kind, guardName)
		for key, want := range wantAnnotations {
			if got := object.GetAnnotations()[key]; got != want {
				t.Errorf("%s/%s annotation %s = %q, want %q", kind, guardName, key, got, want)
			}
		}
	}
}

func legacyControllerFixture(serviceAccountName string) map[string]any {
	name := releaseName + "-ptah-operator"
	return map[string]any{
		"deployment": map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": helmOwnedMetadata(name, releaseNamespace, "deployment-uid", map[string]any{
				"app.kubernetes.io/name":      "ptah-operator",
				"app.kubernetes.io/component": "controller",
			}),
			"spec": map[string]any{
				"template": map[string]any{
					"metadata": map[string]any{},
					"spec": map[string]any{
						"serviceAccountName": serviceAccountName,
					},
				},
			},
		},
		"clusterRoleBinding": map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "ClusterRoleBinding",
			"metadata":   helmOwnedMetadata(name, "", "cluster-binding-uid", nil),
			"roleRef": map[string]any{
				"apiGroup": "rbac.authorization.k8s.io",
				"kind":     "ClusterRole",
				"name":     name,
			},
			"subjects": []any{serviceAccountSubject(serviceAccountName)},
		},
		"coordinationRoleBinding": map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "RoleBinding",
			"metadata":   helmOwnedMetadata(name, releaseNamespace, "coordination-binding-uid", nil),
			"roleRef": map[string]any{
				"apiGroup": "rbac.authorization.k8s.io",
				"kind":     "Role",
				"name":     name,
			},
			"subjects": []any{serviceAccountSubject(serviceAccountName)},
		},
		"serviceAccount": map[string]any{
			"apiVersion": "v1",
			"kind":       "ServiceAccount",
			"metadata":   helmOwnedMetadata(serviceAccountName, releaseNamespace, "service-account-uid", nil),
		},
	}
}

func retainedControllerPrincipalObject(kind, weight, previousName string) map[string]any {
	name := "ptah-operator-service-account-origin-guard-v2-" + provenanceHookIdentityDigest()[:12]
	previousUID := "service-account-uid"
	if previousName == "" {
		previousUID = ""
	}
	return map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       kind,
		"metadata": map[string]any{
			"name": name,
			"uid":  "retained-guard-uid",
			"annotations": map[string]any{
				"helm.sh/hook":                                                  "pre-install,pre-upgrade",
				"helm.sh/hook-weight":                                           weight,
				"helm.sh/resource-policy":                                       "keep",
				"operator.ptah.dev/rollout-guard-version":                       "1",
				"operator.ptah.dev/release-name":                                releaseName,
				"operator.ptah.dev/release-namespace":                           releaseNamespace,
				"operator.ptah.dev/controller-state-version":                    "1",
				"operator.ptah.dev/admission-contract-version":                  "1",
				"operator.ptah.dev/release-sequence":                            "1",
				"operator.ptah.dev/manager-image":                               provenanceManagerImage(),
				"operator.ptah.dev/hook-service-account-name":                   provenanceHookServiceAccount(),
				"operator.ptah.dev/controller-service-account-name":             provenanceCandidateControllerServiceAccount(),
				"operator.ptah.dev/controller-service-account-managed":          "true",
				"operator.ptah.dev/previous-controller-service-account-name":    previousName,
				"operator.ptah.dev/previous-controller-service-account-uid":     previousUID,
				"operator.ptah.dev/previous-controller-service-account-managed": "false",
				"operator.ptah.dev/previous-controller-release-sequence":        "0",
			},
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "ptah-operator",
				"app.kubernetes.io/instance":   releaseName,
				"app.kubernetes.io/component":  "service-account-origin-guard",
			},
		},
	}
}

func helmOwnedMetadata(name, namespace, uid string, additionalLabels map[string]any) map[string]any {
	labels := map[string]any{
		"app.kubernetes.io/managed-by": "Helm",
		"app.kubernetes.io/instance":   releaseName,
	}
	for key, value := range additionalLabels {
		labels[key] = value
	}
	return map[string]any{
		"name":      name,
		"namespace": namespace,
		"uid":       uid,
		"annotations": map[string]any{
			"meta.helm.sh/release-name":      releaseName,
			"meta.helm.sh/release-namespace": releaseNamespace,
		},
		"labels": labels,
	}
}

func serviceAccountSubject(name string) map[string]any {
	return map[string]any{
		"kind":      "ServiceAccount",
		"name":      name,
		"namespace": releaseNamespace,
	}
}

func metadataOf(object map[string]any) map[string]any {
	return object["metadata"].(map[string]any)
}

func fixtureObject(fixture map[string]any, key string) map[string]any {
	return fixture[key].(map[string]any)
}

func setLegacyPrincipalName(fixture map[string]any, name string) {
	fixtureObject(fixture, "deployment")["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["serviceAccountName"] = name
	for _, key := range []string{"clusterRoleBinding", "coordinationRoleBinding"} {
		fixtureObject(fixture, key)["subjects"].([]any)[0].(map[string]any)["name"] = name
	}
	metadataOf(fixtureObject(fixture, "serviceAccount"))["name"] = name
}

func provenanceManagerImage() string {
	return "ghcr.io/stokaro/ptah-operator@sha256:" + managerDigest
}

func provenanceHookIdentityDigest() string {
	identity := releaseNamespace + "\n" + releaseName + "\n1\n" + provenanceManagerImage()
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
}

func provenanceHookServiceAccount() string {
	base := releaseName + "-ptah-operator"
	if len(base) > 24 {
		base = base[:24]
	}
	return base + "-crd-v1-" + provenanceHookIdentityDigest()[:12]
}

func provenanceCleanupServiceAccount() string {
	base := releaseName + "-ptah-operator"
	if len(base) > 24 {
		base = base[:24]
	}
	return base + "-cleanup-v1-" + provenanceHookIdentityDigest()[:12]
}

func provenanceQuiesceIdentity() string {
	base := releaseName + "-ptah-operator"
	if len(base) > 24 {
		base = base[:24]
	}
	return base + "-quiesce-v1-" + provenanceHookIdentityDigest()[:12]
}

func provenanceCertificateServiceAccount() string {
	base := releaseName + "-ptah-operator"
	if len(base) > 39 {
		base = base[:39]
	}
	return base + "-cert-rotator"
}

func provenanceCandidateControllerServiceAccount() string {
	sourceBase := releaseName + "-ptah-operator"
	base := sourceBase
	if len(base) > 38 {
		base = base[:38]
	}
	identity := sourceBase + "\n1\n" + provenanceHookIdentityDigest()
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
	return base + "-v1-" + digest[:12]
}

func renderPredecessorProvenance(t *testing.T, fixture map[string]any) (*unstructured.Unstructured, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for chart render tests")
	}
	_, filename, _, _ := runtime.Caller(0)
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	helpers, err := os.ReadFile(filepath.Join(repositoryRoot, "charts", "ptah-operator", "templates", "_helpers.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	chartDirectory := t.TempDir()
	templatesDirectory := filepath.Join(chartDirectory, "templates")
	if err := os.Mkdir(templatesDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(chartDirectory, "Chart.yaml"), []byte("apiVersion: v2\nname: ptah-operator\ntype: application\nversion: 0.1.0\nappVersion: 0.1.0\n"))
	writeTestFile(t, filepath.Join(templatesDirectory, "_helpers.tpl"), helpers)
	writeTestFile(t, filepath.Join(templatesDirectory, "probe.yaml"), []byte(predecessorProvenanceProbeTemplate))
	values, err := json.Marshal(map[string]any{
		"nameOverride":     "",
		"fullnameOverride": "",
		"coordination": map[string]any{
			"namespace": "",
		},
		"serviceAccount": map[string]any{
			"create": true,
			"name":   "",
		},
		"image": map[string]any{
			"repository":         "ghcr.io/stokaro/ptah-operator",
			"digest":             "sha256:" + managerDigest,
			"allowMutableTag":    false,
			"testIdentityDigest": "",
		},
		"fixture": fixture,
	})
	if err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(chartDirectory, "values.json")
	writeTestFile(t, valuesPath, values)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, helm,
		"template", releaseName, chartDirectory,
		"--namespace", releaseNamespace,
		"--values", valuesPath,
	)
	temporaryHome := t.TempDir()
	command.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(temporaryHome, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(temporaryHome, "config"),
		"HELM_DATA_HOME="+filepath.Join(temporaryHome, "data"),
	)
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if object.GetKind() == "ConfigMap" && object.GetName() == "provenance-result" {
			return object, nil
		}
	}
	return nil, fmt.Errorf("provenance result ConfigMap was not rendered")
}

func writeTestFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertPredecessorResult(t *testing.T, object *unstructured.Unstructured, wantName, wantSequence, wantUID, wantManaged string) {
	t.Helper()
	gotName, _, err := unstructured.NestedString(object.Object, "data", "previousName")
	if err != nil {
		t.Fatal(err)
	}
	gotSequence, _, err := unstructured.NestedString(object.Object, "data", "previousSequence")
	if err != nil {
		t.Fatal(err)
	}
	gotUID, _, err := unstructured.NestedString(object.Object, "data", "previousUID")
	if err != nil {
		t.Fatal(err)
	}
	gotManaged, _, err := unstructured.NestedString(object.Object, "data", "previousManaged")
	if err != nil {
		t.Fatal(err)
	}
	if gotName != wantName || gotSequence != wantSequence || gotUID != wantUID || gotManaged != wantManaged {
		t.Fatalf("previous controller identity = %q/%q/%q/%q, want %q/%q/%q/%q", gotName, gotSequence, gotUID, gotManaged, wantName, wantSequence, wantUID, wantManaged)
	}
}
