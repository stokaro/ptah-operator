package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLifecycleProofNamespaceOK(t *testing.T) {
	t.Parallel()
	for _, accepted := range []string{"ptah-crd-proof", "a", "a1", strings.Repeat("a", 63)} {
		if err := lifecycleProofNamespaceOK(accepted); err != nil {
			t.Errorf("%q was refused: %v", accepted, err)
		}
	}
	for _, refused := range []string{"", "Upper", "-leading", "trailing-", "under_score", "dot.ted", strings.Repeat("a", 64)} {
		if lifecycleProofNamespaceOK(refused) == nil {
			t.Errorf("%q was accepted", refused)
		}
	}
}

func TestLifecycleKubernetesMajorMinor(t *testing.T) {
	t.Parallel()
	if got, err := lifecycleKubernetesMajorMinor("1.37.0"); err != nil || got != "1.37" {
		t.Fatalf("lifecycleKubernetesMajorMinor(1.37.0) = %q, %v", got, err)
	}
	for _, refused := range []string{"", "1.37", "v1.37.0", "1.37.0-rc.1", "1.37.x", " 1.37.0"} {
		if _, err := lifecycleKubernetesMajorMinor(refused); err == nil {
			t.Errorf("%q was read as an exact version", refused)
		}
	}
}

func TestLifecycleServerVersionMatches(t *testing.T) {
	t.Parallel()
	for _, accepted := range []string{"v1.37.0", "v1.37.0-rc.1", "v1.37.0-kind"} {
		if !lifecycleServerVersionMatches(accepted, "1.37.0") {
			t.Errorf("%q did not match 1.37.0", accepted)
		}
	}
	// A prefix that is a different patch release must not match.
	for _, refused := range []string{"1.37.0", "v1.37.01", "v1.37.00", "v1.37.1", "v1.370.0", ""} {
		if lifecycleServerVersionMatches(refused, "1.37.0") {
			t.Errorf("%q matched 1.37.0", refused)
		}
	}
}

func TestLifecycleCandidateCRDSchemaVersion(t *testing.T) {
	t.Parallel()
	crd, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "operator.ptah.run_ptahschemas.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The generated CRD is what the phase reads, so it has to carry a stamp.
	if version, err := lifecycleCandidateCRDSchemaVersion(crd); err != nil || version < 1 {
		t.Fatalf("the generated CRD reads as %d, %v", version, err)
	}
	if version, err := lifecycleCandidateCRDSchemaVersion([]byte("metadata:\n  annotations:\n    operator.ptah.run/crd-schema-version: \"12\"\n")); err != nil || version != 12 {
		t.Fatalf("a quoted stamp reads as %d, %v", version, err)
	}
	for name, document := range map[string]string{
		"no stamp":        "metadata:\n  annotations: {}\n",
		"zero":            "    operator.ptah.run/crd-schema-version: \"0\"\n",
		"leading zero":    "    operator.ptah.run/crd-schema-version: \"07\"\n",
		"not a number":    "    operator.ptah.run/crd-schema-version: \"seven\"\n",
		"empty":           "    operator.ptah.run/crd-schema-version: \"\"\n",
		"key in a value":  "    description: operator.ptah.run/crd-schema-version: 3\n",
		"negative number": "    operator.ptah.run/crd-schema-version: \"-1\"\n",
		// The first stamp line decides, as awk's exit did, even when a later
		// one would read.
		"no value first": "    operator.ptah.run/crd-schema-version:\n    operator.ptah.run/crd-schema-version: \"3\"\n",
	} {
		if _, err := lifecycleCandidateCRDSchemaVersion([]byte(document)); err == nil {
			t.Errorf("%s was read as a schema version", name)
		}
	}
}

func TestLifecycleProductionControllerImage(t *testing.T) {
	t.Parallel()
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	image, err := lifecycleProductionControllerImage([]byte(`{"image":{"repository":"registry.local/ptah-operator","digest":"` + digest + `"},"replicaCount":2}`))
	if err != nil || image != "registry.local/ptah-operator@"+digest {
		t.Fatalf("lifecycleProductionControllerImage = %q, %v", image, err)
	}
	for name, values := range map[string]string{
		"not JSON":                   `image: {}`,
		"no image":                   `{}`,
		"image a string":             `{"image":"registry.local/ptah-operator@` + digest + `"}`,
		"no repository":              `{"image":{"digest":"` + digest + `"}}`,
		"repository with a space":    `{"image":{"repository":"registry local","digest":"` + digest + `"}}`,
		"repository with a digest":   `{"image":{"repository":"registry.local/ptah@x","digest":"` + digest + `"}}`,
		"no digest":                  `{"image":{"repository":"registry.local/ptah-operator"}}`,
		"short digest":               `{"image":{"repository":"registry.local/ptah-operator","digest":"sha256:0123"}}`,
		"upper-case digest":          `{"image":{"repository":"registry.local/ptah-operator","digest":"sha256:` + strings.ToUpper(digest[7:]) + `"}}`,
		"digest a number":            `{"image":{"repository":"registry.local/ptah-operator","digest":7}}`,
		"mutable tag allowed":        `{"image":{"repository":"registry.local/ptah-operator","digest":"` + digest + `","allowMutableTag":false}}`,
		"test identity":              `{"image":{"repository":"registry.local/ptah-operator","digest":"` + digest + `","testIdentityDigest":"` + digest + `"}}`,
		"null test identity is kept": `{"image":{"repository":"registry.local/ptah-operator","digest":"` + digest + `","testIdentityDigest":null}}`,
		"empty test identity":        `{"image":{"repository":"registry.local/ptah-operator","digest":"` + digest + `","testIdentityDigest":""}}`,
	} {
		if _, err := lifecycleProductionControllerImage([]byte(values)); err == nil {
			t.Errorf("%s produced an image identity", name)
		}
	}
}

// renderWithHooks is the shape the chart's CRD hook template renders to, cut
// down to what the parser reads.
const renderWithHooks = `---
# Source: ptah-operator/templates/crd-upgrade.yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: "ptah-e2e-crd-reconcile"
  labels:
    app.kubernetes.io/component: crd-manager
  annotations:
    helm.sh/hook-weight: "0"
---
apiVersion: batch/v1
kind: Job
metadata:
  name: ptah-e2e-crd-preflight
  labels:
    app.kubernetes.io/component: crd-manager
  annotations:
    helm.sh/hook-weight: "-5"
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ptah-e2e-crd-manager
  labels:
    app.kubernetes.io/component: crd-manager
  annotations:
    helm.sh/hook-weight: "0"
`

func TestLifecycleRenderedHookJobNames(t *testing.T) {
	t.Parallel()
	if got := lifecycleRenderedHookJobNames([]byte(renderWithHooks), "crd-manager", "0"); !slices.Equal(got, []string{"ptah-e2e-crd-reconcile"}) {
		t.Fatalf("weight 0 = %v", got)
	}
	if got := lifecycleRenderedHookJobNames([]byte(renderWithHooks), "crd-manager", "-5"); !slices.Equal(got, []string{"ptah-e2e-crd-preflight"}) {
		t.Fatalf("weight -5 = %v", got)
	}
	// A ServiceAccount carrying the same labels is not a Job, and another
	// component is not the reconcile hook.
	if got := lifecycleRenderedHookJobNames([]byte(renderWithHooks), "controller", "0"); len(got) != 0 {
		t.Fatalf("another component = %v", got)
	}
	name, err := lifecycleReconcileHookName([]byte(renderWithHooks))
	if err != nil || name != "ptah-e2e-crd-reconcile" {
		t.Fatalf("lifecycleReconcileHookName = %q, %v", name, err)
	}
	for description, render := range map[string]string{
		"no reconcile Job":     strings.Replace(renderWithHooks, `helm.sh/hook-weight: "0"`+"\n---", `helm.sh/hook-weight: "1"`+"\n---", 1),
		"two reconcile Jobs":   renderWithHooks + strings.SplitN(renderWithHooks, "---\n", 3)[1],
		"an invalid name":      strings.Replace(renderWithHooks, `"ptah-e2e-crd-reconcile"`, `"Ptah_Reconcile"`, 1),
		"an overlong name":     strings.Replace(renderWithHooks, `"ptah-e2e-crd-reconcile"`, `"`+strings.Repeat("a", 64)+`"`, 1),
		"nothing rendered":     "",
		"a Job with no labels": "---\nkind: Job\nmetadata:\n  name: bare\n",
	} {
		if _, err := lifecycleReconcileHookName([]byte(render)); err == nil {
			t.Errorf("%s yielded a reconcile hook name", description)
		}
	}
}

func TestLifecycleEvidenceIsStableAndComplete(t *testing.T) {
	t.Parallel()
	object := map[string]any{
		"metadata": map[string]any{"uid": "u-1", "resourceVersion": "7", "name": "proof"},
		"spec":     map[string]any{"b": 2, "a": 1},
	}
	first, err := lifecycleObjectEvidence(object)
	if err != nil {
		t.Fatal(err)
	}
	// A status the object does not have reads as {}, as jq's // {} wrote it,
	// and a resourceVersion is not part of object evidence.
	if string(first) != `{"spec":{"a":1,"b":2},"status":{},"uid":"u-1"}` {
		t.Fatalf("object evidence = %s", first)
	}
	object["metadata"].(map[string]any)["resourceVersion"] = "8"
	if second, _ := lifecycleObjectEvidence(object); string(second) != string(first) {
		t.Fatal("a new resourceVersion changed the object evidence")
	}
	object["status"] = map[string]any{"phase": "Suspended"}
	if third, _ := lifecycleObjectEvidence(object); string(third) == string(first) {
		t.Fatal("a status change left the object evidence as it was")
	}

	crd := map[string]any{"metadata": map[string]any{"uid": "c-1", "resourceVersion": "3"}, "spec": map[string]any{"group": "g"}}
	evidence, err := lifecycleCRDEvidence(crd)
	if err != nil || string(evidence) != `{"annotations":{},"resourceVersion":"3","spec":{"group":"g"},"uid":"c-1"}` {
		t.Fatalf("CRD evidence = %s, %v", evidence, err)
	}

	deployments := []map[string]any{
		{"metadata": map[string]any{"name": "b", "uid": "2", "generation": int64(1)}, "spec": map[string]any{}},
		{"metadata": map[string]any{"name": "a", "uid": "1", "generation": int64(3), "labels": map[string]any{"x": "y"}}, "spec": map[string]any{}},
	}
	listed, err := lifecycleDeploymentEvidence(deployments)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"annotations":{},"generation":3,"labels":{"x":"y"},"name":"a","ownerReferences":[],"spec":{},"uid":"1"},` +
		`{"annotations":{},"generation":1,"labels":{},"name":"b","ownerReferences":[],"spec":{},"uid":"2"}]`
	if string(listed) != want {
		t.Fatalf("Deployment evidence = %s", listed)
	}
}

func TestUnstructuredInt64(t *testing.T) {
	t.Parallel()
	document := map[string]any{"status": map[string]any{"binding": map[string]any{
		"int": int64(2), "float": float64(3), "fraction": 1.5, "text": "4",
	}}}
	if value, found, err := unstructuredInt64(document, "status", "binding", "int"); err != nil || !found || value != 2 {
		t.Fatalf("int = %d, %v, %v", value, found, err)
	}
	if value, found, err := unstructuredInt64(document, "status", "binding", "float"); err != nil || !found || value != 3 {
		t.Fatalf("float = %d, %v, %v", value, found, err)
	}
	if _, found, _ := unstructuredInt64(document, "status", "absent"); found {
		t.Fatal("an absent field was found")
	}
	for _, field := range []string{"fraction", "text"} {
		if _, _, err := unstructuredInt64(document, "status", "binding", field); err == nil {
			t.Errorf("%s read as an integer", field)
		}
	}
}
