// Package approve turns a person's review of a stored plan into the approval
// the operator acts on.
//
// An approval names three things a person should never have to copy by hand:
// the resource with its UID, the plan with its UID, and the plan's
// fingerprint. This package reads them from the cluster, shows the plan they
// identify, and creates the approval only while they still identify the plan
// that was shown. It decides nothing the operator does not: admission stamps
// the approver and checks the binding, Kubernetes RBAC decides who may create
// the object, and the operator decides when the approved plan runs.
package approve

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/desiredspec"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/operate"
	"github.com/stokaro/ptah-operator/internal/statusview"
	"github.com/stokaro/ptah-operator/pkg/planread"
)

// Family is the kind of resource an approval is for. The two families approve
// different things: a schema plan is SQL, a migration plan is a sequence of
// versions whose SQL lives in the artifact.
type Family string

const (
	// Schema approves the current plan of a PtahSchema.
	Schema Family = "schema"
	// Migration approves the current plan of a PtahMigration.
	Migration Family = "migration"
)

// ErrNotFound is returned when the named resource is not there.
var ErrNotFound = errors.New("no such resource")

// ErrNothingToApprove is returned when the resource has no current plan.
var ErrNothingToApprove = errors.New("no plan to approve")

// Kind is the resource kind of the family, as the API names it.
func (f Family) Kind() string {
	if f == Migration {
		return "PtahMigration"
	}
	return "PtahSchema"
}

// ApprovalKind is the kind of the approval the family takes.
func (f Family) ApprovalKind() string {
	if f == Migration {
		return "PtahMigrationApproval"
	}
	return "PtahSchemaApproval"
}

// ParseTarget reads a target written as family/name, the way kubectl names a
// resource: schema/storefront or migration/shipments, or the kind's plural or
// short name, as in ptahschema/storefront. It reads the family the way
// suspend, resume, refresh and wait do (operate.ParseFamily). The family is
// required: a schema and a migration may share a name, and an approval for
// the wrong one is an approval nobody reviewed.
func ParseTarget(target string) (Family, string, error) {
	family, name, found := strings.Cut(target, "/")
	if !found || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("name the resource as schema/NAME or migration/NAME, not %q", target)
	}
	parsed, err := operate.ParseFamily(family)
	if err != nil {
		return "", "", fmt.Errorf("%q is not a family this command approves; use schema/NAME or migration/NAME", family)
	}
	if parsed == operate.FamilyMigration {
		return Migration, name, nil
	}
	return Schema, name, nil
}

// Review is the plan a person is asked to approve and the identities an
// approval of it carries. It is read once and shown; Submit creates the
// approval only while the cluster still names the same plan.
type Review struct {
	Family    Family
	Namespace string
	Name      string
	// ResourceUID is the schema's or migration's UID: a resource deleted and
	// created again under the same name is another resource.
	ResourceUID types.UID
	// Spec is the resource's spec as the API server sent it, as canonical JSON
	// without spec.suspend (desiredspec.Of): the fields this plugin's types
	// know and those a newer API added, as admission compares them. A spec
	// changed since, other than by a suspend or a resume, is not the one the
	// person reviewed the plan under. Admission does not compare it, so this
	// is the only check against a spec the person never saw.
	Spec string

	PlanName        string
	PlanUID         types.UID
	Fingerprint     string
	ContractVersion int32

	// SchemaPlan is the verified plan of a schema: its statements with their
	// severity and reason, read through the operator's own plan store.
	SchemaPlan *planread.Plan
	// MigrationPlan is the stored plan of a migration: versions, checksums
	// and order. It holds no SQL, and nothing here claims to have shown any.
	MigrationPlan *operatorv1alpha1.PtahMigrationPlan

	// Open says whether the operator would admit an approval now. Closed
	// carries why not, in words a person acts on.
	Open   bool
	Closed string
	// Reading says the operator is reading the resource again, which it does
	// at its interval while a plan waits for a person, and on request. The
	// gate is closed until that read ends and reopens by itself; Submit waits
	// for it when asked to.
	Reading bool
	// Missing says the plan the status names is not stored: the object under
	// its name is another plan, or another resource's. Nothing can approve it,
	// in any mode, and Closed says why. The review then carries the identity
	// the status names and no fingerprint, so nothing built from it binds the
	// object that took the name.
	Missing bool
}

// Load reads the current plan of a schema or a migration and says whether
// an approval of it would be admitted now.
func Load(ctx context.Context, reader client.Reader, family Family, namespace, name string) (Review, error) {
	if reader == nil {
		return Review{}, errors.New("a cluster reader is required")
	}
	if namespace == "" || name == "" {
		return Review{}, errors.New("a namespace and a name are required")
	}
	switch family {
	case Schema:
		return loadSchema(ctx, reader, namespace, name)
	case Migration:
		return loadMigration(ctx, reader, namespace, name)
	default:
		return Review{}, fmt.Errorf("unknown family %q", family)
	}
}

func loadSchema(ctx context.Context, reader client.Reader, namespace, name string) (Review, error) {
	review, err := identifySchema(ctx, reader, namespace, name)
	if err != nil {
		return Review{}, err
	}
	// planread.Load and not LoadForDisplay: an approval is a decision about the
	// whole document, and a field this plugin cannot read may change what the
	// statements do when they run.
	plan, err := planread.Load(ctx, reader, namespace, name, planread.Current)
	if errors.Is(err, planread.ErrUnreadFields) {
		return Review{}, fmt.Errorf("read the current plan of PtahSchema %s/%s: %w; "+
			"this plugin approves only a plan it reads whole, so move it to the operator's version; "+
			"kubectl ptah plan %s -n %s shows what it can read", namespace, name, err, name, namespace)
	}
	if err != nil {
		return Review{}, fmt.Errorf("read the current plan of PtahSchema %s/%s: %w", namespace, name, err)
	}
	// The status and the plan are two reads. A plan replaced between them
	// is not shown under the identity of the one before it.
	if plan.SchemaUID != review.ResourceUID || plan.PlanName != review.PlanName ||
		plan.PlanUID != review.PlanUID || plan.Fingerprint != review.Fingerprint {
		return Review{}, fmt.Errorf("the current plan of PtahSchema %s/%s changed while it was read; run the command again", namespace, name)
	}
	review.ContractVersion = plan.Manifest.Spec.ContractVersion
	review.SchemaPlan = &plan
	return review, nil
}

// identifySchema reads who the schema and its current plan are, and whether
// an approval would be admitted now, without reading the plan's content.
// It is what a submission checks again: a stored plan is immutable, so the
// same UID and fingerprint are the same plan that was verified and shown.
func identifySchema(ctx context.Context, reader client.Reader, namespace, name string) (Review, error) {
	schema := &operatorv1alpha1.PtahSchema{}
	spec, err := readResource(ctx, reader, "PtahSchema", namespace, name, schema)
	if err != nil {
		return Review{}, err
	}
	review := Review{
		Family:      Schema,
		Namespace:   namespace,
		Name:        name,
		ResourceUID: schema.UID,
		Spec:        spec,
		Reading:     schemaReading(schema),
	}
	current := schema.Status.Plan
	if current == nil || current.Name == "" {
		// The harvest of a reading clears the plan, and the plan computed from
		// it is published when the read ends.
		if review.Reading {
			return review, fmt.Errorf("%w: PtahSchema %s/%s has no current plan while the operator reads it again; "+
				"it publishes one when that read ends", ErrNothingToApprove, namespace, name)
		}
		return review, fmt.Errorf("%w: PtahSchema %s/%s has no current plan", ErrNothingToApprove, namespace, name)
	}
	review.PlanName, review.PlanUID, review.Fingerprint = current.Name, current.UID, current.Fingerprint
	review.Open, review.Closed = schemaOpen(schema, review.Reading)
	return review, nil
}

func loadMigration(ctx context.Context, reader client.Reader, namespace, name string) (Review, error) {
	return identifyMigration(ctx, reader, namespace, name)
}

// identifyMigration reads a migration and the plan it names. A migration plan
// is small and holds no SQL, so reading it to show it and reading it to check
// it again are the same read.
func identifyMigration(ctx context.Context, reader client.Reader, namespace, name string) (Review, error) {
	migration := &operatorv1alpha1.PtahMigration{}
	spec, err := readResource(ctx, reader, "PtahMigration", namespace, name, migration)
	if err != nil {
		return Review{}, err
	}
	review := Review{
		Family:      Migration,
		Namespace:   namespace,
		Name:        name,
		ResourceUID: migration.UID,
		Spec:        spec,
		Reading:     migrationReading(migration),
	}
	ref := migration.Status.Plan
	if ref == nil || ref.Name == "" {
		if review.Reading {
			return review, fmt.Errorf("%w: PtahMigration %s/%s has no current plan while the operator reads it again; "+
				"it publishes one when that read ends", ErrNothingToApprove, namespace, name)
		}
		return review, fmt.Errorf("%w: PtahMigration %s/%s has no current plan", ErrNothingToApprove, namespace, name)
	}
	review.PlanName, review.PlanUID = ref.Name, ref.UID
	plan := &operatorv1alpha1.PtahMigrationPlan{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, plan); err != nil {
		if apierrors.IsNotFound(err) {
			// The plan the status names is gone. The review still names the
			// migration, so a submission reports the plan gone rather than the
			// migration replaced.
			review.Missing = true
			return review, fmt.Errorf("%w: PtahMigration %s/%s names plan %s, which is not stored", ErrNothingToApprove, namespace, name, ref.Name)
		}
		return Review{}, fmt.Errorf("read PtahMigrationPlan %s/%s: %w", namespace, ref.Name, err)
	}
	if plan.UID != ref.UID || plan.Spec.MigrationRef.UID != migration.UID || plan.Spec.MigrationRef.Name != migration.Name {
		// The stored object under that name is another plan, or another
		// migration's: the plan the status names is gone. Its UID and
		// fingerprint belong to that other object and stay out of the review.
		review.Missing = true
		review.Open, review.Closed = false, fmt.Sprintf("PtahMigrationPlan %s/%s is not the plan PtahMigration %s names.", namespace, ref.Name, name)
		return review, nil
	}
	if !migrationplan.SupportedContractVersion(plan.Spec.ContractVersion) {
		// The review names the plan, so a submission can say a newer plan
		// replaced the one reviewed rather than read it again.
		review.Fingerprint = plan.Spec.Fingerprint
		return review, unreadable{fmt.Errorf("PtahMigrationPlan %s/%s is written in contract %d, which this plugin does not read",
			namespace, ref.Name, plan.Spec.ContractVersion)}
	}
	review.Fingerprint = plan.Spec.Fingerprint
	review.ContractVersion = plan.Spec.ContractVersion
	review.MigrationPlan = plan
	review.Open, review.Closed = migrationOpen(migration, review.Reading)
	return review, nil
}

// readResource reads the resource once, as the API server sends it, into
// object for everything a review reads, and returns its spec as Review.Spec
// holds it. The spec is taken from the raw object rather than from object, so
// a field a newer API added, which object's type drops, still counts.
func readResource(ctx context.Context, reader client.Reader, kind, namespace, name string, object client.Object) (string, error) {
	raw := &unstructured.Unstructured{}
	raw.SetGroupVersionKind(operatorv1alpha1.GroupVersion.WithKind(kind))
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, raw); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("%w: %s %s/%s", ErrNotFound, kind, namespace, name)
		}
		return "", fmt.Errorf("read %s %s/%s: %w", kind, namespace, name, err)
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw.Object, object); err != nil {
		return "", unreadable{fmt.Errorf("decode %s %s/%s: %w", kind, namespace, name, err)}
	}
	spec, err := desiredspec.Of(raw.Object["spec"])
	if err != nil {
		return "", unreadable{fmt.Errorf("read the spec of %s %s/%s: %w", kind, namespace, name, err)}
	}
	return spec, nil
}

// unreadable is an answer the API server gave that this plugin cannot use.
// Reading again changes nothing, so a wait does not retry it as it does a
// read that failed on the way.
type unreadable struct{ err error }

func (u unreadable) Error() string { return u.err.Error() }
func (u unreadable) Unwrap() error { return u.err }

// schemaOpen says whether admission would take an approval of the schema's
// current plan now, and why not when it would not. It asks what the webhook
// asks of the status (internal/admission, validateBinding): the plan waiting
// for a person in AwaitingApproval, with approval required, no operation
// running and no approval recorded. Admission refuses an approval while the
// operator reads the schema again, so a read closes the gate here too, and
// Reading tells that apart from a gate that will not reopen on its own.
func schemaOpen(schema *operatorv1alpha1.PtahSchema, reading bool) (bool, string) {
	status := schema.Status
	switch {
	case status.Plan.Approval != nil:
		return false, fmt.Sprintf("The plan is approved already, by %s (%s).",
			statusview.Clean(status.Plan.Approval.Approver.Username), statusview.Clean(status.Plan.Approval.Name))
	case reading:
		return false, fmt.Sprintf("The operator is reading this schema again (%s); admission takes an approval once it is back at the approval gate.",
			readingWhat(string(operationType(status.ActiveOperation)), string(status.Phase)))
	case policyHolds(status.Conditions):
		// Ahead of an operation in flight: the interval's read of such a plan
		// ends where it began, not at the approval gate.
		held := meta.FindStatusCondition(status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
		closed := fmt.Sprintf("spec.policy holds this plan (%s), and no approval is asked for until the spec changes", held.Reason)
		if message := strings.TrimSuffix(statusview.Clean(held.Message), "."); message != "" {
			closed += ": " + message
		}
		return false, closed + "."
	case status.ActiveOperation != nil:
		return false, fmt.Sprintf("The operator is running %s for this schema. An approval is admitted once it is back at the approval gate; run this command again then.",
			status.ActiveOperation.Type)
	case status.Phase != operatorv1alpha1.PhaseAwaitingApproval:
		return false, fmt.Sprintf("The schema is %s, not waiting for an approval.", phaseOr(string(status.Phase)))
	case !conditionTrue(status.Conditions, operatorv1alpha1.ConditionApprovalRequired):
		return false, "The operator does not require an approval of this plan."
	}
	return true, ""
}

// migrationOpen is schemaOpen for a migration, which also needs the history
// read its plan was computed against.
func migrationOpen(migration *operatorv1alpha1.PtahMigration, reading bool) (bool, string) {
	status := migration.Status
	var operation string
	if status.ActiveOperation != nil {
		operation = string(status.ActiveOperation.Type)
	}
	switch {
	case reading:
		return false, fmt.Sprintf("The operator is reading this migration again (%s); admission takes an approval once it is back at the approval gate.",
			readingWhat(operation, string(status.Phase)))
	case status.ActiveOperation != nil:
		return false, fmt.Sprintf("The operator is running %s for this migration. An approval is admitted once it is back at the approval gate; run this command again then.",
			status.ActiveOperation.Type)
	case status.History == nil:
		return false, "The operator has not read the database's history yet."
	case status.Phase != operatorv1alpha1.MigrationPhaseAwaitingApproval:
		return false, fmt.Sprintf("The migration is %s, not waiting for an approval.", phaseOr(string(status.Phase)))
	case !conditionTrue(status.Conditions, operatorv1alpha1.ConditionMigrationApprovalRequired):
		return false, "The operator does not require an approval of this plan."
	}
	return true, ""
}

// schemaReading reports a schema the operator is reading again while its plan
// waits for a person: a read-only operation in flight, or a phase such a read
// passes through between two of them, with ApprovalRequired True. The
// operator does this at its interval, so a person reviewing a plan for longer
// than the interval meets it. The read ends at the approval gate again by
// itself, with the same stored plan when nothing it read changed: the plan
// store reuses a byte-identical manifest under its name and UID
// (planstore.Store.Publish).
//
// ApprovalRequired is what says the gate was open before the read began. The
// claims of a read do not write it, and neither does the Observe harvest; the
// Plan harvest writes it again with the plan. A plan the policy holds keeps it
// False through the read, and that read ends Blocked, not at the gate. The
// proof after an Apply is not such a read either.
func schemaReading(schema *operatorv1alpha1.PtahSchema) bool {
	status := schema.Status
	if status.PendingObservation != nil ||
		!conditionTrue(status.Conditions, operatorv1alpha1.ConditionApprovalRequired) {
		return false
	}
	if operation := status.ActiveOperation; operation != nil {
		switch operation.Type {
		case operatorv1alpha1.OperationResolve, operatorv1alpha1.OperationVerify,
			operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan:
			return true
		}
		return false
	}
	switch status.Phase {
	case operatorv1alpha1.PhaseResolving, operatorv1alpha1.PhaseVerifying,
		operatorv1alpha1.PhaseObserving, operatorv1alpha1.PhasePlanning:
		return true
	}
	return false
}

// migrationReading is schemaReading for a migration: Resolve, Verify and the
// read of the history, and the phases between them, with ApprovalRequired
// True. Only the publication of a plan, an Apply, a discarded plan and a
// realm refusal write that condition; the claims of a read and the harvest of
// the history do not. A migration whose plan waited for a person keeps it
// True through the read, and Planning is then the write between the history
// and the plan published from it. Under apply policy Always or Never a
// migration rests in Planning with its plan published and ApprovalRequired
// False, and no read of it ends at the approval gate.
func migrationReading(migration *operatorv1alpha1.PtahMigration) bool {
	status := migration.Status
	if !conditionTrue(status.Conditions, operatorv1alpha1.ConditionMigrationApprovalRequired) {
		return false
	}
	if operation := status.ActiveOperation; operation != nil {
		switch operation.Type {
		case operatorv1alpha1.MigrationOperationResolve, operatorv1alpha1.MigrationOperationVerify,
			operatorv1alpha1.MigrationOperationHistory:
			return true
		}
		return false
	}
	switch status.Phase {
	case operatorv1alpha1.MigrationPhaseResolving, operatorv1alpha1.MigrationPhaseVerifying,
		operatorv1alpha1.MigrationPhaseReading, operatorv1alpha1.MigrationPhasePlanning:
		return true
	}
	return false
}

func operationType(operation *operatorv1alpha1.ActiveOperationStatus) operatorv1alpha1.OperationType {
	if operation == nil {
		return ""
	}
	return operation.Type
}

// readingWhat names the step of a read: the operation in flight, or the
// phase between two of them.
func readingWhat(operation, phase string) string {
	if operation != "" {
		return operation
	}
	return "phase " + phaseOr(phase)
}

// policyHolds reports ApprovalRequired False for a reason that says the
// schema's own policy holds its plan: a destructive plan
// spec.policy.allowDestructive does not allow, or any plan under apply policy
// Never (setPlanPolicyStatus in internal/controller).
func policyHolds(conditions []metav1.Condition) bool {
	held := meta.FindStatusCondition(conditions, operatorv1alpha1.ConditionApprovalRequired)
	if held == nil || held.Status != metav1.ConditionFalse {
		return false
	}
	switch operatorv1alpha1.ConditionReason(held.Reason) {
	case operatorv1alpha1.ReasonDestructiveChangesDisabled, operatorv1alpha1.ReasonApplyDisabled:
		return true
	}
	return false
}

func phaseOr(phase string) string {
	if phase == "" {
		return "in no phase yet"
	}
	return phase
}

func conditionTrue(conditions []metav1.Condition, kind string) bool {
	for _, condition := range conditions {
		if condition.Type == kind {
			return condition.Status == metav1.ConditionTrue
		}
	}
	return false
}
