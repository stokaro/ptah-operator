// Package jobclaim decides whether a Job is the one a claim built.
//
// A claim reserves one Job name and records what that Job has to be: the
// operation, its identity, the execution epoch it runs under, the admission
// snapshot of its Pod template. Two processes ask whether a Job they hold is
// that Job. The controller asks when it reads back a Job it has just created,
// on every pass that adopts or supervises the Job under a claim's reserved
// name, and when it cleans up after a claim a binding rotation retired. The
// controller-write webhook asks when the manager creates a Job, schedules its
// cleanup, or publishes a plan from it. Both ask Match, so the controller
// never touches a Job the webhook would refuse it, and the webhook never
// admits a Job the controller would not recognize.
package jobclaim

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// CleanupTTLSeconds is the TTL the controller sets on a Job once it no longer
// needs it, and the one change a stored Job may carry beyond what its claim
// built.
const CleanupTTLSeconds int32 = 300

var (
	managerImagePattern = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
	sha256Pattern       = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	epochPattern        = regexp.MustCompile(`^v1-[0-9a-f]{32}$`)
)

// Owner is the resource a claim belongs to, which has to be its Job's one
// controller owner.
type Owner struct {
	Kind      string
	Namespace string
	Name      string
	UID       types.UID
}

// SchemaOwner is the owner a schema's claims name.
func SchemaOwner(schema *operatorv1alpha1.PtahSchema) Owner {
	if schema == nil {
		return Owner{}
	}
	return Owner{Kind: "PtahSchema", Namespace: schema.Namespace, Name: schema.Name, UID: schema.UID}
}

// MigrationOwner is the owner a migration's claims name.
func MigrationOwner(migration *operatorv1alpha1.PtahMigration) Owner {
	if migration == nil {
		return Owner{}
	}
	return Owner{Kind: "PtahMigration", Namespace: migration.Namespace, Name: migration.Name, UID: migration.UID}
}

// family is what the builder writes differently for each kind of owner: the
// component label and the label that names the owner.
type family struct {
	component    string
	subjectLabel string
}

var families = map[string]family{
	"PtahSchema":    {component: workload.ComponentSchemaOperation, subjectLabel: workload.LabelSchema},
	"PtahMigration": {component: workload.ComponentMigrationOperation, subjectLabel: workload.LabelMigration},
}

// Claim is what a claim fixes about its Job.
type Claim struct {
	Owner Owner
	// Operation is the operation type as the API spells it.
	Operation string
	// Mutating says the operation may change the database. Its Job carries
	// the marks the builder writes on every mutating Job.
	Mutating bool
	ID       string
	// InputFingerprint is the fingerprint the claim was made from. A record
	// that did not keep it leaves it empty, and then the Job's own has to be
	// a SHA-256 digest; the template digest pins which one it was.
	InputFingerprint string
	// Epoch is the execution binding epoch the claim was made under, which
	// the Job has to carry.
	Epoch string
	// Binding, when set, is the execution binding in force: the claim has to
	// have been made under it, and a Job held to the envelope has to carry its
	// versions. A caller judging the Job of a claim a rotation retired leaves
	// it unset and holds the epoch to the retirement record instead.
	Binding *operatorv1alpha1.ExecutionBindingStatus
	// Plan is the plan a schema Apply runs, whose binding its Job carries.
	Plan    *operatorv1alpha1.CurrentPlanStatus
	JobName string
	// JobUID is the UID the claim recorded, and empty before it recorded one.
	JobUID types.UID
	// Snapshot is the admission snapshot the claim made durable before it
	// dispatched: the Job's Pod template has to be the one it recorded.
	Snapshot *operatorv1alpha1.PodAdmissionSnapshot
	// Built is the Job the claim's inputs build now, for a caller that can
	// rebuild it. The Job then has to be Built apart from what the API server
	// generates. Without it the Job is held to the envelope the claim fixes,
	// which is what a caller has when the inputs may have moved since
	// dispatch.
	Built *batchv1.Job
	// Stored says the Job was read back from the API server rather than
	// submitted to it. A stored Job may carry the cleanup TTL where Built
	// carries none, and its metadata is compared in its labels, annotations
	// and owner only; a submitted Job's metadata has to be Built's apart from
	// the fields the API server assigns.
	Stored bool
}

// SchemaOperation is the claim a schema's active operation makes. A schema
// Apply runs the schema's current plan, and a missing one leaves a plan that
// no Job can match.
func SchemaOperation(schema *operatorv1alpha1.PtahSchema, operation *operatorv1alpha1.ActiveOperationStatus) Claim {
	if schema == nil || operation == nil {
		return Claim{}
	}
	claim := Claim{
		Owner:            SchemaOwner(schema),
		Operation:        string(operation.Type),
		Mutating:         mutationlifecycle.SchemaOperation(operation.Type).Mutating,
		ID:               operation.ID,
		InputFingerprint: operation.InputFingerprint,
		Epoch:            operation.ExecutionBindingID,
		JobName:          operation.JobName,
		JobUID:           operation.JobUID,
		Snapshot:         operation.AdmissionSnapshot,
	}
	if operation.Type == operatorv1alpha1.OperationApply {
		plan := operatorv1alpha1.CurrentPlanStatus{}
		if schema.Status.Plan != nil {
			plan = *schema.Status.Plan
		}
		claim.Plan = &plan
	}
	return claim
}

// PendingApply is the claim a schema Apply left in its pending observation
// when it was settled as outcome-unknown. The observation keeps no input
// fingerprint, so the Job's own is read and pinned by the template digest.
func PendingApply(schema *operatorv1alpha1.PtahSchema, pending *operatorv1alpha1.PendingObservationStatus) Claim {
	if schema == nil || pending == nil {
		return Claim{}
	}
	plan := pending.Plan
	return Claim{
		Owner:     SchemaOwner(schema),
		Operation: string(operatorv1alpha1.OperationApply),
		Mutating:  mutationlifecycle.SchemaOperation(operatorv1alpha1.OperationApply).Mutating,
		ID:        pending.ApplyOperationID,
		Epoch:     pending.Plan.ExecutionBindingID,
		Plan:      &plan,
		JobName:   pending.ApplyJobName,
		JobUID:    pending.ApplyJobUID,
		Snapshot:  pending.AdmissionSnapshot,
	}
}

// MigrationOperation is the claim a migration's active operation makes.
func MigrationOperation(migration *operatorv1alpha1.PtahMigration, operation *operatorv1alpha1.MigrationOperationStatus) Claim {
	if migration == nil || operation == nil {
		return Claim{}
	}
	return Claim{
		Owner:            MigrationOwner(migration),
		Operation:        string(operation.Type),
		Mutating:         mutationlifecycle.MigrationOperation(operation.Type).Mutating,
		ID:               operation.ID,
		InputFingerprint: operation.InputFingerprint,
		Epoch:            operation.ExecutionBindingID,
		JobName:          operation.JobName,
		JobUID:           operation.JobUID,
		Snapshot:         operation.AdmissionSnapshot,
	}
}

// Match reports why job is not the Job claim built, or nil when it is.
//
// Every Job is held to the same account of the claim: its name, namespace
// and owner, the UID the claim recorded, the epoch the claim was made under,
// and the Pod template the claim's admission snapshot recorded. What differs
// is what the rest of the Job is held to -- the Job the claim rebuilds, when
// the caller has it, or else the labels and annotations the claim fixes.
func Match(job *batchv1.Job, claim Claim) error {
	shape, err := claim.validate()
	if err != nil {
		return err
	}
	if job == nil || job.UID == "" {
		return errors.New("the Job has no API-assigned identity")
	}
	if job.Namespace != claim.Owner.Namespace || job.Name != claim.JobName {
		return errors.New("the Job's name or namespace is not the one the claim reserved")
	}
	if claim.JobUID != "" && job.UID != claim.JobUID {
		return errors.New("the Job's UID is not the one the claim recorded")
	}
	if err := claim.Owner.controls(job.OwnerReferences); err != nil {
		return err
	}
	if claim.Binding != nil && claim.Epoch != claim.Binding.Epoch {
		return errors.New("the claim was made under an execution epoch that is no longer in force")
	}
	if job.Annotations[workload.AnnotationExecutionBindingID] != claim.Epoch {
		return errors.New("the Job runs under another execution epoch than its claim")
	}
	normalized := job.DeepCopy()
	if err := normalizeGeneratedIdentity(normalized); err != nil {
		return err
	}
	if claim.Built != nil {
		err = matchBuilt(job, normalized, claim)
	} else {
		err = matchEnvelope(job, normalized, claim, shape)
	}
	if err != nil {
		return err
	}
	templateDigest, err := podintent.DigestTemplate(&normalized.Spec.Template)
	if err != nil {
		return fmt.Errorf("digest the Job's Pod template: %w", err)
	}
	if templateDigest != claim.Snapshot.TemplateDigest {
		return errors.New("the Job's Pod template does not match the persisted admission snapshot")
	}
	return nil
}

func (c Claim) validate() (family, error) {
	shape, known := families[c.Owner.Kind]
	if !known || c.Owner.Namespace == "" || c.Owner.Name == "" || c.Owner.UID == "" {
		return family{}, errors.New("the claim names no owner that dispatches Jobs")
	}
	if c.Operation == "" || c.ID == "" || c.JobName == "" {
		return family{}, errors.New("the claim names no operation or Job")
	}
	if !epochPattern.MatchString(c.Epoch) {
		return family{}, errors.New("the claim names no execution epoch")
	}
	if err := podintent.ValidateSnapshot(c.Snapshot); err != nil {
		return family{}, fmt.Errorf("the claim's admission snapshot is invalid: %w", err)
	}
	return shape, nil
}

func (o Owner) controls(references []metav1.OwnerReference) error {
	if len(references) != 1 {
		return errors.New("the Job does not have exactly one owner")
	}
	reference := references[0]
	if reference.APIVersion != operatorv1alpha1.GroupVersion.String() || reference.Kind != o.Kind ||
		reference.Name != o.Name || reference.UID != o.UID ||
		reference.Controller == nil || !*reference.Controller ||
		reference.BlockOwnerDeletion == nil || !*reference.BlockOwnerDeletion {
		return fmt.Errorf("the Job's controller owner is not %s %s", o.Kind, o.Name)
	}
	return nil
}

// matchBuilt holds a Job to the one its claim rebuilds. What the API server
// generates -- the UID selector and the template labels bound to it -- is not
// the claim's, and neither is the cleanup TTL a stored Job may have been
// given.
func matchBuilt(job, normalized *batchv1.Job, claim Claim) error {
	built := claim.Built
	if !reflect.DeepEqual(job.Labels, built.Labels) || !reflect.DeepEqual(job.Annotations, built.Annotations) {
		return errors.New("the Job's labels or annotations are not the ones its claim builds")
	}
	if !claim.Stored {
		submitted, want := job.ObjectMeta.DeepCopy(), built.ObjectMeta.DeepCopy()
		scrubAssignedMetadata(submitted)
		scrubAssignedMetadata(want)
		if !reflect.DeepEqual(submitted, want) {
			return errors.New("the Job's metadata is not the metadata its claim builds")
		}
	}
	actual, expected := normalized.DeepCopy(), built.DeepCopy()
	if err := normalizeGeneratedIdentity(expected); err != nil {
		return err
	}
	normalizeServiceAccountAlias(&actual.Spec.Template.Spec)
	normalizeServiceAccountAlias(&expected.Spec.Template.Spec)
	// A CA overlap can change after the claim captured its template. Preserve
	// only that public bundle from the submitted Job; Match still checks the
	// entire template against the claim's persisted digest below. Neither an
	// arbitrary replacement bundle nor a changed token projection can pass it.
	if jobconfig.UsesPodToken(actual) && jobconfig.UsesPodToken(expected) {
		if _, err := jobconfig.Read(actual, claim.Owner.UID, claim.ID); err != nil {
			return err
		}
		for _, env := range actual.Spec.Template.Spec.Containers[0].Env {
			if env.Name == jobconfig.ServerTrust {
				for index := range expected.Spec.Template.Spec.Containers[0].Env {
					if expected.Spec.Template.Spec.Containers[0].Env[index].Name == jobconfig.ServerTrust {
						expected.Spec.Template.Spec.Containers[0].Env[index] = env
					}
				}
			}
		}
	}
	if claim.Stored && actual.Spec.TTLSecondsAfterFinished != nil &&
		*actual.Spec.TTLSecondsAfterFinished == CleanupTTLSeconds {
		actual.Spec.TTLSecondsAfterFinished = expected.Spec.TTLSecondsAfterFinished
	}
	if !apiequality.Semantic.DeepEqualWithNilDifferentFromEmpty(actual.Spec, expected.Spec) {
		return errors.New("the Job's spec is not the one its claim builds")
	}
	return nil
}

// matchEnvelope holds a Job to the labels and annotations its claim fixes,
// without rebuilding it. Beside them the Job may carry what
// spec.execution.podMetadata declared when it was dispatched, by
// workload.ValidateClaimedMetadata; which keys those were is pinned by the
// template digest Match compares last. The manager that dispatched the Job is
// read from the Job, since a later manager of the same binding may have
// dispatched it, and is pinned the same way.
func matchEnvelope(job, normalized *batchv1.Job, claim Claim, shape family) error {
	labels := map[string]string{
		workload.LabelManagedBy:   "ptah-operator",
		workload.LabelComponent:   shape.component,
		shape.subjectLabel:        claim.Owner.Name,
		workload.LabelOperation:   strings.ToLower(claim.Operation),
		workload.LabelOperationID: workload.OperationIDLabelValue(claim.ID),
	}
	if err := workload.ValidateClaimedMetadata(job.Labels, labels); err != nil {
		return fmt.Errorf("the Job's labels are not its claim's: %w", err)
	}
	record := job.Annotations
	if err := validateManagerRecord(record); err != nil {
		return err
	}
	ptahVersion := record[workload.AnnotationPtahVersion]
	stateVersion := record[workload.AnnotationControllerStateVersion]
	if binding := claim.Binding; binding != nil &&
		(ptahVersion != binding.PtahVersion || stateVersion != strconv.FormatInt(int64(binding.ControllerStateVersion), 10)) {
		return errors.New("the Job does not carry the versions of the execution binding in force")
	}
	inputFingerprint := claim.InputFingerprint
	if inputFingerprint == "" {
		inputFingerprint = record[workload.AnnotationInputFingerprint]
		if !sha256Pattern.MatchString(inputFingerprint) {
			return errors.New("the Job's input fingerprint is not a SHA-256 digest")
		}
	}
	annotations := map[string]string{
		workload.AnnotationOperationID:             claim.ID,
		workload.AnnotationInputFingerprint:        inputFingerprint,
		workload.AnnotationExecutionBindingID:      claim.Epoch,
		workload.AnnotationAdmissionSnapshotDigest: claim.Snapshot.Digest,
		workload.AnnotationPtahVersion:             ptahVersion,
		workload.AnnotationControllerStateVersion:  stateVersion,
		workload.AnnotationControllerImage:         record[workload.AnnotationControllerImage],
		workload.AnnotationControllerRevision:      record[workload.AnnotationControllerRevision],
	}
	if plan := claim.Plan; plan != nil {
		if plan.Name == "" || plan.UID == "" || !sha256Pattern.MatchString(plan.Fingerprint) ||
			!sha256Pattern.MatchString(plan.ContentDigest) || plan.ExecutionBindingID != claim.Epoch ||
			plan.PtahVersion != ptahVersion ||
			strconv.FormatInt(int64(plan.ControllerStateVersion), 10) != stateVersion {
			return errors.New("the Job does not carry the plan binding its claim runs")
		}
		annotations[workload.AnnotationPlanFingerprint] = plan.Fingerprint
		annotations[workload.AnnotationPlanContentDigest] = plan.ContentDigest
	}
	if claim.Mutating {
		workload.MarkMutatingOperation(annotations)
	}
	if err := workload.ValidateClaimedMetadata(job.Annotations, annotations); err != nil {
		return fmt.Errorf("the Job's annotations are not its claim's: %w", err)
	}
	// The template carries what the object carries, declared metadata
	// included, and the template digest pins both.
	if !reflect.DeepEqual(job.Spec.Template.Annotations, job.Annotations) {
		return errors.New("the Job's Pod template annotations differ from the Job's")
	}
	if !reflect.DeepEqual(normalized.Spec.Template.Labels, job.Labels) {
		return errors.New("the Job's Pod template labels differ from the Job's")
	}
	return nil
}

// validateManagerRecord holds the annotations that say which manager
// dispatched a Job to values that name one manager unambiguously. Where the
// envelope is compared against the binding in force a well-formed value
// still has to be the right one; on a retired claim's Job it is compared
// against a plan or nothing, and these rules are all that makes it
// unambiguous.
func validateManagerRecord(annotations map[string]string) error {
	if !managerImagePattern.MatchString(annotations[workload.AnnotationControllerImage]) {
		return errors.New("the Job's controller image is not pinned by a lowercase SHA-256 digest")
	}
	if err := controllerstate.ValidateRevision(annotations[workload.AnnotationControllerRevision]); err != nil {
		return fmt.Errorf("the Job's controller revision is invalid: %w", err)
	}
	stateVersion := annotations[workload.AnnotationControllerStateVersion]
	parsed, err := strconv.ParseInt(stateVersion, 10, 32)
	if err != nil || parsed < 1 || strconv.FormatInt(parsed, 10) != stateVersion {
		return errors.New("the Job's controller state version is not a canonical positive integer")
	}
	ptahVersion := annotations[workload.AnnotationPtahVersion]
	if ptahVersion == "" || strings.TrimSpace(ptahVersion) != ptahVersion || len(ptahVersion) > 128 {
		return errors.New("the Job's data-plane version is empty or ambiguous")
	}
	return nil
}

// normalizeGeneratedIdentity removes what the API server writes on a Job it
// stores -- the selector bound to the Job's UID, and the template labels
// that repeat it -- after holding each value to the Job's own UID and name.
func normalizeGeneratedIdentity(job *batchv1.Job) error {
	generated := map[string]string{
		batchv1.ControllerUidLabel: string(job.UID),
		batchv1.JobNameLabel:       job.Name,
		"controller-uid":           string(job.UID),
		"job-name":                 job.Name,
	}
	if job.Spec.Selector != nil {
		if job.UID == "" || len(job.Spec.Selector.MatchExpressions) != 0 || len(job.Spec.Selector.MatchLabels) == 0 {
			return errors.New("the Job's selector is not the one the API server generates")
		}
		for key, value := range job.Spec.Selector.MatchLabels {
			expected, ok := generated[key]
			if !ok || value != expected || !strings.Contains(key, "controller-uid") {
				return errors.New("the Job's selector is not bound to its own UID")
			}
		}
		job.Spec.Selector = nil
	}
	for key, expected := range generated {
		if value, ok := job.Spec.Template.Labels[key]; ok {
			if job.UID == "" || value != expected {
				return errors.New("the Job's Pod template carries a generated identity label that is not its own")
			}
			delete(job.Spec.Template.Labels, key)
		}
	}
	return nil
}

// normalizeServiceAccountAlias clears the deprecated service account field
// where it repeats the one in force: Kubernetes 1.37 fills it in and earlier
// releases leave it empty, and both are the same Pod.
func normalizeServiceAccountAlias(spec *corev1.PodSpec) {
	name := spec.ServiceAccountName
	if name == "" {
		name = spec.DeprecatedServiceAccount
	}
	if spec.DeprecatedServiceAccount == name {
		spec.DeprecatedServiceAccount = ""
	}
}

func scrubAssignedMetadata(metadata *metav1.ObjectMeta) {
	metadata.UID = ""
	metadata.ResourceVersion = ""
	metadata.Generation = 0
	metadata.CreationTimestamp = metav1.Time{}
	metadata.ManagedFields = nil
}
