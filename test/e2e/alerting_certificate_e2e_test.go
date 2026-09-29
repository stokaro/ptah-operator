//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// certificateExpiry lets a real serving certificate expire while its rotator
// is stopped. The certificate keeps its CA, key and DNS names, so the API
// refusal must name expiry rather than an unrelated trust or identity fault.
func (a *alertingRun) certificateExpiry() {
	a.t.Helper()
	a.waitForTargets()
	a.waitForAPIServerTargets()
	if !a.noActiveAlerts(`ALERTS{alertname="PtahOperatorAdmissionUnavailable"}`) {
		a.fatalf("the admission alert was active before the certificate fault")
	}
	if !a.noActiveAlerts(`ALERTS{alertname="PtahOperatorWebhookCertificateExpiring"}`) {
		a.fatalf("the certificate alert was active before the expiry fault")
	}
	a.check(a.approvalCertificateProbe(a.ctx), "verify approval admission before the certificate fault")
	fault := a.certificateFault()
	defer fault.restore()
	a.check(fault.scaleRotator(a.ctx, 0), "stop certificate renewal for the expiry fault")
	a.check(harness.Wait(a.ctx, "the certificate rotator to stop", alTimeout, alDeliveryPoll,
		func(ctx context.Context) (bool, string, error) {
			pods := &corev1.PodList{}
			if err := a.cluster.Client.List(ctx, pods, client.InNamespace(a.in.OperatorNamespace),
				client.MatchingLabels(fault.rotator.Spec.Selector.MatchLabels)); err != nil {
				return false, "", err
			}
			return len(pods.Items) == 0, "the rotator still has a Pod", nil
		}), "wait for certificate renewal to stop")
	// Re-read after the last rotator Pod has stopped; an in-flight renewal may
	// have changed the Secret between discovery and scale-down.
	stoppedSecret := &corev1.Secret{}
	a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(fault.secret), stoppedSecret), "read the stopped rotator's certificate")
	if stoppedSecret.UID != fault.secret.UID {
		a.fatalf("the serving certificate Secret was replaced while the rotator stopped")
	}
	fault.secret = stoppedSecret
	var err error
	fault.shortLeaf, fault.expiry, err = alShortServingCertificate(fault.secret.Data, time.Now())
	a.check(err, "prepare the serving certificate expiry")
	originalLeaf, err := firstCertificate(fault.secret.Data["tls.crt"])
	a.check(err, "read the original serving certificate expiry")
	_, managerPods := a.managerSnapshot()
	podNames := make([]string, 0, len(managerPods))
	for _, pod := range managerPods {
		podNames = append(podNames, pod.Name)
	}
	from := a.deliveryCount()
	a.check(fault.writeLeaf(a.ctx, fault.shortLeaf), "install the short-lived serving certificate")
	a.check(a.waitForCertificateExpiry(podNames, fault.expiry, alCertificateProjection), "observe the short-lived certificate on every manager")
	warningAt := fault.expiry.Add(-alCertificateWarning)
	if !time.Now().Before(warningAt) {
		a.fatalf("the certificate was not projected to every manager before its warning threshold")
	}
	a.check(fault.writeBundle(a.ctx, alFreshCertificateBundle(fault.bundle, 1)), "require a fresh TLS connection before expiry")
	a.check(harness.Wait(a.ctx, "approval admission with the short-lived certificate before expiry", alDetectionSlack, alDeliveryPoll,
		func(ctx context.Context) (bool, string, error) {
			return a.approvalCertificateProbe(ctx) == nil, "the short-lived certificate has not admitted the probe", nil
		}), "verify admission before certificate expiry")
	delivered, index := a.waitForDelivery(alMatch{status: "firing", alertName: alCertificateAlert},
		"the certificate expiry warning", time.Until(warningAt.Add(alDetectionSlack)), from)
	if delay := delivered.ReceivedAt.Sub(warningAt); delay < 0 || delay > alDetectionSlack {
		a.fatalf("the certificate alert arrived %s after its signed expiry crossed the warning threshold; want 0 to %s", delay, alDetectionSlack)
	}
	if delivered.Labels["severity"] != "critical" || delivered.Annotations["runbook_url"] != a.runbookBase+"#webhook-certificate-lifecycle" ||
		!alRunbookAnchor(a.operationsPage(), "webhook-certificate-lifecycle") {
		a.fatalf("the certificate alert omitted its critical severity or usable runbook link")
	}
	a.logf("PASS certificate alert: signedExpiry=%s warningThreshold=%s receivedAt=%s; delivery preceded expiry by %s",
		fault.expiry.Format(time.RFC3339), warningAt.Format(time.RFC3339), delivered.ReceivedAt.Format(time.RFC3339Nano), fault.expiry.Sub(delivered.ReceivedAt))
	if remaining := time.Until(fault.expiry.Add(time.Second)); remaining > 0 {
		a.sleep(remaining)
	}
	// A keep-alive TLS connection established before expiry remains valid.
	// Changing only PEM whitespace gives the API server a new webhook client
	// and TLS transport without changing the trusted certificates. The pinned
	// apiserver keys its webhook client by CABundle and client-go keys its
	// transport by the raw CAData bytes.
	a.check(fault.writeBundle(a.ctx, alFreshCertificateBundle(fault.bundle, 2)), "require a fresh admission TLS connection")
	a.check(harness.Wait(a.ctx, "approval admission to refuse the expired serving certificate", time.Until(fault.expiry.Add(alDetectionSlack)), alDeliveryPoll,
		func(ctx context.Context) (bool, string, error) {
			return alExpiredApprovalError(a.approvalCertificateProbe(ctx)), "no expiry-specific admission refusal yet", nil
		}), "verify admission refusal after certificate expiry")
	a.logf("PASS approval admission refused the expired serving certificate")
	admissionIndex := a.admissionFailureDelivered(from, fault.expiry)
	restoredAt := time.Now()
	a.check(fault.writeLeaf(a.ctx, fault.secret.Data["tls.crt"]), "restore the valid serving certificate")
	a.check(a.waitForCertificateExpiry(podNames, originalLeaf.NotAfter, alCertificateProjection), "observe the restored certificate on every manager")
	a.check(fault.writeBundle(a.ctx, alFreshCertificateBundle(fault.bundle, 3)), "require a fresh TLS connection after restoration")
	a.check(harness.Wait(a.ctx, "approval admission after certificate restoration", alDetectionSlack, alDeliveryPoll,
		func(ctx context.Context) (bool, string, error) {
			return a.approvalCertificateProbe(ctx) == nil, "approval admission has not recovered", nil
		}), "verify admission recovered")
	resolved, _ := a.waitForDelivery(alMatch{status: "resolved", alertName: alCertificateAlert},
		"the restored serving certificate's resolution", alDetectionSlack, index+1)
	if elapsed := resolved.ReceivedAt.Sub(restoredAt); elapsed < 0 || elapsed > alCertificateProjection+alDetectionSlack {
		a.fatalf("the certificate alert resolved after %s; want at most %s from restoration", elapsed, alCertificateProjection+alDetectionSlack)
	}
	a.admissionRecovered(admissionIndex+1, restoredAt)
	a.check(fault.writeBundle(a.ctx, fault.bundle), "restore the admission trust bundle")
	a.check(fault.scaleRotator(a.ctx, *fault.rotator.Spec.Replicas), "restart certificate renewal")
	a.check(a.cluster.WaitForRollout(a.ctx, a.in.OperatorNamespace, fault.rotator.Name, alTimeout), "wait for certificate renewal to recover")
	fault.restored = true
	a.logf("PASS restored serving certificate cleared the receiver's alert and admitted the approval probe")
}

func (a *alertingRun) approvalCertificateProbe(ctx context.Context) error {
	approval := &ptahv1alpha1.PtahSchemaApproval{ObjectMeta: metav1.ObjectMeta{Name: "e2e-approval", Namespace: a.in.TestNamespace}}
	patch := client.RawPatch(types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"operator.ptah.run/certificate-alert-probe":"true"}}}`))
	return a.cluster.Client.Patch(ctx, approval, patch, client.DryRunAll, client.FieldOwner(harness.FieldOwner))
}

func (a *alertingRun) waitForCertificateExpiry(pods []string, expiry time.Time, timeout time.Duration) error {
	return harness.Wait(a.ctx, "every manager to report the expected certificate expiry", timeout, alDeliveryPoll,
		func(ctx context.Context) (bool, string, error) {
			body, err := a.prometheus(ctx, "/api/v1/query", map[string]string{"query": alCertificateMetric})
			if err != nil {
				return false, "", err
			}
			return alCertificateExpiries(body, pods, expiry), "manager certificate metrics have not all changed", nil
		})
}

type alCertificateFault struct {
	a                 *alertingRun
	rotator           *appsv1.Deployment
	secret            *corev1.Secret
	webhooks          *admissionregistrationv1.ValidatingWebhookConfiguration
	bundle, shortLeaf []byte
	expiry            time.Time
	restored          bool
}

func (a *alertingRun) certificateFault() *alCertificateFault {
	a.t.Helper()
	fault := &alCertificateFault{a: a, secret: &corev1.Secret{}}
	manager := &appsv1.Deployment{}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.in.OperatorNamespace, Name: a.manager}, manager), "read the manager's certificate volume")
	secretName, err := webhookCertificateSecret(manager)
	a.check(err, "locate the serving certificate Secret")
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.in.OperatorNamespace, Name: secretName}, fault.secret), "read the serving certificate Secret")
	rotators := &appsv1.DeploymentList{}
	a.check(a.cluster.Client.List(a.ctx, rotators, client.InNamespace(a.in.OperatorNamespace), client.MatchingLabels{
		"app.kubernetes.io/instance": a.in.HelmRelease, "app.kubernetes.io/component": "certificate-rotation",
	}), "locate the certificate rotator")
	if len(rotators.Items) != 1 || rotators.Items[0].Spec.Replicas == nil || *rotators.Items[0].Spec.Replicas < 1 ||
		rotators.Items[0].Spec.Selector == nil || len(rotators.Items[0].Spec.Selector.MatchLabels) == 0 {
		a.fatalf("the release does not have exactly one active certificate rotator with a Pod selector")
	}
	fault.rotator = rotators.Items[0].DeepCopy()
	webhooks := &admissionregistrationv1.ValidatingWebhookConfigurationList{}
	a.check(a.cluster.Client.List(a.ctx, webhooks, client.MatchingLabels{"app.kubernetes.io/instance": a.in.HelmRelease}), "locate approval admission")
	for i := range webhooks.Items {
		for _, hook := range webhooks.Items[i].Webhooks {
			if hook.Name != alApprovalWebhook {
				continue
			}
			if fault.webhooks != nil || len(hook.ClientConfig.CABundle) == 0 || !alApprovalProbeWebhook(hook) {
				a.fatalf("the release does not have exactly one approval webhook with a trust bundle")
			}
			fault.webhooks = webhooks.Items[i].DeepCopy()
			fault.bundle = bytes.Clone(hook.ClientConfig.CABundle)
		}
	}
	if fault.webhooks == nil {
		a.fatalf("the approval webhook is missing")
	}
	return fault
}

func (f *alCertificateFault) scaleRotator(ctx context.Context, replicas int32) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &appsv1.Deployment{}
		if err := f.a.cluster.Client.Get(ctx, client.ObjectKeyFromObject(f.rotator), current); err != nil {
			return err
		}
		if current.UID != f.rotator.UID {
			return errors.New("the certificate rotator was replaced during the fault")
		}
		current.Spec.Replicas = ptr.To(replicas)
		return f.a.cluster.Client.Update(ctx, current)
	})
}

func (f *alCertificateFault) writeLeaf(ctx context.Context, leaf []byte) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &corev1.Secret{}
		if err := f.a.cluster.Client.Get(ctx, client.ObjectKeyFromObject(f.secret), current); err != nil {
			return err
		}
		if current.UID != f.secret.UID {
			return errors.New("the serving certificate Secret was replaced during the fault")
		}
		for _, field := range []string{"ca.crt", "ca.key", "tls.key"} {
			if !bytes.Equal(current.Data[field], f.secret.Data[field]) {
				return errors.New("the serving certificate identity changed during the fault")
			}
		}
		if !bytes.Equal(current.Data["tls.crt"], f.secret.Data["tls.crt"]) && !bytes.Equal(current.Data["tls.crt"], f.shortLeaf) {
			return errors.New("the serving certificate changed outside the expiry fault")
		}
		current.Data["tls.crt"] = bytes.Clone(leaf)
		return f.a.cluster.Client.Update(ctx, current)
	})
}

func (f *alCertificateFault) writeBundle(ctx context.Context, bundle []byte) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &admissionregistrationv1.ValidatingWebhookConfiguration{}
		if err := f.a.cluster.Client.Get(ctx, client.ObjectKeyFromObject(f.webhooks), current); err != nil {
			return err
		}
		if current.UID != f.webhooks.UID {
			return errors.New("the approval webhook configuration was replaced during the fault")
		}
		for i := range current.Webhooks {
			hook := &current.Webhooks[i]
			if hook.Name == alApprovalWebhook {
				known := false
				for generation := range 4 {
					known = known || bytes.Equal(hook.ClientConfig.CABundle, alFreshCertificateBundle(f.bundle, generation))
				}
				if !known {
					return errors.New("the approval trust bundle changed outside the expiry fault")
				}
				hook.ClientConfig.CABundle = bytes.Clone(bundle)
				return f.a.cluster.Client.Update(ctx, current)
			}
		}
		return errors.New("the approval webhook disappeared during the fault")
	})
}

func (f *alCertificateFault) restore() {
	if f.restored {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), alTimeout)
	defer cancel()
	// Attempt every restoration even if the previous one fails. No Secret
	// values or raw API errors are printed by failure cleanup.
	for _, action := range []struct {
		name  string
		apply func() error
	}{
		{"serving certificate", func() error { return f.writeLeaf(ctx, f.secret.Data["tls.crt"]) }},
		{"approval trust bundle", func() error { return f.writeBundle(ctx, f.bundle) }},
		{"certificate rotator", func() error { return f.scaleRotator(ctx, *f.rotator.Spec.Replicas) }},
	} {
		if action.apply() != nil {
			f.a.t.Errorf("restore %s after certificate expiry failed", action.name)
		}
	}
}
