package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const lifecycleInstallQuota = "e2e-install-quota"

// This Pod is only submitted with dryRun=All. Its host-PID variant differs
// in one field, so a refusal naming that field proves Pod Security is active.
func lifecycleAdmissionProbe(namespace, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-install-admission", Namespace: namespace},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: ptr.To(false),
			RestartPolicy:                corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name: "probe", Image: image,
				// These admission-only controls never run a container. Keep
				// explicit bounds if a probe is accidentally unsuspended.
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
					Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}

// A suspended Job exercises quota admission without creating a Pod. A real
// allowed CREATE, followed by UID-bound deletion, is the positive control.
func lifecycleQuotaProbe(namespace, image string) *batchv1.Job {
	pod := lifecycleAdmissionProbe(namespace, image)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-install-quota-control", Namespace: namespace},
		Spec:       batchv1.JobSpec{Suspend: ptr.To(true), Template: corev1.PodTemplateSpec{Spec: pod.Spec}},
	}
}

func lifecyclePodSecurityRefusal(err error, pod, minor string) bool {
	var status apierrors.APIStatus
	if !apierrors.IsForbidden(err) || !errors.As(err, &status) {
		return false
	}
	details := status.Status().Details
	message := status.Status().Message
	return pod != "" && minor != "" && details != nil && details.Group == "" && details.Kind == "pods" && details.Name == pod &&
		strings.Contains(message, `violates PodSecurity "restricted:v`+minor+`"`) &&
		strings.Contains(message, "host namespaces (hostPID=true)")
}

func lifecycleQuotaRefusal(err error, job string) bool {
	var status apierrors.APIStatus
	if !apierrors.IsForbidden(err) || !errors.As(err, &status) {
		return false
	}
	details := status.Status().Details
	return job != "" && details != nil && details.Group == "batch" && details.Kind == "jobs" && details.Name == job &&
		strings.Contains(status.Status().Message, "exceeded quota: "+lifecycleInstallQuota+",") &&
		strings.Contains(status.Status().Message, "limited: count/jobs.batch=0")
}

// Helm records a failed hook CREATE in the release description. This is not
// the failed-hook-pod path: quota refused the Job before any Pod could run.
func lifecycleQuotaInstallRefused(raw []byte, release, namespace, hook string, revision int) error {
	var status struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Version   int    `json:"version"`
		Info      struct {
			Status      string `json:"status"`
			Description string `json:"description"`
		} `json:"info"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return errors.New("quota refusal has no valid Helm status")
	}
	if release == "" || namespace == "" || hook == "" || revision < 1 ||
		status.Name != release || status.Namespace != namespace || status.Version != revision || status.Info.Status != "failed" {
		return errors.New("quota refusal is not the expected failed release revision")
	}
	for _, required := range []string{
		fmt.Sprintf(`jobs.batch %q is forbidden`, hook),
		"exceeded quota: " + lifecycleInstallQuota + ",",
		"limited: count/jobs.batch=0",
	} {
		if !strings.Contains(status.Info.Description, required) {
			return errors.New("failed install does not identify the intended hook Job quota refusal")
		}
	}
	return nil
}
