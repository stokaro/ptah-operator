// Package historyview reads what one PtahSchema or PtahMigration retained of
// its own past: the lifecycle journal the operator keeps in its status, and
// every decision people made about it that is still stored.
//
// The journal is the operator's record, bounded by count and age, and the
// decisions are objects admission stamped. Neither is an audit archive, and
// the view says what each covers rather than presenting them as one. It
// joins an entry to the objects it names only by what the entry recorded: a
// plan by name, and by UID where the entry has one, an approval by UID, and
// an acknowledgment by name and operation. An entry that recorded too little
// to join is left unjoined and says so, rather than matched to whatever holds
// the name today.
//
// It connects to no database, reads no Secret, and reads no plan chunk. The
// plans are listed as metadata, for their names, UIDs and owners alone.
// Listing plans and decisions takes permissions reading the resource does
// not, so each list that fails leaves the journal as it was, with a line that
// says what is missing and why.
package historyview

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// Kind is the resource kind a history is read for.
type Kind string

const (
	// Schema is a PtahSchema.
	Schema Kind = "PtahSchema"
	// Migration is a PtahMigration.
	Migration Kind = "PtahMigration"
)

// ErrNotFound is returned when the named resource is not there. It is
// separate from a read failure: "there is nothing by that name" is an answer.
var ErrNotFound = errors.New("no resource by that name")

// View is everything this command prints, resolved before anything is written.
type View struct {
	Kind      Kind   `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// UID and CreatedAt are this incarnation of the resource. A resource
	// deleted and created again under the same name starts a journal of its
	// own, and the decisions listed are the ones that name this UID.
	UID       string      `json:"uid"`
	CreatedAt metav1.Time `json:"createdAt"`

	// Journal is status.journal, entry for entry, with what this plugin found
	// of the objects each entry names. It is absent when the status holds no
	// journal, which is a different answer from a journal with no entries.
	Journal *Journal `json:"journal,omitempty"`
	// PlansUnreadable is why no entry says whether its plan is still stored.
	PlansUnreadable *statusview.Unreadable `json:"plansUnreadable,omitempty"`

	// Approvals are every approval that names this resource by name and UID,
	// oldest first. It is absent when they could not be listed, and then
	// ApprovalsUnreadable says why: no list is not an empty list.
	Approvals           *statusview.Decisions[Approval] `json:"approvals,omitempty"`
	ApprovalsUnreadable *statusview.Unreadable          `json:"approvalsUnreadable,omitempty"`
	// Acknowledgments are the same for the run acknowledgments that name a
	// migration. A schema has none to list.
	Acknowledgments           *statusview.Decisions[Acknowledgment] `json:"acknowledgments,omitempty"`
	AcknowledgmentsUnreadable *statusview.Unreadable                `json:"acknowledgmentsUnreadable,omitempty"`
}

// Journal is the operator's bounded record of the resource's lifecycle.
type Journal struct {
	// CoveredSince is the instant from which the journal is complete. Entries
	// dated before it are ones the bounds keep, or ones dated by a time their
	// status carried from before it began.
	CoveredSince metav1.Time `json:"coveredSince"`
	// Entries are the journal's entries in the order the operator recorded
	// them, oldest first. It is empty, and never null, for a journal that has
	// recorded nothing since CoveredSince.
	Entries []Entry `json:"entries"`
}

// Entry is one journal entry as stored, with what this plugin found of the
// objects it names. Each finding is absent where the entry names no such
// object, recorded too little to join it, or the list it needs could not be
// read; false is an object the entry identifies that is no longer stored.
type Entry struct {
	operatorv1alpha1.JournalEntry

	// PlanStored says the plan the entry names is still stored as this
	// resource's plan: by name and UID where the entry recorded the UID, by
	// name otherwise, which a plan's name, derived from its content, is enough
	// for.
	PlanStored *bool `json:"planStored,omitempty"`
	// ApprovalStored says the approval the entry names by UID is still stored.
	// An entry with no approvalUID was written by a release before 0.2.10 and
	// is not joined to an approval that holds its name today.
	ApprovalStored *bool `json:"approvalStored,omitempty"`
	// AcknowledgmentStored says an acknowledgment of the entry's name that
	// settles the entry's operation is still stored.
	AcknowledgmentStored *bool `json:"acknowledgmentStored,omitempty"`
	// BeforeCoverage marks an entry dated before CoveredSince.
	BeforeCoverage bool `json:"beforeCoverage,omitempty"`
	// Proof says what became of the reading of the database that proves a
	// PtahSchema's Apply, on its ApplyFinished entry and no other. An Apply's
	// own outcome is not the verdict; this is where the verdict is.
	Proof *Proof `json:"proof,omitempty"`
}

// ProofState is what a schema Apply's proof came to, as far as the journal
// and the status say.
type ProofState string

const (
	// ProofVerified is an Apply a Verified entry for its plan follows, before
	// another Apply of that plan.
	ProofVerified ProofState = "Verified"
	// ProofOwed is the newest Apply, no Verified entry follows it, and
	// status.pendingObservation still owes the reading of its plan.
	ProofOwed ProofState = "Owed"
	// ProofNotInJournal is an Apply the journal records no proof of and the
	// status owes none for, as when the journal started or was restored
	// between the Apply and its proof.
	ProofNotInJournal ProofState = "NotInJournal"
)

// Proof is the reading that proves one schema Apply.
type Proof struct {
	State ProofState `json:"state"`
	// At and Outcome are the Verified entry's, where State is Verified.
	At      *metav1.Time                    `json:"at,omitempty"`
	Outcome operatorv1alpha1.JournalOutcome `json:"outcome,omitempty"`
}

// Approval is one approval of either family, with its own UID, which is what
// a journal entry joins it by.
type Approval struct {
	Name      string      `json:"name"`
	UID       string      `json:"uid"`
	CreatedAt metav1.Time `json:"createdAt"`
	// PlanRef and PlanFingerprint are the exact plan the approval names.
	PlanRef         operatorv1alpha1.ImmutableObjectReference `json:"planRef"`
	PlanFingerprint string                                    `json:"planFingerprint"`
	// Approver and ApprovedAt are what admission stamped from the
	// authenticated request.
	Approver   operatorv1alpha1.ApprovalIdentity `json:"approver"`
	ApprovedAt metav1.Time                       `json:"approvedAt"`
	Conditions []statusview.Condition            `json:"conditions,omitempty"`
}

// Acknowledgment is one PtahMigrationRunAcknowledgment that names the
// migration: the run it settles, and who made the statement.
type Acknowledgment struct {
	Name           string                            `json:"name"`
	UID            string                            `json:"uid"`
	CreatedAt      metav1.Time                       `json:"createdAt"`
	OperationID    string                            `json:"operationID"`
	AcknowledgedBy operatorv1alpha1.ApprovalIdentity `json:"acknowledgedBy"`
	AcknowledgedAt metav1.Time                       `json:"acknowledgedAt"`
	Conditions     []statusview.Condition            `json:"conditions,omitempty"`
}

// Load reads one resource's history.
//
// The resource is read first, and only a failure to read it fails the view.
// The plans and the decisions are listed after it, and each list that fails
// is recorded as the reason its part is missing.
func Load(ctx context.Context, reader client.Reader, kind Kind, namespace, name string) (View, error) {
	if reader == nil {
		return View{}, errors.New("a cluster reader is required")
	}
	if namespace == "" || name == "" {
		return View{}, errors.New("a namespace and a resource name are required")
	}
	key := client.ObjectKey{Namespace: namespace, Name: name}
	var (
		object  client.Object
		journal *operatorv1alpha1.LifecycleJournal
		pending *operatorv1alpha1.PendingObservationStatus
	)
	switch kind {
	case Schema:
		schema := &operatorv1alpha1.PtahSchema{}
		object = schema
		if err := get(ctx, reader, key, schema, kind); err != nil {
			return View{}, err
		}
		journal = schema.Status.Journal
		pending = schema.Status.PendingObservation
	case Migration:
		migration := &operatorv1alpha1.PtahMigration{}
		object = migration
		if err := get(ctx, reader, key, migration, kind); err != nil {
			return View{}, err
		}
		journal = migration.Status.Journal
	default:
		return View{}, fmt.Errorf("unknown resource kind %q", kind)
	}

	view := View{
		Kind:      kind,
		Namespace: object.GetNamespace(),
		Name:      object.GetName(),
		UID:       string(object.GetUID()),
		CreatedAt: object.GetCreationTimestamp(),
	}
	owner := object.GetUID()

	plans, plansUnreadable := storedPlans(ctx, reader, kind, namespace, owner)
	view.PlansUnreadable = plansUnreadable
	approvals, approvalsUnreadable := listApprovals(ctx, reader, kind, namespace, name, owner)
	view.ApprovalsUnreadable = approvalsUnreadable
	if approvalsUnreadable == nil {
		view.Approvals = statusview.Every(approvals,
			func(approval Approval) metav1.Time { return approval.CreatedAt },
			func(approval Approval) string { return approval.Name })
	}
	var acknowledgments []Acknowledgment
	if kind == Migration {
		var unreadable *statusview.Unreadable
		acknowledgments, unreadable = listAcknowledgments(ctx, reader, namespace, name, owner)
		view.AcknowledgmentsUnreadable = unreadable
		if unreadable == nil {
			view.Acknowledgments = statusview.Every(acknowledgments,
				func(acknowledgment Acknowledgment) metav1.Time { return acknowledgment.CreatedAt },
				func(acknowledgment Acknowledgment) string { return acknowledgment.Name })
		}
	}

	if journal != nil {
		view.Journal = &Journal{CoveredSince: journal.CoveredSince, Entries: make([]Entry, 0, len(journal.Entries))}
		for _, recorded := range journal.Entries {
			entry := Entry{JournalEntry: *recorded.DeepCopy()}
			entry.BeforeCoverage = recorded.At.Before(&journal.CoveredSince)
			if plansUnreadable == nil {
				entry.PlanStored = planStored(plans, recorded)
			}
			if approvalsUnreadable == nil {
				entry.ApprovalStored = approvalStored(approvals, recorded)
			}
			if kind == Migration && view.AcknowledgmentsUnreadable == nil {
				entry.AcknowledgmentStored = acknowledgmentStored(acknowledgments, recorded)
			}
			view.Journal.Entries = append(view.Journal.Entries, entry)
		}
		if kind == Schema {
			joinProofs(view.Journal.Entries, pending)
		}
	}
	return view, nil
}

// joinProofs sets the Proof of every ApplyFinished entry. A schema journal
// joins an Apply to its proof by plan name alone, and only forward: the
// Verified entry that settles an Apply comes after it, and no other Apply of
// that plan can come between, since an Apply is not claimed while a proof is
// owed. An earlier Verified entry of the same plan proved an earlier Apply.
func joinProofs(entries []Entry, pending *operatorv1alpha1.PendingObservationStatus) {
	newest := -1
	for index, entry := range entries {
		if entry.Type == operatorv1alpha1.JournalApplyFinished {
			newest = index
		}
	}
	for index := range entries {
		apply := &entries[index]
		if apply.Type != operatorv1alpha1.JournalApplyFinished {
			continue
		}
		apply.Proof = &Proof{State: ProofNotInJournal}
		for _, later := range entries[index+1:] {
			if later.Plan != apply.Plan {
				continue
			}
			if later.Type == operatorv1alpha1.JournalApplyFinished {
				break
			}
			if later.Type == operatorv1alpha1.JournalVerified {
				at := later.At
				apply.Proof = &Proof{State: ProofVerified, At: &at, Outcome: later.Outcome}
				break
			}
		}
		if apply.Proof.State == ProofNotInJournal && index == newest && pending != nil && pending.Plan.Name == apply.Plan {
			apply.Proof = &Proof{State: ProofOwed}
		}
	}
}

func get(ctx context.Context, reader client.Reader, key client.ObjectKey, object client.Object, kind Kind) error {
	if err := reader.Get(ctx, key, object); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Errorf("%w: %s %s/%s", ErrNotFound, kind, key.Namespace, key.Name)
		}
		return fmt.Errorf("read the %s: %w", kind, err)
	}
	return nil
}

// storedPlan is one plan this resource owns, as its metadata names it.
type storedPlan struct {
	name string
	uid  types.UID
}

// storedPlans lists the plans the resource owns. It lists metadata alone:
// the question is which plans are still stored, and a plan's content is what
// `kubectl ptah plan` reads, through the store's own checks.
//
// A plan is this resource's when its controller owner reference names the
// resource's UID, which the operator writes on every plan it publishes. A
// plan of a predecessor under the same name is not this resource's, and an
// entry of this journal never names one.
func storedPlans(ctx context.Context, reader client.Reader, kind Kind, namespace string, owner types.UID) ([]storedPlan, *statusview.Unreadable) {
	list := &metav1.PartialObjectMetadataList{}
	listKind := "PtahSchemaPlanList"
	if kind == Migration {
		listKind = "PtahMigrationPlanList"
	}
	list.SetGroupVersionKind(operatorv1alpha1.GroupVersion.WithKind(listKind))
	if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, statusview.UnreadableFrom(err)
	}
	var plans []storedPlan
	for index := range list.Items {
		plan := &list.Items[index]
		if controller := metav1.GetControllerOfNoCopy(plan); controller != nil && controller.UID == owner {
			plans = append(plans, storedPlan{name: plan.Name, uid: plan.UID})
		}
	}
	return plans, nil
}

func planStored(plans []storedPlan, entry operatorv1alpha1.JournalEntry) *bool {
	if entry.Plan == "" {
		return nil
	}
	for _, plan := range plans {
		if plan.name == entry.Plan && (entry.PlanUID == "" || plan.uid == entry.PlanUID) {
			return found(true)
		}
	}
	return found(false)
}

func approvalStored(approvals []Approval, entry operatorv1alpha1.JournalEntry) *bool {
	if entry.ApprovalUID == "" {
		return nil
	}
	for _, approval := range approvals {
		if approval.UID == string(entry.ApprovalUID) {
			return found(true)
		}
	}
	return found(false)
}

// acknowledgmentStored joins an entry to an acknowledgment by name and by the
// operation it settles. The entry records no UID, and a later acknowledgment
// may take the name; one that takes the name and settles the same operation
// of the same migration makes the same statement.
func acknowledgmentStored(acknowledgments []Acknowledgment, entry operatorv1alpha1.JournalEntry) *bool {
	if entry.Acknowledgment == "" || entry.OperationID == "" {
		return nil
	}
	for _, acknowledgment := range acknowledgments {
		if acknowledgment.Name == entry.Acknowledgment && acknowledgment.OperationID == entry.OperationID {
			return found(true)
		}
	}
	return found(false)
}

func found(value bool) *bool { return &value }

// listApprovals lists the approvals that name the resource by name and UID.
// A resource deleted and created again under the same name is another
// object, and an approval written for the first one says nothing about the
// second. Any error is the reason the list is missing rather than a failure
// of the view.
func listApprovals(
	ctx context.Context,
	reader client.Reader,
	kind Kind,
	namespace, name string,
	owner types.UID,
) ([]Approval, *statusview.Unreadable) {
	names := func(reference operatorv1alpha1.ImmutableObjectReference) bool {
		return reference.Name == name && reference.UID == owner
	}
	var approvals []Approval
	switch kind {
	case Schema:
		list := &operatorv1alpha1.PtahSchemaApprovalList{}
		if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return nil, statusview.UnreadableFrom(err)
		}
		for index := range list.Items {
			approval := &list.Items[index]
			if !names(approval.Spec.SchemaRef) {
				continue
			}
			approvals = append(approvals, approvalOf(approval.ObjectMeta, approval.Spec.PlanRef, approval.Spec.PlanFingerprint,
				approval.Spec.Approver, approval.Spec.ApprovedAt, approval.Status.Conditions))
		}
	case Migration:
		list := &operatorv1alpha1.PtahMigrationApprovalList{}
		if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return nil, statusview.UnreadableFrom(err)
		}
		for index := range list.Items {
			approval := &list.Items[index]
			if !names(approval.Spec.MigrationRef) {
				continue
			}
			approvals = append(approvals, approvalOf(approval.ObjectMeta, approval.Spec.PlanRef, approval.Spec.PlanFingerprint,
				approval.Spec.Approver, approval.Spec.ApprovedAt, approval.Status.Conditions))
		}
	}
	return approvals, nil
}

func approvalOf(
	object metav1.ObjectMeta,
	plan operatorv1alpha1.ImmutableObjectReference,
	fingerprint string,
	approver operatorv1alpha1.ApprovalIdentity,
	approvedAt metav1.Time,
	conditions []metav1.Condition,
) Approval {
	return Approval{
		Name:            object.Name,
		UID:             string(object.UID),
		CreatedAt:       object.CreationTimestamp,
		PlanRef:         plan,
		PlanFingerprint: fingerprint,
		Approver:        *approver.DeepCopy(),
		ApprovedAt:      approvedAt,
		Conditions:      statusview.FromConditions(conditions),
	}
}

func listAcknowledgments(
	ctx context.Context,
	reader client.Reader,
	namespace, name string,
	owner types.UID,
) ([]Acknowledgment, *statusview.Unreadable) {
	list := &operatorv1alpha1.PtahMigrationRunAcknowledgmentList{}
	if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, statusview.UnreadableFrom(err)
	}
	var acknowledgments []Acknowledgment
	for index := range list.Items {
		acknowledgment := &list.Items[index]
		if acknowledgment.Spec.MigrationRef.Name != name || acknowledgment.Spec.MigrationRef.UID != owner {
			continue
		}
		acknowledgments = append(acknowledgments, Acknowledgment{
			Name:           acknowledgment.Name,
			UID:            string(acknowledgment.UID),
			CreatedAt:      acknowledgment.CreationTimestamp,
			OperationID:    acknowledgment.Spec.OperationID,
			AcknowledgedBy: *acknowledgment.Spec.AcknowledgedBy.DeepCopy(),
			AcknowledgedAt: acknowledgment.Spec.AcknowledgedAt,
			Conditions:     statusview.FromConditions(acknowledgment.Status.Conditions),
		})
	}
	return acknowledgments, nil
}
