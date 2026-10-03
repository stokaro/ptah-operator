package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// The labels and annotations the operator puts on every operation Job and Pod.
const (
	labelManagedBy            = "app.kubernetes.io/managed-by"
	labelComponent            = "app.kubernetes.io/component"
	labelSchema               = "operator.ptah.run/schema"
	labelOperation            = "operator.ptah.run/operation"
	labelOperationID          = "operator.ptah.run/operation-id"
	annotationOperationID     = "operator.ptah.run/operation-id"
	annotationAdmissionDigest = "operator.ptah.run/admission-snapshot-digest"
	annotationControllerImage = "operator.ptah.run/controller-image"
	annotationControllerRev   = "operator.ptah.run/controller-revision"
	annotationControllerState = "operator.ptah.run/controller-state-version"
	managedByOperator         = "ptah-operator"
	schemaOperationComponent  = "schema-operation"
	ptahSchemaAPIVersion      = "operator.ptah.run/v1alpha1"
)

var schemaOperations = []string{"resolve", "verify", "observe", "plan", "apply"}

// operationLabel is the label value an operation ID becomes: the first
// sixteen hex characters of its SHA-256.
func operationLabel(operationID string) string {
	sum := sha256.Sum256([]byte(operationID))
	return hex.EncodeToString(sum[:])[:16]
}

// jobEvidence is what the phase keeps of one completed operation Job: the Job
// and its one Pod as they were read when the Job was audited, the ptah
// container's diagnostic log, and the validated result read through the Job's
// selected transport. The controller stamps a TTL on every Job it finished
// reading, and a lifecycle
// outlasts it, so a proof about a Job's history reads this rather than the API.
type jobEvidence struct {
	job    *batchv1.Job
	pod    *corev1.Pod
	log    []byte
	result runner.Result
}

func conditionTrue(conditions []batchv1.JobCondition, kind batchv1.JobConditionType) bool {
	for _, condition := range conditions {
		if condition.Type == kind && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// jobComplete is a Job that completed and did not fail.
func jobComplete(job *batchv1.Job) bool {
	return conditionTrue(job.Status.Conditions, batchv1.JobComplete) &&
		!conditionTrue(job.Status.Conditions, batchv1.JobFailed)
}

// jobTerminal is a Job that completed or failed.
func jobTerminal(job *batchv1.Job) bool {
	return conditionTrue(job.Status.Conditions, batchv1.JobComplete) ||
		conditionTrue(job.Status.Conditions, batchv1.JobFailed)
}

func isController(reference metav1.OwnerReference) bool {
	return reference.Controller != nil && *reference.Controller
}

// controllerOwners is every controller reference of the kind given.
func controllerOwners(references []metav1.OwnerReference, apiVersion, kind string) []metav1.OwnerReference {
	var owners []metav1.OwnerReference
	for _, reference := range references {
		if reference.APIVersion == apiVersion && reference.Kind == kind && isController(reference) {
			owners = append(owners, reference)
		}
	}
	return owners
}

// ownedExactlyOnce reports whether references hold exactly one controller
// reference to the object named.
func ownedExactlyOnce(references []metav1.OwnerReference, apiVersion, kind, name string, uid types.UID) bool {
	count := 0
	for _, owner := range controllerOwners(references, apiVersion, kind) {
		if owner.Name == name && owner.UID == uid {
			count++
		}
	}
	return count == 1
}

// schemaOwnerUID is the UID of the one PtahSchema controller reference named
// for schema.
func schemaOwnerUID(job *batchv1.Job, schema string) (types.UID, error) {
	var uids []types.UID
	for _, owner := range controllerOwners(job.OwnerReferences, ptahSchemaAPIVersion, "PtahSchema") {
		if owner.Name == schema && owner.UID != "" {
			uids = append(uids, owner.UID)
		}
	}
	if len(uids) != 1 {
		return "", errors.New("supplied Job evidence lacks one exact PtahSchema controller owner UID")
	}
	return uids[0], nil
}

func allStatuses(pod *corev1.Pod) []corev1.ContainerStatus {
	return slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses,
		pod.Status.EphemeralContainerStatuses)
}

func noRestarts(pod *corev1.Pod) bool {
	for _, status := range allStatuses(pod) {
		if status.RestartCount != 0 {
			return false
		}
	}
	return true
}

// ptahExitedZero is a Pod whose one ptah container terminated with status 0.
func ptahExitedZero(pod *corev1.Pod) bool {
	count := 0
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "ptah" && status.State.Terminated != nil && status.State.Terminated.ExitCode == 0 {
			count++
		}
	}
	return count == 1
}

// suppliedEvidenceIdentity holds a Job and its Pod to the identity the phase
// is about to archive them under: the names, the UIDs, the operation labels
// and annotations on both and on the Job's template, and the two owner
// references that tie the Pod to the Job and the Job to the schema.
func suppliedEvidenceIdentity(job *batchv1.Job, pod *corev1.Pod, schema string, schemaUID types.UID,
	operation, operationID string,
) error {
	label := operationLabel(operationID)
	template := job.Spec.Template
	if job.Labels[labelSchema] != schema || job.Labels[labelOperation] != operation ||
		job.Labels[labelOperationID] != label || job.Annotations[annotationOperationID] != operationID ||
		!ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, "PtahSchema", schema, schemaUID) ||
		template.Labels[labelSchema] != schema || template.Labels[labelOperation] != operation ||
		template.Labels[labelOperationID] != label || template.Annotations[annotationOperationID] != operationID ||
		pod.GenerateName != job.Name+"-" ||
		pod.Labels[labelSchema] != schema || pod.Labels[labelOperation] != operation ||
		pod.Labels[labelOperationID] != label || pod.Annotations[annotationOperationID] != operationID ||
		!ownedExactlyOnce(pod.OwnerReferences, "batch/v1", "Job", job.Name, job.UID) {
		return errors.New("supplied Job and Pod evidence has a mismatched immutable identity or owner binding")
	}
	return nil
}

// validateJobEvidence holds archived evidence to the Job it is filed under:
// the schema and operation, the Job UID, and the schema UID when one is
// expected. The Job completed with no replacement and no retry, its one Pod
// succeeded with no container restarted, and the result is the one this
// operation ID produced, whole.
func validateJobEvidence(evidence *jobEvidence, schema, operation string, uid, expectedSchemaUID types.UID,
	protocolVersion int64,
) error {
	job, pod := evidence.job, evidence.pod
	operationID := job.Annotations[annotationOperationID]
	if !sha256Pattern.MatchString(operationID) {
		return errors.New("job evidence carries no SHA-256 operation ID")
	}
	schemaUID, err := schemaOwnerUID(job, schema)
	if err != nil {
		return err
	}
	if expectedSchemaUID != "" && schemaUID != expectedSchemaUID {
		return errors.New("job evidence is owned by another PtahSchema")
	}
	label := operationLabel(operationID)
	template := job.Spec.Template
	if job.UID != uid || job.Name == "" ||
		job.Labels[labelManagedBy] != managedByOperator || job.Labels[labelComponent] != schemaOperationComponent ||
		job.Labels[labelSchema] != schema || job.Labels[labelOperation] != operation ||
		job.Labels[labelOperationID] != label ||
		template.Labels[labelSchema] != schema || template.Labels[labelOperation] != operation ||
		template.Labels[labelOperationID] != label || template.Annotations[annotationOperationID] != operationID ||
		!ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, "PtahSchema", schema, schemaUID) ||
		job.Spec.PodReplacementPolicy == nil || *job.Spec.PodReplacementPolicy != batchv1.Failed ||
		job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 ||
		!jobComplete(job) {
		return errors.New("job evidence has mismatched exact Job JSON")
	}
	if pod.UID == "" || pod.Name == "" || pod.GenerateName != job.Name+"-" ||
		pod.Labels[labelSchema] != schema || pod.Labels[labelOperation] != operation ||
		pod.Labels[labelOperationID] != label || pod.Annotations[annotationOperationID] != operationID ||
		!ownedExactlyOnce(pod.OwnerReferences, "batch/v1", "Job", job.Name, job.UID) ||
		pod.Status.Phase != corev1.PodSucceeded || !ptahExitedZero(pod) || !noRestarts(pod) {
		return errors.New("job evidence has mismatched exact Pod JSON")
	}
	result := evidence.result
	if int64(result.ProtocolVersion) != protocolVersion || string(result.Operation) != operation ||
		result.OperationID != operationID || result.Truncation != nil {
		return errors.New("job evidence lost its normalized runner protocol binding")
	}
	return nil
}

// sameEvidenceIdentity holds evidence already archived for a Job UID to the
// evidence offered for it again: one schema, one operation ID, one Job name
// and one Pod. A second archive for one UID is a collision, not an update.
func sameEvidenceIdentity(existing, supplied *jobEvidence, schema string) error {
	existingSchemaUID, err := schemaOwnerUID(existing.job, schema)
	if err != nil {
		return err
	}
	suppliedSchemaUID, err := schemaOwnerUID(supplied.job, schema)
	if err != nil {
		return err
	}
	if existingSchemaUID != suppliedSchemaUID ||
		existing.job.Annotations[annotationOperationID] != supplied.job.Annotations[annotationOperationID] ||
		existing.job.Name != supplied.job.Name ||
		existing.pod.UID != supplied.pod.UID || existing.pod.Name != supplied.pod.Name {
		return errors.New("existing Job evidence archive collides with the supplied immutable identity")
	}
	return nil
}

// managedCompleteJob is a completed operation Job of the operator's, which the
// audit archives: its labels name a schema and one of the five operations,
// and it completed without failing.
func managedCompleteJob(job *batchv1.Job) bool {
	return job.Labels[labelManagedBy] == managedByOperator &&
		job.Labels[labelComponent] == schemaOperationComponent &&
		job.Labels[labelSchema] != "" &&
		slices.Contains(schemaOperations, job.Labels[labelOperation]) &&
		jobComplete(job)
}

// admittedUnderRuntimeClass is an operation Job the admission fixtures reach:
// its template names the phase's RuntimeClass.
func admittedUnderRuntimeClass(job *batchv1.Job) bool {
	return job.Labels[labelComponent] == schemaOperationComponent &&
		job.Spec.Template.Spec.RuntimeClassName != nil &&
		*job.Spec.Template.Spec.RuntimeClassName == admissionRuntimeClass
}

// controllerIdentity is what the manager stamps on every Job and Pod it
// creates.
type controllerIdentity struct {
	image, revision, stateVersion string
}

func (identity controllerIdentity) stampedOn(annotations map[string]string) bool {
	return annotations[annotationControllerImage] == identity.image &&
		annotations[annotationControllerRev] == identity.revision &&
		annotations[annotationControllerState] == identity.stateVersion
}

// jobAdmissionBinding holds a Job admitted under the runtime class to the
// admission snapshot it was built from and the manager that built it, on the
// Job and on its template alike.
func jobAdmissionBinding(job *batchv1.Job, identity controllerIdentity) error {
	digest := job.Annotations[annotationAdmissionDigest]
	template := job.Spec.Template
	if !sha256Pattern.MatchString(digest) || template.Annotations[annotationAdmissionDigest] != digest ||
		!identity.stampedOn(job.Annotations) || !identity.stampedOn(template.Annotations) ||
		template.Spec.RuntimeClassName == nil || *template.Spec.RuntimeClassName != admissionRuntimeClass {
		return fmt.Errorf("managed Job %s lacks its persisted admission binding", job.Name)
	}
	return nil
}

// terminalPodEvidence holds a Pod of a terminal Job to complete terminal
// evidence: the UID the audit selected it by, the Job's controller reference,
// a terminal phase, no restart, and every declared container terminated, so
// the logs the audit reads are the whole of what each container wrote.
func terminalPodEvidence(pod *corev1.Pod, podUID, jobUID types.UID) bool {
	if pod.UID != podUID || !controlledByJob(pod.OwnerReferences, jobUID) ||
		(pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed) || !noRestarts(pod) {
		return false
	}
	return slices.Equal(declaredContainers(pod), terminatedContainers(pod))
}

func declaredContainers(pod *corev1.Pod) []string {
	var names []string
	for _, container := range pod.Spec.InitContainers {
		names = append(names, container.Name)
	}
	for _, container := range pod.Spec.Containers {
		names = append(names, container.Name)
	}
	for _, container := range pod.Spec.EphemeralContainers {
		names = append(names, container.Name)
	}
	slices.Sort(names)
	return names
}

func terminatedContainers(pod *corev1.Pod) []string {
	var names []string
	for _, status := range allStatuses(pod) {
		if status.State.Terminated != nil {
			names = append(names, status.Name)
		}
	}
	slices.Sort(names)
	return names
}

// startedContainers are the containers of a Pod that ran or are running, in
// the order the Pod's status lists them.
func startedContainers(pod *corev1.Pod) []string {
	var names []string
	for _, status := range allStatuses(pod) {
		if status.State.Running != nil || status.State.Terminated != nil {
			names = append(names, status.Name)
		}
	}
	return names
}

// terminatedInStatusOrder are the containers that terminated, in the order the
// Pod's status lists them.
func terminatedInStatusOrder(pod *corev1.Pod) []string {
	var names []string
	for _, status := range allStatuses(pod) {
		if status.State.Terminated != nil {
			names = append(names, status.Name)
		}
	}
	return names
}

func quantityIs(resources corev1.ResourceList, name corev1.ResourceName, want string) bool {
	quantity, found := resources[name]
	return found && quantity.String() == want
}

func hasToleration(tolerations []corev1.Toleration, key string, effect corev1.TaintEffect, seconds *int64) bool {
	for _, toleration := range tolerations {
		if toleration.Key != key || toleration.Operator != corev1.TolerationOpExists || toleration.Effect != effect {
			continue
		}
		if seconds == nil || (toleration.TolerationSeconds != nil && *toleration.TolerationSeconds == *seconds) {
			return true
		}
	}
	return false
}

// podAdmissionApplied holds a Pod admitted under the runtime class to what
// admission added to it: the LimitRange's defaults for resources the Job left
// unset, the exact declared resources on every other container, the
// default ServiceAccount with no token and the pull Secret it names, the
// RuntimeClass's overhead, node selector and toleration, and the default
// not-ready and unreachable tolerations. The manager's identity and the
// admission snapshot digest are on the Pod as they were on the Job.
func podAdmissionApplied(job *batchv1.Job, pod *corev1.Pod, identity controllerIdentity, pullSecret string) bool {
	if job == nil || pod == nil || job.UID == "" || pod.Namespace != job.Namespace ||
		!ownedExactlyOnce(pod.OwnerReferences, "batch/v1", "Job", job.Name, job.UID) {
		return false
	}
	spec := pod.Spec
	if !sha256Pattern.MatchString(pod.Annotations[annotationAdmissionDigest]) || !identity.stampedOn(pod.Annotations) ||
		spec.RuntimeClassName == nil || *spec.RuntimeClassName != admissionRuntimeClass ||
		spec.ServiceAccountName != "default" ||
		spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken ||
		spec.NodeSelector["kubernetes.io/os"] != "linux" ||
		!quantityIs(spec.Overhead, corev1.ResourceMemory, "8Mi") ||
		len(spec.ImagePullSecrets) != 1 || spec.ImagePullSecrets[0].Name != pullSecret {
		return false
	}
	for _, volume := range spec.Volumes {
		if volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if source.ServiceAccountToken != nil {
				return false
			}
		}
	}
	if len(spec.Containers) == 0 || !admittedContainerResources(spec.Containers, job.Spec.Template.Spec.Containers) ||
		!admittedContainerResources(spec.InitContainers, job.Spec.Template.Spec.InitContainers) {
		return false
	}
	defaultSeconds := int64(300)
	return hasToleration(spec.Tolerations, "node.kubernetes.io/not-ready", corev1.TaintEffectNoExecute, &defaultSeconds) &&
		hasToleration(spec.Tolerations, "node.kubernetes.io/unreachable", corev1.TaintEffectNoExecute, &defaultSeconds) &&
		hasToleration(spec.Tolerations, admissionRuntimeTaint, corev1.TaintEffectNoSchedule, nil)
}

func admittedContainerResources(actual, declared []corev1.Container) bool {
	if len(actual) != len(declared) {
		return false
	}
	defaults := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
	}
	expected := map[string]corev1.ResourceRequirements{}
	for _, container := range declared {
		if container.Name == "" {
			return false
		}
		if _, duplicate := expected[container.Name]; duplicate {
			return false
		}
		want := container.Resources.DeepCopy()
		if want.Requests == nil {
			want.Requests = corev1.ResourceList{}
		}
		if want.Limits == nil {
			want.Limits = corev1.ResourceList{}
		}
		for name, value := range defaults.Requests {
			if _, set := want.Requests[name]; !set {
				want.Requests[name] = value.DeepCopy()
			}
		}
		for name, value := range defaults.Limits {
			if _, set := want.Limits[name]; !set {
				want.Limits[name] = value.DeepCopy()
			}
		}
		expected[container.Name] = *want
	}
	for _, container := range actual {
		want, found := expected[container.Name]
		if !found || !equality.Semantic.DeepEqual(container.Resources, want) {
			return false
		}
		delete(expected, container.Name)
	}
	return len(expected) == 0
}

// archivedJobsComplete uses the terminal evidence captured before Job TTL
// collection. A missing API object is not completion; only the full validated
// Job, Pod and result archive can prove it after collection.
func archivedJobsComplete(records []observedJob, schema, operation string, minimum int, protocol int64, archive map[string]*jobEvidence) (bool, error) {
	if minimum <= 0 || len(records) < minimum {
		return false, nil
	}
	seen := map[string]bool{}
	for _, record := range records {
		if record.Name == "" || record.UID == "" || record.Schema != schema || record.Operation != operation || seen[record.UID] {
			return false, errors.New("completed Job ledger has missing, repeated or mismatched identities")
		}
		seen[record.UID] = true
		evidence := archive[record.UID]
		if evidence == nil {
			return false, nil
		}
		if evidence.job == nil || evidence.pod == nil {
			return false, errors.New("completed Job archive has missing workload evidence")
		}
		if evidence.job.Name != record.Name || record.Created == "" || evidence.job.CreationTimestamp.UTC().Format(time.RFC3339) != record.Created {
			return false, errors.New("completed Job archive disagrees with the recorded workload")
		}
		if err := validateJobEvidence(evidence, schema, operation, types.UID(record.UID), "", protocol); err != nil {
			return false, err
		}
	}
	return true, nil
}
