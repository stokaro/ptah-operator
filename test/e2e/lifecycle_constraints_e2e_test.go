//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// installWithQuotaAndPodSecurity starts after uninstall removed the runtime.
// The exact candidate first fails to create its reconcile hook under a zero
// Job quota, then the same chart and values recover with quota available.
// Restricted Pod Security stays enforced through the subsequent uninstall.
func (l *lifecycleRun) installWithQuotaAndPodSecurity() {
	l.t.Helper()
	namespace := &corev1.Namespace{}
	l.check(l.cluster.Client.Get(l.ctx, types.NamespacedName{Name: l.in.operatorNamespace}, namespace), "read the release namespace")
	original := namespace.DeepCopy()
	labels := map[string]string{}
	for _, mode := range []string{"enforce", "warn", "audit"} {
		labels["pod-security.kubernetes.io/"+mode] = "restricted"
		labels["pod-security.kubernetes.io/"+mode+"-version"] = "v" + l.kubernetesMajorMinor
	}
	t := l.t
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current := &corev1.Namespace{}
			if err := l.cluster.Client.Get(ctx, types.NamespacedName{Name: original.Name}, current); err != nil {
				return err
			}
			if current.UID != original.UID {
				return fmt.Errorf("release namespace changed UID during admission proof")
			}
			if current.Labels == nil {
				current.Labels = map[string]string{}
			}
			for key := range labels {
				if value, exists := original.Labels[key]; exists {
					current.Labels[key] = value
				} else {
					delete(current.Labels, key)
				}
			}
			return l.cluster.Client.Update(ctx, current)
		})
		if err != nil {
			t.Errorf("restore Pod Security labels: %v", err)
		}
	})
	if namespace.Labels == nil {
		namespace.Labels = map[string]string{}
	}
	for key, value := range labels {
		namespace.Labels[key] = value
	}
	l.check(l.cluster.Client.Update(l.ctx, namespace), "enforce restricted Pod Security")
	allowed := lifecycleAdmissionProbe(namespace.Name, l.in.controllerImage)
	forbidden := allowed.DeepCopy()
	// Host PID sharing is valid Pod API input but forbidden by Pod Security.
	// Combining privileged=true with allowPrivilegeEscalation=false would
	// also violate API validation and could not isolate the intended guard.
	forbidden.Spec.HostPID = true
	l.check(harness.Wait(l.ctx, "restricted Pod Security to refuse the host-PID control", 30*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			err := l.cluster.Client.Create(ctx, forbidden.DeepCopy(), client.DryRunAll)
			if lifecyclePodSecurityRefusal(err, forbidden.Name, l.kubernetesMajorMinor) {
				return true, "", nil
			}
			if err != nil {
				return false, "Pod admission failed at a different guard", err
			}
			return false, "waiting for the namespace's restricted policy", nil
		}), "observe the Pod Security refusal")
	l.check(l.cluster.Client.Create(l.ctx, allowed, client.DryRunAll), "admit the otherwise identical restricted Pod control")

	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: lifecycleInstallQuota, Namespace: namespace.Name},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			"count/jobs.batch": resource.MustParse("0"), corev1.ResourcePods: resource.MustParse("24"),
		}},
	}
	l.check(l.cluster.Client.Create(l.ctx, quota), "create the exhausted installation quota")
	quotaUID := quota.UID
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := l.cluster.Client.Delete(ctx, quota, client.Preconditions{UID: &quotaUID}); client.IgnoreNotFound(err) != nil {
			t.Errorf("remove the installation quota: %v", err)
		}
	})
	l.waitInstallationQuota(quota, 0)
	probe := lifecycleQuotaProbe(namespace.Name, l.in.controllerImage)
	if err := l.cluster.Client.Create(l.ctx, probe.DeepCopy(), client.DryRunAll); !lifecycleQuotaRefusal(err, probe.Name) {
		l.fatalf("the zero Job quota did not produce its exact API refusal")
	}
	candidate, err := lifecycleFailureReadCandidate(l.in.chartPackage, l.in.candidateValuesFile, l.in.controllerImage)
	l.check(err, "record the constrained installation's chart and values")
	l.prepareExpectedHookNames(l.in.chartPackage, l.in.candidateValuesFile)
	_, stderr, err := l.helm("install", l.in.helmRelease, l.in.chartPackage, "--namespace", namespace.Name,
		"--values", l.in.candidateValuesFile, "--force-conflicts", "--wait", "--timeout", "2m")
	if err == nil {
		l.fatalf("installation succeeded with no quota for its hook Job")
	}
	l.printDebug(stderr)
	status := l.mustHelm("read the failed installation revision", "status", l.in.helmRelease,
		"--namespace", namespace.Name, "--revision", "1", "-o", "json")
	l.check(lifecycleQuotaInstallRefused(status, l.in.helmRelease, namespace.Name, l.expectedReconcileHookName, 1),
		"identify the hook Job quota refusal")
	l.assertProofUnchanged("-before")
	deployments := &appsv1.DeploymentList{}
	l.check(l.cluster.Client.List(l.ctx, deployments, client.InNamespace(namespace.Name)), "read Deployments after quota refusal")
	if len(deployments.Items) != 0 {
		l.fatalf("the refused installation left %d runtime Deployments", len(deployments.Items))
	}
	jobs := &batchv1.JobList{}
	l.check(l.cluster.Client.List(l.ctx, jobs, client.InNamespace(namespace.Name)), "read Jobs after quota refusal")
	if len(jobs.Items) != 0 {
		l.fatalf("the zero Job quota admitted %d Jobs", len(jobs.Items))
	}
	l.logf("PASS restricted Pod Security enforced; exact candidate hook Job refused by quota before runtime creation")

	l.check(l.cluster.Client.Get(l.ctx, client.ObjectKeyFromObject(quota), quota), "read the quota before recovery")
	if quota.UID != quotaUID {
		l.fatalf("the installation quota was replaced before recovery")
	}
	quota.Spec.Hard["count/jobs.batch"] = resource.MustParse("512")
	l.check(l.cluster.Client.Update(l.ctx, quota), "allow installation Job quota")
	l.waitInstallationQuota(quota, 512)
	l.check(l.cluster.Client.Create(l.ctx, probe), "create the allowed suspended Job quota control")
	probeUID := probe.UID
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := l.cluster.Client.Delete(ctx, probe, client.Preconditions{UID: &probeUID}); client.IgnoreNotFound(err) != nil {
			t.Errorf("remove the quota control Job: %v", err)
		}
	})
	l.check(l.cluster.Client.Delete(l.ctx, probe, client.Preconditions{UID: &probe.UID}), "delete the exact quota control Job")
	l.check(harness.Wait(l.ctx, "quota control Job removal before the Helm retry", time.Minute, time.Second,
		func(ctx context.Context) (bool, string, error) {
			err := l.cluster.Client.Get(ctx, client.ObjectKeyFromObject(probe), &batchv1.Job{})
			if apierrors.IsNotFound(err) {
				return true, "", nil
			}
			return false, "waiting for the suspended Job to disappear", err
		}), "remove the quota control")
	current, err := lifecycleFailureReadCandidate(l.in.chartPackage, l.in.candidateValuesFile, l.in.controllerImage)
	l.check(err, "read the candidate before retry")
	if !lifecycleFailureCandidateUnchanged(candidate, current) {
		l.fatalf("quota recovery changed the candidate chart, values or runtime image")
	}
	l.mustHelm("retry the same candidate after restoring Job quota", "upgrade", "--install", l.in.helmRelease,
		l.in.chartPackage, "--namespace", namespace.Name, "--values", l.in.candidateValuesFile, "--wait", "--timeout", "5m")
	if l.deployedRevision() != 2 {
		l.fatalf("quota recovery did not deploy revision 2 of the same release")
	}
	l.logf("PASS the same candidate recovered from the refused install with quota available and restricted Pod Security still enforced")
}

func (l *lifecycleRun) waitInstallationQuota(quota *corev1.ResourceQuota, jobs int64) {
	l.t.Helper()
	l.check(harness.Wait(l.ctx, "installation quota accounting to catch up", time.Minute, time.Second,
		func(ctx context.Context) (bool, string, error) {
			current := &corev1.ResourceQuota{}
			if err := l.cluster.Client.Get(ctx, client.ObjectKeyFromObject(quota), current); err != nil {
				return false, "", err
			}
			if current.UID != quota.UID {
				return false, "", fmt.Errorf("installation quota changed UID")
			}
			hard, found := current.Status.Hard["count/jobs.batch"]
			used, measured := current.Status.Used["count/jobs.batch"]
			return found && measured && hard.Value() == jobs && used.Value() == 0,
				"waiting for the exact quota limit and measured zero Job use", nil
		}), "observe installation quota accounting")
}
