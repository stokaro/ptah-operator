//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

func (a *alertingRun) viewReadFailures() {
	for _, resource := range []string{"ptahschemas", "ptahmigrations"} {
		a.viewReadFailure(resource)
	}
}

func (a *alertingRun) viewReadFailure(resource string) {
	a.t.Helper()
	a.waitForTargets()
	if !a.noActiveAlerts(`ALERTS{alertname="` + alViewReadAlert + `"}`) {
		a.fatalf("the read-failure alert was already active before refusing %s", resource)
	}
	lease, pods := a.managerSnapshot()
	if !alSameManagers(lease, pods, lease, pods) {
		a.fatalf("the read fault needs a stable leader and healthy followers")
	}
	leader := haLeaderPodName(haLeaseHolder(lease))
	var names []string
	account := pods[0].Spec.ServiceAccountName
	for _, pod := range pods {
		if account == "" || pod.Spec.ServiceAccountName != account {
			a.fatalf("manager Pods do not share the recorded ServiceAccount")
		}
		names = append(names, pod.Name)
		a.logf("read-fault manager identity: pod=%s uid=%s", pod.Name, pod.UID)
	}
	asManager, err := a.cluster.As(rest.ImpersonationConfig{UserName: "system:serviceaccount:" + a.in.OperatorNamespace + ":" + account})
	a.check(err, "create the actual manager-identity list probe")
	list := func(ctx context.Context) error {
		var objects client.ObjectList = &ptahv1.PtahSchemaList{}
		if resource == "ptahmigrations" {
			objects = &ptahv1.PtahMigrationList{}
		}
		return asManager.List(ctx, objects)
	}
	waitList := func(ctx context.Context, allowed bool) error {
		return harness.Wait(ctx, fmt.Sprintf("manager %s list allowed=%t", resource, allowed), 30*time.Second, time.Second,
			func(ctx context.Context) (bool, string, error) {
				err := list(ctx)
				if err != nil && !apierrors.IsForbidden(err) {
					return false, "", err
				}
				return (err == nil) == allowed, fmt.Sprintf("list allowed=%t", err == nil), nil
			})
	}
	a.check(waitList(a.ctx, true), "establish the allowed manager list control")
	original := &rbacv1.ClusterRole{}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Name: a.manager}, original), "read the installed manager grant")
	index, verbs, err := alViewReadRule(original, resource)
	a.check(err, "select the exact %s list grant", resource)
	if original.UID == "" {
		a.fatalf("the manager grant has no identity")
	}
	held := original.DeepCopy()
	held.Rules[index].Verbs = verbs
	setRules := func(ctx context.Context, from, to *rbacv1.ClusterRole) error {
		patch, err := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": original.UID},
			{"op": "test", "path": "/rules", "value": from.Rules},
			{"op": "replace", "path": "/rules", "value": to.Rules},
		})
		if err != nil {
			return err
		}
		return a.cluster.Client.Patch(ctx, original.DeepCopy(), client.RawPatch(types.JSONPatchType, patch))
	}
	restored := false
	defer func() {
		if restored {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		live := &rbacv1.ClusterRole{}
		if err := a.cluster.Client.Get(ctx, client.ObjectKeyFromObject(original), live); err != nil {
			a.t.Errorf("read the manager grant during fault cleanup: %v", err)
			return
		}
		if live.UID != original.UID {
			a.t.Error("the manager grant was replaced during the read fault")
			return
		}
		if !reflect.DeepEqual(live.Rules, original.Rules) {
			if err := setRules(ctx, held, original); err != nil {
				a.t.Errorf("restore the exact manager list grant: %v", err)
				return
			}
		}
		if err := waitList(ctx, true); err != nil {
			a.t.Errorf("verify restored list access: %v", err)
		}
	}()
	checkManagers := func() {
		currentLease, currentPods := a.managerSnapshot()
		if !alSameManagers(currentLease, currentPods, lease, pods) {
			a.fatalf("a manager or leadership changed during the state-read fault")
		}
		body, err := a.prometheus(a.ctx, "/api/v1/targets", nil)
		a.check(err, "read manager scrape targets during the API fault")
		if !alTargetsReady(body, int(a.replicas)) {
			a.fatalf("a manager's metrics endpoint became unavailable")
		}
	}
	// Healthy targets can have only their first successful scrape. Establish
	// the history this fault measures without depending on an earlier scenario
	// to keep Prometheus running long enough. Other invalid histories fail.
	a.check(harness.Wait(a.ctx, "a complete pre-fault state-read baseline", alDetectionSlack, time.Second,
		func(context.Context) (bool, string, error) {
			checkManagers()
			at := time.Now().UTC()
			_, err := a.queryViewHistory(names, leader, at, at, "")
			if errors.Is(err, errAlViewReadBaseline) {
				return false, "waiting for two scrape intervals before the state-read fault", nil
			}
			return err == nil, "waiting for a complete state-read baseline", err
		}), "establish the manager state-read baseline before changing RBAC")
	from := a.deliveryCount()
	started := time.Now().UTC()
	a.check(setRules(a.ctx, original, held), "refuse the manager's actual %s list", resource)
	a.check(waitList(a.ctx, false), "observe an actual Forbidden list as the manager")
	delivery, deliveredAt := a.waitForDeliveryWithCheck(alMatch{status: "firing", alertName: alViewReadAlert, labels: map[string]string{"operator_namespace": a.in.OperatorNamespace, "operator_metrics_service": a.metricsService}},
		"the read-failure notification for "+resource, alDetectionSlack, from, checkManagers)
	reading := a.readViewHistory(names, leader, started, resource+"-firing")
	if reading.firstLower.IsZero() || delivery.StartsAt.Before(started) || delivery.ReceivedAt.Before(started) || delivery.ReceivedAt.After(reading.firstLower.Add(alDetectionSlack)) {
		a.fatalf("read-failure delivery at %s missed the native first-failure bound %s", delivery.ReceivedAt, reading.firstLower)
	}
	if delivery.Labels["operator_namespace"] != a.in.OperatorNamespace || delivery.Labels["operator_metrics_service"] != a.metricsService ||
		delivery.Labels["severity"] != "warning" || delivery.Annotations["runbook_url"] != a.runbookBase+"#unresolved-gauges" ||
		!alRunbookAnchor(a.operationsPage(), "unresolved-gauges") {
		a.fatalf("the read-failure alert omitted its warning severity or usable runbook")
	}
	for _, label := range []string{"family", "operation", "resource", "pod", "job", "plan", "execution"} {
		if _, present := delivery.Labels[label]; present {
			a.fatalf("installation-wide read alert acquired label %s", label)
		}
	}
	restoredAt := time.Now().UTC()
	a.check(setRules(a.ctx, held, original), "restore the original %s list grant", resource)
	a.check(waitList(a.ctx, true), "verify actual list access after restoration")
	live := &rbacv1.ClusterRole{}
	a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(original), live), "read back the restored manager grant")
	if live.UID != original.UID || !reflect.DeepEqual(live.Rules, original.Rules) {
		a.fatalf("the restored grant differs from the original")
	}
	restored = true
	resolved, _ := a.waitForDeliveryWithCheck(alMatch{status: "resolved", alertName: alViewReadAlert, labels: map[string]string{"operator_namespace": a.in.OperatorNamespace, "operator_metrics_service": a.metricsService}},
		"the read-failure resolution for "+resource, alViewReadWindow+alDetectionSlack, deliveredAt+1, checkManagers)
	// Retain scrapes through the receiver's actual resolution, not just through
	// the earlier evaluation that decided to send it.
	a.check(harness.Wait(a.ctx, "all manager histories through the delivered read-failure resolution", 2*alScrapeInterval, time.Second,
		func(context.Context) (bool, string, error) {
			checkManagers()
			reading = a.readViewHistory(names, leader, started, "")
			return !reading.scrapedThrough.Before(resolved.ReceivedAt), "a manager has not scraped through the resolution", nil
		}), "retain the complete read-failure recovery interval")
	reading = a.readViewHistory(names, leader, started, resource+"-resolved")
	if reading.lastLower.IsZero() || !resolved.StartsAt.Equal(delivery.StartsAt) || resolved.ReceivedAt.Before(restoredAt) ||
		resolved.ReceivedAt.Before(reading.lastLower.Add(alViewReadWindow)) ||
		resolved.ReceivedAt.After(reading.lastLower.Add(alViewReadWindow+alDetectionSlack)) ||
		!a.noActiveAlerts(`ALERTS{alertname="`+alViewReadAlert+`"}`) {
		a.fatalf("read-failure resolution at %s is outside the native last-failure window [%s, %s]",
			resolved.ReceivedAt, reading.lastLower.Add(alViewReadWindow), reading.lastLower.Add(alViewReadWindow+alDetectionSlack))
	}
	a.logf("PASS %s state-read alert: leader=%s grantUID=%s firstFailureLower=%s firingReceived=%s lastFailureInterval=(%s,%s] resolvedReceived=%s",
		resource, leader, original.UID, reading.firstLower.Format(time.RFC3339Nano), delivery.ReceivedAt.Format(time.RFC3339Nano),
		reading.lastLower.Format(time.RFC3339Nano), reading.lastUpper.Format(time.RFC3339Nano), resolved.ReceivedAt.Format(time.RFC3339Nano))
}

func (a *alertingRun) readViewHistory(pods []string, leader string, started time.Time, label string) alViewReadHistory {
	a.t.Helper()
	at := time.Now().UTC()
	history, err := a.queryViewHistory(pods, leader, started, at, label)
	a.check(err, "validate complete manager state-read histories")
	return history
}

func (a *alertingRun) queryViewHistory(pods []string, leader string, started, at time.Time, label string) (alViewReadHistory, error) {
	a.t.Helper()
	groups, body := a.historySnapshot(a.ctx, at, alScrapeJob, "", alViewReadMetric, "up", "scrape_duration_seconds")
	history, err := alReadViewHistory(groups[alViewReadMetric], groups["up"], groups["scrape_duration_seconds"], pods, leader, started, at)
	if label != "" || err != nil {
		a.logf("manager state-read native history %s: queriedAt=%s snapshot=%s", label, at.Format(time.RFC3339Nano), body)
	}
	return history, err
}
