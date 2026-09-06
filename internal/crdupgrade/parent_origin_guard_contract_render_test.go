package crdupgrade_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

// parentOriginContractProbe renders a copy of the chart whose
// parent-workload-guard template additionally publishes, as JSON, the
// in-template dicts named by keys. Those dicts are what the retained-guard
// checks compare live objects against; nothing else renders them.
func parentOriginContractProbe(t *testing.T, helm, repository string, keys ...string) (map[string]map[string]any, map[string]string) {
	t.Helper()
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
	var document strings.Builder
	document.WriteString("\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + probeName + "\ndata:\n")
	for _, key := range keys {
		document.WriteString("  " + key + ": {{ toJson $" + key + " | quote }}\n")
	}
	if _, err := probe.WriteString(document.String()); err != nil {
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	objects := parentOriginRenderObjects(t, helm, chart)
	contracts := map[string]string{}
	for _, object := range objects {
		if object["kind"] != "ConfigMap" || object["metadata"].(map[string]any)["name"] != probeName {
			continue
		}
		for key, value := range object["data"].(map[string]any) {
			contracts[key] = value.(string)
		}
	}
	if len(contracts) != len(keys) {
		t.Fatalf("contract probe published %d of %d dicts", len(contracts), len(keys))
	}
	return objects, contracts
}

func assertContractMatchesObject(t *testing.T, contract string, object map[string]any) {
	t.Helper()
	var want map[string]any
	if err := json.Unmarshal([]byte(contract), &want); err != nil {
		t.Fatal(err)
	}
	got := object["spec"].(map[string]any)
	for key := range want {
		if !reflect.DeepEqual(want[key], got[key]) {
			t.Errorf("spec.%s: the emitted manifest and the retained-object contract differ", key)
		}
	}
	for key := range got {
		if _, present := want[key]; !present {
			t.Errorf("spec.%s: emitted but absent from the retained-object contract", key)
		}
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("an object the chart emits would be refused by its own retained-object check")
	}
}

// TestParentOriginGuardManifestMatchesItsContract requires the contract each
// current hook-origin guard is compared against on a retry to equal the spec
// the chart emits for that guard.
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
	objects, contracts := parentOriginContractProbe(t, helm, repository, "hookOriginSpec", "hookPodSpec")
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
			assertContractMatchesObject(t, contracts[contractKey], object)
		})
	}
}

// TestLegacyParentOriginGuardContractsMatchTheManagedChart requires the v1
// hook-origin guard contracts, which the retained-legacy check compares a
// managed predecessor's live guards against, to equal the guards the last
// managed release actually created: the chart at parentOriginManagedRevision.
//
// Those contracts describe frozen historical objects. Written from today's
// helpers, they followed the dyn() field-access change and stopped matching
// the guards that release emitted, so an upgrade from it was refused at render
// as differing from every crash-reachable v1 state.
func TestLegacyParentOriginGuardContractsMatchTheManagedChart(t *testing.T) {
	t.Parallel()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for parent-origin guard contract render tests")
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	_, contracts := parentOriginContractProbe(t, helm, repository,
		"legacyHookOriginSpec", "legacyHookPodOriginSpec", "legacyHookOriginBindingSpec", "legacyHookPodOriginBindingSpec")
	managed := parentOriginRenderObjects(t, helm, parentOriginHistoricalChart(t, repository, parentOriginManagedRevision))
	jobName := strings.Replace(crdupgrade.ParentHookJobOriginGuardPolicyName(parentOriginNamespace, parentOriginRelease), "-v2-", "-v1-", 1)
	podName := strings.Replace(crdupgrade.ParentHookPodOriginGuardPolicyName(parentOriginNamespace, parentOriginRelease), "-v2-", "-v1-", 1)
	tests := []struct {
		contract string
		kind     string
		name     string
	}{
		{contract: "legacyHookOriginSpec", kind: "ValidatingAdmissionPolicy", name: jobName},
		{contract: "legacyHookPodOriginSpec", kind: "ValidatingAdmissionPolicy", name: podName},
		{contract: "legacyHookOriginBindingSpec", kind: "ValidatingAdmissionPolicyBinding", name: jobName},
		{contract: "legacyHookPodOriginBindingSpec", kind: "ValidatingAdmissionPolicyBinding", name: podName},
	}
	for _, test := range tests {
		t.Run(test.kind+"/"+test.name, func(t *testing.T) {
			object := managed[test.kind+"//"+test.name]
			if object == nil {
				t.Fatalf("managed chart %s does not emit %s %s", parentOriginManagedRevision, test.kind, test.name)
			}
			assertContractMatchesObject(t, contracts[test.contract], object)
		})
	}
}
