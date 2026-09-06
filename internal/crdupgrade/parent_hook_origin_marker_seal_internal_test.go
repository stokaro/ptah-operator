package crdupgrade

// White-box testing required: the sealed marker transition is admitted by one
// validation of the compiled hook parent origin policy, and the request that
// exercises it is the crd-manager's own seal, whose builders and field manager
// are unexported.

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The crd-manager hook seals the convergence marker with its own principal,
// which holds no admission authority: its role names policies rather than
// granting create on the kind. The hook parent origin guard matches the
// marker by name, so this test evaluates its compiled validations against
// the exact seal transition with every authorizer decision forced to false,
// and requires each one to admit the write.
func TestParentHookOriginGuardAdmitsTheHookServiceAccountMarkerSeal(t *testing.T) {
	t.Parallel()

	guard := runtimePodGuardFixture()
	parent := NewParentWorkloadGuard(guard)
	policy := parent.hookJobOriginPolicy()
	convergence := NewAdmissionConvergenceGuard(guard)
	unsealed := convergence.unsealedMarker()
	unsealed.UID = "marker-uid"
	unsealed.ResourceVersion = "7"
	entries := predecessorRetirementExpectedEntries(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	for index := range entries {
		entries[index].UID = "uid"
		entries[index].Digest = strings.Repeat("d", 64)
	}
	encoded, err := encodePredecessorRetirementInventory(predecessorRetirementInventory{Version: PredecessorRetirementInventoryVersion, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	sealed := unsealed.DeepCopy()
	sealed.Data = cloneStringMap(unsealed.Data)
	sealed.Data[PredecessorRetirementInventoryDataKey] = encoded
	immutable := true
	sealed.Immutable = &immutable
	object := markerSealCELObject(t, sealed)
	oldObject := markerSealCELObject(t, unsealed)
	request := map[string]any{
		"operation": "UPDATE",
		"resource":  map[string]any{"group": "", "version": "v1", "resource": "configmaps"},
		"namespace": guard.ReleaseNamespace,
		"name":      unsealed.Name,
		"userInfo": map[string]any{
			"username": "system:serviceaccount:" + guard.ReleaseNamespace + ":" + guard.HookServiceAccountName,
			"groups":   []any{"system:serviceaccounts", "system:serviceaccounts:" + guard.ReleaseNamespace, "system:authenticated"},
		},
		"options": map[string]any{"fieldManager": predecessorRetirementSealManager},
	}
	if !evaluatePolicyMatchConditions(t, policy, object, oldObject, request, nil) {
		t.Fatal("the hook parent origin guard does not match the marker seal it must judge")
	}
	authorizer := regexp.MustCompile(`authorizer\.group\("[^"]*"\)\.resource\("[^"]*"\)(?:\.name\("[^"]*"\))?\.check\("[^"]*"\)\.allowed\(\)`)
	withoutAuthority := policy.DeepCopy()
	for index := range withoutAuthority.Spec.Validations {
		withoutAuthority.Spec.Validations[index].Expression = authorizer.ReplaceAllString(withoutAuthority.Spec.Validations[index].Expression, "false")
	}
	for index, allowed := range evaluatePolicyValidations(t, withoutAuthority, object, oldObject, request, nil) {
		if !allowed {
			t.Errorf("validation %d refused the hook principal's marker seal: %.200s", index, policy.Spec.Validations[index].Expression)
		}
	}

	foreign := map[string]any{
		"operation": "UPDATE",
		"resource":  map[string]any{"group": "", "version": "v1", "resource": "configmaps"},
		"namespace": guard.ReleaseNamespace,
		"name":      unsealed.Name,
		"userInfo":  map[string]any{"username": "system:serviceaccount:" + guard.ReleaseNamespace + ":default", "groups": []any{"system:authenticated"}},
		"options":   map[string]any{"fieldManager": predecessorRetirementSealManager},
	}
	results := evaluatePolicyValidations(t, withoutAuthority, object, oldObject, foreign, nil)
	refused := false
	for _, allowed := range results {
		refused = refused || !allowed
	}
	if !refused {
		t.Fatal("a principal without admission authority sealed the marker under a foreign ServiceAccount")
	}
}

func markerSealCELObject(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	return object
}
