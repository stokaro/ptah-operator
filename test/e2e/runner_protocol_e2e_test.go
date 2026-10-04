//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

func publishRunnerProtocolVariant(t *testing.T, ctx context.Context, cluster *harness.Cluster, namespace, name, fixture, source string,
	protect func(...string), scan func([]byte, string),
) (string, *batchv1.Job) {
	t.Helper()
	repository, digest, pinned := strings.Cut(source, "@")
	host, _, path := strings.Cut(repository, "/")
	if !pinned || !path || !sha256Pattern.MatchString(digest) || !digestSuffix.MatchString(fixture) {
		t.Fatal("unsupported runner publication needs exact task image digests")
	}
	credentials := &corev1.Secret{}
	storedStateCheck(t, cluster.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: registryAuthSecret}, credentials), "read the runner fixture's registry grant")
	if string(credentials.Data["registry"]) != host || len(credentials.Data["username"]) == 0 || len(credentials.Data["password"]) == 0 {
		t.Fatal("unsupported runner publication escaped its task registry grant")
	}
	if protect != nil {
		protect(string(credentials.Data["password"]))
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data: map[string][]byte{"registry": []byte(host), "username": slices.Clone(credentials.Data["username"]), "password": slices.Clone(credentials.Data["password"])}}
	storedStateCheck(t, cluster.Client.Create(ctx, secret), "create the registry-only runner publication grant")
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: batchv1.JobSpec{
		BackoffLimit: ptr.To(int32(0)), ActiveDeadlineSeconds: ptr.To(int64(180)),
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: ptr.To(false),
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: registryPullSecret}},
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers: []corev1.Container{{Name: "publisher", Image: fixture, ImagePullPolicy: corev1.PullIfNotPresent,
				Command: []string{"/e2e-handcraft-oci"}, Args: []string{"runner-protocol-variant", source},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
				},
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
					Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
			}},
		}},
	}}
	for _, entry := range []struct{ name, key string }{{"PTAH_OCI_REGISTRY", "registry"}, {"PTAH_OCI_USERNAME", "username"}, {"PTAH_OCI_PASSWORD", "password"}} {
		job.Spec.Template.Spec.Containers[0].Env = append(job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: entry.name,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: entry.key}}})
	}
	storedStateCheck(t, cluster.Client.Create(ctx, job), "publish the unsupported runner into the private task registry")
	uid := job.UID
	var pod *corev1.Pod
	storedStateCheck(t, harness.Wait(ctx, "the exact unsupported runner publication", 3*time.Minute, time.Second,
		func(ctx context.Context) (bool, string, error) {
			if err := cluster.Client.Get(ctx, client.ObjectKeyFromObject(job), job); err != nil {
				return false, "publication Job missing", err
			}
			if job.UID != uid || !publisherJobIsolation(job, fixture, name) {
				return false, "publication identity or isolation changed", fmt.Errorf("the unsupported runner publisher changed identity or acquired a credential")
			}
			if conditionTrue(job.Status.Conditions, batchv1.JobFailed) {
				return false, "publication failed", fmt.Errorf("the exact unsupported runner publisher failed")
			}
			pods := &corev1.PodList{}
			if err := cluster.Client.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{"job-name": name}); err != nil {
				return false, "publication Pod list failed", err
			}
			owned := ownedPods(pods.Items, uid)
			if len(owned) > 1 {
				return false, "publisher replayed", fmt.Errorf("the unsupported runner publisher created multiple Pods")
			}
			if len(owned) == 1 && owned[0].Status.Phase == corev1.PodSucceeded && noRestarts(&owned[0]) && conditionTrue(job.Status.Conditions, batchv1.JobComplete) {
				pod = owned[0].DeepCopy()
				return true, "publication completed", nil
			}
			return false, "waiting for the one exact publisher Pod", nil
		}), "complete the unsupported runner publication")
	logs, err := cluster.ContainerLog(ctx, namespace, pod.Name, "publisher")
	storedStateCheck(t, err, "read the exact unsupported runner publisher's log")
	scan(logs, "unsupported runner publication output")
	for _, object := range []any{job, pod} {
		raw, err := json.Marshal(object)
		storedStateCheck(t, err, "encode unsupported runner publication evidence")
		scan(raw, "unsupported runner publication Kubernetes metadata")
	}
	retained := &corev1.Pod{}
	storedStateCheck(t, cluster.Client.Get(ctx, client.ObjectKeyFromObject(pod), retained), "retain the exact publisher Pod UID")
	if retained.UID != pod.UID {
		t.Fatal("the unsupported runner publisher Pod was replaced during its log audit")
	}
	updated, err := runnerVariantReference(source, string(logs))
	storedStateCheck(t, err, "bind the unsupported runner to its exact published image")
	t.Logf("unsupported runner fixture: original=%s replacement=%s; refusal scope only", source, updated)
	return updated, job.DeepCopy()
}

func waitRunnerApply(t *testing.T, ctx context.Context, cluster *harness.Cluster, namespace, label, name string, scan func([]byte, string)) runnerProtocolApply {
	t.Helper()
	var pair runnerProtocolApply
	storedStateCheck(t, harness.Wait(ctx, "the exact unsupported-runner Apply transport", waitTimeout, time.Second,
		func(ctx context.Context) (bool, string, error) {
			jobs, pods := &batchv1.JobList{}, &corev1.PodList{}
			selector := client.MatchingLabels{label: name, labelOperation: "apply"}
			if err := cluster.Client.List(ctx, jobs, client.InNamespace(namespace), selector); err != nil {
				return false, "Apply Jobs could not be read", err
			}
			if err := cluster.Client.List(ctx, pods, client.InNamespace(namespace), selector); err != nil {
				return false, "Apply Pods could not be read", err
			}
			if len(jobs.Items) > 1 || len(pods.Items) > 1 {
				return false, "Apply was replayed", fmt.Errorf("the unsupported runner's admitted decision dispatched multiple workloads")
			}
			if len(jobs.Items) != 1 || len(pods.Items) != 1 || !jobTerminal(&jobs.Items[0]) {
				return false, "waiting for one terminal Apply Job and Pod", nil
			}
			job, pod := jobs.Items[0].DeepCopy(), pods.Items[0].DeepCopy()
			if job.UID == "" || pod.UID == "" || !ownedExactlyOnce(pod.OwnerReferences, "batch/v1", "Job", job.Name, job.UID) || !noRestarts(pod) {
				return false, "Apply identity was lost", fmt.Errorf("unsupported runner Apply has missing ownership or restarted containers")
			}
			if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				return false, "waiting for the exact terminal Pod", nil
			}
			pair = runnerProtocolApply{job, pod}
			return true, "exact terminal workload received", nil
		}), "retain the refused runner's actual workload")
	for _, object := range []any{pair.job, pair.pod} {
		raw, err := json.Marshal(object)
		storedStateCheck(t, err, "encode unsupported runner Apply evidence")
		scan(raw, "unsupported runner Apply Kubernetes evidence")
	}
	for _, container := range startedContainers(pair.pod) {
		logs, err := cluster.ContainerLog(ctx, namespace, pair.pod.Name, container)
		storedStateCheck(t, err, "read the refused runner's exact started container log")
		scan(logs, "unsupported runner complete container log")
	}
	retained := &corev1.Pod{}
	storedStateCheck(t, cluster.Client.Get(ctx, client.ObjectKeyFromObject(pair.pod), retained), "recheck the refused runner's exact Pod UID")
	if retained.UID != pair.pod.UID {
		t.Fatal("the refused runner Pod was replaced while its logs were audited")
	}
	return pair
}

// Capture every failed client during inventory polling, including read-only
// guards. A missing archive fails attribution even if Kubernetes already GC'd
// that client's Pod; absence is never treated as a successful log audit.
func retainRunnerRefusalLogs(ctx context.Context, cluster *harness.Cluster, pods map[types.UID]corev1.Pod,
	retained map[types.UID]runnerRefusalLogs, scan func([]byte, string),
) error {
	for uid, pod := range pods {
		if pod.Status.Phase != corev1.PodFailed {
			continue
		}
		if evidence, found := retained[uid]; found {
			if err := evidence.matches(&pod); err != nil {
				return err
			}
			continue
		}
		evidence, err := captureRunnerRefusalLogs(&pod, func() (*corev1.Pod, error) {
			current := &corev1.Pod{}
			err := cluster.Client.Get(ctx, client.ObjectKeyFromObject(&pod), current)
			return current, err
		}, func(container string) ([]byte, error) {
			return cluster.ContainerLog(ctx, pod.Namespace, pod.Name, container)
		}, scan)
		if err != nil {
			return err
		}
		retained[uid] = evidence
	}
	return nil
}

func closeRunnerWatches(t *testing.T, recorders []recorder, scan func([]byte, string)) {
	t.Helper()
	for _, r := range recorders {
		storedStateCheck(t, r.alive(), "retain the live runner proof watch")
		r.requestStop()
	}
	for _, r := range recorders {
		storedStateCheck(t, r.await(35*time.Second), "receive the runner proof watch's natural EOF")
		raw, count, err := r.history()
		storedStateCheck(t, err, "read the complete runner proof history")
		if count == 0 {
			t.Fatal("the runner proof closed an empty history")
		}
		scan(raw, "complete runner proof watch history")
	}
}
