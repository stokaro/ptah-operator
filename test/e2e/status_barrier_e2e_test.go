//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// controllerStatusBarrier keeps admission available while the controller
// cannot persist an Apply claim. A row can approve a plan, change an input,
// and then let reconciliation resume without racing Job creation. The stored-
// state injection also holds metadata so finalizer writes cannot invalidate its
// resourceVersion precondition before the request reaches admission.
type controllerStatusBarrier struct {
	rbacPause
	cluster                         *harness.Cluster
	role, user, namespace, resource string
	metadataOnly                    bool
}

func (b *controllerStatusBarrier) target() (string, string) {
	if b.metadataOnly {
		return b.resource, ""
	}
	return b.resource + "/status", "status"
}

func (b *controllerStatusBarrier) readRole(ctx context.Context) (*rbacv1.ClusterRole, error) {
	role := &rbacv1.ClusterRole{}
	err := b.cluster.Client.Get(ctx, types.NamespacedName{Name: b.role}, role)
	return role, err
}

func (b *controllerStatusBarrier) pause(ctx context.Context) error {
	if b.paused {
		return errors.New("controller status writes are already paused")
	}
	role, err := b.readRole(ctx)
	if err != nil {
		return err
	}
	target, _ := b.target()
	index, err := statusRuleIndex(role, target)
	if err != nil {
		return err
	}
	rule := role.Rules[index]
	saved := rbacPause{paused: true, apiGroups: slices.Clone(rule.APIGroups),
		resources: slices.Clone(rule.Resources), verbs: slices.Clone(rule.Verbs), heldVerbs: []string{"get"}}
	if b.metadataOnly {
		saved.heldVerbs, err = metadataBarrierVerbs(rule.Verbs)
		if err != nil {
			return err
		}
	}
	patch, err := ruleVerbsPatch(index, rule.APIGroups, rule.Resources, rule.Verbs, saved.heldVerbs)
	if err != nil {
		return err
	}
	if err := b.cluster.Client.Patch(ctx, role, client.RawPatch(types.JSONPatchType, patch)); err != nil {
		return err
	}
	// Save the original rule before any read-back can fail, so cleanup can
	// restore it even when authorization propagation times out.
	b.rbacPause = saved
	live, err := b.readRole(ctx)
	if err != nil {
		return err
	}
	if len(live.Rules) <= index || !slices.Equal(live.Rules[index].Verbs, b.heldVerbs) {
		return errors.New("controller status-write rule did not enter the barrier")
	}
	return b.waitForAuthorization(ctx, false)
}

func (b *controllerStatusBarrier) resume(ctx context.Context) error {
	if !b.paused {
		return nil
	}
	role, err := b.readRole(ctx)
	if err != nil {
		return err
	}
	target, _ := b.target()
	index, err := statusRuleIndex(role, target)
	if err != nil {
		return err
	}
	rule := role.Rules[index]
	if !slices.Equal(rule.APIGroups, b.apiGroups) || !slices.Equal(rule.Resources, b.resources) {
		return errors.New("status rule identity changed while writes were paused")
	}
	if !slices.Equal(rule.Verbs, b.heldVerbs) && !slices.Equal(rule.Verbs, b.verbs) {
		return fmt.Errorf("paused status rule grants %v, want %v", rule.Verbs, b.heldVerbs)
	}
	// A previous restore may have succeeded before its read-back or
	// authorization probe failed. Cleanup can finish that restore safely.
	if !slices.Equal(rule.Verbs, b.verbs) {
		patch, err := ruleVerbsPatch(index, b.apiGroups, b.resources, b.heldVerbs, b.verbs)
		if err != nil {
			return err
		}
		if err := b.cluster.Client.Patch(ctx, role, client.RawPatch(types.JSONPatchType, patch)); err != nil {
			return err
		}
	}
	live, err := b.readRole(ctx)
	if err != nil {
		return err
	}
	if len(live.Rules) <= index || !slices.Equal(live.Rules[index].Verbs, b.verbs) {
		return errors.New("status rule did not get its verbs back")
	}
	if err := b.waitForAuthorization(ctx, true); err != nil {
		return err
	}
	b.paused = false
	return nil
}

func (b *controllerStatusBarrier) waitForAuthorization(ctx context.Context, expected bool) error {
	target, subresource := b.target()
	return harness.Wait(ctx, fmt.Sprintf("%s patch authorization to be %t", target, expected),
		30*time.Second, time.Second, func(ctx context.Context) (bool, string, error) {
			allowed, err := b.cluster.CanI(ctx, b.user, authorizationv1.ResourceAttributes{
				Namespace: b.namespace, Verb: "patch", Group: "operator.ptah.run",
				Resource: b.resource, Subresource: subresource,
			})
			if err != nil {
				return false, err.Error(), nil
			}
			return allowed == expected, fmt.Sprintf("allowed=%t", allowed), nil
		})
}
