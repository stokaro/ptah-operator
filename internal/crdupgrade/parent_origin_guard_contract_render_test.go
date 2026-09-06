package crdupgrade_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

// TestParentOriginGuardManifestMatchesItsContract renders the chart with a
// probe that publishes the in-template contract each hook-origin guard is
// compared against on a retry, and requires it to equal the spec the chart
// actually emits for that guard.
//
// The retained-guard check deepEquals a live object, which was created from
// the emitted manifest, with the contract dict built in the template. The
// lifecycle render fixture cannot see a divergence between the two, because it
// feeds the helper the emitted spec on both sides. One closing parenthesis
// missing from the contract's copy of a validation refused every upgrade
// retried after a failed attempt at render time.
func TestParentOriginGuardManifestMatchesItsContract(t *testing.T) {
	t.Parallel()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for parent-origin guard contract render tests")
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	chart := t.TempDir()
	if err := os.CopyFS(chart, os.DirFS(filepath.Join(repository, "charts", "ptah-operator"))); err != nil {
		t.Fatal(err)
	}
	guard := filepath.Join(chart, "templates", "parent-workload-guard.yaml")
	probe, err := os.OpenFile(guard, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	const probeName = "parent-origin-guard-contract-probe"
	if _, err := probe.WriteString("\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + probeName + "\ndata:\n" +
		"  hookOriginSpec: {{ toJson $hookOriginSpec | quote }}\n  hookPodSpec: {{ toJson $hookPodSpec | quote }}\n"); err != nil {
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	objects := parentOriginRenderObjects(t, helm, chart)
	var contracts map[string]any
	for key, object := range objects {
		if object["kind"] == "ConfigMap" && object["metadata"].(map[string]any)["name"] == probeName {
			contracts = object["data"].(map[string]any)
			t.Logf("contract probe rendered as %s", key)
		}
	}
	if contracts == nil {
		t.Fatal("contract probe ConfigMap was not rendered")
	}
	guards := map[string]string{
		"hookOriginSpec": crdupgrade.ParentHookJobOriginGuardPolicyName(parentOriginNamespace, parentOriginRelease),
		"hookPodSpec":    crdupgrade.ParentHookPodOriginGuardPolicyName(parentOriginNamespace, parentOriginRelease),
	}
	for contractKey, name := range guards {
		t.Run(name, func(t *testing.T) {
			object := objects["ValidatingAdmissionPolicy//"+name]
			if object == nil {
				t.Fatalf("rendered chart is missing ValidatingAdmissionPolicy %s", name)
			}
			var contract map[string]any
			if err := json.Unmarshal([]byte(contracts[contractKey].(string)), &contract); err != nil {
				t.Fatal(err)
			}
			emitted := object["spec"].(map[string]any)
			for key := range contract {
				if !reflect.DeepEqual(contract[key], emitted[key]) {
					t.Errorf("spec.%s: the emitted manifest and the retained-guard contract differ", key)
				}
			}
			for key := range emitted {
				if _, present := contract[key]; !present {
					t.Errorf("spec.%s: emitted but absent from the retained-guard contract", key)
				}
			}
			if !reflect.DeepEqual(contract, emitted) {
				t.Errorf("a guard the chart emits would be refused by its own retained-guard check")
			}
		})
	}
}
