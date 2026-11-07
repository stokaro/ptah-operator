package e2e

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const (
	missingSecretWords = `secret "e2e-postgresql-pod-configuration-db" not found`
	missingKeyWords    = "couldn't find key url in Secret"
)

func podConfigurationSchema(mutate func(*ptahv1alpha1.PtahSchema)) *ptahv1alpha1.PtahSchema {
	schema := &ptahv1alpha1.PtahSchema{}
	schema.Name, schema.UID = "e2e-pod-configuration", "schema-uid"
	schema.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{JobName: "observe-job"}
	schema.Status.Conditions = []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: "PodConfigurationError",
		Message: "Job observe-job cannot start its Pod: " + missingSecretWords,
	}}
	if mutate != nil {
		mutate(schema)
	}
	return schema
}

func TestPodConfigurationReportedIsTheConditionInTheKubeletsWords(t *testing.T) {
	t.Parallel()
	if !podConfigurationReported(podConfigurationSchema(nil), missingSecretWords) {
		t.Fatal("podConfigurationReported() refused the reading the operator writes")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"the claim is gone":        func(schema *ptahv1alpha1.PtahSchema) { schema.Status.ActiveOperation = nil },
		"an operation in progress": func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Conditions[0].Reason = "OperationInProgress" },
		"a Pod admission refused":  func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Conditions[0].Reason = "PodAdmissionRefused" },
		"another condition":        func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Conditions[0].Type = "PlanReady" },
		"Ready is True":            func(schema *ptahv1alpha1.PtahSchema) { schema.Status.Conditions[0].Status = metav1.ConditionTrue },
		"other words": func(schema *ptahv1alpha1.PtahSchema) {
			schema.Status.Conditions[0].Message = "Job observe-job cannot start its Pod: " + missingKeyWords
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if podConfigurationReported(podConfigurationSchema(mutate), missingSecretWords) {
				t.Fatal("podConfigurationReported() accepted it")
			}
		})
	}
}

func TestPodConfigurationEventIsThisSchemasWarningInTheSameWords(t *testing.T) {
	t.Parallel()
	event := func(mutate func(*corev1.Event)) []corev1.Event {
		recorded := corev1.Event{
			InvolvedObject: corev1.ObjectReference{Kind: "PtahSchema", Name: "e2e-pod-configuration", UID: "schema-uid"},
			Type:           corev1.EventTypeWarning, Reason: "PodConfigurationError",
			Message: "Job observe-job cannot start its Pod: " + missingSecretWords,
		}
		if mutate != nil {
			mutate(&recorded)
		}
		return []corev1.Event{recorded}
	}
	schema := podConfigurationSchema(nil)
	if !podConfigurationEventRecorded(event(nil), schema, missingSecretWords) {
		t.Fatal("podConfigurationEventRecorded() refused the Event the operator records")
	}
	if podConfigurationEventRecorded(nil, schema, missingSecretWords) {
		t.Fatal("podConfigurationEventRecorded() accepted no Event")
	}
	for name, mutate := range map[string]func(*corev1.Event){
		"another UID of the name": func(event *corev1.Event) { event.InvolvedObject.UID = "other" },
		"a Pod's Failed Event":    func(event *corev1.Event) { event.InvolvedObject.Kind = "Pod"; event.Reason = "Failed" },
		"a Normal Event":          func(event *corev1.Event) { event.Type = corev1.EventTypeNormal },
		"other words":             func(event *corev1.Event) { event.Message = missingKeyWords },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if podConfigurationEventRecorded(event(mutate), schema, missingSecretWords) {
				t.Fatal("podConfigurationEventRecorded() accepted it")
			}
		})
	}
}

func TestKubeletReportedMissingKeyReadsThisJobsWaitingContainer(t *testing.T) {
	t.Parallel()
	pod := func(mutate func(*corev1.Pod)) []corev1.Pod {
		waiting := corev1.Pod{}
		waiting.Labels = map[string]string{"job-name": "observe-job"}
		waiting.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: missingKeyWords + " ns/name"},
		}}}
		if mutate != nil {
			mutate(&waiting)
		}
		return []corev1.Pod{waiting}
	}
	if !kubeletReportedMissingKey(pod(nil), "observe-job", missingKeyWords) {
		t.Fatal("kubeletReportedMissingKey() refused the status the kubelet writes")
	}
	if !kubeletReportedMissingKey(pod(func(waiting *corev1.Pod) {
		waiting.Status.InitContainerStatuses, waiting.Status.ContainerStatuses = waiting.Status.ContainerStatuses, nil
	}), "observe-job", missingKeyWords) {
		t.Fatal("kubeletReportedMissingKey() refused an init container")
	}
	if kubeletReportedMissingKey(nil, "observe-job", missingKeyWords) {
		t.Fatal("kubeletReportedMissingKey() accepted no Pod")
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"another Job's Pod": func(waiting *corev1.Pod) { waiting.Labels["job-name"] = "other" },
		"another reason": func(waiting *corev1.Pod) {
			waiting.Status.ContainerStatuses[0].State.Waiting.Reason = "ImagePullBackOff"
		},
		"other words": func(waiting *corev1.Pod) {
			waiting.Status.ContainerStatuses[0].State.Waiting.Message = missingSecretWords
		},
		"a running container": func(waiting *corev1.Pod) {
			waiting.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if kubeletReportedMissingKey(pod(mutate), "observe-job", missingKeyWords) {
				t.Fatal("kubeletReportedMissingKey() accepted it")
			}
		})
	}
}

func TestHoldsNoSecretValueRefusesAValueAndAnEmptyReading(t *testing.T) {
	t.Parallel()
	const url = "postgres://ptah_e2e:e2ePg1Q7@db:5432/app"
	document := []byte(`{"message":"couldn't find key url in Secret ns/name"}`)
	if err := holdsNoSecretValue(document, url, "e2ePg1Q7"); err != nil {
		t.Fatalf("holdsNoSecretValue() refused a document that holds neither: %v", err)
	}
	for name, row := range map[string]struct {
		document []byte
		values   []string
	}{
		"the whole URL":       {document: []byte(`{"message":"` + url + `"}`), values: []string{url, "e2ePg1Q7"}},
		"the password alone":  {document: []byte(`{"message":"e2ePg1Q7"}`), values: []string{url, "e2ePg1Q7"}},
		"nothing to read":     {values: []string{url}},
		"nothing to look for": {document: document, values: []string{""}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := holdsNoSecretValue(row.document, row.values...); err == nil {
				t.Fatal("holdsNoSecretValue() accepted it")
			}
		})
	}
}

func TestMigrationPodConfigurationReadingsHoldTheConditionToTheWords(t *testing.T) {
	t.Parallel()
	migration := func(mutate func(*ptahv1alpha1.PtahMigration)) *ptahv1alpha1.PtahMigration {
		resource := &ptahv1alpha1.PtahMigration{}
		resource.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{JobName: "history-job"}
		resource.Status.Conditions = []metav1.Condition{{
			Type: ptahv1alpha1.ConditionMigrationProgressing, Status: metav1.ConditionFalse, Reason: "PodConfigurationError",
			Message: "Job history-job cannot start its Pod: " + missingSecretWords,
		}}
		if mutate != nil {
			mutate(resource)
		}
		return resource
	}
	if !migrationPodConfigurationReported(migration(nil), missingSecretWords) {
		t.Fatal("migrationPodConfigurationReported() refused the reading the operator writes")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration){
		"the claim is gone": func(resource *ptahv1alpha1.PtahMigration) { resource.Status.ActiveOperation = nil },
		"in progress": func(resource *ptahv1alpha1.PtahMigration) {
			resource.Status.Conditions[0].Reason = "OperationInProgress"
		},
		"Progressing True": func(resource *ptahv1alpha1.PtahMigration) {
			resource.Status.Conditions[0].Status = metav1.ConditionTrue
		},
		"another condition":     func(resource *ptahv1alpha1.PtahMigration) { resource.Status.Conditions[0].Type = "Ready" },
		"the words of a failed": func(resource *ptahv1alpha1.PtahMigration) { resource.Status.Conditions[0].Message = missingKeyWords },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if migrationPodConfigurationReported(migration(mutate), missingSecretWords) {
				t.Fatal("migrationPodConfigurationReported() accepted it")
			}
		})
	}

	failed := func(reason, message string) *ptahv1alpha1.PtahMigration {
		resource := &ptahv1alpha1.PtahMigration{}
		resource.Status.Conditions = []metav1.Condition{{Type: ptahv1alpha1.ConditionMigrationProgressing, Status: metav1.ConditionTrue, Reason: reason, Message: message}}
		return resource
	}
	said := "read History result: ptah runner result frame not found; Job history-job cannot start its Pod: " + missingSecretWords
	if !migrationFailureNamesUnstartedPod(failed("OperationFailed", said), missingSecretWords) {
		t.Fatal("migrationFailureNamesUnstartedPod() refused the failure the operator writes")
	}
	for name, resource := range map[string]*ptahv1alpha1.PtahMigration{
		// What this reading exists to refuse: the failure with the reason lost.
		"the failure with nothing about the Pod": failed("OperationFailed", "read History result: ptah runner result frame not found"),
		"the words with no failure":              failed("PodConfigurationError", "Job history-job cannot start its Pod: "+missingSecretWords),
		"another reason":                         failed("OperationInProgress", said),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if migrationFailureNamesUnstartedPod(resource, missingSecretWords) {
				t.Fatal("migrationFailureNamesUnstartedPod() accepted it")
			}
		})
	}
}
