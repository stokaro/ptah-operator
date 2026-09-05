package crdupgrade_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

const quotaObjectCountPods corev1.ResourceName = "count/pods"

func TestRuntimeResourceQuotaPreflightAllowsCleanNamespace(t *testing.T) {
	t.Parallel()

	preflight, quotas, pods := runtimeQuotaFixture()
	if err := preflight.Check(context.Background()); err != nil {
		t.Fatalf("Check() rejected a namespace without quotas: %v", err)
	}
	if quotas.lastOptions.Limit != 500 {
		t.Fatalf("ResourceQuota list limit = %d, want 500", quotas.lastOptions.Limit)
	}
	if pods.lastOptions.Limit != 500 {
		t.Fatalf("Pod list limit = %d, want 500", pods.lastOptions.Limit)
	}
}

func TestRuntimeResourceQuotaPreflightDoesNotDoubleCountRecreatedPods(t *testing.T) {
	t.Parallel()

	preflight, quotas, pods := runtimeQuotaFixture()
	pods.list.Items = []corev1.Pod{
		runtimeQuotaProtectedPod(preflight, true, 0),
		runtimeQuotaProtectedPod(preflight, true, 1),
		runtimeQuotaProtectedPod(preflight, false, 0),
	}
	used := runtimeQuotaResources(map[corev1.ResourceName]string{
		corev1.ResourcePods:                     "3",
		quotaObjectCountPods:                    "3",
		corev1.ResourceCPU:                      "450m",
		corev1.ResourceRequestsCPU:              "450m",
		corev1.ResourceMemory:                   "1000Mi",
		corev1.ResourceRequestsMemory:           "1000Mi",
		corev1.ResourceEphemeralStorage:         "11Gi",
		corev1.ResourceRequestsEphemeralStorage: "11Gi",
		corev1.ResourceLimitsCPU:                "700m",
		corev1.ResourceLimitsMemory:             "1300Mi",
		corev1.ResourceLimitsEphemeralStorage:   "14Gi",
	})
	quota := runtimeQuotaObject("runtime", used, used)
	quota.Spec.Scopes = []corev1.ResourceQuotaScope{
		corev1.ResourceQuotaScopeNotTerminating,
		corev1.ResourceQuotaScopeNotBestEffort,
	}
	quotas.list.Items = []corev1.ResourceQuota{quota}

	if err := preflight.Check(context.Background()); err != nil {
		t.Fatalf("Check() double-counted old protected Pods during Recreate: %v", err)
	}
}

func TestRuntimeResourceQuotaPreflightDistinguishesPrefixedDeploymentNames(t *testing.T) {
	t.Parallel()

	preflight, _, pods := runtimeQuotaFixture()
	preflight.ControllerDeploymentName = "ptah-operator"
	preflight.CertificateDeploymentName = "ptah-operator-cert-rotator"
	pods.list.Items = []corev1.Pod{runtimeQuotaProtectedPod(preflight, false, 0)}

	if err := preflight.Check(context.Background()); err != nil {
		t.Fatalf("Check() treated the certificate Deployment as its controller name prefix: %v", err)
	}
}

func TestRuntimeResourceQuotaPreflightAccountsForEverySupportedPodResource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		resource corev1.ResourceName
		exact    string
		tooSmall string
	}{
		{name: "pods", resource: corev1.ResourcePods, exact: "1", tooSmall: "0"},
		{name: "object count", resource: quotaObjectCountPods, exact: "1", tooSmall: "0"},
		{name: "cpu alias", resource: corev1.ResourceCPU, exact: "100m", tooSmall: "99m"},
		{name: "cpu requests", resource: corev1.ResourceRequestsCPU, exact: "100m", tooSmall: "99m"},
		{name: "memory alias", resource: corev1.ResourceMemory, exact: "400Mi", tooSmall: "399Mi"},
		{name: "memory requests", resource: corev1.ResourceRequestsMemory, exact: "400Mi", tooSmall: "399Mi"},
		{name: "ephemeral storage alias", resource: corev1.ResourceEphemeralStorage, exact: "3Gi", tooSmall: "3071Mi"},
		{name: "ephemeral storage requests", resource: corev1.ResourceRequestsEphemeralStorage, exact: "3Gi", tooSmall: "3071Mi"},
		{name: "cpu limits", resource: corev1.ResourceLimitsCPU, exact: "200m", tooSmall: "199m"},
		{name: "memory limits", resource: corev1.ResourceLimitsMemory, exact: "500Mi", tooSmall: "499Mi"},
		{name: "ephemeral storage limits", resource: corev1.ResourceLimitsEphemeralStorage, exact: "4Gi", tooSmall: "4095Mi"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			preflight, quotas, _ := runtimeQuotaFixture()
			preflight.Contract.CertificateRuntimeEnabled = false
			preflight.ControllerReplicas = 1
			quotas.list.Items = []corev1.ResourceQuota{runtimeQuotaObject(
				"exact",
				runtimeQuotaResources(map[corev1.ResourceName]string{tc.resource: tc.exact}),
				runtimeQuotaResources(map[corev1.ResourceName]string{tc.resource: "0"}),
			)}
			if err := preflight.Check(context.Background()); err != nil {
				t.Fatalf("Check() rejected exact %s capacity: %v", tc.resource, err)
			}

			quotas.list.Items = []corev1.ResourceQuota{runtimeQuotaObject(
				"small",
				runtimeQuotaResources(map[corev1.ResourceName]string{tc.resource: tc.tooSmall}),
				runtimeQuotaResources(map[corev1.ResourceName]string{tc.resource: "0"}),
			)}
			err := preflight.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), "would exceed") {
				t.Fatalf("Check() error = %v, want insufficient %s capacity", err, tc.resource)
			}
		})
	}
}

func TestRuntimeResourceQuotaPreflightAccountsForHugePageAndExtendedRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		podResource   corev1.ResourceName
		quotaResource corev1.ResourceName
		quantity      string
	}{
		{
			name:          "huge page alias",
			podResource:   corev1.ResourceName("hugepages-2Mi"),
			quotaResource: corev1.ResourceName("hugepages-2Mi"),
			quantity:      "64Mi",
		},
		{
			name:          "huge page request",
			podResource:   corev1.ResourceName("hugepages-2Mi"),
			quotaResource: corev1.ResourceName("requests.hugepages-2Mi"),
			quantity:      "64Mi",
		},
		{
			name:          "extended resource request",
			podResource:   corev1.ResourceName("example.com/gpu"),
			quotaResource: corev1.ResourceName("requests.example.com/gpu"),
			quantity:      "1",
		},
		{
			name:          "device class request",
			podResource:   corev1.ResourceName("deviceclass.resource.kubernetes.io/accelerator"),
			quotaResource: corev1.ResourceName("requests.deviceclass.resource.kubernetes.io/accelerator"),
			quantity:      "1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			preflight, quotas, pods := runtimeQuotaFixture()
			preflight.Contract.CertificateRuntimeEnabled = false
			preflight.ControllerReplicas = 1
			pod := runtimeQuotaProtectedPod(preflight, true, 0)
			pod.Spec.Containers[0].Resources.Requests[tc.podResource] = resource.MustParse(tc.quantity)
			pod.Spec.Containers[0].Resources.Limits[tc.podResource] = resource.MustParse(tc.quantity)
			pods.list.Items = []corev1.Pod{pod}
			quotas.list.Items = []corev1.ResourceQuota{runtimeQuotaObject(
				"stale-special-resource",
				runtimeQuotaResources(map[corev1.ResourceName]string{tc.quotaResource: tc.quantity}),
				runtimeQuotaResources(map[corev1.ResourceName]string{tc.quotaResource: "0"}),
			)}

			err := preflight.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), "smaller than exact protected runtime Pod usage") {
				t.Fatalf("Check() error = %v, want exact %s accounting", err, tc.quotaResource)
			}
		})
	}
}

func TestRuntimeResourceQuotaPreflightUsesInitContainerMaximumForOldPods(t *testing.T) {
	t.Parallel()

	preflight, quotas, pods := runtimeQuotaFixture()
	preflight.Contract.CertificateRuntimeEnabled = false
	preflight.ControllerReplicas = 1
	pod := runtimeQuotaProtectedPod(preflight, true, 0)
	pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("50m")
	pod.Spec.InitContainers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("500m")
	pods.list.Items = []corev1.Pod{pod}
	quotas.list.Items = []corev1.ResourceQuota{runtimeQuotaObject(
		"init-maximum",
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "500m"}),
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "500m"}),
	)}

	if err := preflight.Check(context.Background()); err != nil {
		t.Fatalf("Check() did not subtract the old init-container maximum exactly: %v", err)
	}
}

func TestRuntimeResourceQuotaPreflightRejectsResizedOldPodsAcrossVersionWindow(t *testing.T) {
	t.Parallel()

	preflight, quotas, pods := runtimeQuotaFixture()
	preflight.Contract.CertificateRuntimeEnabled = false
	preflight.ControllerReplicas = 1
	pod := runtimeQuotaProtectedPod(preflight, true, 0)
	pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("50m")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "runtime",
		Resources: &corev1.ResourceRequirements{Requests: runtimeQuotaResources(map[corev1.ResourceName]string{
			corev1.ResourceCPU: "500m",
		})},
		AllocatedResources: runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceCPU: "400m"}),
	}}
	pods.list.Items = []corev1.Pod{pod}
	quotas.list.Items = []corev1.ResourceQuota{runtimeQuotaObject(
		"actuated",
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "100m"}),
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "500m"}),
	)}

	err := preflight.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot be projected consistently across the supported Kubernetes window") {
		t.Fatalf("Check() error = %v, want version-dependent resize rejection", err)
	}
}

func TestRuntimeResourceQuotaPreflightRejectsPendingResizeWithoutCompleteStatus(t *testing.T) {
	t.Parallel()

	preflight, _, pods := runtimeQuotaFixture()
	pod := runtimeQuotaProtectedPod(preflight, true, 0)
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodResizePending,
		Status: corev1.ConditionTrue,
		Reason: corev1.PodReasonInfeasible,
	}}
	pods.list.Items = []corev1.Pod{pod}

	err := preflight.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "PodResizePending status") {
		t.Fatalf("Check() error = %v, want pending resize rejection", err)
	}
}

func TestRuntimeResourceQuotaPreflightUsesRestartableInitSemanticsForOldPods(t *testing.T) {
	t.Parallel()

	preflight, quotas, pods := runtimeQuotaFixture()
	preflight.Contract.CertificateRuntimeEnabled = false
	preflight.ControllerReplicas = 1
	pod := runtimeQuotaProtectedPod(preflight, true, 0)
	pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("50m")
	restartAlways := corev1.ContainerRestartPolicyAlways
	pod.Spec.InitContainers = []corev1.Container{
		{
			Name:          "sidecar",
			RestartPolicy: &restartAlways,
			Resources: corev1.ResourceRequirements{Requests: runtimeQuotaResources(map[corev1.ResourceName]string{
				corev1.ResourceCPU: "100m",
			})},
		},
		{
			Name: "setup",
			Resources: corev1.ResourceRequirements{Requests: runtimeQuotaResources(map[corev1.ResourceName]string{
				corev1.ResourceCPU: "500m",
			})},
		},
	}
	pods.list.Items = []corev1.Pod{pod}
	quotas.list.Items = []corev1.ResourceQuota{runtimeQuotaObject(
		"restartable-init",
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "600m"}),
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "600m"}),
	)}

	if err := preflight.Check(context.Background()); err != nil {
		t.Fatalf("Check() did not use restartable init-container quota semantics: %v", err)
	}
}

func TestRuntimeResourceQuotaPreflightRejectsScaleUpWithoutCapacity(t *testing.T) {
	t.Parallel()

	preflight, quotas, pods := runtimeQuotaFixture()
	preflight.Contract.CertificateRuntimeEnabled = false
	pods.list.Items = []corev1.Pod{runtimeQuotaProtectedPod(preflight, true, 0)}
	quotas.list.Items = []corev1.ResourceQuota{runtimeQuotaObject(
		"pods",
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "1"}),
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "1"}),
	)}

	err := preflight.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "projected 2, hard 1") {
		t.Fatalf("Check() error = %v, want post-Recreate scale-up rejection", err)
	}
}

func TestRuntimeResourceQuotaPreflightEvaluatesPodScopes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		scopes        []corev1.ResourceQuotaScope
		selector      *corev1.ScopeSelector
		priorityClass string
		bestEffort    bool
		wantMatch     bool
	}{
		{name: "unscoped", wantMatch: true},
		{name: "terminating", scopes: []corev1.ResourceQuotaScope{corev1.ResourceQuotaScopeTerminating}},
		{name: "not terminating", scopes: []corev1.ResourceQuotaScope{corev1.ResourceQuotaScopeNotTerminating}, wantMatch: true},
		{name: "best effort does not match", scopes: []corev1.ResourceQuotaScope{corev1.ResourceQuotaScopeBestEffort}},
		{name: "best effort matches", scopes: []corev1.ResourceQuotaScope{corev1.ResourceQuotaScopeBestEffort}, bestEffort: true, wantMatch: true},
		{name: "not best effort", scopes: []corev1.ResourceQuotaScope{corev1.ResourceQuotaScopeNotBestEffort}, wantMatch: true},
		{name: "priority in", priorityClass: "operator-critical", selector: runtimeQuotaScope(corev1.ResourceQuotaScopePriorityClass, corev1.ScopeSelectorOpIn, "operator-critical"), wantMatch: true},
		{name: "priority in differs", priorityClass: "operator-critical", selector: runtimeQuotaScope(corev1.ResourceQuotaScopePriorityClass, corev1.ScopeSelectorOpIn, "batch")},
		{name: "priority not in", priorityClass: "operator-critical", selector: runtimeQuotaScope(corev1.ResourceQuotaScopePriorityClass, corev1.ScopeSelectorOpNotIn, "batch"), wantMatch: true},
		{name: "missing priority is not in", selector: runtimeQuotaScope(corev1.ResourceQuotaScopePriorityClass, corev1.ScopeSelectorOpNotIn, "batch"), wantMatch: true},
		{name: "priority exists", priorityClass: "operator-critical", selector: runtimeQuotaScope(corev1.ResourceQuotaScopePriorityClass, corev1.ScopeSelectorOpExists), wantMatch: true},
		{name: "priority does not exist", selector: runtimeQuotaScope(corev1.ResourceQuotaScopePriorityClass, corev1.ScopeSelectorOpDoesNotExist), wantMatch: true},
		{name: "cross namespace affinity", scopes: []corev1.ResourceQuotaScope{corev1.ResourceQuotaScopeCrossNamespacePodAffinity}},
		{
			name:          "scopes and selector are both required",
			priorityClass: "operator-critical",
			scopes:        []corev1.ResourceQuotaScope{corev1.ResourceQuotaScopeNotTerminating},
			selector:      runtimeQuotaScope(corev1.ResourceQuotaScopePriorityClass, corev1.ScopeSelectorOpIn, "operator-critical"),
			wantMatch:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			preflight, quotas, _ := runtimeQuotaFixture()
			preflight.Contract.CertificateRuntimeEnabled = false
			preflight.Contract.PriorityClassName = tc.priorityClass
			if tc.bestEffort {
				preflight.Contract.CommonInitContainerResources = corev1.ResourceRequirements{}
				preflight.Contract.ControllerContainerResources = corev1.ResourceRequirements{}
			}
			quota := runtimeQuotaObject(
				"scoped",
				runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "0"}),
				runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "0"}),
			)
			quota.Spec.Scopes = tc.scopes
			quota.Spec.ScopeSelector = tc.selector
			quotas.list.Items = []corev1.ResourceQuota{quota}

			err := preflight.Check(context.Background())
			if tc.wantMatch && (err == nil || !strings.Contains(err.Error(), "would exceed")) {
				t.Fatalf("Check() error = %v, want matching scope to enforce quota", err)
			}
			if !tc.wantMatch && err != nil {
				t.Fatalf("Check() rejected candidate outside quota scope: %v", err)
			}
		})
	}
}

func TestRuntimeResourceQuotaPreflightAllowsUnaffectedQuotaResourcesAndScopes(t *testing.T) {
	t.Parallel()

	preflight, quotas, _ := runtimeQuotaFixture()
	hard := runtimeQuotaResources(map[corev1.ResourceName]string{
		corev1.ResourceSecrets:                                                         "5",
		corev1.ResourceServices:                                                        "3",
		corev1.ResourcePersistentVolumeClaims:                                          "4",
		corev1.ResourceRequestsStorage:                                                 "20Gi",
		corev1.ResourceConfigMaps:                                                      "8",
		corev1.ResourceReplicationControllers:                                          "6",
		corev1.ResourceQuotas:                                                          "4",
		corev1.ResourceServicesNodePorts:                                               "2",
		corev1.ResourceServicesLoadBalancers:                                           "2",
		corev1.ResourceName("count/configmaps"):                                        "8",
		corev1.ResourceName("count/widgets.example.com"):                               "2",
		corev1.ResourceName("gold.storageclass.storage.k8s.io/requests.storage"):       "20Gi",
		corev1.ResourceName("gold.storageclass.storage.k8s.io/persistentvolumeclaims"): "4",
	})
	used := runtimeQuotaResources(map[corev1.ResourceName]string{
		corev1.ResourceSecrets:                                                         "2",
		corev1.ResourceServices:                                                        "1",
		corev1.ResourcePersistentVolumeClaims:                                          "1",
		corev1.ResourceRequestsStorage:                                                 "5Gi",
		corev1.ResourceConfigMaps:                                                      "4",
		corev1.ResourceReplicationControllers:                                          "2",
		corev1.ResourceQuotas:                                                          "2",
		corev1.ResourceServicesNodePorts:                                               "1",
		corev1.ResourceServicesLoadBalancers:                                           "1",
		corev1.ResourceName("count/configmaps"):                                        "4",
		corev1.ResourceName("count/widgets.example.com"):                               "1",
		corev1.ResourceName("gold.storageclass.storage.k8s.io/requests.storage"):       "5Gi",
		corev1.ResourceName("gold.storageclass.storage.k8s.io/persistentvolumeclaims"): "1",
	})
	quota := runtimeQuotaObject("unaffected", hard, used)
	storageQuota := runtimeQuotaObject(
		"storage-class",
		runtimeQuotaResources(map[corev1.ResourceName]string{
			corev1.ResourcePersistentVolumeClaims: "4",
			corev1.ResourceRequestsStorage:        "20Gi",
		}),
		runtimeQuotaResources(map[corev1.ResourceName]string{
			corev1.ResourcePersistentVolumeClaims: "1",
			corev1.ResourceRequestsStorage:        "5Gi",
		}),
	)
	storageQuota.Spec.Scopes = []corev1.ResourceQuotaScope{corev1.ResourceQuotaScopeVolumeAttributesClass}
	quotas.list.Items = []corev1.ResourceQuota{quota, storageQuota}

	if err := preflight.Check(context.Background()); err != nil {
		t.Fatalf("Check() rejected quota resources unaffected by runtime Pods: %v", err)
	}
}

func TestRuntimeResourceQuotaPreflightRejectsMalformedQuotaContracts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*corev1.ResourceQuota)
		want   string
	}{
		{
			name: "unsynchronized hard status",
			mutate: func(quota *corev1.ResourceQuota) {
				quota.Status.Hard[corev1.ResourcePods] = resource.MustParse("3")
			},
			want: "status.hard is not synchronized",
		},
		{
			name: "missing used counter",
			mutate: func(quota *corev1.ResourceQuota) {
				quota.Status.Used = corev1.ResourceList{}
			},
			want: "status.used does not contain exactly",
		},
		{
			name: "negative used counter",
			mutate: func(quota *corev1.ResourceQuota) {
				quota.Status.Used[corev1.ResourcePods] = resource.MustParse("-1")
			},
			want: "negative hard or used",
		},
		{
			name: "used exceeds hard for unaffected resource",
			mutate: func(quota *corev1.ResourceQuota) {
				quota.Spec.Hard = runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "1"})
				quota.Status.Hard = quota.Spec.Hard.DeepCopy()
				quota.Status.Used = runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "2"})
			},
			want: "exceeds status.hard",
		},
		{
			name: "unsupported scope",
			mutate: func(quota *corev1.ResourceQuota) {
				quota.Spec.Scopes = []corev1.ResourceQuotaScope{corev1.ResourceQuotaScopeVolumeAttributesClass}
			},
			want: "unsupported scope",
		},
		{
			name: "non-priority selector operator",
			mutate: func(quota *corev1.ResourceQuota) {
				quota.Spec.ScopeSelector = runtimeQuotaScope(corev1.ResourceQuotaScopeTerminating, corev1.ScopeSelectorOpIn, "true")
			},
			want: "must use Exists with no values",
		},
		{
			name: "priority in without values",
			mutate: func(quota *corev1.ResourceQuota) {
				quota.Spec.ScopeSelector = runtimeQuotaScope(corev1.ResourceQuotaScopePriorityClass, corev1.ScopeSelectorOpIn)
			},
			want: "values",
		},
		{
			name: "priority exists with values",
			mutate: func(quota *corev1.ResourceQuota) {
				quota.Spec.ScopeSelector = runtimeQuotaScope(corev1.ResourceQuotaScopePriorityClass, corev1.ScopeSelectorOpExists, "critical")
			},
			want: "values",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			preflight, quotas, _ := runtimeQuotaFixture()
			quota := runtimeQuotaObject(
				"malformed",
				runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "2"}),
				runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "0"}),
			)
			tc.mutate(&quota)
			quotas.list.Items = []corev1.ResourceQuota{quota}

			err := preflight.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestRuntimeResourceQuotaPreflightRejectsStaleProtectedUsage(t *testing.T) {
	t.Parallel()

	preflight, quotas, pods := runtimeQuotaFixture()
	preflight.Contract.CertificateRuntimeEnabled = false
	pods.list.Items = []corev1.Pod{runtimeQuotaProtectedPod(preflight, true, 0)}
	quotas.list.Items = []corev1.ResourceQuota{runtimeQuotaObject(
		"stale",
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "2"}),
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "0"}),
	)}

	err := preflight.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "counters are stale or malformed") {
		t.Fatalf("Check() error = %v, want stale protected usage rejection", err)
	}
}

func TestRuntimeResourceQuotaPreflightRejectsAmbiguousProtectedPods(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*corev1.Pod)
		want   string
	}{
		{name: "wrong service account", mutate: func(pod *corev1.Pod) { pod.Spec.ServiceAccountName = "foreign" }, want: "uses ServiceAccount"},
		{name: "missing owner", mutate: func(pod *corev1.Pod) { pod.OwnerReferences = nil }, want: "exactly one ReplicaSet owner"},
		{name: "wrong hash", mutate: func(pod *corev1.Pod) { pod.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "fedcba" }, want: "does not bind its ReplicaSet owner"},
		{name: "terminal", mutate: func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodFailed }, want: "deleting or terminal"},
		{name: "deleting", mutate: func(pod *corev1.Pod) { now := metav1.NewTime(time.Now()); pod.DeletionTimestamp = &now }, want: "deleting or terminal"},
		{name: "terminating scope", mutate: func(pod *corev1.Pod) { value := int64(60); pod.Spec.ActiveDeadlineSeconds = &value }, want: "fixed candidate ResourceQuota contract is non-terminating"},
		{
			name: "cross namespace affinity",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Affinity = &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{NamespaceSelector: &metav1.LabelSelector{}}},
				}}
			},
			want: "fixed candidate ResourceQuota contract does not",
		},
		{name: "foreign namespace", mutate: func(pod *corev1.Pod) { pod.Namespace = "foreign" }, want: "foreign or incomplete Pod"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			preflight, _, pods := runtimeQuotaFixture()
			pod := runtimeQuotaProtectedPod(preflight, true, 0)
			tc.mutate(&pod)
			pods.list.Items = []corev1.Pod{pod}
			err := preflight.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestRuntimeResourceQuotaPreflightRequiresContainerCPUAndMemoryFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		resource corev1.ResourceName
		mutate   func(*corev1.ResourceRequirements)
		want     string
	}{
		{
			name:     "cpu request",
			resource: corev1.ResourceRequestsCPU,
			mutate: func(resources *corev1.ResourceRequirements) {
				delete(resources.Requests, corev1.ResourceCPU)
				delete(resources.Limits, corev1.ResourceCPU)
			},
			want: "request for cpu",
		},
		{
			name:     "memory limit",
			resource: corev1.ResourceLimitsMemory,
			mutate:   func(resources *corev1.ResourceRequirements) { delete(resources.Limits, corev1.ResourceMemory) },
			want:     "limit for memory",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			preflight, quotas, _ := runtimeQuotaFixture()
			preflight.Contract.CertificateRuntimeEnabled = false
			tc.mutate(&preflight.Contract.ControllerContainerResources)
			quotas.list.Items = []corev1.ResourceQuota{runtimeQuotaObject(
				"required-fields",
				runtimeQuotaResources(map[corev1.ResourceName]string{tc.resource: "100Gi"}),
				runtimeQuotaResources(map[corev1.ResourceName]string{tc.resource: "0"}),
			)}
			err := preflight.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestRuntimeResourceQuotaPreflightExhaustsLargePaginatedInventories(t *testing.T) {
	t.Parallel()

	preflight, quotas, pods := runtimeQuotaFixture()
	quotaItems := make([]corev1.ResourceQuota, 536)
	for index := range quotaItems {
		quotaItems[index] = runtimeQuotaObject(
			fmt.Sprintf("quota-%03d", index),
			runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "1"}),
			runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "0"}),
		)
	}
	quotaRemainingEstimate := int64(41)
	quotas.pages = map[string]*corev1.ResourceQuotaList{
		"": {
			ListMeta: metav1.ListMeta{ResourceVersion: "quota-rv", Continue: "quota-page-2"},
		},
		"quota-page-2": {
			ListMeta: metav1.ListMeta{ResourceVersion: "quota-rv", Continue: "quota-page-3"},
			Items:    quotaItems[:501],
		},
		"quota-page-3": {
			ListMeta: metav1.ListMeta{ResourceVersion: "quota-rv", RemainingItemCount: &quotaRemainingEstimate},
			Items:    quotaItems[501:],
		},
	}

	podItems := make([]corev1.Pod, 1100)
	for index := range podItems {
		podItems[index] = corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("unrelated-%04d", index),
			Namespace: preflight.ReleaseNamespace,
			UID:       types.UID(fmt.Sprintf("unrelated-pod-%04d", index)),
		}}
	}
	podRemainingEstimate := int64(73)
	pods.pages = map[string]*corev1.PodList{
		"": {
			ListMeta: metav1.ListMeta{ResourceVersion: "pod-rv", Continue: "pod-page-2"},
		},
		"pod-page-2": {
			ListMeta: metav1.ListMeta{ResourceVersion: "pod-rv", Continue: "pod-page-3"},
			Items:    podItems[:501],
		},
		"pod-page-3": {
			ListMeta: metav1.ListMeta{ResourceVersion: "pod-rv", RemainingItemCount: &podRemainingEstimate},
			Items:    podItems[501:],
		},
	}

	if err := preflight.Check(context.Background()); err != nil {
		t.Fatalf("Check() rejected complete large inventories: %v", err)
	}
	assertRuntimeQuotaListCalls(t, "ResourceQuota", quotas.calls, []string{"", "quota-page-2", "quota-page-3", "", "quota-page-2", "quota-page-3"})
	assertRuntimeQuotaListCalls(t, "Pod", pods.calls, []string{"", "pod-page-2", "pod-page-3"})
}

func TestRuntimeResourceQuotaPreflightRejectsQuotaChangeAcrossPodSnapshot(t *testing.T) {
	t.Parallel()

	preflight, quotas, _ := runtimeQuotaFixture()
	before := runtimeQuotaObject(
		"moving",
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "10"}),
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "1"}),
	)
	after := before.DeepCopy()
	after.ResourceVersion = "changed-after-pod-list"
	after.Status.Used[corev1.ResourcePods] = resource.MustParse("2")
	quotas.responses = []*corev1.ResourceQuotaList{
		{ListMeta: metav1.ListMeta{ResourceVersion: "quota-before-rv"}, Items: []corev1.ResourceQuota{before}},
		{ListMeta: metav1.ListMeta{ResourceVersion: "quota-after-rv"}, Items: []corev1.ResourceQuota{*after}},
	}

	err := preflight.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ResourceQuota inventory changed while the Pod inventory was read") {
		t.Fatalf("Check() error = %v, want unstable cross-resource snapshot refusal", err)
	}
}

func TestRuntimeResourceQuotaPreflightWaitsForPostQuiesceStatusConvergence(t *testing.T) {
	preflight, quotas, pods := runtimeQuotaFixture()
	hard := runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "3"})
	stale := runtimeQuotaObject(
		"runtime-pods",
		hard,
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "3"}),
	)
	converged := runtimeQuotaObject(
		"runtime-pods",
		hard,
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "0"}),
	)
	quotas.responses = []*corev1.ResourceQuotaList{
		{ListMeta: metav1.ListMeta{ResourceVersion: "quota-stale-before"}, Items: []corev1.ResourceQuota{stale}},
		{ListMeta: metav1.ListMeta{ResourceVersion: "quota-stale-after"}, Items: []corev1.ResourceQuota{stale}},
		{ListMeta: metav1.ListMeta{ResourceVersion: "quota-converged-before"}, Items: []corev1.ResourceQuota{converged}},
		{ListMeta: metav1.ListMeta{ResourceVersion: "quota-converged-after"}, Items: []corev1.ResourceQuota{converged}},
	}

	if err := preflight.WaitForCapacityAfterQuiesce(context.Background(), time.Nanosecond); err != nil {
		t.Fatalf("WaitForCapacityAfterQuiesce() error = %v", err)
	}
	if len(quotas.calls) != 4 || len(pods.calls) != 2 {
		t.Fatalf("post-quiesce list calls = %d quota/%d Pod, want 4/2", len(quotas.calls), len(pods.calls))
	}
}

func TestRuntimeResourceQuotaPreflightPostQuiesceWaitRetriesTransientListFailures(t *testing.T) {
	tests := []struct {
		name           string
		mutate         func(*runtimeQuotaFakeQuotas, *runtimeQuotaFakePods)
		wantQuotaCalls int
		wantPodCalls   int
	}{
		{
			name: "ResourceQuota throttling",
			mutate: func(quotas *runtimeQuotaFakeQuotas, _ *runtimeQuotaFakePods) {
				quotas.callErrors = []error{apierrors.NewTooManyRequests("quota controller is busy", 1)}
			},
			wantQuotaCalls: 3,
			wantPodCalls:   1,
		},
		{
			name: "Pod service unavailable",
			mutate: func(_ *runtimeQuotaFakeQuotas, pods *runtimeQuotaFakePods) {
				pods.callErrors = []error{apierrors.NewServiceUnavailable("Pod API is restarting")}
			},
			wantQuotaCalls: 3,
			wantPodCalls:   2,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preflight, quotas, pods := runtimeQuotaFixture()
			test.mutate(quotas, pods)
			if err := preflight.WaitForCapacityAfterQuiesce(context.Background(), time.Nanosecond); err != nil {
				t.Fatalf("WaitForCapacityAfterQuiesce() error = %v", err)
			}
			if len(quotas.calls) != test.wantQuotaCalls || len(pods.calls) != test.wantPodCalls {
				t.Fatalf(
					"post-quiesce list calls = %d quota/%d Pod, want %d/%d",
					len(quotas.calls),
					len(pods.calls),
					test.wantQuotaCalls,
					test.wantPodCalls,
				)
			}
		})
	}
}

func TestRuntimeResourceQuotaPreflightPostQuiesceWaitDoesNotRetryAuthorizationFailure(t *testing.T) {
	preflight, quotas, pods := runtimeQuotaFixture()
	quotas.err = apierrors.NewUnauthorized("expired cleanup credential")

	err := preflight.WaitForCapacityAfterQuiesce(context.Background(), time.Nanosecond)
	if err == nil || !apierrors.IsUnauthorized(err) {
		t.Fatalf("WaitForCapacityAfterQuiesce() error = %v, want unauthorized refusal", err)
	}
	if len(quotas.calls) != 1 || len(pods.calls) != 0 {
		t.Fatalf("post-quiesce list calls = %d quota/%d Pod, want 1/0", len(quotas.calls), len(pods.calls))
	}
}

func TestRuntimeResourceQuotaPreflightPostQuiesceWaitReportsLastTransientListFailure(t *testing.T) {
	preflight, quotas, _ := runtimeQuotaFixture()
	quotas.err = apierrors.NewTooManyRequests("quota controller is busy", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := preflight.WaitForCapacityAfterQuiesce(ctx, time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "last observation") ||
		!strings.Contains(err.Error(), "quota controller is busy") {
		t.Fatalf("WaitForCapacityAfterQuiesce() error = %v, want deadline and last transient list failure", err)
	}
}

func TestRuntimeResourceQuotaPreflightPostQuiesceWaitFailsImmediatelyOnMalformedContract(t *testing.T) {
	preflight, quotas, _ := runtimeQuotaFixture()
	malformed := runtimeQuotaObject(
		"malformed",
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "3"}),
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "0"}),
	)
	malformed.UID = ""
	quotas.list.Items = []corev1.ResourceQuota{malformed}

	err := preflight.WaitForCapacityAfterQuiesce(context.Background(), time.Nanosecond)
	if err == nil || !strings.Contains(err.Error(), "foreign or incomplete ResourceQuota") {
		t.Fatalf("WaitForCapacityAfterQuiesce() error = %v, want malformed contract refusal", err)
	}
	if len(quotas.calls) != 2 {
		t.Fatalf("ResourceQuota list calls = %d, want one non-retried snapshot", len(quotas.calls))
	}
}

func TestRuntimeResourceQuotaPreflightPostQuiesceWaitReturnsLastObservationOnDeadline(t *testing.T) {
	preflight, quotas, _ := runtimeQuotaFixture()
	hard := runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourcePods: "3"})
	stale := runtimeQuotaObject("runtime-pods", hard, hard)
	quotas.list.Items = []corev1.ResourceQuota{stale}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := preflight.WaitForCapacityAfterQuiesce(ctx, time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "last observation") ||
		!strings.Contains(err.Error(), "would exceed ResourceQuota") {
		t.Fatalf("WaitForCapacityAfterQuiesce() error = %v, want deadline and last capacity observation", err)
	}
}

func TestRuntimeResourceQuotaPreflightRejectsDuplicatesAcrossPages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*runtimeResourceQuotaPreflightTestFixture)
		want   string
	}{
		{
			name: "ResourceQuota name",
			mutate: func(fixture *runtimeResourceQuotaPreflightTestFixture) {
				quota := runtimeQuotaObject(
					"duplicate",
					runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "1"}),
					runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "0"}),
				)
				fixture.quotas.pages = map[string]*corev1.ResourceQuotaList{
					"":     {ListMeta: metav1.ListMeta{ResourceVersion: "quota-rv", Continue: "next"}, Items: []corev1.ResourceQuota{quota}},
					"next": {ListMeta: metav1.ListMeta{ResourceVersion: "quota-rv"}, Items: []corev1.ResourceQuota{quota}},
				}
			},
			want: "ResourceQuota inventory returned operators/duplicate more than once",
		},
		{
			name: "ResourceQuota UID",
			mutate: func(fixture *runtimeResourceQuotaPreflightTestFixture) {
				first := runtimeQuotaObject(
					"first",
					runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "1"}),
					runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "0"}),
				)
				second := runtimeQuotaObject(
					"second",
					runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "1"}),
					runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "0"}),
				)
				second.UID = first.UID
				fixture.quotas.pages = map[string]*corev1.ResourceQuotaList{
					"":     {ListMeta: metav1.ListMeta{ResourceVersion: "quota-rv", Continue: "next"}, Items: []corev1.ResourceQuota{first}},
					"next": {ListMeta: metav1.ListMeta{ResourceVersion: "quota-rv"}, Items: []corev1.ResourceQuota{second}},
				}
			},
			want: "share UID quota-first",
		},
		{
			name: "Pod name",
			mutate: func(fixture *runtimeResourceQuotaPreflightTestFixture) {
				pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "duplicate", Namespace: "operators", UID: "pod-duplicate"}}
				fixture.pods.pages = map[string]*corev1.PodList{
					"":     {ListMeta: metav1.ListMeta{ResourceVersion: "pod-rv", Continue: "next"}, Items: []corev1.Pod{pod}},
					"next": {ListMeta: metav1.ListMeta{ResourceVersion: "pod-rv"}, Items: []corev1.Pod{pod}},
				}
			},
			want: "Pod operators/duplicate more than once",
		},
		{
			name: "Pod UID",
			mutate: func(fixture *runtimeResourceQuotaPreflightTestFixture) {
				first := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "operators", UID: "shared"}}
				second := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "operators", UID: "shared"}}
				fixture.pods.pages = map[string]*corev1.PodList{
					"":     {ListMeta: metav1.ListMeta{ResourceVersion: "pod-rv", Continue: "next"}, Items: []corev1.Pod{first}},
					"next": {ListMeta: metav1.ListMeta{ResourceVersion: "pod-rv"}, Items: []corev1.Pod{second}},
				}
			},
			want: "share UID shared",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			preflight, quotas, pods := runtimeQuotaFixture()
			fixture := &runtimeResourceQuotaPreflightTestFixture{quotas: quotas, pods: pods}
			tc.mutate(fixture)
			err := preflight.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestRuntimeResourceQuotaPreflightPropagatesPaginatedListFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*runtimeQuotaFakeQuotas, *runtimeQuotaFakePods)
		want   string
	}{
		{name: "quota error", mutate: func(quotas *runtimeQuotaFakeQuotas, _ *runtimeQuotaFakePods) {
			quotas.err = errors.New("quota API unavailable")
		}, want: "quota API unavailable"},
		{name: "nil quota list", mutate: func(quotas *runtimeQuotaFakeQuotas, _ *runtimeQuotaFakePods) { quotas.list = nil }, want: "nil result"},
		{name: "empty first quota resource version", mutate: func(quotas *runtimeQuotaFakeQuotas, _ *runtimeQuotaFakePods) {
			quotas.list.ResourceVersion = ""
		}, want: "ResourceQuota inventory returned an empty resourceVersion"},
		{name: "repeated quota continuation", mutate: func(quotas *runtimeQuotaFakeQuotas, _ *runtimeQuotaFakePods) {
			quotas.list.Continue = "next"
			quotas.list.Items = []corev1.ResourceQuota{runtimeQuotaObject(
				"first",
				runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "1"}),
				runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "0"}),
			)}
		}, want: "repeated continue token"},
		{name: "later quota error", mutate: func(quotas *runtimeQuotaFakeQuotas, _ *runtimeQuotaFakePods) {
			quotas.pages = map[string]*corev1.ResourceQuotaList{"": {
				ListMeta: metav1.ListMeta{ResourceVersion: "quota-rv", Continue: "next"},
				Items: []corev1.ResourceQuota{runtimeQuotaObject(
					"first",
					runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "1"}),
					runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "0"}),
				)},
			}}
			quotas.errorsByContinue = map[string]error{"next": errors.New("second quota page unavailable")}
		}, want: "second quota page unavailable"},
		{name: "nil later quota page", mutate: func(quotas *runtimeQuotaFakeQuotas, _ *runtimeQuotaFakePods) {
			quotas.pages = map[string]*corev1.ResourceQuotaList{
				"": {
					ListMeta: metav1.ListMeta{ResourceVersion: "quota-rv", Continue: "next"},
					Items: []corev1.ResourceQuota{runtimeQuotaObject(
						"first",
						runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "1"}),
						runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "0"}),
					)},
				},
				"next": nil,
			}
		}, want: "nil result"},
		{name: "empty later quota resource version", mutate: func(quotas *runtimeQuotaFakeQuotas, _ *runtimeQuotaFakePods) {
			quotas.pages = map[string]*corev1.ResourceQuotaList{
				"": {
					ListMeta: metav1.ListMeta{ResourceVersion: "quota-rv", Continue: "next"},
					Items: []corev1.ResourceQuota{runtimeQuotaObject(
						"first",
						runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "1"}),
						runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "0"}),
					)},
				},
				"next": {},
			}
		}, want: "ResourceQuota inventory returned an empty resourceVersion"},
		{name: "quota resource version changes", mutate: func(quotas *runtimeQuotaFakeQuotas, _ *runtimeQuotaFakePods) {
			quotas.pages = map[string]*corev1.ResourceQuotaList{
				"": {
					ListMeta: metav1.ListMeta{Continue: "next", ResourceVersion: "10"},
					Items: []corev1.ResourceQuota{runtimeQuotaObject(
						"first",
						runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "1"}),
						runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceSecrets: "0"}),
					)},
				},
				"next": {ListMeta: metav1.ListMeta{ResourceVersion: "11"}},
			}
		}, want: "resourceVersion changed across pages"},
		{name: "negative ResourceQuota remaining count", mutate: func(quotas *runtimeQuotaFakeQuotas, _ *runtimeQuotaFakePods) {
			remaining := int64(-1)
			quotas.list.RemainingItemCount = &remaining
		}, want: "negative remainingItemCount -1"},
		{name: "pod error", mutate: func(_ *runtimeQuotaFakeQuotas, pods *runtimeQuotaFakePods) {
			pods.err = errors.New("pod API unavailable")
		}, want: "pod API unavailable"},
		{name: "nil pod list", mutate: func(_ *runtimeQuotaFakeQuotas, pods *runtimeQuotaFakePods) { pods.list = nil }, want: "nil result"},
		{name: "empty first Pod resource version", mutate: func(_ *runtimeQuotaFakeQuotas, pods *runtimeQuotaFakePods) {
			pods.list.ResourceVersion = ""
		}, want: "Pod inventory returned an empty resourceVersion"},
		{name: "repeated pod continuation", mutate: func(_ *runtimeQuotaFakeQuotas, pods *runtimeQuotaFakePods) {
			pods.list.Continue = "next"
			pods.list.Items = []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "operators", UID: "first"}}}
		}, want: "repeated continue token"},
		{name: "later pod error", mutate: func(_ *runtimeQuotaFakeQuotas, pods *runtimeQuotaFakePods) {
			pods.pages = map[string]*corev1.PodList{"": {
				ListMeta: metav1.ListMeta{ResourceVersion: "pod-rv", Continue: "next"},
				Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{
					Name: "first", Namespace: "operators", UID: "first",
				}}},
			}}
			pods.errorsByContinue = map[string]error{"next": errors.New("second Pod page unavailable")}
		}, want: "second Pod page unavailable"},
		{name: "nil later Pod page", mutate: func(_ *runtimeQuotaFakeQuotas, pods *runtimeQuotaFakePods) {
			pods.pages = map[string]*corev1.PodList{
				"": {
					ListMeta: metav1.ListMeta{ResourceVersion: "pod-rv", Continue: "next"},
					Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{
						Name: "first", Namespace: "operators", UID: "first",
					}}},
				},
				"next": nil,
			}
		}, want: "nil result"},
		{name: "empty later Pod resource version", mutate: func(_ *runtimeQuotaFakeQuotas, pods *runtimeQuotaFakePods) {
			pods.pages = map[string]*corev1.PodList{
				"": {
					ListMeta: metav1.ListMeta{ResourceVersion: "pod-rv", Continue: "next"},
					Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{
						Name: "first", Namespace: "operators", UID: "first",
					}}},
				},
				"next": {},
			}
		}, want: "Pod inventory returned an empty resourceVersion"},
		{name: "pod resource version changes", mutate: func(_ *runtimeQuotaFakeQuotas, pods *runtimeQuotaFakePods) {
			pods.pages = map[string]*corev1.PodList{
				"": {
					ListMeta: metav1.ListMeta{Continue: "next", ResourceVersion: "10"},
					Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{
						Name: "first", Namespace: "operators", UID: "first",
					}}},
				},
				"next": {ListMeta: metav1.ListMeta{ResourceVersion: "11"}},
			}
		}, want: "resourceVersion changed across pages"},
		{name: "negative Pod remaining count", mutate: func(_ *runtimeQuotaFakeQuotas, pods *runtimeQuotaFakePods) {
			remaining := int64(-1)
			pods.list.RemainingItemCount = &remaining
		}, want: "negative remainingItemCount -1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			preflight, quotas, pods := runtimeQuotaFixture()
			tc.mutate(quotas, pods)
			err := preflight.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func runtimeQuotaFixture() (*crdupgrade.RuntimeResourceQuotaPreflight, *runtimeQuotaFakeQuotas, *runtimeQuotaFakePods) {
	contract := crdupgrade.RuntimeAdmissionContract{
		Namespace: "operators",
		CommonInitContainerResources: runtimeQuotaRequirements(
			map[corev1.ResourceName]string{
				corev1.ResourceCPU: "100m", corev1.ResourceMemory: "200Mi", corev1.ResourceEphemeralStorage: "3Gi",
			},
			map[corev1.ResourceName]string{
				corev1.ResourceCPU: "200m", corev1.ResourceMemory: "300Mi", corev1.ResourceEphemeralStorage: "4Gi",
			},
		),
		ControllerContainerResources: runtimeQuotaRequirements(
			map[corev1.ResourceName]string{
				corev1.ResourceCPU: "50m", corev1.ResourceMemory: "400Mi", corev1.ResourceEphemeralStorage: "2Gi",
			},
			map[corev1.ResourceName]string{
				corev1.ResourceCPU: "150m", corev1.ResourceMemory: "500Mi", corev1.ResourceEphemeralStorage: "2Gi",
			},
		),
		CertificateContainerResources: runtimeQuotaRequirements(
			map[corev1.ResourceName]string{
				corev1.ResourceCPU: "250m", corev1.ResourceMemory: "100Mi", corev1.ResourceEphemeralStorage: "5Gi",
			},
			map[corev1.ResourceName]string{
				corev1.ResourceCPU: "300m", corev1.ResourceMemory: "100Mi", corev1.ResourceEphemeralStorage: "6Gi",
			},
		),
		ControllerServiceAccountName:  "ptah-controller",
		CertificateServiceAccountName: "ptah-cert-rotator",
		CertificateRuntimeEnabled:     true,
	}
	quotas := &runtimeQuotaFakeQuotas{list: &corev1.ResourceQuotaList{
		ListMeta: metav1.ListMeta{ResourceVersion: "quota-rv"},
	}}
	pods := &runtimeQuotaFakePods{list: &corev1.PodList{
		ListMeta: metav1.ListMeta{ResourceVersion: "pod-rv"},
	}}
	return crdupgrade.NewRuntimeResourceQuotaPreflight(
		contract,
		2,
		"ptah",
		"operators",
		"ptah-controller",
		"ptah-cert-rotator",
		quotas,
		pods,
	), quotas, pods
}

func runtimeQuotaProtectedPod(preflight *crdupgrade.RuntimeResourceQuotaPreflight, controller bool, index int) corev1.Pod {
	deployment := preflight.CertificateDeploymentName
	serviceAccount := preflight.Contract.CertificateServiceAccountName
	component := "certificate-rotation"
	resources := preflight.Contract.CertificateContainerResources.DeepCopy()
	if controller {
		deployment = preflight.ControllerDeploymentName
		serviceAccount = preflight.Contract.ControllerServiceAccountName
		component = "controller"
		resources = preflight.Contract.ControllerContainerResources.DeepCopy()
	}
	hash := fmt.Sprintf("abc12%d", index)
	replicaSet := deployment + "-" + hash
	controllerValue := true
	blockOwnerDeletion := true
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:         fmt.Sprintf("%s-abcde", replicaSet),
			GenerateName: replicaSet + "-",
			Namespace:    preflight.ReleaseNamespace,
			UID:          types.UID(fmt.Sprintf("pod-%s-%d", component, index)),
			Labels: map[string]string{
				"app.kubernetes.io/name":               "ptah-operator",
				"app.kubernetes.io/instance":           preflight.ReleaseName,
				"app.kubernetes.io/component":          component,
				appsv1.DefaultDeploymentUniqueLabelKey: hash,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         "apps/v1",
				Kind:               "ReplicaSet",
				Name:               replicaSet,
				UID:                types.UID("rs-" + replicaSet),
				Controller:         &controllerValue,
				BlockOwnerDeletion: &blockOwnerDeletion,
			}},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccount,
			Containers: []corev1.Container{{
				Name:      "runtime",
				Resources: *resources,
			}},
			InitContainers: []corev1.Container{{
				Name:      "verify-candidate-runtime",
				Resources: *preflight.Contract.CommonInitContainerResources.DeepCopy(),
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func runtimeQuotaObject(name string, hard, used corev1.ResourceList) corev1.ResourceQuota {
	return corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "operators",
			UID:       types.UID("quota-" + name),
		},
		Spec: corev1.ResourceQuotaSpec{Hard: hard.DeepCopy()},
		Status: corev1.ResourceQuotaStatus{
			Hard: hard.DeepCopy(),
			Used: used.DeepCopy(),
		},
	}
}

func runtimeQuotaScope(scope corev1.ResourceQuotaScope, operator corev1.ScopeSelectorOperator, values ...string) *corev1.ScopeSelector {
	return &corev1.ScopeSelector{MatchExpressions: []corev1.ScopedResourceSelectorRequirement{{
		ScopeName: scope,
		Operator:  operator,
		Values:    values,
	}}}
}

func runtimeQuotaRequirements(requests, limits map[corev1.ResourceName]string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: runtimeQuotaResources(requests),
		Limits:   runtimeQuotaResources(limits),
	}
}

func runtimeQuotaResources(values map[corev1.ResourceName]string) corev1.ResourceList {
	result := make(corev1.ResourceList, len(values))
	for name, value := range values {
		result[name] = resource.MustParse(value)
	}
	return result
}

type runtimeQuotaFakeQuotas struct {
	list             *corev1.ResourceQuotaList
	pages            map[string]*corev1.ResourceQuotaList
	responses        []*corev1.ResourceQuotaList
	callErrors       []error
	err              error
	errorsByContinue map[string]error
	lastOptions      metav1.ListOptions
	calls            []metav1.ListOptions
}

type runtimeResourceQuotaPreflightTestFixture struct {
	quotas *runtimeQuotaFakeQuotas
	pods   *runtimeQuotaFakePods
}

func (f *runtimeQuotaFakeQuotas) List(_ context.Context, options metav1.ListOptions) (*corev1.ResourceQuotaList, error) {
	f.lastOptions = options
	f.calls = append(f.calls, options)
	callIndex := len(f.calls) - 1
	if callIndex < len(f.callErrors) && f.callErrors[callIndex] != nil {
		return nil, f.callErrors[callIndex]
	}
	if f.err != nil {
		return nil, f.err
	}
	if err := f.errorsByContinue[options.Continue]; err != nil {
		return nil, err
	}
	if f.responses != nil {
		if callIndex >= len(f.responses) {
			return nil, fmt.Errorf("unexpected ResourceQuota list call %d", callIndex+1)
		}
		if f.responses[callIndex] == nil {
			return nil, nil
		}
		return f.responses[callIndex].DeepCopy(), nil
	}
	if f.pages != nil {
		page, found := f.pages[options.Continue]
		if !found {
			return nil, fmt.Errorf("unexpected ResourceQuota continue token %q", options.Continue)
		}
		if page == nil {
			return nil, nil
		}
		return page.DeepCopy(), nil
	}
	if f.list == nil {
		return nil, nil
	}
	return f.list.DeepCopy(), nil
}

type runtimeQuotaFakePods struct {
	list             *corev1.PodList
	pages            map[string]*corev1.PodList
	callErrors       []error
	err              error
	errorsByContinue map[string]error
	lastOptions      metav1.ListOptions
	calls            []metav1.ListOptions
}

func (f *runtimeQuotaFakePods) List(_ context.Context, options metav1.ListOptions) (*corev1.PodList, error) {
	f.lastOptions = options
	f.calls = append(f.calls, options)
	callIndex := len(f.calls) - 1
	if callIndex < len(f.callErrors) && f.callErrors[callIndex] != nil {
		return nil, f.callErrors[callIndex]
	}
	if f.err != nil {
		return nil, f.err
	}
	if err := f.errorsByContinue[options.Continue]; err != nil {
		return nil, err
	}
	if f.pages != nil {
		page, found := f.pages[options.Continue]
		if !found {
			return nil, fmt.Errorf("unexpected Pod continue token %q", options.Continue)
		}
		if page == nil {
			return nil, nil
		}
		return page.DeepCopy(), nil
	}
	if f.list == nil {
		return nil, nil
	}
	return f.list.DeepCopy(), nil
}

func assertRuntimeQuotaListCalls(t *testing.T, resourceName string, calls []metav1.ListOptions, wantContinue []string) {
	t.Helper()
	if len(calls) != len(wantContinue) {
		t.Fatalf("%s list calls = %d, want %d: %#v", resourceName, len(calls), len(wantContinue), calls)
	}
	for index := range calls {
		if calls[index].Limit != 500 || calls[index].Continue != wantContinue[index] {
			t.Fatalf(
				"%s list call %d options = {Limit:%d Continue:%q}, want {Limit:500 Continue:%q}",
				resourceName,
				index,
				calls[index].Limit,
				calls[index].Continue,
				wantContinue[index],
			)
		}
	}
}

// A kubelet in the supported window reports pod-level status.resources and
// status.allocatedResources for an ordinary Pod that was never resized.
// Measured on Kubernetes 1.37.0: a Running cert-rotator Pod carries
// status.resources{requests:{cpu:5m,memory:16Mi},limits:{memory:32Mi}} and
// status.allocatedResources{cpu:5m,memory:16Mi}, mirroring its container spec.
//
// The preflight refused those fields on presence, so every protected Pod was
// refused, the pre-upgrade hook failed on every supported minor, and no release
// could be upgraded at all.
func TestRuntimeResourceQuotaPreflightAcceptsUnresizedPodLevelStatusResources(t *testing.T) {
	t.Parallel()

	preflight, quotas, pods := runtimeQuotaFixture()
	preflight.Contract.CertificateRuntimeEnabled = false
	preflight.ControllerReplicas = 1
	pod := runtimeQuotaProtectedPod(preflight, true, 0)
	requests := pod.Spec.Containers[0].Resources.Requests.DeepCopy()
	limits := pod.Spec.Containers[0].Resources.Limits.DeepCopy()
	pod.Status.Resources = &corev1.ResourceRequirements{Requests: requests, Limits: limits}
	pod.Status.AllocatedResources = requests.DeepCopy()
	pods.list.Items = []corev1.Pod{pod}
	quotas.list.Items = []corev1.ResourceQuota{runtimeQuotaObject(
		"actuated",
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "10"}),
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "100m"}),
	)}

	if err := preflight.Check(context.Background()); err != nil {
		t.Fatalf("Check() error = %v, want an unresized Pod to be accepted", err)
	}
}

// The control for the change above: dropping the presence refusal must not
// drop the guarantee it was standing in for. A container whose reported
// allocation differs from its spec is still refused, so a real resize is still
// caught -- by the container-level rule that was always the one doing the work.
func TestRuntimeResourceQuotaPreflightStillRejectsResizedPodCarryingPodLevelStatus(t *testing.T) {
	t.Parallel()

	preflight, quotas, pods := runtimeQuotaFixture()
	preflight.Contract.CertificateRuntimeEnabled = false
	preflight.ControllerReplicas = 1
	pod := runtimeQuotaProtectedPod(preflight, true, 0)
	requests := pod.Spec.Containers[0].Resources.Requests.DeepCopy()
	pod.Status.Resources = &corev1.ResourceRequirements{Requests: requests.DeepCopy()}
	pod.Status.AllocatedResources = requests.DeepCopy()
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:               "runtime",
		AllocatedResources: runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceCPU: "400m"}),
	}}
	pods.list.Items = []corev1.Pod{pod}
	quotas.list.Items = []corev1.ResourceQuota{runtimeQuotaObject(
		"actuated",
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "10"}),
		runtimeQuotaResources(map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "100m"}),
	)}

	err := preflight.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot be projected consistently across the supported Kubernetes window") {
		t.Fatalf("Check() error = %v, want the resized container to still be rejected", err)
	}
}
