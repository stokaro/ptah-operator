//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

func (a *alertingRun) apiServerMetrics() {
	a.t.Helper()
	nodes := &corev1.NodeList{}
	a.check(a.cluster.Client.List(a.ctx, nodes), "list the control-plane nodes")
	endpoints := &discoveryv1.EndpointSliceList{}
	a.check(a.cluster.Client.List(a.ctx, endpoints, client.InNamespace("default"),
		client.MatchingLabels{discoveryv1.LabelServiceName: "kubernetes"}), "read the API server endpoints")
	var err error
	a.apiServerTargets, err = alControlPlaneTargets(nodes.Items, endpoints.Items)
	a.check(err, "identify every API server metrics target")
	role, binding := alAPIMetricsRBAC(alMonitoringNamespace)
	for _, object := range []client.Object{role, binding} {
		a.mustCreate(object, "the API server metrics grant")
		a.apiMetricsObjects = append(a.apiMetricsObjects, object)
	}
}

func (a *alertingRun) waitForAPIServerTargets() {
	a.t.Helper()
	a.check(harness.Wait(a.ctx, "every API server metrics target to be healthy", alTimeout, alDeliveryPoll,
		func(ctx context.Context) (bool, string, error) {
			body, err := a.prometheus(ctx, "/api/v1/targets", nil)
			if err != nil {
				return false, "", err
			}
			return alAPIServerTargetsReady(body, a.apiServerTargets), "API server metrics targets are not all healthy", nil
		}), "wait for the API server metrics")
}

func (a *alertingRun) requireAPIServerTargets() {
	a.t.Helper()
	body, err := a.prometheus(a.ctx, "/api/v1/targets", nil)
	a.check(err, "read the API server scrape targets")
	if !alAPIServerTargetsReady(body, a.apiServerTargets) {
		a.fatalf("admission alert evidence lost a healthy API server scrape")
	}
}

// A counter series may first appear at one. Repeat the actual refused request
// across scrapes so increase() observes an increase, rather than assuming an
// absent series was previously zero. The detection target starts at the signed
// certificate expiry, including propagation delay in the webhook configuration.
// Background admission calls may encounter expiry before the first probe.
func (a *alertingRun) admissionFailureDelivered(from int, started time.Time) (int, alAdmissionHistory) {
	a.t.Helper()
	delivery, index := a.waitForDeliveryWithCheck(alMatch{status: "firing", alertName: alAdmissionAlert},
		"the admission failure alert", time.Until(started.Add(alDetectionSlack)), from, func() {
			a.requireAPIServerTargets()
			if !alExpiredApprovalError(a.approvalCertificateProbe(a.ctx)) {
				a.fatalf("the admission fault stopped producing expiry-specific refusals before alert delivery")
			}
		})
	if elapsed := delivery.ReceivedAt.Sub(started); elapsed < 0 || elapsed > alDetectionSlack {
		a.fatalf("the admission alert arrived after %s; want at most %s from certificate expiry", elapsed, alDetectionSlack)
	}
	if delivery.Labels["severity"] != "critical" || delivery.Annotations["runbook_url"] != a.runbookBase+"#webhook-certificate-lifecycle" ||
		!alRunbookAnchor(a.operationsPage(), "webhook-certificate-lifecycle") {
		a.fatalf("the admission alert omitted its critical severity or usable runbook link")
	}
	body, err := a.prometheus(a.ctx, "/api/v1/query", map[string]string{"query": alAdmissionRejections})
	a.check(err, "read the API server's rejection counter for the approval webhook")
	if !alAdmissionCounterIncreased(body) {
		a.fatalf("the approval webhook's calling_webhook_error counter did not increase")
	}
	reading := a.readAdmissionHistory(a.ctx, started, nil, "firing")
	if !reading.approvalIncreased || reading.lastLower.IsZero() {
		a.fatalf("the admission fault has no native approval-counter increase")
	}
	a.logf("PASS admission alert: certificateExpired=%s receivedAt=%s, with %d healthy API server scrapes and an increased approval rejection counter",
		started.Format(time.RFC3339Nano), delivery.ReceivedAt.Format(time.RFC3339Nano), len(a.apiServerTargets))
	return index, reading
}

func (a *alertingRun) readAdmissionHistory(ctx context.Context, started time.Time, previous *alAdmissionHistory, label string) alAdmissionHistory {
	a.t.Helper()
	a.requireAPIServerTargets()
	queriedAt := time.Now().UTC()
	query := func(expression string) []byte {
		body, err := a.prometheus(ctx, "/api/v1/query", map[string]string{
			"query": expression, "time": queriedAt.Format(time.RFC3339Nano),
		})
		a.check(err, "read the API servers' native admission history")
		return body
	}
	counters, up, durations := query(alAdmissionCounterHistory), query(alAdmissionUpHistory), query(alAdmissionScrapeHistory)
	reading, err := alReadAdmissionHistory(counters, up, durations, a.apiServerTargets, started, queriedAt, previous)
	a.check(err, "validate the API servers' native admission history")
	if label != "" {
		a.logf("admission native history %s: queriedAt=%s counters=%s up=%s scrapeDurations=%s", label,
			queriedAt.Format(time.RFC3339Nano), counters, up, durations)
	}
	return reading
}

func (a *alertingRun) admissionRecovered(from int, started, restored, healthyAt time.Time, previous alAdmissionHistory) {
	a.t.Helper()
	reading := a.readAdmissionHistory(a.ctx, started, &previous, "restored")
	var delivery alDelivery
	// The outer bound allows the evidence scrape after a delivered resolution.
	// The actual pass/fail deadline comes only from the native increment below.
	a.check(harness.Wait(a.ctx, "the five-minute admission window to resolve at the receiver",
		time.Until(healthyAt.Add(alAdmissionRecovery+alAdmissionSampleGap)), alDeliveryPoll,
		func(ctx context.Context) (bool, string, error) {
			if err := a.approvalCertificateProbe(ctx); err != nil {
				return false, "", fmt.Errorf("admission failed again during recovery: %w", err)
			}
			reading = a.readAdmissionHistory(ctx, started, &reading, "")
			if reading.lastLower.After(healthyAt) {
				return false, "", fmt.Errorf("a native calling-webhook rejection occurred after verified admission recovery")
			}
			log, err := a.deploymentLog(ctx, "alert-sink")
			if err != nil {
				return false, "", err
			}
			deliveries, err := alDeliveries(log)
			if err != nil {
				return false, "", err
			}
			candidate, _, found := alFirstDelivery(deliveries, from, alMatch{status: "resolved", alertName: alAdmissionAlert})
			if found {
				if candidate.ReceivedAt.Before(restored) || candidate.ReceivedAt.Before(reading.lastLower.Add(alAdmissionWindow)) ||
					candidate.ReceivedAt.After(reading.lastLower.Add(alAdmissionRecovery)) {
					return false, "", fmt.Errorf("admission resolved at %s outside the native last-rejection window [%s, %s]",
						candidate.ReceivedAt.Format(time.RFC3339Nano), reading.lastLower.Add(alAdmissionWindow).Format(time.RFC3339Nano),
						reading.lastLower.Add(alAdmissionRecovery).Format(time.RFC3339Nano))
				}
				if !alAdmissionResolutionWithinBounds(reading, candidate.ReceivedAt, restored) {
					return false, "not every API server has scraped through the delivered resolution", nil
				}
				if !a.noActiveAlerts(`ALERTS{alertname="` + alAdmissionAlert + `"}`) {
					return false, "Prometheus still has an active admission incident", nil
				}
				delivery = candidate
				return true, "", nil
			}
			if time.Now().After(reading.lastLower.Add(alAdmissionRecovery)) {
				return false, "", fmt.Errorf("the admission resolution missed its native last-rejection deadline")
			}
			return false, "the receiver has not resolved the five-minute admission window", nil
		}), "verify the admission alert's complete resolution interval")
	reading = a.readAdmissionHistory(a.ctx, started, &reading, "resolved")
	if reading.lastLower.After(healthyAt) || !alAdmissionResolutionWithinBounds(reading, delivery.ReceivedAt, restored) {
		a.fatalf("the final admission history invalidated the delivered recovery")
	}
	a.logf("PASS admission recovery: native last rejection lies in (%s, %s]; resolvedAt=%s; upper elapsed bound=%s, limit=%s",
		reading.lastLower.Format(time.RFC3339Nano), reading.lastUpper.Format(time.RFC3339Nano),
		delivery.ReceivedAt.Format(time.RFC3339Nano), delivery.ReceivedAt.Sub(reading.lastLower), alAdmissionRecovery)
}
