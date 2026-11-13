package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// A credential is a file in a Secret volume, and a Secret or a key the volume
// cannot find stops the Pod before any container: the kubelet says why only in
// a FailedMount Event against the Pod (measured on kind 1.36). These rows read
// that Event onto the reading, and no other.

const (
	mountMissingSecret = `MountVolume.SetUp failed for volume "database-url" : secret "application-database" not found`
	mountMissingKey    = `MountVolume.SetUp failed for volume "database-url" : references non-existent secret key: url`
	mountWaitTimedOut  = `Unable to attach or mount volumes: unmounted volumes=[database-url], unattached volumes=[]: timed out waiting for the condition`
)

// unmountedPod is the Pod of job on a node, every container waiting in the
// default state the kubelet gives a Pod whose volumes it has not set up.
func unmountedPod(job *batchv1.Job) *corev1.Pod {
	waiting := func(name, reason string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: job.Namespace, Name: job.Name + "-abcde", UID: "unmounted-pod",
			Labels:          map[string]string{batchv1.ControllerUidLabel: string(job.UID)},
			OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
		},
		Spec: corev1.PodSpec{
			NodeName: "worker",
			Volumes: []corev1.Volume{
				{Name: "database-url", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "application-database"}}},
				{Name: "registry-ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "registry-ca"},
				}}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReadyToStartContainers, Status: corev1.ConditionFalse},
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
			InitContainerStatuses: []corev1.ContainerStatus{waiting("install-runner", "PodInitializing")},
			ContainerStatuses:     []corev1.ContainerStatus{waiting(executorContainerName, "PodInitializing")},
		},
	}
}

// mountEvent is what the kubelet records against pod, last seen at seen.
func mountEvent(pod *corev1.Pod, name, reason, message string, seen time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name + "." + name},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "v1", Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID,
		},
		Reason: reason, Message: message, Type: corev1.EventTypeWarning,
		LastTimestamp: metav1.NewTime(seen),
	}
}

func TestPodConfigurationErrorReadsTheVolumeTheKubeletCouldNotSetUp(t *testing.T) {
	t.Parallel()

	earlier := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Minute)
	type fixture struct {
		pod    *corev1.Pod
		events []*corev1.Event
	}
	for _, row := range []struct {
		name   string
		mutate func(*fixture)
		want   string
		// listsEvents says the reading has to ask the API server for the
		// Pod's Events, and a row that says no holds it to not asking.
		listsEvents bool
	}{
		{
			name: "the Secret is missing",
			mutate: func(f *fixture) {
				f.events = append(f.events, mountEvent(f.pod, "a", failedMountEventReason, mountMissingSecret, earlier))
			},
			want: mountMissingSecret, listsEvents: true,
		},
		{
			name: "the key is missing, which names no Secret",
			mutate: func(f *fixture) {
				f.events = append(f.events, mountEvent(f.pod, "a", failedMountEventReason, mountMissingKey, earlier))
			},
			want:        mountMissingKey + " (volume database-url reads Secret team-a/application-database)",
			listsEvents: true,
		},
		{
			name: "a ConfigMap key is missing",
			mutate: func(f *fixture) {
				f.events = append(f.events, mountEvent(f.pod, "a", failedMountEventReason,
					`MountVolume.SetUp failed for volume "registry-ca" : references non-existent config key: ca.pem`, earlier))
			},
			want:        `MountVolume.SetUp failed for volume "registry-ca" : references non-existent config key: ca.pem (volume registry-ca reads ConfigMap team-a/registry-ca)`,
			listsEvents: true,
		},
		{
			// The Secret arrived without the key: the newer Event says so,
			// whichever the API server lists first.
			name: "the newest of two reasons",
			mutate: func(f *fixture) {
				f.events = append(f.events,
					mountEvent(f.pod, "b", failedMountEventReason, mountMissingKey, later),
					mountEvent(f.pod, "a", failedMountEventReason, mountMissingSecret, earlier))
			},
			want:        mountMissingKey + " (volume database-url reads Secret team-a/application-database)",
			listsEvents: true,
		},
		{
			name: "the aggregator's prefix",
			mutate: func(f *fixture) {
				f.events = append(f.events, mountEvent(f.pod, "a", failedMountEventReason, combinedEvents+mountMissingSecret, earlier))
			},
			want: mountMissingSecret, listsEvents: true,
		},
		{
			name: "the kubelet's wait timed out, which says no reason",
			mutate: func(f *fixture) {
				f.events = append(f.events, mountEvent(f.pod, "a", failedMountEventReason, mountWaitTimedOut, later))
			},
			listsEvents: true,
		},
		{
			name: "another reason in the same words",
			mutate: func(f *fixture) {
				f.events = append(f.events, mountEvent(f.pod, "a", "FailedScheduling", mountMissingSecret, earlier))
			},
			listsEvents: true,
		},
		{
			name: "an Event against another Pod",
			mutate: func(f *fixture) {
				event := mountEvent(f.pod, "a", failedMountEventReason, mountMissingSecret, earlier)
				event.InvolvedObject.UID = "another-pod"
				f.events = append(f.events, event)
			},
			listsEvents: true,
		},
		{
			name: "the sandbox is ready, so the volumes were set up",
			mutate: func(f *fixture) {
				f.pod.Status.Conditions[0].Status = corev1.ConditionTrue
				f.events = append(f.events, mountEvent(f.pod, "a", failedMountEventReason, mountMissingSecret, earlier))
			},
		},
		{
			name: "an init container ran, so the volumes were set up",
			mutate: func(f *fixture) {
				f.pod.Status.InitContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
				f.events = append(f.events, mountEvent(f.pod, "a", failedMountEventReason, mountMissingSecret, earlier))
			},
		},
		{
			name: "a container waits for another reason",
			mutate: func(f *fixture) {
				f.pod.Status.ContainerStatuses[0].State.Waiting.Reason = "ImagePullBackOff"
				f.events = append(f.events, mountEvent(f.pod, "a", failedMountEventReason, mountMissingSecret, earlier))
			},
		},
		{
			name: "a Pod on no node",
			mutate: func(f *fixture) {
				f.pod.Spec.NodeName = ""
				f.events = append(f.events, mountEvent(f.pod, "a", failedMountEventReason, mountMissingSecret, earlier))
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "ptah-observe-app", UID: "job-uid"},
				Status:     batchv1.JobStatus{Active: 1},
			}
			f := &fixture{pod: unmountedPod(job)}
			row.mutate(f)
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "application-database"},
				Data:       map[string][]byte{"url": []byte("postgres://app:DO_NOT_PRINT@db/app")},
			}
			objects := []client.Object{f.pod, secret}
			for _, event := range f.events {
				objects = append(objects, event)
			}
			listedEvents := false
			reader := interceptor.NewClient(
				withEventIndex(fake.NewClientBuilder().WithScheme(scheme.Scheme)).WithObjects(objects...).WithStatusSubresource(f.pod).Build(),
				interceptor.Funcs{
					Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
						if _, isSecret := object.(*corev1.Secret); isSecret {
							t.Errorf("read Secret %s", key)
						}
						return inner.Get(ctx, key, object, options...)
					},
					List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
						switch list.(type) {
						case *corev1.SecretList:
							t.Error("listed Secrets")
						case *corev1.EventList:
							listedEvents = true
						}
						return inner.List(ctx, list, options...)
					},
				},
			)

			reading, err := podConfigurationError(context.Background(), reader, job)
			if err != nil {
				t.Fatalf("podConfigurationError() error = %v", err)
			}
			if !reading.alive {
				t.Fatal("podConfigurationError() read a live Pod as gone")
			}
			if listedEvents != row.listsEvents {
				t.Fatalf("podConfigurationError() listed the Pod's Events = %t, want %t", listedEvents, row.listsEvents)
			}
			if strings.Contains(reading.message, "DO_NOT_PRINT") {
				t.Fatalf("podConfigurationError() carried the Secret's value: %q", reading.message)
			}
			if row.want == "" {
				if reading.message != "" {
					t.Fatalf("podConfigurationError() = %q, want nothing", reading.message)
				}
				return
			}
			if want := "Job ptah-observe-app cannot start its Pod: " + row.want; reading.message != want {
				t.Fatalf("podConfigurationError() = %q, want %q", reading.message, want)
			}
		})
	}
}
