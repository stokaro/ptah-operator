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
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
)

const predecessorProvenanceProbeTemplate = `{{- $fixture := default (dict) .Values.fixture -}}
{{- $principal := include "ptah-operator.previousControllerPrincipalFromObjectsJSON" (dict
      "root" .
      "guardPolicy" (get $fixture "guardPolicy")
      "guardBinding" (get $fixture "guardBinding")
      "deployment" (get $fixture "deployment")
      "clusterRoleBinding" (get $fixture "clusterRoleBinding")
      "coordinationRoleBinding" (get $fixture "coordinationRoleBinding")
      "serviceAccount" (get $fixture "serviceAccount")
      "predecessorGuard" (get $fixture "predecessorGuard")) | fromJson -}}
apiVersion: v1
kind: ConfigMap
metadata:
  name: provenance-result
data:
  previousName: {{ $principal.name | quote }}
  previousSequence: {{ $principal.releaseSequence | quote }}
  previousUID: {{ $principal.uid | quote }}
  previousManaged: {{ $principal.managed | quote }}
  previousManagerImage: {{ $principal.managerImage | quote }}
`

// The manager image the live predecessor Deployment runs. It differs from the
// candidate's so a rendered identity cannot mix the two up unnoticed.
const provenancePredecessorManagerImage = "ghcr.io/stokaro/ptah-operator@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestControllerPredecessorProvenanceRender(t *testing.T) {
	predecessorName := provenanceControllerServiceAccount(1)
	candidateName := provenanceControllerServiceAccount(2)

	t.Run("fresh install", func(t *testing.T) {
		result, err := renderPredecessorProvenance(t, 1, map[string]any{})
		if err != nil {
			t.Fatalf("fresh install provenance render: %v", err)
		}
		assertPredecessorResult(t, result, "", "0", "", "false", "")
	})

	t.Run("released predecessor", func(t *testing.T) {
		result, err := renderPredecessorProvenance(t, 2, releasedControllerFixture(predecessorName, "true"))
		if err != nil {
			t.Fatalf("released predecessor provenance render: %v", err)
		}
		assertPredecessorResult(t, result, predecessorName, "1", "service-account-uid", "true", provenancePredecessorManagerImage)
	})

	// Live ownership metadata can be copied onto any ServiceAccount, so the
	// identity mode is read from the guard the predecessor release wrote.
	t.Run("spoofed Helm ownership markers do not grant deletion authority", func(t *testing.T) {
		result, err := renderPredecessorProvenance(t, 2, releasedControllerFixture(predecessorName, "false"))
		if err != nil {
			t.Fatalf("spoofed ownership provenance render: %v", err)
		}
		assertPredecessorResult(t, result, predecessorName, "1", "service-account-uid", "false", provenancePredecessorManagerImage)
	})

	t.Run("released predecessor with external ServiceAccount", func(t *testing.T) {
		fixture := releasedControllerFixture(predecessorName, "false")
		serviceAccountMetadata := metadataOf(fixtureObject(fixture, "serviceAccount"))
		serviceAccountMetadata["annotations"] = map[string]any{}
		serviceAccountMetadata["labels"] = map[string]any{"app.kubernetes.io/managed-by": "platform-team"}
		result, err := renderPredecessorProvenance(t, 2, fixture)
		if err != nil {
			t.Fatalf("external predecessor ServiceAccount provenance render: %v", err)
		}
		assertPredecessorResult(t, result, predecessorName, "1", "service-account-uid", "false", provenancePredecessorManagerImage)
	})

	// Every release records its sequence on the controller it installs. A live
	// controller that records none was not installed by one, and no release
	// upgrades from it, the first one included.
	for _, sequence := range []int{1, 2} {
		t.Run(fmt.Sprintf("controller without a release identity at sequence %d", sequence), func(t *testing.T) {
			fixture := releasedControllerFixture(predecessorName, "true")
			delete(fixture, "predecessorGuard")
			deployment := fixtureObject(fixture, "deployment")
			metadataOf(deployment)["annotations"] = map[string]any{
				"meta.helm.sh/release-name":      releaseName,
				"meta.helm.sh/release-namespace": releaseNamespace,
			}
			deployment["spec"].(map[string]any)["template"].(map[string]any)["metadata"] = map[string]any{}
			_, err := renderPredecessorProvenance(t, sequence, fixture)
			if err == nil || !strings.Contains(err.Error(), "records no release identity and is not a supported predecessor") {
				t.Fatalf("render error = %v, want a refusal of the controller without a release identity", err)
			}
		})
	}

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
			name: "partial release identity",
			mutate: func(fixture map[string]any) {
				deployment := fixtureObject(fixture, "deployment")
				delete(metadataOf(deployment)["annotations"].(map[string]any), "operator.ptah.run/controller-state-version")
				delete(deployment["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any), "operator.ptah.run/controller-state-version")
			},
		},
		{
			name: "Pod template disagrees on the release sequence",
			mutate: func(fixture map[string]any) {
				deployment := fixtureObject(fixture, "deployment")
				deployment["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)["operator.ptah.run/release-sequence"] = "0"
			},
		},
		{
			name: "missing predecessor guard",
			mutate: func(fixture map[string]any) {
				delete(fixture, "predecessorGuard")
			},
		},
		{
			name: "predecessor guard names another controller",
			mutate: func(fixture map[string]any) {
				metadataOf(fixtureObject(fixture, "predecessorGuard"))["annotations"].(map[string]any)["operator.ptah.run/controller-service-account-name"] = "other-controller"
			},
		},
		{
			name: "same-sequence candidate checkpoint",
			mutate: func(fixture map[string]any) {
				deployment := fixtureObject(fixture, "deployment")
				metadataOf(deployment)["annotations"].(map[string]any)["operator.ptah.run/release-sequence"] = "2"
				deployment["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)["operator.ptah.run/release-sequence"] = "2"
				setPredecessorPrincipalName(fixture, candidateName)
			},
		},
		{
			name: "predecessor collides with hook ServiceAccount",
			mutate: func(fixture map[string]any) {
				setPredecessorPrincipalName(fixture, provenanceHookServiceAccount(2))
			},
		},
		{
			name: "predecessor collides with cleanup ServiceAccount",
			mutate: func(fixture map[string]any) {
				setPredecessorPrincipalName(fixture, provenanceCleanupServiceAccount(2))
			},
		},
		{
			name: "predecessor collides with quiesce identity",
			mutate: func(fixture map[string]any) {
				setPredecessorPrincipalName(fixture, provenanceQuiesceIdentity(2))
			},
		},
		{
			name: "predecessor collides with certificate ServiceAccount",
			mutate: func(fixture map[string]any) {
				setPredecessorPrincipalName(fixture, provenanceCertificateServiceAccount())
			},
		},
	}
	for _, test := range failureTests {
		t.Run(test.name, func(t *testing.T) {
			fixture := releasedControllerFixture(predecessorName, "true")
			test.mutate(fixture)
			if _, err := renderPredecessorProvenance(t, 2, fixture); err == nil {
				t.Fatal("Helm accepted invalid predecessor provenance")
			}
		})
	}
}

func TestRetainedControllerPrincipalProvenanceRender(t *testing.T) {
	predecessorName := provenanceControllerServiceAccount(1)
	noPredecessor := retainedPredecessor{}
	released := retainedPredecessor{name: predecessorName, sequence: "1", managerImage: provenancePredecessorManagerImage}

	for _, test := range []struct {
		name     string
		sequence int
		fixture  map[string]any
		want     retainedPredecessor
	}{
		{name: "failed fresh install retry", sequence: 1, want: noPredecessor, fixture: map[string]any{
			"guardPolicy":  retainedControllerPrincipalObject("ValidatingAdmissionPolicy", "-129", 1, noPredecessor),
			"guardBinding": retainedControllerPrincipalObject("ValidatingAdmissionPolicyBinding", "-128", 1, noPredecessor),
		}},
		{name: "complete upgrade retry", sequence: 2, want: released, fixture: map[string]any{
			"guardPolicy":  retainedControllerPrincipalObject("ValidatingAdmissionPolicy", "-129", 2, released),
			"guardBinding": retainedControllerPrincipalObject("ValidatingAdmissionPolicyBinding", "-128", 2, released),
		}},
		{name: "upgrade retry after policy hook only", sequence: 2, want: released, fixture: map[string]any{
			"guardPolicy": retainedControllerPrincipalObject("ValidatingAdmissionPolicy", "-129", 2, released),
		}},
		{name: "upgrade retry after binding hook only", sequence: 2, want: released, fixture: map[string]any{
			"guardBinding": retainedControllerPrincipalObject("ValidatingAdmissionPolicyBinding", "-128", 2, released),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := renderPredecessorProvenance(t, test.sequence, test.fixture)
			if err != nil {
				t.Fatalf("retained principal provenance render: %v", err)
			}
			wantUID := ""
			if test.want.name != "" {
				wantUID = "service-account-uid"
			}
			wantSequence := test.want.sequence
			if wantSequence == "" {
				wantSequence = "0"
			}
			assertPredecessorResult(t, result, test.want.name, wantSequence, wantUID, "false", test.want.managerImage)
		})
	}

	// A retained guard that names a predecessor also records the sequence it
	// ran at. One that names a predecessor at sequence 0 describes a release
	// that never existed.
	t.Run("named predecessor without a release sequence", func(t *testing.T) {
		guard := retainedControllerPrincipalObject("ValidatingAdmissionPolicy", "-129", 1, retainedPredecessor{name: "static-controller"})
		_, err := renderPredecessorProvenance(t, 1, map[string]any{"guardPolicy": guard})
		if err == nil || !strings.Contains(err.Error(), "inconsistent previous controller identity") {
			t.Fatalf("render error = %v, want an inconsistent previous controller identity refusal", err)
		}
	})

	t.Run("malformed immutable annotations", func(t *testing.T) {
		malformed := retainedControllerPrincipalObject("ValidatingAdmissionPolicy", "-129", 1, noPredecessor)
		metadataOf(malformed)["annotations"].(map[string]any)["operator.ptah.run/release-sequence"] = "01"
		if _, err := renderPredecessorProvenance(t, 1, map[string]any{"guardPolicy": malformed}); err == nil {
			t.Fatal("Helm accepted malformed retained principal annotations")
		}
	})

	t.Run("same-sequence checkpoint without immutable tuple", func(t *testing.T) {
		checkpoint := retainedControllerPrincipalObject("ValidatingAdmissionPolicy", "-129", 2, released)
		annotations := metadataOf(checkpoint)["annotations"].(map[string]any)
		delete(annotations, "operator.ptah.run/controller-service-account-name")
		delete(annotations, "operator.ptah.run/manager-image")
		if _, err := renderPredecessorProvenance(t, 2, map[string]any{"guardPolicy": checkpoint}); err == nil {
			t.Fatal("Helm accepted a retained same-sequence checkpoint without the immutable tuple")
		}
	})

	t.Run("policy and binding disagree", func(t *testing.T) {
		other := released
		other.name = "other-controller"
		if _, err := renderPredecessorProvenance(t, 2, map[string]any{
			"guardPolicy":  retainedControllerPrincipalObject("ValidatingAdmissionPolicy", "-129", 2, released),
			"guardBinding": retainedControllerPrincipalObject("ValidatingAdmissionPolicyBinding", "-128", 2, other),
		}); err == nil {
			t.Fatal("Helm accepted disagreeing retained principal annotations")
		}
	})
}

func TestRenderedControllerPrincipalGuardCarriesRetryTuple(t *testing.T) {
	objects := renderChart(t)
	guardName := "ptah-operator-service-account-origin-guard-v2-" + provenanceHookIdentityDigest(1, provenanceManagerImage())[:12]
	wantAnnotations := map[string]string{
		"operator.ptah.run/controller-state-version":                    strconv.FormatInt(int64(controllerstate.CurrentVersion), 10),
		"operator.ptah.run/admission-contract-version":                  "2",
		"operator.ptah.run/release-sequence":                            "1",
		"operator.ptah.run/manager-image":                               provenanceManagerImage(),
		"operator.ptah.run/hook-service-account-name":                   provenanceHookServiceAccount(1),
		"operator.ptah.run/controller-service-account-name":             provenanceControllerServiceAccount(1),
		"operator.ptah.run/controller-service-account-managed":          "true",
		"operator.ptah.run/previous-controller-service-account-name":    "",
		"operator.ptah.run/previous-controller-service-account-uid":     "",
		"operator.ptah.run/previous-controller-service-account-managed": "false",
		"operator.ptah.run/previous-controller-release-sequence":        "0",
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

// releasedControllerFixture is the live controller release sequence 1 left
// behind: a Deployment that records its release identity, the two stable
// bindings, the ServiceAccount they name, and the origin guard it retained,
// which records the identity mode.
func releasedControllerFixture(serviceAccountName, managed string) map[string]any {
	name := releaseName + "-ptah-operator"
	identity := map[string]any{
		"operator.ptah.run/release-sequence":         "1",
		"operator.ptah.run/controller-state-version": strconv.FormatInt(int64(controllerstate.CurrentVersion), 10),
	}
	deploymentMetadata := helmOwnedMetadata(name, releaseNamespace, "deployment-uid", map[string]any{
		"app.kubernetes.io/name":      "ptah-operator",
		"app.kubernetes.io/component": "controller",
	})
	for key, value := range identity {
		deploymentMetadata["annotations"].(map[string]any)[key] = value
	}
	templateAnnotations := map[string]any{}
	for key, value := range identity {
		templateAnnotations[key] = value
	}
	return map[string]any{
		"deployment": map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   deploymentMetadata,
			"spec": map[string]any{
				"template": map[string]any{
					"metadata": map[string]any{"annotations": templateAnnotations},
					"spec": map[string]any{
						"serviceAccountName": serviceAccountName,
						"containers": []any{map[string]any{
							"name":  "manager",
							"image": provenancePredecessorManagerImage,
						}},
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
		"predecessorGuard": map[string]any{
			"apiVersion": "admissionregistration.k8s.io/v1",
			"kind":       "ValidatingAdmissionPolicy",
			"metadata": map[string]any{
				"name": "ptah-operator-service-account-origin-guard-v2-" + provenanceHookIdentityDigest(1, provenancePredecessorManagerImage)[:12],
				"uid":  "predecessor-guard-uid",
				"annotations": map[string]any{
					"operator.ptah.run/release-name":                       releaseName,
					"operator.ptah.run/release-namespace":                  releaseNamespace,
					"operator.ptah.run/release-sequence":                   "1",
					"operator.ptah.run/manager-image":                      provenancePredecessorManagerImage,
					"operator.ptah.run/controller-service-account-name":    serviceAccountName,
					"operator.ptah.run/controller-service-account-managed": managed,
				},
			},
		},
	}
}

// retainedPredecessor is the previous controller a retained origin guard
// records. The zero value records none.
type retainedPredecessor struct {
	name         string
	sequence     string
	managerImage string
}

func retainedControllerPrincipalObject(kind, weight string, sequence int, previous retainedPredecessor) map[string]any {
	name := "ptah-operator-service-account-origin-guard-v2-" + provenanceHookIdentityDigest(sequence, provenanceManagerImage())[:12]
	previousUID := ""
	if previous.name != "" {
		previousUID = "service-account-uid"
	}
	previousSequence := previous.sequence
	if previousSequence == "" {
		previousSequence = "0"
	}
	return map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       kind,
		"metadata": map[string]any{
			"name": name,
			"uid":  "retained-guard-uid",
			"annotations": map[string]any{
				"helm.sh/hook":                            "pre-install,pre-upgrade",
				"helm.sh/hook-weight":                     weight,
				"helm.sh/resource-policy":                 "keep",
				"operator.ptah.run/rollout-guard-version": "1",
				"operator.ptah.run/release-name":          releaseName,
				"operator.ptah.run/release-namespace":     releaseNamespace,
				// The chart refuses a retained principal whose annotation tuple
				// disagrees with the compiled constant, so this cannot be a
				// literal: it is the version the chart renders today.
				"operator.ptah.run/controller-state-version":                    strconv.FormatInt(int64(controllerstate.CurrentVersion), 10),
				"operator.ptah.run/admission-contract-version":                  "2",
				"operator.ptah.run/release-sequence":                            strconv.Itoa(sequence),
				"operator.ptah.run/manager-image":                               provenanceManagerImage(),
				"operator.ptah.run/hook-service-account-name":                   provenanceHookServiceAccount(sequence),
				"operator.ptah.run/controller-service-account-name":             provenanceControllerServiceAccount(sequence),
				"operator.ptah.run/controller-service-account-managed":          "true",
				"operator.ptah.run/previous-controller-service-account-name":    previous.name,
				"operator.ptah.run/previous-controller-service-account-uid":     previousUID,
				"operator.ptah.run/previous-controller-service-account-managed": "false",
				"operator.ptah.run/previous-controller-release-sequence":        previousSequence,
				"operator.ptah.run/previous-controller-manager-image":           previous.managerImage,
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

func setPredecessorPrincipalName(fixture map[string]any, name string) {
	fixtureObject(fixture, "deployment")["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["serviceAccountName"] = name
	for _, key := range []string{"clusterRoleBinding", "coordinationRoleBinding"} {
		fixtureObject(fixture, key)["subjects"].([]any)[0].(map[string]any)["name"] = name
	}
	metadataOf(fixtureObject(fixture, "serviceAccount"))["name"] = name
	metadataOf(fixtureObject(fixture, "predecessorGuard"))["annotations"].(map[string]any)["operator.ptah.run/controller-service-account-name"] = name
}

func provenanceManagerImage() string {
	return "ghcr.io/stokaro/ptah-operator@sha256:" + managerDigest
}

func provenanceHookIdentityDigest(sequence int, managerImage string) string {
	identity := releaseNamespace + "\n" + releaseName + "\n" + strconv.Itoa(sequence) + "\n" + managerImage
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
}

func provenanceHookBase() string {
	base := releaseName + "-ptah-operator"
	if len(base) > 24 {
		base = base[:24]
	}
	return base
}

func provenanceHookServiceAccount(sequence int) string {
	return fmt.Sprintf("%s-crd-v%d-%s", provenanceHookBase(), sequence, provenanceHookIdentityDigest(sequence, provenanceManagerImage())[:12])
}

func provenanceCleanupServiceAccount(sequence int) string {
	return fmt.Sprintf("%s-cleanup-v%d-%s", provenanceHookBase(), sequence, provenanceHookIdentityDigest(sequence, provenanceManagerImage())[:12])
}

func provenanceQuiesceIdentity(sequence int) string {
	return fmt.Sprintf("%s-quiesce-v%d-%s", provenanceHookBase(), sequence, provenanceHookIdentityDigest(sequence, provenanceManagerImage())[:12])
}

func provenanceCertificateServiceAccount() string {
	base := releaseName + "-ptah-operator"
	if len(base) > 39 {
		base = base[:39]
	}
	return base + "-cert-rotator"
}

// provenanceControllerServiceAccount is the managed controller identity the
// chart gives the release at sequence when it runs the candidate image.
func provenanceControllerServiceAccount(sequence int) string {
	sourceBase := releaseName + "-ptah-operator"
	base := sourceBase
	if len(base) > 38 {
		base = base[:38]
	}
	// The chart folds the controller-state version into this digest, so the
	// literal that used to sit here made the computed principal disagree with
	// the rendered one the first time that version moved.
	identity := sourceBase + "\n" + strconv.FormatInt(int64(controllerstate.CurrentVersion), 10) + "\n" +
		provenanceHookIdentityDigest(sequence, provenanceManagerImage())
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
	return fmt.Sprintf("%s-v%d-%s", base, sequence, digest[:12])
}

// renderPredecessorProvenance renders the chart's predecessor discovery as
// the release at sequence would. The sequence is a constant of the chart, so
// the helper that holds it is replaced in the temporary copy.
func renderPredecessorProvenance(t *testing.T, sequence int, fixture map[string]any) (*unstructured.Unstructured, error) {
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
	const compiledSequence = `{{- define "ptah-operator.releaseSequence" -}}1{{- end -}}`
	if bytes.Count(helpers, []byte(compiledSequence)) != 1 {
		t.Fatal("compiled chart release sequence boundary changed")
	}
	helpers = bytes.Replace(helpers, []byte(compiledSequence),
		[]byte(fmt.Sprintf(`{{- define "ptah-operator.releaseSequence" -}}%d{{- end -}}`, sequence)), 1)
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
		"certificateRotation": map[string]any{
			"enabled": true,
		},
		"webhook": map[string]any{
			"existingSecret": "",
		},
		"image": map[string]any{
			"repository": "ghcr.io/stokaro/ptah-operator",
			"digest":     "sha256:" + managerDigest,
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
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, stderr.String())
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

func assertPredecessorResult(t *testing.T, object *unstructured.Unstructured, wantName, wantSequence, wantUID, wantManaged, wantManagerImage string) {
	t.Helper()
	got := map[string]string{}
	for _, key := range []string{"previousName", "previousSequence", "previousUID", "previousManaged", "previousManagerImage"} {
		value, _, err := unstructured.NestedString(object.Object, "data", key)
		if err != nil {
			t.Fatal(err)
		}
		got[key] = value
	}
	want := map[string]string{
		"previousName":         wantName,
		"previousSequence":     wantSequence,
		"previousUID":          wantUID,
		"previousManaged":      wantManaged,
		"previousManagerImage": wantManagerImage,
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("previous controller identity = %v, want %v", got, want)
		}
	}
}
