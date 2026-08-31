package podintent_test

import (
	"context"
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/workload"
)

func TestValidationHandlerAcceptsResolvedPodBeforeScheduling(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	response := handler.Handle(context.Background(), podRequest(t, pod))
	if !response.Allowed {
		t.Fatalf("Handle() denied resolved Pod: %#v", response.Result)
	}
}

func TestValidationHandlerRejectsMutationBeforeScheduling(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	pod.Spec.Containers[0].Command = []string{"/bin/sh"}
	response := handler.Handle(context.Background(), podRequest(t, pod))
	if response.Allowed {
		t.Fatal("Handle() allowed a changed executable command")
	}
}

func TestValidationHandlerRejectsUnboundPodMetadata(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	pod.Annotations["unbound.example/inject"] = "true"
	response := handler.Handle(context.Background(), podRequest(t, pod))
	if response.Allowed {
		t.Fatal("Handle() allowed Pod metadata outside the bound Job template")
	}
}

func TestValidationHandlerRejectsChangedJobExecutionEnvelope(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*batchv1.Job){
		"parallelism": func(job *batchv1.Job) { value := int32(2); job.Spec.Parallelism = &value },
		"completions": func(job *batchv1.Job) { value := int32(2); job.Spec.Completions = &value },
		"backoff":     func(job *batchv1.Job) { value := int32(1); job.Spec.BackoffLimit = &value },
		"deadline": func(job *batchv1.Job) {
			value := *job.Spec.ActiveDeadlineSeconds + 1
			job.Spec.ActiveDeadlineSeconds = &value
		},
		"replacement policy": func(job *batchv1.Job) {
			value := batchv1.TerminatingOrFailed
			job.Spec.PodReplacementPolicy = &value
		},
		"indexed mode": func(job *batchv1.Job) {
			value := batchv1.IndexedCompletion
			job.Spec.CompletionMode = &value
		},
		"suspended": func(job *batchv1.Job) { value := true; job.Spec.Suspend = &value },
		"manual selector": func(job *batchv1.Job) {
			value := true
			job.Spec.ManualSelector = &value
		},
		"custom controller": func(job *batchv1.Job) {
			value := "example.test/job-controller"
			job.Spec.ManagedBy = &value
		},
		"cleanup TTL": func(job *batchv1.Job) { value := int32(300); job.Spec.TTLSecondsAfterFinished = &value },
		"selector drift": func(job *batchv1.Job) {
			job.Spec.Selector.MatchLabels["untrusted.example/select"] = "true"
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			handler, pod := validationHandlerFixture(t)
			api := handler.Reader.(client.Client)
			job := &batchv1.Job{}
			key := client.ObjectKey{Namespace: pod.Namespace, Name: pod.OwnerReferences[0].Name}
			if err := api.Get(context.Background(), key, job); err != nil {
				t.Fatal(err)
			}
			mutate(job)
			if err := api.Update(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			response := handler.Handle(context.Background(), podRequest(t, pod))
			if response.Allowed {
				t.Fatalf("Handle() allowed changed Job execution envelope for %s", name)
			}
		})
	}
}

func TestValidationHandlerRejectsInjectedFinalizer(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	pod.Finalizers = append(pod.Finalizers, "untrusted.example/hold")
	response := handler.Handle(context.Background(), podRequest(t, pod))
	if response.Allowed {
		t.Fatal("Handle() allowed an injected Pod finalizer")
	}
}

func TestValidationHandlerAcceptsOnlyLimitRangerMutationAnnotation(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	pod.Annotations["kubernetes.io/limit-ranger"] = "LimitRanger plugin set: cpu request for container ptah"
	response := handler.Handle(context.Background(), podRequest(t, pod))
	if !response.Allowed {
		t.Fatalf("Handle() denied the LimitRanger mutation annotation: %#v", response.Result)
	}

	pod.Annotations["kubernetes.io/limit-ranger"] = "caller-supplied value"
	response = handler.Handle(context.Background(), podRequest(t, pod))
	if response.Allowed {
		t.Fatal("Handle() allowed an unrecognized LimitRanger annotation value")
	}
}

func TestValidationHandlerRejectsEphemeralContainerUpdate(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	original := pod.DeepCopy()
	pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "busybox:latest"},
	}}
	request := podUpdateRequest(t, original, pod)
	request.SubResource = "ephemeralcontainers"
	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("Handle() allowed an ephemeral container on an operation Pod")
	}
}

func TestValidationHandlerRejectsResizeUpdate(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	original := pod.DeepCopy()
	pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("192Mi")
	request := podUpdateRequest(t, original, pod)
	request.SubResource = "resize"
	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("Handle() allowed an out-of-envelope Pod resize")
	}
}

func TestValidationHandlerRejectsUpdateForDifferentPodUID(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	original := pod.DeepCopy()
	pod.UID = "replacement-pod-uid"
	response := handler.Handle(context.Background(), podUpdateRequest(t, original, pod))
	if response.Allowed {
		t.Fatal("Handle() allowed an update rebound to another Pod UID")
	}
}

func TestValidationHandlerAllowsBenignMetadataUpdate(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	original := pod.DeepCopy()
	pod.Annotations["kubernetes.io/benign-observation"] = "present"
	response := handler.Handle(context.Background(), podUpdateRequest(t, original, pod))
	if !response.Allowed {
		t.Fatalf("Handle() denied a metadata-only update: %#v", response.Result)
	}
}

func TestValidationHandlerAllowsExactJobTrackingFinalizerCleanupWithoutLiveJob(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	pod.Status.Phase = corev1.PodSucceeded
	api := handler.Reader.(client.Client)
	job := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: pod.OwnerReferences[0].Name}, job); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	original := pod.DeepCopy()
	pod.Finalizers = nil
	response := handler.Handle(context.Background(), podUpdateRequest(t, original, pod))
	if !response.Allowed {
		t.Fatalf("Handle() denied exact Job tracking finalizer cleanup without a live Job: %#v", response.Result)
	}
}

func TestValidationHandlerAllowsExactJobTrackingFinalizerCleanupAfterOperationClears(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	pod.Status.Phase = corev1.PodFailed
	api := handler.Reader.(client.Client)
	schema := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: "app"}, schema); err != nil {
		t.Fatal(err)
	}
	schema.Status.ActiveOperation = nil
	if err := api.Update(context.Background(), schema); err != nil {
		t.Fatal(err)
	}
	original := pod.DeepCopy()
	pod.Finalizers = nil
	response := handler.Handle(context.Background(), podUpdateRequest(t, original, pod))
	if !response.Allowed {
		t.Fatalf("Handle() denied exact Job tracking finalizer cleanup after status cleared: %#v", response.Result)
	}
}

func TestValidationHandlerAllowsFinalizerCleanupWithServerLifecycleMetadata(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	api := handler.Reader.(client.Client)
	job := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: pod.OwnerReferences[0].Name}, job); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	original := pod.DeepCopy()
	pod.Finalizers = nil
	pod.ResourceVersion = "server-prepared-resource-version"
	pod.Generation++
	now := metav1.Now()
	grace := int64(0)
	pod.DeletionTimestamp = &now
	pod.DeletionGracePeriodSeconds = &grace
	pod.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kube-controller-manager"}}
	response := handler.Handle(context.Background(), podUpdateRequest(t, original, pod))
	if !response.Allowed {
		t.Fatalf("Handle() denied Job finalizer cleanup after server lifecycle preparation: %#v", response.Result)
	}
}

func TestValidationHandlerRejectsFinalizerCleanupWithSpecMutation(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	original := pod.DeepCopy()
	pod.Finalizers = nil
	pod.Spec.Containers[0].Command = []string{"/bin/sh"}
	response := handler.Handle(context.Background(), podUpdateRequest(t, original, pod))
	if response.Allowed {
		t.Fatal("Handle() allowed a spec mutation disguised as Job finalizer cleanup")
	}
}

func TestValidationHandlerRejectsLiveJobTrackingFinalizerCleanup(t *testing.T) {
	t.Parallel()

	for _, phase := range []corev1.PodPhase{corev1.PodPending, corev1.PodRunning} {
		phase := phase
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()

			handler, pod := validationHandlerFixture(t)
			pod.Status.Phase = phase
			original := pod.DeepCopy()
			pod.Finalizers = nil
			response := handler.Handle(context.Background(), podUpdateRequest(t, original, pod))
			if response.Allowed {
				t.Fatalf("Handle() allowed Job tracking finalizer cleanup from a %s Pod", phase)
			}
		})
	}
}

func TestValidationHandlerRejectsChangedRuntimeClassBinding(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	api := handler.Reader.(client.Client)
	runtimeClass := &nodev1.RuntimeClass{}
	if err := api.Get(context.Background(), client.ObjectKey{Name: "sandbox"}, runtimeClass); err != nil {
		t.Fatal(err)
	}
	runtimeClass.Handler = "changed-handler"
	if err := api.Update(context.Background(), runtimeClass); err != nil {
		t.Fatal(err)
	}
	response := handler.Handle(context.Background(), podRequest(t, pod))
	if response.Allowed {
		t.Fatal("Handle() allowed a Pod after its bound RuntimeClass changed")
	}
}

func TestValidationHandlerRejectsReplacedJobOwner(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	pod.OwnerReferences[0].UID = "replaced-job-uid"
	response := handler.Handle(context.Background(), podRequest(t, pod))
	if response.Allowed {
		t.Fatal("Handle() allowed a replaced Job owner")
	}
}

func TestValidationHandlerRejectsAmbiguousJobOwnership(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	pod.OwnerReferences = append(pod.OwnerReferences, metav1.OwnerReference{
		APIVersion: "example.test/v1",
		Kind:       "Observer",
		Name:       "extra-owner",
		UID:        "extra-owner-uid",
	})
	response := handler.Handle(context.Background(), podRequest(t, pod))
	if response.Allowed {
		t.Fatal("Handle() allowed a Job-controlled Pod with ambiguous ownership")
	}
}

func TestValidationHandlerRejectsReplacementJobRedefiningIntentBeforeUIDPersistence(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	api, ok := handler.Reader.(client.Client)
	if !ok {
		t.Fatal("validation fixture reader is not a client")
	}
	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: pod.Namespace, Name: pod.OwnerReferences[0].Name}
	if err := api.Get(context.Background(), key, job); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	replacement := job.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "replacement-job-uid"
	replacement.Spec.Template.Spec.Containers[0].Command = []string{"/bin/sh"}
	for key, value := range map[string]string{
		batchv1.ControllerUidLabel: string(replacement.UID),
		batchv1.JobNameLabel:       replacement.Name,
		"controller-uid":           string(replacement.UID),
		"job-name":                 replacement.Name,
	} {
		replacement.Spec.Template.Labels[key] = value
		pod.Labels[key] = value
	}
	if err := api.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	pod.OwnerReferences[0].UID = replacement.UID
	pod.Spec.Containers[0].Command = []string{"/bin/sh"}
	response := handler.Handle(context.Background(), podRequest(t, pod))
	if response.Allowed {
		t.Fatal("Handle() allowed a replacement Job to redefine executable intent before JobUID persistence")
	}
}

func TestValidationHandlerIgnoresUnmanagedPods(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	pod.OwnerReferences = nil
	pod.Labels = nil
	response := handler.Handle(context.Background(), podRequest(t, pod))
	if !response.Allowed {
		t.Fatalf("Handle() denied unrelated Pod: %#v", response.Result)
	}
}

func TestValidationHandlerAllowsUnmanagedJobPodForCertificateRecovery(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	api := handler.Reader.(client.Client)
	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: pod.Namespace, Name: pod.OwnerReferences[0].Name}
	if err := api.Get(context.Background(), key, job); err != nil {
		t.Fatal(err)
	}
	job.OwnerReferences = nil
	job.Labels[workload.LabelManagedBy] = "Helm"
	job.Labels[workload.LabelComponent] = "certificate-rotation"
	if err := api.Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	pod.Labels[workload.LabelManagedBy] = "Helm"
	pod.Labels[workload.LabelComponent] = "certificate-rotation"
	response := handler.Handle(context.Background(), podRequest(t, pod))
	if !response.Allowed {
		t.Fatalf("Handle() denied an unrelated certificate-rotation Job Pod: %#v", response.Result)
	}
}

func validationHandlerFixture(t *testing.T) (*podintent.ValidationHandler, *corev1.Pod) {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		batchv1.AddToScheme,
		nodev1.AddToScheme,
		schedulingv1.AddToScheme,
		operatorv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	operationID := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	labels := map[string]string{
		workload.LabelManagedBy:   "ptah-operator",
		workload.LabelComponent:   "schema-operation",
		workload.LabelSchema:      "app",
		workload.LabelOperation:   "apply",
		workload.LabelOperationID: workload.OperationIDLabelValue(operationID),
	}
	annotations := map[string]string{workload.AnnotationOperationID: operationID}
	options := podintent.DefaultOptions()
	options.ExtendedResourceTolerationEnabled = true
	options.AlwaysPullImagesEnabled = true
	template, snapshot := resolvedFixtureWithOptionsAndMetadata(t, options, metav1.ObjectMeta{
		Labels: labels, Annotations: annotations,
	})
	deadline := int64(900)
	template.ActiveDeadlineSeconds = &deadline
	// The deadline is part of the pre-admission template digest.
	snapshot, err := podintent.Resolve(context.Background(),
		fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(admissionFixtureObjects()...).Build(),
		"team-a", &corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations}, Spec: *template.DeepCopy()}, options)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		batchv1.ControllerUidLabel: "job-uid",
		batchv1.JobNameLabel:       "apply-job",
		"controller-uid":           "job-uid",
		"job-name":                 "apply-job",
	} {
		labels[key] = value
	}
	annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshot.Digest
	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app", UID: "schema-uid", ResourceVersion: "21"},
		Status: operatorv1alpha1.PtahSchemaStatus{ActiveOperation: &operatorv1alpha1.ActiveOperationStatus{
			Type: operatorv1alpha1.OperationApply, ID: operationID, JobName: "apply-job", AdmissionSnapshot: snapshot,
		}},
	}
	parallelism := int32(1)
	completions := int32(1)
	backoffLimit := int32(0)
	replacementPolicy := batchv1.Failed
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "apply-job", UID: "job-uid", ResourceVersion: "22",
			Labels: labels, Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(schema, operatorv1alpha1.GroupVersion.WithKind("PtahSchema"))},
		},
		Spec: batchv1.JobSpec{
			Parallelism: &parallelism, Completions: &completions, BackoffLimit: &backoffLimit,
			ActiveDeadlineSeconds: &deadline, PodReplacementPolicy: &replacementPolicy,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{batchv1.ControllerUidLabel: "job-uid"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations}, Spec: *template.DeepCopy(),
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "apply-job-pod", GenerateName: "apply-job-", UID: "pod-uid", ResourceVersion: "23",
			Labels: labels, Annotations: annotations, Finalizers: []string{batchv1.JobTrackingFinalizer},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))},
		},
		Spec: *admittedPodSpec(template, snapshot),
	}
	objects := append(admissionFixtureObjects(), schema, job)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
	return &podintent.ValidationHandler{Reader: reader, Decoder: cradmission.NewDecoder(scheme)}, pod
}

func podRequest(t *testing.T, pod *corev1.Pod) cradmission.Request {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	return cradmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: types.UID("admission-request"), Namespace: pod.Namespace, Name: pod.Name,
		Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: raw},
	}}
}

func podUpdateRequest(t *testing.T, oldPod, pod *corev1.Pod) cradmission.Request {
	t.Helper()
	request := podRequest(t, pod)
	request.Operation = admissionv1.Update
	raw, err := json.Marshal(oldPod)
	if err != nil {
		t.Fatal(err)
	}
	request.OldObject = runtime.RawExtension{Raw: raw}
	return request
}
