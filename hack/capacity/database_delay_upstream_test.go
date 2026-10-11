package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func delayTestPod(name, ip string, proxy, ready bool) corev1.Pod {
	labels := map[string]string{"app.kubernetes.io/name": "capacity-postgres"}
	if proxy {
		labels[databaseDelayLabel] = "true"
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.PodStatus{PodIP: ip, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}}}
}

// The 0.2.1 arm64 soak's proxy accepted 90 sessions and delayed none: the
// fixture namespace denies egress, and its upstream was a Service that also
// selected the proxy. It now dials the database Pod, which these rows pick.
func TestTheDelayProxyDialsTheDatabasePodAndNotItself(t *testing.T) {
	t.Parallel()
	address, err := readyDatabaseAddress([]corev1.Pod{
		delayTestPod("capacity-postgres-delay-1", "10.0.0.9", true, true),
		delayTestPod("capacity-postgres-1", "10.0.0.5", false, true),
	})
	if err != nil || address != "10.0.0.5" {
		t.Fatalf("picked %q, %v; want the database Pod 10.0.0.5", address, err)
	}
	for name, pods := range map[string][]corev1.Pod{
		"only the proxy":            {delayTestPod("delay", "10.0.0.9", true, true)},
		"a database Pod not ready":  {delayTestPod("db", "10.0.0.5", false, false)},
		"a database Pod with no IP": {delayTestPod("db", "", false, true)},
		"two ready database Pods":   {delayTestPod("a", "10.0.0.5", false, true), delayTestPod("b", "10.0.0.6", false, true)},
	} {
		if address, err := readyDatabaseAddress(pods); err == nil {
			t.Errorf("%s: picked %q", name, address)
		}
	}
}

func TestTheDelayProxyMayReachOnlyTheDatabasePort(t *testing.T) {
	t.Parallel()
	db := capacityDatabase{name: "capacity-postgres", namespace: "fixtures", port: 5432, portName: "postgresql"}
	policy := delayProxyEgress(db)
	if policy.Spec.PodSelector.MatchLabels[databaseDelayLabel] != "true" || len(policy.Spec.PolicyTypes) != 1 || policy.Spec.PolicyTypes[0] != "Egress" {
		t.Fatalf("the policy does not select only the proxy's egress: %+v", policy.Spec)
	}
	if len(policy.Spec.Egress) != 1 || len(policy.Spec.Egress[0].Ports) != 1 || policy.Spec.Egress[0].Ports[0].Port.IntValue() != 5432 ||
		policy.Spec.Egress[0].To[0].PodSelector.MatchLabels["app.kubernetes.io/name"] != "capacity-postgres" {
		t.Fatalf("the proxy may reach more than the database port: %+v", policy.Spec.Egress)
	}
}
