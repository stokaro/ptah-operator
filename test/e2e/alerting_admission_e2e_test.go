//go:build e2e

package e2e

import (
	"context"
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
func (a *alertingRun) admissionFailureDelivered(from int, started time.Time) int {
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
	a.logf("PASS admission alert: certificateExpired=%s receivedAt=%s, with %d healthy API server scrapes and an increased approval rejection counter",
		started.Format(time.RFC3339Nano), delivery.ReceivedAt.Format(time.RFC3339Nano), len(a.apiServerTargets))
	return index
}

func (a *alertingRun) admissionRecovered(from int, restored time.Time) {
	a.t.Helper()
	delivery, _ := a.waitForDeliveryWithCheck(alMatch{status: "resolved", alertName: alAdmissionAlert},
		"the admission failure alert's resolution", time.Until(restored.Add(alAdmissionRecovery)), from, func() {
			a.requireAPIServerTargets()
			a.check(a.approvalCertificateProbe(a.ctx), "admission must stay healthy while its alert resolves")
		})
	if elapsed := delivery.ReceivedAt.Sub(restored); elapsed < 0 || elapsed > alAdmissionRecovery {
		a.fatalf("the admission alert resolved after %s; want at most %s from certificate restoration", elapsed, alAdmissionRecovery)
	}
	a.logf("PASS admission alert resolved %s after certificate restoration", delivery.ReceivedAt.Sub(restored))
}
