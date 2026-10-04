package e2e

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"

	rbacv1 "k8s.io/api/rbac/v1"
)

// rbacPause is the manager's write grant while a row holds it back: which
// rule of the ClusterRole it is, and what that rule granted before.
type rbacPause struct {
	paused    bool
	apiGroups []string
	resources []string
	verbs     []string
	heldVerbs []string
}

// statusRuleIndex is the one rule of the ClusterRole that grants the
// requested resource or status subresource.
func statusRuleIndex(role *rbacv1.ClusterRole, subresource string) (int, error) {
	found := -1
	for index, rule := range role.Rules {
		if slices.Equal(rule.APIGroups, []string{"operator.ptah.run"}) && slices.Contains(rule.Resources, subresource) {
			if found >= 0 {
				return -1, errors.New("expected exactly one controller status rule")
			}
			found = index
		}
	}
	if found < 0 {
		return -1, errors.New("expected exactly one controller status rule")
	}
	return found, nil
}

// ruleVerbsPatch is a JSON patch that replaces one rule's verbs, and applies
// only while the rule still names the groups and resources it was read with
// and still grants from.
func ruleVerbsPatch(index int, apiGroups, resources, from, to []string) ([]byte, error) {
	path := "/rules/" + strconv.Itoa(index)
	return json.Marshal([]map[string]any{
		{"op": "test", "path": path + "/apiGroups", "value": apiGroups},
		{"op": "test", "path": path + "/resources", "value": resources},
		{"op": "test", "path": path + "/verbs", "value": from},
		{"op": "replace", "path": path + "/verbs", "value": to},
	})
}

// Metadata writes can advance resourceVersion even while status is held.
// Preserve reads and unrelated verbs while withholding finalizer writes.
func metadataBarrierVerbs(verbs []string) ([]string, error) {
	if !slices.Contains(verbs, "patch") || slices.Contains(verbs, "*") {
		return nil, errors.New("metadata barrier requires an explicit patch grant without wildcards")
	}
	held := slices.DeleteFunc(slices.Clone(verbs), func(verb string) bool { return verb == "patch" || verb == "update" })
	if !slices.Contains(held, "get") {
		return nil, errors.New("metadata barrier must preserve reads")
	}
	return held, nil
}
