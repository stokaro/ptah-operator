//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// lostScrapeTarget breaks only the leader's metrics path. Kubernetes still
// sees the same ready managers and leader, and Prometheus still scrapes the
// follower. Its zero view-synced gauge cannot hide the loss of the leader's
// state behind a healthy aggregate or a successful scrape of the Service.
func (a *alertingRun) lostScrapeTarget() {
	a.t.Helper()
	if a.replicas < 2 {
		a.fatalf("a partial scrape failure needs at least two manager replicas")
	}
	a.waitForTargets()
	if !a.noActiveAlerts(`ALERTS{alertname="PtahOperatorUnresolvedViewNotSynced"}`) {
		a.fatalf("the unresolved-view alert was active before the scrape fault")
	}
	lease, pods := a.managerSnapshot()
	if !alSameManagers(lease, pods, lease, pods) {
		a.fatalf("the scrape fault needs a leader and distinct ready manager Pods")
	}
	pod := haLeaderPodName(haLeaseHolder(lease))
	var names []string
	for _, manager := range pods {
		names = append(names, manager.Name)
	}
	checkManagers := func() {
		currentLease, currentPods := a.managerSnapshot()
		if !alSameManagers(currentLease, currentPods, lease, pods) {
			a.fatalf("a manager restarted, lost readiness or changed leader during a scrape-only fault")
		}
	}
	config := &corev1.ConfigMap{}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: alMonitoringNamespace, Name: "prometheus"}, config),
		"read the original Prometheus configuration")
	original := config.Data["prometheus.yml"]
	if original != alPrometheusConfig(alMonitoringNamespace, a.in.OperatorNamespace, a.metricsService, a.apiServerTargets...) {
		a.fatalf("the Prometheus configuration changed before the scrape fault")
	}
	restored := false
	defer func() {
		if restored {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), alTimeout)
		defer cancel()
		if _, err := a.loadScrapeConfig(ctx, original, ""); err != nil {
			a.t.Errorf("restore Prometheus after the scrape fault: %v", err)
		}
	}()
	from := a.deliveryCount()
	loaded, err := a.loadScrapeConfig(a.ctx, alScrapeFaultConfig(original, pod), pod)
	a.check(err, "load the leader scrape fault")
	a.check(harness.Wait(a.ctx, "one failed leader scrape with every follower up", alDetectionSlack, alDeliveryPoll,
		func(ctx context.Context) (bool, string, error) {
			checkManagers()
			body, err := a.prometheus(ctx, "/api/v1/targets", nil)
			if err != nil {
				return false, "", err
			}
			return alOneTargetLost(body, int(a.replicas), pod, loaded), "waiting for the selected target's failed scrape", nil
		}), "observe the partial scrape failure")
	checkFault := func() {
		checkManagers()
		body, err := a.prometheus(a.ctx, "/api/v1/targets", nil)
		a.check(err, "read targets while the scrape fault holds")
		if !alOneTargetLost(body, int(a.replicas), pod, loaded) {
			a.fatalf("the fault no longer holds exactly one failed scrape and healthy followers")
		}
	}
	labels := map[string]string{"operator_namespace": a.in.OperatorNamespace, "operator_metrics_service": a.metricsService}
	delivery, index := a.waitForDeliveryWithCheck(alMatch{status: "firing", alertName: alViewNotSynced, labels: labels},
		"the unresolved-view alert after only the leader's scrape failed", alViewUnsyncedFor+alDetectionSlack, from, checkFault)
	reading := a.readScrapeHistory(names, pod, loaded, time.Time{}, "firing")
	if !alScrapeFailureDelivered(delivery, reading) {
		a.fatalf("the partial-scrape alert did not meet its native failure bound: firstFailure=%s startsAt=%s receivedAt=%s",
			reading.firstFailure, delivery.StartsAt, delivery.ReceivedAt)
	}
	if delivery.Labels["severity"] != "warning" || delivery.Annotations["runbook_url"] != a.runbookBase+"#unresolved-gauges" ||
		!alRunbookAnchor(a.operationsPage(), "unresolved-gauges") {
		a.fatalf("the partial-scrape alert omitted its warning severity or usable runbook link")
	}
	for _, label := range []string{"family", "operation", "resource", "pod", "job", "plan", "execution"} {
		if _, present := delivery.Labels[label]; present {
			a.fatalf("installation-wide scrape alert acquired label %s", label)
		}
	}
	a.logf("leader %s stayed ready while its scrape failed; all %d followers stayed up; first failed native scrape=%s firing received=%s", pod, a.replicas-1, reading.firstFailure, delivery.ReceivedAt)
	restoredAt, err := a.loadScrapeConfig(a.ctx, original, "")
	a.check(err, "restore the leader's metrics path")
	restored = true
	a.waitForTargets()
	resolved, _ := a.waitForDeliveryWithCheck(alMatch{status: "resolved", alertName: alViewNotSynced, labels: labels},
		"the partial-scrape alert's resolution", alDetectionSlack, index+1, checkManagers)
	a.check(harness.Wait(a.ctx, "all scrape histories through the receiver's resolution", 2*alScrapeInterval, time.Second,
		func(context.Context) (bool, string, error) {
			checkManagers()
			reading = a.readScrapeHistory(names, pod, loaded, restoredAt, "")
			return !reading.scrapedThrough.Before(resolved.ReceivedAt), "a manager has not scraped through resolution", nil
		}), "retain the complete partial-scrape recovery interval")
	reading = a.readScrapeHistory(names, pod, loaded, restoredAt, "resolved")
	queriedAt := time.Now().UTC()
	body, err := a.prometheus(a.ctx, "/api/v1/query", map[string]string{
		"query": fmt.Sprintf(`ptah_operator_unresolved_view_synced{job=%q,pod=%q}[%ds]`, alScrapeJob, pod, int(alAdmissionHistoryWindow/time.Second)),
		"time":  queriedAt.Format(time.RFC3339Nano),
	})
	a.check(err, "read the recovered leader's native synchronized-view history")
	a.check(alRecoveredScrapeSynced(body, pod, reading, queriedAt), "bind synchronized recovery to the original leader's first healthy scrape")
	a.logf("recovered leader view history: queriedAt=%s body=%s", queriedAt.Format(time.RFC3339Nano), body)
	if !alScrapeFailureCleared(delivery, resolved, reading) || !a.noActiveAlerts(`ALERTS{alertname="`+alViewNotSynced+`"}`) {
		a.fatalf("the partial-scrape incident did not resolve within 45 seconds of its native synchronized scrape")
	}
	a.logf("PASS leader scrape failure and recovery: leader=%s firstFailure=%s firingReceived=%s firstRecoveredScrape=%s resolvedReceived=%s",
		pod, reading.firstFailure, delivery.ReceivedAt, reading.recovered, resolved.ReceivedAt)

}

func (a *alertingRun) managerSnapshot() (*coordinationv1.Lease, []corev1.Pod) {
	a.t.Helper()
	lease := &coordinationv1.Lease{}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.in.OperatorNamespace, Name: leaderLeaseName}, lease),
		"read the manager leader Lease")
	pods := &corev1.PodList{}
	a.check(a.cluster.Client.List(a.ctx, pods, client.InNamespace(a.in.OperatorNamespace), client.MatchingLabels(a.managerLabels)),
		"read the manager Pods")
	if len(pods.Items) != int(a.replicas) {
		a.fatalf("found %d manager Pods, want %d", len(pods.Items), a.replicas)
	}
	return lease, pods.Items
}

// Reload until the running configuration contains the changed volume. Each
// successful reload is read back immediately; an API error fails the row,
// since retrying it could date the fault later than it actually started.
// The returned instant precedes the reload that first installed the change,
// so slow observation cannot make the alert's detection time look shorter.
func (a *alertingRun) loadScrapeConfig(ctx context.Context, config, pod string) (time.Time, error) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm := &corev1.ConfigMap{}
		if err := a.cluster.Client.Get(ctx, types.NamespacedName{Namespace: alMonitoringNamespace, Name: "prometheus"}, cm); err != nil {
			return err
		}
		cm.Data["prometheus.yml"] = config
		return a.cluster.Client.Update(ctx, cm)
	})
	if err != nil {
		return time.Time{}, err
	}
	var loaded time.Time
	err = harness.Wait(ctx, "Prometheus to load its updated scrape configuration", alTimeout, alDeliveryPoll,
		func(ctx context.Context) (bool, string, error) {
			before := time.Now()
			_, err := a.cluster.Clientset.CoreV1().RESTClient().Post().Namespace(alMonitoringNamespace).
				Resource("services").Name("http:prometheus:9090").SubResource("proxy").Suffix("-/reload").DoRaw(ctx)
			if err != nil {
				return false, "", fmt.Errorf("reload Prometheus: %w", err)
			}
			body, err := a.prometheus(ctx, "/api/v1/status/config", nil)
			if err != nil {
				return false, "", fmt.Errorf("read running Prometheus configuration: %w", err)
			}
			if !alScrapeFaultLoaded(body, pod) {
				return false, "the mounted configuration has not changed yet", nil
			}
			loaded = before
			return true, "", nil
		})
	return loaded, err
}

func (a *alertingRun) readScrapeHistory(pods []string, leader string, started, restored time.Time, label string) alScrapeHistory {
	a.t.Helper()
	at := time.Now().UTC()
	groups, body := a.historySnapshot(a.ctx, at, alScrapeJob, "", "up", "scrape_duration_seconds")
	history, err := alReadScrapeHistoryWithReloads(groups["up"], groups["scrape_duration_seconds"], pods, leader, started, at, started, restored)
	if label != "" || err != nil {
		a.logf("leader and follower scrape native history %s: loadedAt=%s restoredAt=%s queriedAt=%s snapshot=%s", label,
			started.Format(time.RFC3339Nano), restored.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), body)
	}
	a.check(err, "validate complete leader and follower scrape histories")
	return history
}
