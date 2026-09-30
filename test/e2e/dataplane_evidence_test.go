package e2e

import (
	"cmp"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	evidenceSchema      = "e2e-postgresql"
	evidenceSchemaUID   = types.UID("schema-uid")
	evidenceOperation   = "plan"
	evidenceJobUID      = types.UID("job-uid")
	evidenceJobName     = "e2e-postgresql-plan-abc"
	evidencePodUID      = types.UID("pod-uid")
	evidencePodName     = "e2e-postgresql-plan-abc-xyz12"
	evidenceProtocol    = int64(9)
	evidenceOperationID = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// validEvidence is a completed Plan Job the audit kept: the Job, its one
// succeeded Pod and the result its frame carried.
func validEvidence() *jobEvidence {
	label := operationLabel(evidenceOperationID)
	labels := map[string]string{
		labelManagedBy: managedByOperator, labelComponent: schemaOperationComponent,
		labelSchema: evidenceSchema, labelOperation: evidenceOperation, labelOperationID: label,
	}
	annotations := map[string]string{annotationOperationID: evidenceOperationID}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: evidenceJobName, UID: evidenceJobUID, Labels: labels, Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: ptahSchemaAPIVersion, Kind: "PtahSchema", Name: evidenceSchema, UID: evidenceSchemaUID,
				Controller: ptr.To(true),
			}},
		},
		Spec: batchv1.JobSpec{
			PodReplacementPolicy: ptr.To(batchv1.Failed), BackoffLimit: ptr.To[int32](0),
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations}},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: evidencePodName, UID: evidencePodUID, GenerateName: evidenceJobName + "-",
			Labels: labels, Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: evidenceJobName, UID: evidenceJobUID, Controller: ptr.To(true),
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "install-runner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "ptah", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			}},
		},
	}
	return &jobEvidence{
		job: job, pod: pod, log: []byte("frame"),
		result: runner.Result{ProtocolVersion: int(evidenceProtocol), Operation: runner.OperationPlan, OperationID: evidenceOperationID},
	}
}

func TestValidateJobEvidenceHoldsTheArchiveToItsJob(t *testing.T) {
	t.Parallel()
	if err := validateJobEvidence(validEvidence(), evidenceSchema, evidenceOperation, evidenceJobUID, evidenceSchemaUID, evidenceProtocol); err != nil {
		t.Fatalf("valid evidence refused: %v", err)
	}
	if err := validateJobEvidence(validEvidence(), evidenceSchema, evidenceOperation, evidenceJobUID, "", evidenceProtocol); err != nil {
		t.Fatalf("valid evidence refused with no schema UID expected: %v", err)
	}
	for _, test := range []struct {
		name string
		edit func(*jobEvidence)
		// schema, operation and uid are what the archive is read for, when
		// they differ from what it holds.
		schema, operation string
		uid, schemaUID    types.UID
	}{
		{name: "another schema", schema: "e2e-mysql"},
		{name: "another operation", operation: "observe"},
		{name: "another Job UID", uid: "replayed-uid"},
		{name: "another schema UID", schemaUID: "recreated-schema-uid"},
		{name: "operation ID not a digest", edit: func(e *jobEvidence) { e.job.Annotations[annotationOperationID] = "plain" }},
		{name: "operation label of another ID", edit: func(e *jobEvidence) { e.job.Labels[labelOperationID] = "0000000000000000" }},
		{name: "template of another operation", edit: func(e *jobEvidence) { e.job.Spec.Template.Labels[labelOperation] = "apply" }},
		{name: "not managed", edit: func(e *jobEvidence) { e.job.Labels[labelManagedBy] = "someone" }},
		{name: "two schema owners", edit: func(e *jobEvidence) {
			e.job.OwnerReferences = append(e.job.OwnerReferences, e.job.OwnerReferences[0])
		}},
		{name: "schema owner not a controller", edit: func(e *jobEvidence) { e.job.OwnerReferences[0].Controller = nil }},
		{name: "Pods replaced on termination", edit: func(e *jobEvidence) {
			e.job.Spec.PodReplacementPolicy = ptr.To(batchv1.TerminatingOrFailed)
		}},
		{name: "retries", edit: func(e *jobEvidence) { e.job.Spec.BackoffLimit = ptr.To[int32](1) }},
		{name: "Job failed", edit: func(e *jobEvidence) {
			e.job.Status.Conditions = append(e.job.Status.Conditions, batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue})
		}},
		{name: "Job not complete", edit: func(e *jobEvidence) { e.job.Status.Conditions = nil }},
		{name: "Pod of another generateName", edit: func(e *jobEvidence) { e.pod.GenerateName = "other-" }},
		{name: "Pod of another Job", edit: func(e *jobEvidence) { e.pod.OwnerReferences[0].UID = "other-job" }},
		{name: "Pod of another operation ID", edit: func(e *jobEvidence) { e.pod.Annotations = map[string]string{annotationOperationID: "x"} }},
		{name: "Pod failed", edit: func(e *jobEvidence) { e.pod.Status.Phase = corev1.PodFailed }},
		{name: "ptah exited non-zero", edit: func(e *jobEvidence) {
			e.pod.Status.ContainerStatuses[0].State.Terminated.ExitCode = 1
		}},
		{name: "an init container restarted", edit: func(e *jobEvidence) { e.pod.Status.InitContainerStatuses[0].RestartCount = 1 }},
		{name: "result of another protocol", edit: func(e *jobEvidence) { e.result.ProtocolVersion = 8 }},
		{name: "result of another operation", edit: func(e *jobEvidence) { e.result.Operation = runner.OperationObserve }},
		{name: "result of another operation ID", edit: func(e *jobEvidence) { e.result.OperationID = "other" }},
		{name: "truncated result", edit: func(e *jobEvidence) { e.result.Truncation = &runner.TruncationMetadata{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			evidence := validEvidence()
			if test.edit != nil {
				test.edit(evidence)
			}
			schema, operation := cmp.Or(test.schema, evidenceSchema), cmp.Or(test.operation, evidenceOperation)
			uid, schemaUID := evidenceJobUID, evidenceSchemaUID
			if test.uid != "" {
				uid = test.uid
			}
			if test.schemaUID != "" {
				schemaUID = test.schemaUID
			}
			if err := validateJobEvidence(evidence, schema, operation, uid, schemaUID, evidenceProtocol); err == nil {
				t.Fatal("the evidence was accepted")
			}
		})
	}
}

func TestSuppliedEvidenceIdentityBindsPodToJobToSchema(t *testing.T) {
	t.Parallel()
	valid := validEvidence()
	if err := suppliedEvidenceIdentity(valid.job, valid.pod, evidenceSchema, evidenceSchemaUID, evidenceOperation, evidenceOperationID); err != nil {
		t.Fatalf("valid supplied evidence refused: %v", err)
	}
	for name, edit := range map[string]func(*jobEvidence){
		"Pod of a same-name replacement Job": func(e *jobEvidence) { e.pod.OwnerReferences[0].UID = "replacement" },
		"Job of another schema UID":          func(e *jobEvidence) { e.job.OwnerReferences[0].UID = "recreated" },
		"template of another operation ID": func(e *jobEvidence) {
			e.job.Spec.Template.Annotations = map[string]string{annotationOperationID: "other"}
		},
		"Pod labeled for another schema": func(e *jobEvidence) {
			e.pod.Labels = map[string]string{labelSchema: "other", labelOperation: evidenceOperation}
		},
	} {
		evidence := validEvidence()
		edit(evidence)
		if err := suppliedEvidenceIdentity(evidence.job, evidence.pod, evidenceSchema, evidenceSchemaUID, evidenceOperation, evidenceOperationID); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	missingOwner := validEvidence()
	missingOwner.job.OwnerReferences = nil
	if _, err := schemaOwnerUID(missingOwner.job, evidenceSchema); err == nil {
		t.Error("a Job with no schema controller owner has a schema UID")
	}
}

// The same Job offered twice is the same Job. Anything else is a collision:
// the audit ran twice over two different objects under one UID.
func TestSameEvidenceIdentityRefusesACollision(t *testing.T) {
	t.Parallel()
	if err := sameEvidenceIdentity(validEvidence(), validEvidence(), evidenceSchema); err != nil {
		t.Fatalf("the same evidence twice was refused: %v", err)
	}
	for name, edit := range map[string]func(*jobEvidence){
		"schema UID":   func(e *jobEvidence) { e.job.OwnerReferences[0].UID = "recreated" },
		"operation ID": func(e *jobEvidence) { e.job.Annotations[annotationOperationID] = "sha256:" + strings.Repeat("2", 64) },
		"Job name":     func(e *jobEvidence) { e.job.Name = "renamed" },
		"Pod UID":      func(e *jobEvidence) { e.pod.UID = "replacement-pod" },
		"Pod name":     func(e *jobEvidence) { e.pod.Name = "replacement-pod" },
	} {
		supplied := validEvidence()
		edit(supplied)
		if err := sameEvidenceIdentity(validEvidence(), supplied, evidenceSchema); err == nil {
			t.Errorf("a collision on the %s was accepted", name)
		}
	}
}

func TestManagedCompleteJobIsAnOperationThatCompleted(t *testing.T) {
	t.Parallel()
	if !managedCompleteJob(validEvidence().job) {
		t.Fatal("a completed operation Job does not read as one")
	}
	for name, edit := range map[string]func(*batchv1.Job){
		"another manager":   func(job *batchv1.Job) { job.Labels[labelManagedBy] = "helm" },
		"no schema":         func(job *batchv1.Job) { delete(job.Labels, labelSchema) },
		"unknown operation": func(job *batchv1.Job) { job.Labels[labelOperation] = "migrate" },
		"failed": func(job *batchv1.Job) {
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
		},
	} {
		job := validEvidence().job
		edit(job)
		if managedCompleteJob(job) {
			t.Errorf("a Job with %s reads as a completed operation", name)
		}
	}
}

func admittedJob() *batchv1.Job {
	identity := controllerIdentity{image: "manager@sha256:" + strings.Repeat("a", 64), revision: "abc", stateVersion: "3"}
	annotations := map[string]string{
		annotationAdmissionDigest: "sha256:" + strings.Repeat("3", 64), annotationControllerImage: identity.image,
		annotationControllerRev: identity.revision, annotationControllerState: identity.stateVersion,
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "job", Labels: map[string]string{labelComponent: schemaOperationComponent}, Annotations: annotations},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Annotations: annotations},
			Spec:       corev1.PodSpec{RuntimeClassName: ptr.To(admissionRuntimeClass)},
		}},
	}
}

func TestJobAdmissionBindingIsOnTheJobAndItsTemplate(t *testing.T) {
	t.Parallel()
	identity := controllerIdentity{image: "manager@sha256:" + strings.Repeat("a", 64), revision: "abc", stateVersion: "3"}
	if !admittedUnderRuntimeClass(admittedJob()) {
		t.Fatal("a Job under the runtime class is not selected")
	}
	if err := jobAdmissionBinding(admittedJob(), identity); err != nil {
		t.Fatalf("a bound Job was refused: %v", err)
	}
	for name, edit := range map[string]func(*batchv1.Job){
		"digest not SHA-256": func(job *batchv1.Job) { job.Annotations[annotationAdmissionDigest] = "md5:x" },
		"template of another digest": func(job *batchv1.Job) {
			job.Spec.Template.Annotations = map[string]string{annotationAdmissionDigest: "sha256:" + strings.Repeat("4", 64)}
		},
		"another manager image": func(job *batchv1.Job) { job.Annotations[annotationControllerImage] = "other" },
		"another runtime class": func(job *batchv1.Job) { job.Spec.Template.Spec.RuntimeClassName = ptr.To("other") },
	} {
		job := admittedJob()
		// The template and the Job share one map in the fixture; copy it so an
		// edit to one is not an edit to both.
		job.Spec.Template.Annotations = cloneStrings(job.Annotations)
		edit(job)
		if err := jobAdmissionBinding(job, identity); err == nil {
			t.Errorf("a Job with %s was accepted", name)
		}
	}
}

func cloneStrings(values map[string]string) map[string]string {
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func TestTerminalPodEvidenceNeedsEveryContainerTerminated(t *testing.T) {
	t.Parallel()
	pod := validEvidence().pod
	pod.Spec = corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "install-runner"}}, Containers: []corev1.Container{{Name: "ptah"}},
	}
	if !terminalPodEvidence(pod, evidencePodUID, evidenceJobUID) {
		t.Fatal("a terminated Pod was refused")
	}
	for name, edit := range map[string]func(*corev1.Pod){
		"another UID":   func(p *corev1.Pod) { p.UID = "other" },
		"still running": func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning },
		"restarted":     func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 2 },
		"sidecar undeclared": func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "proxy"})
		},
	} {
		candidate := pod.DeepCopy()
		edit(candidate)
		if terminalPodEvidence(candidate, evidencePodUID, evidenceJobUID) {
			t.Errorf("a Pod %s was accepted", name)
		}
	}
	if terminalPodEvidence(pod, evidencePodUID, "another-job") {
		t.Error("a Pod of another Job was accepted")
	}
}

func admittedPod() *corev1.Pod {
	identity := controllerIdentity{image: "manager@sha256:" + strings.Repeat("a", 64), revision: "abc", stateVersion: "3"}
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
	}
	seconds := ptr.To[int64](300)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: evidencePodName, Namespace: "test", UID: evidencePodUID,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: evidenceJobName, UID: evidenceJobUID, Controller: ptr.To(true)}},
			Annotations: map[string]string{
				annotationAdmissionDigest: "sha256:" + strings.Repeat("3", 64), annotationControllerImage: identity.image,
				annotationControllerRev: identity.revision, annotationControllerState: identity.stateVersion,
			}},
		Spec: corev1.PodSpec{
			RuntimeClassName: ptr.To(admissionRuntimeClass), ServiceAccountName: "default",
			AutomountServiceAccountToken: ptr.To(false), NodeSelector: map[string]string{"kubernetes.io/os": "linux"},
			Overhead:         corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Mi")},
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: registryPullSecret}},
			InitContainers:   []corev1.Container{{Name: "install-runner", Resources: *resources.DeepCopy()}},
			Containers:       []corev1.Container{{Name: "ptah", Resources: *resources.DeepCopy()}},
			Tolerations: []corev1.Toleration{
				{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: seconds},
				{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: seconds},
				{Key: admissionRuntimeTaint, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
			},
		},
	}
}

func admissionJob() *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: evidenceJobName, Namespace: "test", UID: evidenceJobUID},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "ptah"}}, InitContainers: []corev1.Container{{Name: "install-runner"}},
		}}}}
}

func TestPodAdmissionAppliedCarriesEveryAdmissionDefault(t *testing.T) {
	t.Parallel()
	identity := controllerIdentity{image: "manager@sha256:" + strings.Repeat("a", 64), revision: "abc", stateVersion: "3"}
	if !podAdmissionApplied(admissionJob(), admittedPod(), identity, registryPullSecret) {
		t.Fatal("an admitted Pod was refused")
	}
	for name, edit := range map[string]func(*corev1.Pod){
		"a token mounted":        func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = ptr.To(true) },
		"another ServiceAccount": func(p *corev1.Pod) { p.Spec.ServiceAccountName = "runner" },
		"no overhead":            func(p *corev1.Pod) { p.Spec.Overhead = nil },
		"no pull Secret":         func(p *corev1.Pod) { p.Spec.ImagePullSecrets = nil },
		"a projected token": func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}}},
			}}}}
		},
		"an init container without the LimitRange": func(p *corev1.Pod) {
			p.Spec.InitContainers[0].Resources = corev1.ResourceRequirements{}
		},
		"the not-ready toleration bounded otherwise": func(p *corev1.Pod) {
			p.Spec.Tolerations[0].TolerationSeconds = ptr.To[int64](60)
		},
		"no runtime taint toleration": func(p *corev1.Pod) { p.Spec.Tolerations = p.Spec.Tolerations[:2] },
		"another manager revision": func(p *corev1.Pod) {
			p.Annotations[annotationControllerRev] = "def"
		},
	} {
		pod := admittedPod()
		edit(pod)
		if podAdmissionApplied(admissionJob(), pod, identity, registryPullSecret) {
			t.Errorf("a Pod with %s was accepted", name)
		}
	}
}

func TestPodAdmissionRetainsTheJobsDeclaredResourcesAndUnsetDefaults(t *testing.T) {
	t.Parallel()
	identity := controllerIdentity{image: "manager@sha256:" + strings.Repeat("a", 64), revision: "abc", stateVersion: "3"}
	job, pod := admissionJob(), admittedPod()
	budget := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	job.Spec.Template.Spec.Containers[0].Resources = *budget.DeepCopy()
	pod.Spec.Containers[0].Resources = *budget.DeepCopy()
	if !podAdmissionApplied(job, pod, identity, registryPullSecret) {
		t.Fatal("the native-plan budget with defaulted init-container resources was refused")
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"overwritten declared limit": func(p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("64Mi")
		},
		"overwritten declared request": func(p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("16Mi")
		},
		"extra resource": func(p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse("1Gi")
		},
		"unset init default": func(p *corev1.Pod) { delete(p.Spec.InitContainers[0].Resources.Requests, corev1.ResourceMemory) },
		"changed init default": func(p *corev1.Pod) {
			p.Spec.InitContainers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("200m")
		},
		"missing init container": func(p *corev1.Pod) { p.Spec.InitContainers = nil },
		"extra container":        func(p *corev1.Pod) { p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "extra"}) },
		"changed container name": func(p *corev1.Pod) { p.Spec.Containers[0].Name = "other" },
		"replaced Job":           func(p *corev1.Pod) { p.OwnerReferences[0].UID = "replacement" },
		"other namespace":        func(p *corev1.Pod) { p.Namespace = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := pod.DeepCopy()
			mutate(changed)
			if podAdmissionApplied(job, changed, identity, registryPullSecret) {
				t.Fatal("an unbound budget, missing default or different workload passed the audit")
			}
		})
	}
	if podAdmissionApplied(admissionJob(), pod, identity, registryPullSecret) ||
		podAdmissionApplied(nil, pod, identity, registryPullSecret) || podAdmissionApplied(job, nil, identity, registryPullSecret) {
		t.Fatal("an undeclared budget or missing workload evidence passed")
	}
	partial, admitted := admissionJob(), admittedPod()
	partial.Spec.Template.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m")}
	admitted.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("20m")
	if !podAdmissionApplied(partial, admitted, identity, registryPullSecret) {
		t.Fatal("a partial declaration did not retain the unset memory and limit defaults")
	}
	duplicated := job.DeepCopy()
	duplicated.Spec.Template.Spec.Containers = append(duplicated.Spec.Template.Spec.Containers, duplicated.Spec.Template.Spec.Containers[0])
	actual := pod.DeepCopy()
	actual.Spec.Containers = append(actual.Spec.Containers, actual.Spec.Containers[0])
	if podAdmissionApplied(duplicated, actual, identity, registryPullSecret) {
		t.Fatal("duplicate container identities passed")
	}
}

func TestOperationLabelIsTheOperationIDsDigestPrefix(t *testing.T) {
	t.Parallel()
	// printf '%s' "$id" | sha256sum | cut -c1-16, for the fixture's ID.
	if got := operationLabel("abc"); got != "ba7816bf8f01cfea" {
		t.Fatalf("operationLabel(abc) = %s", got)
	}
}
