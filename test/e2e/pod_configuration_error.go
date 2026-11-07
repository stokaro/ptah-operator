package e2e

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// podConfigurationReported is a schema holding an operation and saying its Pod
// cannot start: Ready False for PodConfigurationError, in the kubelet's words.
// words is what those words must contain.
func podConfigurationReported(schema *ptahv1alpha1.PtahSchema, words string) bool {
	return schema.Status.ActiveOperation != nil &&
		slices.ContainsFunc(schema.Status.Conditions, func(condition metav1.Condition) bool {
			return condition.Type == "Ready" && condition.Status == metav1.ConditionFalse &&
				condition.Reason == "PodConfigurationError" && strings.Contains(condition.Message, words)
		})
}

// podConfigurationEventRecorded is a PodConfigurationError Warning against this
// exact schema in the same words.
func podConfigurationEventRecorded(events []corev1.Event, schema *ptahv1alpha1.PtahSchema, words string) bool {
	return slices.ContainsFunc(events, func(event corev1.Event) bool {
		reference := event.InvolvedObject
		return event.Type == corev1.EventTypeWarning && event.Reason == "PodConfigurationError" &&
			reference.Kind == "PtahSchema" && reference.Name == schema.Name && reference.UID == schema.UID &&
			strings.Contains(event.Message, words)
	})
}

// kubeletReportedMissingKey is a Pod of the Job whose container waits as the
// kubelet leaves one it cannot configure, in the words the condition carries. It
// says the words on the resource are the kubelet's, so what the row reads for a
// credential is what the kubelet wrote.
func kubeletReportedMissingKey(pods []corev1.Pod, job, words string) bool {
	return slices.ContainsFunc(pods, func(pod corev1.Pod) bool {
		if pod.Labels["job-name"] != job {
			return false
		}
		return slices.ContainsFunc(slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses),
			func(status corev1.ContainerStatus) bool {
				waiting := status.State.Waiting
				return waiting != nil && waiting.Reason == "CreateContainerConfigError" && strings.Contains(waiting.Message, words)
			})
	})
}

// migrationPodConfigurationReported is a migration holding an operation and
// saying its Pod cannot start, in the kubelet's words.
func migrationPodConfigurationReported(migration *ptahv1alpha1.PtahMigration, words string) bool {
	return migration.Status.ActiveOperation != nil &&
		slices.ContainsFunc(migration.Status.Conditions, func(condition metav1.Condition) bool {
			return condition.Type == ptahv1alpha1.ConditionMigrationProgressing && condition.Status == metav1.ConditionFalse &&
				condition.Reason == "PodConfigurationError" && strings.Contains(condition.Message, words)
		})
}

// migrationFailureNamesUnstartedPod is the failure a read leaves past its Job's
// deadline: no result arrived, and why, on the condition a reader looks at.
func migrationFailureNamesUnstartedPod(migration *ptahv1alpha1.PtahMigration, words string) bool {
	return slices.ContainsFunc(migration.Status.Conditions, func(condition metav1.Condition) bool {
		return condition.Type == ptahv1alpha1.ConditionMigrationProgressing && condition.Reason == "OperationFailed" &&
			strings.Contains(condition.Message, "result frame not found") && strings.Contains(condition.Message, words)
	})
}

// holdsNoSecretValue refuses a document that carries a value of the Secret, the
// whole of it or the part the row names as the credential.
func holdsNoSecretValue(document []byte, values ...string) error {
	if len(document) == 0 {
		return fmt.Errorf("there is no document to read for a credential")
	}
	for _, value := range values {
		if value == "" {
			return fmt.Errorf("there is no value to look for")
		}
		if bytes.Contains(document, []byte(value)) {
			return fmt.Errorf("the document carries a value of the Secret")
		}
	}
	return nil
}
