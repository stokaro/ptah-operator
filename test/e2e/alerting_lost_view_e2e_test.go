//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// Keep replacements Pending while the original managers disappear. Native
// alert evaluations and scrape samples date both ends of the incident;
// waiting for deletion or rollout cannot extend either delivery allowance.
func (a *alertingRun) lostView() {
	a.t.Helper()
	a.waitForTargets()
	query := `ALERTS{alertname="` + alViewNotSynced + `"}`
	if !a.noActiveAlerts(query) {
		a.fatalf("the view alert was already active before removing managers")
	}
	lease, original := a.managerSnapshot()
	if !alSameManagers(lease, original, lease, original) {
		a.fatalf("view loss requires healthy original managers and their leader")
	}
	leader := haLeaderPodName(haLeaseHolder(lease))
	var originalNames []string
	for _, pod := range original {
		originalNames = append(originalNames, pod.Name)
	}
	monitor := a.negativeMonitorIdentity()
	checkMonitor := func() {
		if !maps.Equal(monitor, a.negativeMonitorIdentity()) {
			a.fatalf("a monitoring process changed during complete manager loss")
		}
		body, err := a.prometheus(a.ctx, "/api/v1/rules", nil)
		a.check(err, "read the rule health during complete manager loss")
		if !alRulesLoaded(body) {
			a.fatalf("a required rule stopped evaluating during complete manager loss")
		}
	}
	nodes := &corev1.NodeList{}
	a.check(a.cluster.Client.List(a.ctx, nodes), "list nodes before the view fault")
	if len(nodes.Items) == 0 {
		a.fatalf("the view fault found no nodes")
	}
	for _, node := range nodes.Items {
		if node.Spec.Unschedulable {
			a.fatalf("node %s was already cordoned before the view fault", node.Name)
		}
	}
	for _, node := range nodes.Items {
		a.cordoned = append(a.cordoned, node.Name)
		a.check(a.setUnschedulable(a.ctx, node.Name, true), "cordon %s", node.Name)
	}
	from := a.deliveryCount()
	started := time.Now().UTC()
	a.check(a.removeManagers(), "remove the original manager Pods")
	checkLost := func() {
		checkMonitor()
		currentNodes := &corev1.NodeList{}
		a.check(a.cluster.Client.List(a.ctx, currentNodes), "retain the cordoned node inventory")
		if len(currentNodes.Items) != len(nodes.Items) {
			a.fatalf("the node inventory changed during view loss")
		}
		for _, node := range currentNodes.Items {
			found := false
			for _, old := range nodes.Items {
				found = found || node.UID == old.UID && node.Name == old.Name
			}
			if !found || !node.Spec.Unschedulable {
				a.fatalf("a node changed or became schedulable during view loss")
			}
		}
		pods := &corev1.PodList{}
		a.check(a.cluster.Client.List(a.ctx, pods, client.InNamespace(a.in.OperatorNamespace), client.MatchingLabels(a.managerLabels)), "read manager replacements during view loss")
		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodPending || pod.Spec.NodeName != "" {
				a.fatalf("a replacement manager escaped the scheduling fault")
			}
		}
	}
	checkLost()
	labels := map[string]string{"operator_namespace": a.in.OperatorNamespace, "operator_metrics_service": a.metricsService}
	firing, index := a.waitForDeliveryWithCheck(alMatch{status: "firing", alertName: alViewNotSynced, labels: labels}, "the lost-view notification", alViewUnsyncedFor+alDetectionSlack, from, checkLost)
	var loss alLostViewHistory
	a.check(harness.Wait(a.ctx, "the full native view-loss history through delivery", 2*alScrapeInterval, time.Second, func(context.Context) (bool, string, error) {
		checkLost()
		at := time.Now().UTC()
		alerts := a.lostViewQuery(query+fmt.Sprintf("[%ds]", int(alAdmissionHistoryWindow/time.Second)), at, "firing-alerts")
		up := a.lostViewQuery(alLostViewManagerQuery("up", originalNames), at, "original-targets")
		var err error
		loss, err = alLostViewFiring(alerts, up, originalNames, leader, started, at)
		if err != nil {
			return false, "", err
		}
		return !loss.through.Before(firing.ReceivedAt), "waiting for the next native rule evaluation", nil
	}), "retain the native loss interval")
	if !alLostViewDelivered(firing, loss) {
		a.fatalf("view-loss notification missed the native transition or first failed scrape deadline")
	}
	if firing.Labels["severity"] != "warning" || firing.Annotations["runbook_url"] != a.runbookBase+"#unresolved-gauges" || !alRunbookAnchor(a.operationsPage(), "unresolved-gauges") {
		a.fatalf("the lost-view notification has no correct severity or usable runbook")
	}
	for _, label := range []string{"family", "operation", "resource", "pod", "job", "plan", "execution"} {
		if _, present := firing.Labels[label]; present {
			a.fatalf("the installation-wide view alert acquired label %s", label)
		}
	}
	restored := time.Now().UTC()
	for len(a.cordoned) > 0 {
		name := a.cordoned[0]
		a.check(a.setUnschedulable(a.ctx, name, false), "uncordon %s", name)
		a.cordoned = a.cordoned[1:]
	}
	a.check(a.cluster.WaitForRollout(a.ctx, a.in.OperatorNamespace, a.manager, alTimeout), "restore the manager rollout")
	a.waitForTargets()
	newLease, replacements := a.managerSnapshot()
	newLeader := haLeaderPodName(haLeaseHolder(newLease))
	var names []string
	for _, pod := range replacements {
		for _, old := range original {
			if pod.UID == old.UID || pod.Name == old.Name {
				a.fatalf("view recovery reused an original manager identity")
			}
		}
		names = append(names, pod.Name)
	}
	checkRecovery := func() {
		checkMonitor()
		currentLease, currentPods := a.managerSnapshot()
		if !alSameManagers(currentLease, currentPods, newLease, replacements) {
			a.fatalf("replacement managers or leadership changed during view recovery")
		}
	}
	checkRecovery()
	resolved, _ := a.waitForDeliveryWithCheck(alMatch{status: "resolved", alertName: alViewNotSynced, labels: labels}, "the lost-view resolution", alDetectionSlack, index+1, checkRecovery)
	var recovery alScrapeHistory
	a.check(harness.Wait(a.ctx, "complete replacement scrapes through receiver resolution", 2*alScrapeInterval, time.Second, func(context.Context) (bool, string, error) {
		checkRecovery()
		at := time.Now().UTC()
		view := a.lostViewQuery(alLostViewManagerQuery("ptah_operator_unresolved_view_synced", names), at, "recovered-view")
		up := a.lostViewQuery(alLostViewManagerQuery("up", names), at, "replacement-targets")
		durations := a.lostViewQuery(alLostViewManagerQuery("scrape_duration_seconds", names), at, "replacement-durations")
		var err error
		recovery, err = alLostViewRecovery(view, up, durations, names, newLeader, restored, at)
		if err != nil {
			return false, "", err
		}
		return !recovery.scrapedThrough.Before(resolved.ReceivedAt), "waiting for every replacement scrape through resolution", nil
	}), "retain the exact synchronized recovery")
	if !alScrapeFailureCleared(firing, resolved, recovery) || !a.noActiveAlerts(query) {
		a.fatalf("view loss did not resolve within 45 seconds of the new leader's first synchronized scrape")
	}
	a.logf("PASS all-manager view loss: originalLeader=%s firstFailedScrape=%s viewTransition=%s firingReceived=%s replacementLeader=%s synchronizedScrape=%s resolvedReceived=%s", leader, loss.firstFailure, loss.transition, firing.ReceivedAt, newLeader, recovery.recovered, resolved.ReceivedAt)
}

func alLostViewManagerQuery(metric string, pods []string) string {
	quoted := make([]string, len(pods))
	for i, pod := range pods {
		quoted[i] = regexp.QuoteMeta(pod)
	}
	return fmt.Sprintf(`%s{job=%q,pod=~%q}[%ds]`, metric, alScrapeJob, strings.Join(quoted, "|"), int(alAdmissionHistoryWindow/time.Second))
}

func (a *alertingRun) lostViewQuery(query string, at time.Time, label string) []byte {
	body, err := a.prometheus(a.ctx, "/api/v1/query", map[string]string{"query": query, "time": at.Format(time.RFC3339Nano)})
	a.check(err, "read native lost-view history %s", label)
	a.logf("lost-view native history %s: queriedAt=%s body=%s", label, at.Format(time.RFC3339Nano), body)
	return body
}
