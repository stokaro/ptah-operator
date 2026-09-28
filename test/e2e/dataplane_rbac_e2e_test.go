//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// rbacPause is the manager's status verb while a row holds it back: which
// rule of the ClusterRole it is, and what that rule granted before.
type rbacPause struct {
	paused    bool
	apiGroups []string
	resources []string
	verbs     []string
}

// statusRuleIndex is the one rule of the ClusterRole that grants the
// PtahSchema status subresource.
func statusRuleIndex(role *rbacv1.ClusterRole) (int, error) {
	found := -1
	for index, rule := range role.Rules {
		if slices.Equal(rule.APIGroups, []string{"operator.ptah.run"}) && slices.Contains(rule.Resources, "ptahschemas/status") {
			if found >= 0 {
				return -1, errors.New("expected exactly one PtahSchema status rule")
			}
			found = index
		}
	}
	if found < 0 {
		return -1, errors.New("expected exactly one PtahSchema status rule")
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

func (d *dataPlane) controllerRole() (*rbacv1.ClusterRole, error) {
	role := &rbacv1.ClusterRole{}
	err := d.cluster.Client.Get(d.ctx, types.NamespacedName{Name: d.controllerName}, role)
	return role, err
}

// pauseStatusWrites takes the manager's status verbs away, leaving it get, so
// the controller cannot persist what it observes while a row changes the world
// under it. The generation stays where it was, and the persisted timer is
// what decides when the controller looks again after the verbs come back.
func (d *dataPlane) pauseStatusWrites() {
	d.t.Helper()
	if d.rbac.paused {
		d.fatalf("controller status-write RBAC is already paused")
	}
	role, err := d.controllerRole()
	d.check(err, "read ClusterRole %s", d.controllerName)
	index, err := statusRuleIndex(role)
	if err != nil {
		d.fatalf("could not identify the controller status-write ClusterRole rule: %v", err)
	}
	rule := role.Rules[index]
	pause := rbacPause{
		apiGroups: slices.Clone(rule.APIGroups), resources: slices.Clone(rule.Resources), verbs: slices.Clone(rule.Verbs),
	}
	patch, err := ruleVerbsPatch(index, pause.apiGroups, pause.resources, pause.verbs, []string{"get"})
	d.check(err, "encode the status-write pause")
	d.check(d.cluster.Client.Patch(d.ctx, role, client.RawPatch(types.JSONPatchType, patch)),
		"patch ClusterRole %s", d.controllerName)
	pause.paused = true
	d.rbac = pause
	live, err := d.controllerRole()
	if err != nil || len(live.Rules) <= index || !slices.Equal(live.Rules[index].Verbs, []string{"get"}) {
		d.fatalf("controller status-write RBAC did not enter the stale-approval barrier")
	}
	if !d.waitForStatusAuthorization(false) {
		d.fatalf("controller status writes remained authorized during the stale-approval barrier")
	}
}

// resumeStatusWrites gives the manager back exactly the verbs it had, on the
// rule that still names the same groups and resources, and waits until the
// API server authorizes them again. It is a no-op when nothing is paused, and
// it returns an error rather than ending the scenario, because the cleanup
// calls it too.
func (d *dataPlane) resumeStatusWrites() error {
	if !d.rbac.paused {
		return nil
	}
	role, err := d.controllerRole()
	if err != nil {
		return err
	}
	index := -1
	for candidate, rule := range role.Rules {
		if slices.Equal(rule.APIGroups, d.rbac.apiGroups) && slices.Equal(rule.Resources, d.rbac.resources) {
			if index >= 0 {
				return errors.New("status rule identity changed while writes were paused")
			}
			index = candidate
		}
	}
	if index < 0 {
		return errors.New("status rule identity changed while writes were paused")
	}
	if !slices.Equal(role.Rules[index].Verbs, []string{"get"}) {
		return fmt.Errorf("the paused status rule grants %v, not get alone", role.Rules[index].Verbs)
	}
	patch, err := ruleVerbsPatch(index, d.rbac.apiGroups, d.rbac.resources, []string{"get"}, d.rbac.verbs)
	if err != nil {
		return err
	}
	if err := d.cluster.Client.Patch(d.ctx, role, client.RawPatch(types.JSONPatchType, patch)); err != nil {
		return err
	}
	live, err := d.controllerRole()
	if err != nil {
		return err
	}
	if len(live.Rules) <= index || !slices.Equal(live.Rules[index].Verbs, d.rbac.verbs) {
		return errors.New("the status rule did not get its verbs back")
	}
	if !d.waitForStatusAuthorization(true) {
		return errors.New("controller status writes stayed unauthorized after the verbs came back")
	}
	d.rbac.paused = false
	return nil
}

// mustResumeStatusWrites is resumeStatusWrites inside a row.
func (d *dataPlane) mustResumeStatusWrites(reason string) {
	d.t.Helper()
	if err := d.resumeStatusWrites(); err != nil {
		d.fatalf("%s: %v", reason, err)
	}
}

// waitForStatusAuthorization asks the API server, as the manager, whether it
// may patch a schema's status, until the answer is the one expected or thirty
// seconds pass.
func (d *dataPlane) waitForStatusAuthorization(expected bool) bool {
	user := "system:serviceaccount:" + d.in.OperatorNamespace + ":" + d.controllerServiceAccount
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		allowed, err := d.cluster.CanI(d.ctx, user, authorizationv1.ResourceAttributes{
			Namespace: d.cluster.Namespace, Verb: "patch", Group: "operator.ptah.run",
			Resource: "ptahschemas", Subresource: "status",
		})
		if err == nil && allowed == expected {
			return true
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-d.ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return false
}
