//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

func (a *alertingRun) stalledOperation() {
	a.createHeldNamespace()
	for _, family := range []string{"schema", "migration"} {
		a.stalledFamily(family)
	}
}

func (a *alertingRun) heldResource(family string) (client.Object, error) {
	var object client.Object = &ptahv1.PtahSchema{}
	if family == "migration" {
		object = &ptahv1.PtahMigration{}
	}
	err := a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: alStalledNamespace, Name: alStalledSchema}, object)
	return object, err
}

func (a *alertingRun) heldWorkload(claim alStalledClaim, podUID types.UID) (*batchv1.Job, *corev1.Pod, bool) {
	a.t.Helper()
	job := &batchv1.Job{}
	err := a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: claim.namespace, Name: claim.jobName}, job)
	if apierrors.IsNotFound(err) && podUID == "" {
		return nil, nil, false
	}
	a.check(err, "read the original held %s Job", claim.family)
	if !alStalledJobMatches(job, claim) {
		a.fatalf("the held %s Job no longer matches its exact operation and owner", claim.family)
	}
	pods := &corev1.PodList{}
	a.check(a.cluster.Client.List(a.ctx, pods, client.InNamespace(claim.namespace)), "list held Resolve Pods")
	var owned []*corev1.Pod
	for index := range pods.Items {
		pod := &pods.Items[index]
		for _, owner := range pod.OwnerReferences {
			if owner.UID == claim.jobUID {
				owned = append(owned, pod)
				break
			}
		}
	}
	if len(owned) == 0 && podUID == "" {
		return job, nil, false
	}
	if len(owned) != 1 || !alStalledPodMatches(owned[0], claim, podUID) {
		a.fatalf("the held %s Job has a missing, replaced or additional executor", claim.family)
	}
	return job, owned[0], true
}

func (a *alertingRun) stalledFamily(family string) {
	a.t.Helper()
	query := fmt.Sprintf(`ALERTS{alertname=%q,family=%q,operation="Resolve"}`, alOperationStall, family)
	if !a.noActiveAlerts(query) {
		a.fatalf("a %s Resolve was already stalled before this fault", family)
	}
	from := a.deliveryCount()
	a.check(a.cluster.Client.Create(a.ctx, &unstructured.Unstructured{Object: alHeldResource(alStalledNamespace, family)},
		client.FieldOwner(harness.FieldOwner), client.FieldValidation("Strict")), "create the held %s", family)
	var claim alStalledClaim
	var podUID types.UID
	var podCreated time.Time
	a.check(harness.Wait(a.ctx, "a dispatched Resolve held off every node", alTimeout, alClaimPoll,
		func(context.Context) (bool, string, error) {
			object, err := a.heldResource(family)
			if err != nil {
				return false, "", err
			}
			reading := alStalledReading(object)
			if !reading.ready() {
				return false, "the Resolve has no exact dispatched claim", nil
			}
			_, pod, found := a.heldWorkload(reading, "")
			if !found {
				return false, "the held Job has no Pod yet", nil
			}
			if !alStalledPodHeld(pod) {
				return false, "", fmt.Errorf("the %s executor was not held before its alert", family)
			}
			claim, podUID = reading, pod.UID
			podCreated = pod.CreationTimestamp.Time
			return true, "", nil
		}), "bind the exact stalled %s claim and Pod", family)
	checkHeld := func() {
		object, err := a.heldResource(family)
		a.check(err, "read the held %s claim", family)
		if alStalledReading(object) != claim {
			a.fatalf("the original %s claim changed during the stall", family)
		}
		_, pod, _ := a.heldWorkload(claim, podUID)
		if !alStalledPodHeld(pod) {
			a.fatalf("the original %s Pod ran before the scheduling gate opened", family)
		}
	}
	labels := map[string]string{"family": family, "operation": "Resolve", "operator_namespace": a.in.OperatorNamespace, "operator_metrics_service": a.metricsService}
	threshold := claim.started.Add(alStalledAfter)
	firing, index := a.waitForDeliveryWithCheck(alMatch{status: "firing", alertName: alOperationStall, labels: labels},
		"the held "+family+" Resolve notification", time.Until(threshold.Add(alDetectionSlack)), from, checkHeld)
	if !alStalledDelivered(firing, claim.started) || firing.StartsAt.Before(podCreated) ||
		firing.Labels["severity"] != "warning" || firing.Annotations["runbook_url"] != a.runbookBase+"#resource-state" ||
		!alRunbookAnchor(a.operationsPage(), "resource-state") {
		a.fatalf("the %s stalled notification missed its native claim bound, severity or runbook", family)
	}
	for _, label := range []string{"resource", "pod", "job", "plan", "execution"} {
		if _, present := firing.Labels[label]; present {
			a.fatalf("the bounded stalled alert acquired label %s", label)
		}
	}
	a.gateOpened = true
	a.check(a.setGate(a.ctx, "open"), "release the original held %s executor", family)
	var finished time.Time
	a.check(harness.Wait(a.ctx, "the original Resolve's native terminal failure", alTimeout, alClaimPoll,
		func(context.Context) (bool, string, error) {
			job, pod, _ := a.heldWorkload(claim, podUID)
			object, err := a.heldResource(family)
			if err != nil {
				return false, "", err
			}
			reading := alStalledReading(object)
			if !claim.sameResource(reading) {
				return false, "", fmt.Errorf("the %s resource changed during the held operation", family)
			}
			if pod.Status.Phase != corev1.PodFailed || job.Status.Failed == 0 || reading.phase != "Failed" {
				return false, "the original Pod, Job or resource has not recorded failure", nil
			}
			finished, err = alStalledFinished(pod, claim, podUID)
			return err == nil, "", err
		}), "observe the original %s Resolve failure", family)
	resolved, _ := a.waitForDelivery(alMatch{status: "resolved", alertName: alOperationStall, labels: labels},
		"the original "+family+" stalled incident resolution", time.Until(finished.Add(alDetectionSlack)), index+1)
	if !alStalledCleared(firing, resolved, finished) || !a.noActiveAlerts(query) {
		a.fatalf("the %s resolution did not match the incident or the original Pod's terminal bound", family)
	}
	object, err := a.heldResource(family)
	a.check(err, "read the finished %s before suspending it", family)
	a.check(a.cluster.Client.Patch(a.ctx, object, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"suspend":true}}`)),
		client.FieldOwner(harness.FieldOwner)), "suspend the finished %s", family)
	a.check(a.setGate(a.ctx, ""), "close the Resolve scheduling gate")
	a.gateOpened = false
	a.logf("PASS %s stalled Resolve: operation=%s jobUID=%s podUID=%s claimedAt=%s firingReceived=%s executorFinishedAt=%s resolvedReceived=%s",
		family, claim.id, claim.jobUID, podUID, claim.started.Format(time.RFC3339Nano), firing.ReceivedAt.Format(time.RFC3339Nano),
		finished.Format(time.RFC3339Nano), resolved.ReceivedAt.Format(time.RFC3339Nano))
}
