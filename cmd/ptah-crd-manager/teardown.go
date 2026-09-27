package main

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

// runTeardownMode is the one pre-delete hook of an uninstall. It stops the
// runtime and then deletes, by exact name, what the release keeps outside
// Helm's own deletion. Helm deletes everything else in the release after it.
//
// The order is the point: the guards that fence the controller's writes go
// only once no controller Pod is left to write.
func runTeardownMode(ctx context.Context, clientset kubernetes.Interface, rollout *crdupgrade.RolloutGuard) error {
	if ctx == nil || clientset == nil || rollout == nil {
		return errors.New("teardown mode dependencies are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	admission := clientset.AdmissionregistrationV1()
	release := crdupgrade.NewReleaseTeardown(
		rollout,
		admission.ValidatingAdmissionPolicies(),
		admission.ValidatingAdmissionPolicyBindings(),
		clientset.CoreV1().ConfigMaps(rollout.ReleaseNamespace),
		clientset.CoreV1().Secrets(rollout.ReleaseNamespace),
	)
	// Check every object this hook is about to delete before stopping
	// anything, so an inventory it cannot delete fails without downtime.
	if err := release.Preflight(ctx); err != nil {
		return fmt.Errorf("preflight retained release inventory: %w", err)
	}
	if err := stopReleaseRuntime(ctx, clientset, rollout); err != nil {
		return err
	}
	if err := release.Teardown(ctx); err != nil {
		return fmt.Errorf("delete retained release inventory: %w", err)
	}
	return nil
}

// stopReleaseRuntime scales both runtime Deployments to zero and waits until no
// Pod in the namespace runs as a runtime identity. The retained rollout guards
// admit that stop only while the release activation records a drain toward
// the active sequence, so the drain is recorded first. An activation that is
// already gone means an earlier attempt deleted it after the guards that read
// it, and the stop needs no record.
func stopReleaseRuntime(ctx context.Context, clientset kubernetes.Interface, rollout *crdupgrade.RolloutGuard) error {
	_, err := clientset.CoreV1().ConfigMaps(rollout.ReleaseNamespace).Get(ctx, crdupgrade.ReleaseActivationName, metav1.GetOptions{})
	switch {
	case err == nil:
		if _, drainErr := rollout.BeginControllerCredentialDrain(ctx); drainErr != nil {
			return fmt.Errorf("record the runtime stop in the release activation: %w", drainErr)
		}
	case apierrors.IsNotFound(err):
	default:
		return fmt.Errorf("read the release activation before stopping the runtime: %w", err)
	}
	if err := rollout.Quiesce(ctx); err != nil {
		return fmt.Errorf("quiesce release runtime: %w", err)
	}
	if err := waitForNoProtectedRuntimePods(ctx, newWorkloadInventory(clientset, rollout), rollout.PollEvery); err != nil {
		return fmt.Errorf("wait for namespace-wide runtime Pod quiescence: %w", err)
	}
	return nil
}
