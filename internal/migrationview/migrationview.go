// Package migrationview reads what the operator stored about one migration.
//
// It is the read side of the migration path: what the database's own history
// said when it was last read, which sequence the operator would run next, and
// what the last run did. It connects to no database and reads no Secret.
//
// Beside the migration it lists the PtahMigrationApprovals and the
// PtahMigrationRunAcknowledgments that name it: the decisions people made
// about its plans and about a run nobody accounted for. Listing them takes a
// permission reading the migration does not, so a reader without it still
// gets the migration, and a line that says why the decisions are missing.
package migrationview

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourceview"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// ErrMigrationNotFound is returned when the named PtahMigration is not there.
// It is separate from a read failure: "there is nothing by that name" is an
// answer.
var ErrMigrationNotFound = errors.New("no PtahMigration by that name")

// View is everything this command prints, resolved before anything is written.
type View struct {
	Namespace string `json:"namespace"`
	Migration string `json:"migration"`
	UID       string `json:"uid,omitempty"`
	// Generation is the spec's, and ObservedGeneration the one the status
	// was written for. They differ while the controller has not yet acted
	// on a spec change.
	Generation         int64         `json:"generation,omitempty"`
	ObservedGeneration int64         `json:"observedGeneration,omitempty"`
	Phase              string        `json:"phase,omitempty"`
	Suspended          bool          `json:"suspended,omitempty"`
	Policy             *PolicyView   `json:"policy,omitempty"`
	Artifact           *ArtifactView `json:"artifact,omitempty"`
	History            *HistoryView  `json:"history,omitempty"`
	Plan               *PlanView     `json:"plan,omitempty"`
	// LastRun is the most recent execution. UnresolvedRun is a different
	// record: the execution nobody has accounted for, which holds the
	// resource whatever the last run or the conditions say. ResolvedRun is
	// how the most recent such record was settled.
	LastRun       *RunView           `json:"lastRun,omitempty"`
	UnresolvedRun *UnresolvedRunView `json:"unresolvedRun,omitempty"`
	ResolvedRun   *ResolvedRunView   `json:"resolvedRun,omitempty"`
	Conditions    []ConditionView    `json:"conditions,omitempty"`
	// Approvals and Acknowledgments are the newest decisions that name this
	// migration, by name and UID. Each is absent when it could not be listed,
	// and then its Unreadable field says why: no list is not an empty list.
	Approvals                 *statusview.Decisions[ApprovalView]       `json:"approvals,omitempty"`
	ApprovalsUnreadable       *statusview.Unreadable                    `json:"approvalsUnreadable,omitempty"`
	Acknowledgments           *statusview.Decisions[AcknowledgmentView] `json:"acknowledgments,omitempty"`
	AcknowledgmentsUnreadable *statusview.Unreadable                    `json:"acknowledgmentsUnreadable,omitempty"`
	// Details is the status as this plugin's API version decodes it, present
	// only when it was asked for, so the view without it reads as it always
	// has.
	Details *Details `json:"details,omitempty"`
}

// PolicyView is the spec's apply mode: whether a planned sequence runs
// never, with an approval, or as soon as it is planned.
type PolicyView struct {
	Apply string `json:"apply,omitempty"`
}

// ArtifactView is the resolved, credential-free source binding: an OCI
// artifact, a ConfigMap and its snapshot, or a Git commit.
type ArtifactView = sourceview.View

// HistoryView is what the revision table said when it was last read.
type HistoryView struct {
	ObservedAt        metav1.Time `json:"observedAt"`
	CurrentVersion    int64       `json:"currentVersion"`
	CheckpointVersion int64       `json:"checkpointVersion,omitempty"`
	AppliedCount      int32       `json:"appliedCount"`
	PendingCount      int32       `json:"pendingCount"`
	Dirty             bool        `json:"dirty,omitempty"`
	ModifiedVersions  []int64     `json:"modifiedVersions,omitempty"`
	// OutOfOrderVersions names migrations that sort below the current version.
	// Linear execution refuses them, so a reader of this view has to see which
	// ones rather than a phase that says the history is blocked.
	OutOfOrderVersions []int64 `json:"outOfOrderVersions,omitempty"`
	Fingerprint        string  `json:"fingerprint,omitempty"`
}

// PlanView is the sequence the operator would run next. A migration plan holds
// versions and checksums rather than SQL: the statements live in the artifact,
// and printing them here would mean fetching it.
type PlanView struct {
	Name           string                 `json:"name"`
	UID            string                 `json:"uid,omitempty"`
	Fingerprint    string                 `json:"fingerprint"`
	CurrentVersion int64                  `json:"currentVersion"`
	CreatedAt      metav1.Time            `json:"createdAt"`
	Migrations     []PlannedMigrationView `json:"migrations"`
}

// PlannedMigrationView is one migration of the sequence.
type PlannedMigrationView struct {
	Version         int64  `json:"version"`
	VersionKey      string `json:"versionKey,omitempty"`
	Description     string `json:"description,omitempty"`
	Checksum        string `json:"checksum"`
	Checkpoint      bool   `json:"checkpoint,omitempty"`
	TransactionMode string `json:"transactionMode,omitempty"`
}

// RunView is what the database accounted for after the last execution, and
// which Job that execution was. The Job's UID is what tells a replacement Job
// with the same name apart from the run it replaced.
type RunView struct {
	Outcome         string       `json:"outcome"`
	JobName         string       `json:"jobName,omitempty"`
	JobUID          string       `json:"jobUID,omitempty"`
	StartedAt       metav1.Time  `json:"startedAt"`
	FinishedAt      *metav1.Time `json:"finishedAt,omitempty"`
	AppliedVersions []int64      `json:"appliedVersions,omitempty"`
	Message         string       `json:"message,omitempty"`
	// Failure is where a failed or partial run stopped, when a statement
	// reached the database and failed there.
	Failure *operatorv1alpha1.MigrationRunFailureStatus `json:"failure,omitempty"`
	// OperationID, PlanRef, PlanFingerprint, Authorization, ArtifactDigest,
	// Source, TargetIdentityDigest, ExecutionBindingID and Verification are what
	// the run's record says it carried out, who allowed it and what the reading
	// after it found. Each is absent where the record does not hold it, which is
	// every one of them on a run a release before those fields recorded.
	OperationID          string                                            `json:"operationID,omitempty"`
	PlanRef              *operatorv1alpha1.ImmutableObjectReference        `json:"planRef,omitempty"`
	PlanFingerprint      string                                            `json:"planFingerprint,omitempty"`
	Authorization        *operatorv1alpha1.MigrationRunAuthorizationStatus `json:"authorization,omitempty"`
	ArtifactDigest       string                                            `json:"artifactDigest,omitempty"`
	Source               *operatorv1alpha1.PlanSourceBinding               `json:"source,omitempty"`
	TargetIdentityDigest string                                            `json:"targetIdentityDigest,omitempty"`
	ExecutionBindingID   string                                            `json:"executionBindingID,omitempty"`
	Verification         *operatorv1alpha1.MigrationRunVerificationStatus  `json:"verification,omitempty"`
}

// UnresolvedRunView is the execution whose effect on the database nobody has
// established. While it is recorded nothing is planned and nothing runs.
type UnresolvedRunView struct {
	Outcome     string `json:"outcome"`
	OperationID string `json:"operationID"`
	// JobName and JobUID are empty where the API server never confirmed the
	// Job was created: the reserved name is not evidence that anything ran.
	JobName string                                    `json:"jobName,omitempty"`
	JobUID  string                                    `json:"jobUID,omitempty"`
	PlanRef operatorv1alpha1.ImmutableObjectReference `json:"planRef"`
	// TargetIdentityDigest is the credential-free identity of the database
	// the run's result named, or the one its plan was computed against where
	// it named none.
	TargetIdentityDigest string      `json:"targetIdentityDigest,omitempty"`
	RecordedAt           metav1.Time `json:"recordedAt"`
}

// ResolvedRunView is how the most recent unresolved run was settled: by a
// history read, or by a person's acknowledgment.
type ResolvedRunView struct {
	OperationID        string      `json:"operationID"`
	Outcome            string      `json:"outcome"`
	Resolution         string      `json:"resolution"`
	AcknowledgmentName string      `json:"acknowledgmentName,omitempty"`
	AcknowledgedBy     string      `json:"acknowledgedBy,omitempty"`
	ResolvedAt         metav1.Time `json:"resolvedAt"`
}

// ConditionView is one published condition: what it says, and when and for
// which generation it was written.
type ConditionView = statusview.Condition

// ApprovalView is one PtahMigrationApproval that names this migration.
// A migration approval carries only Consumed, or Stale when a read withdrew it;
// whether it matches the plan is what CurrentPlan and the migration's own
// ApprovalRequired say.
type ApprovalView = statusview.Approval

// AcknowledgmentView is one PtahMigrationRunAcknowledgment that names this
// migration: the run it settles, who made the statement, and whether the
// controller took it.
type AcknowledgmentView struct {
	Name        string      `json:"name"`
	CreatedAt   metav1.Time `json:"createdAt"`
	OperationID string      `json:"operationID"`
	// NamesUnresolvedRun says the acknowledgment names the run the migration
	// records as unresolved now. One that names any other run settles nothing.
	NamesUnresolvedRun bool                              `json:"namesUnresolvedRun"`
	AcknowledgedBy     operatorv1alpha1.ApprovalIdentity `json:"acknowledgedBy"`
	AcknowledgedAt     metav1.Time                       `json:"acknowledgedAt"`
	Conditions         []ConditionView                   `json:"conditions,omitempty"`
}

// Load reads one migration, the plan it names, and the decisions that name it.
//
// A plan the migration no longer names is not shown: the status reference is
// what makes a stored plan the current one, and a plan read by name alone
// could be one the operator already discarded. A decision list that cannot
// be read leaves the rest of the view as it was.
func Load(
	ctx context.Context,
	reader client.Reader,
	namespace, name string,
) (View, error) {
	return load(ctx, reader, namespace, name, false)
}

// LoadWithDetails reads the view and the status beside it, from the same
// read of the migration.
func LoadWithDetails(ctx context.Context, reader client.Reader, namespace, name string) (View, error) {
	return load(ctx, reader, namespace, name, true)
}

func load(ctx context.Context, reader client.Reader, namespace, name string, details bool) (View, error) {
	if reader == nil {
		return View{}, errors.New("a cluster reader is required")
	}
	if namespace == "" || name == "" {
		return View{}, errors.New("a namespace and a migration name are required")
	}
	migration := &operatorv1alpha1.PtahMigration{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, migration); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return View{}, fmt.Errorf("%w: %s/%s", ErrMigrationNotFound, namespace, name)
		}
		return View{}, fmt.Errorf("read the migration: %w", err)
	}

	view := View{
		Namespace:          migration.Namespace,
		Migration:          migration.Name,
		UID:                string(migration.UID),
		Generation:         migration.Generation,
		ObservedGeneration: migration.Status.ObservedGeneration,
		Phase:              string(migration.Status.Phase),
		Suspended:          migration.Spec.Suspend,
		Policy:             &PolicyView{Apply: string(migration.Spec.Policy.Apply)},
	}
	view.Artifact = sourceview.FromMigration(migration.Status.Artifact, migration.Status.ConfigMap, migration.Status.SourcePolicy)
	if history := migration.Status.History; history != nil {
		view.History = &HistoryView{
			ObservedAt:         history.ObservedAt,
			CurrentVersion:     history.CurrentVersion,
			CheckpointVersion:  history.CheckpointVersion,
			AppliedCount:       history.AppliedCount,
			PendingCount:       history.PendingCount,
			Dirty:              history.Dirty,
			ModifiedVersions:   append([]int64(nil), history.ModifiedVersions...),
			OutOfOrderVersions: append([]int64(nil), history.OutOfOrderVersions...),
			Fingerprint:        history.Fingerprint,
		}
	}
	view.LastRun = runOf(migration.Status.LastRun)
	view.UnresolvedRun = unresolvedRunOf(migration.Status.UnresolvedRun)
	view.ResolvedRun = resolvedRunOf(migration.Status.ResolvedRun)
	view.Conditions = statusview.FromConditions(migration.Status.Conditions)

	plan, err := currentPlan(ctx, reader, migration)
	if err != nil {
		return View{}, err
	}
	view.Plan = planOf(plan)
	view.Approvals, view.ApprovalsUnreadable = approvalsOf(ctx, reader, migration, plan)
	view.Acknowledgments, view.AcknowledgmentsUnreadable = acknowledgmentsOf(ctx, reader, migration)
	if details {
		view.Details = detailsOf(migration)
	}
	return view, nil
}

// currentPlan reads the plan the status names, and returns nil for a
// reference that outlived its object or names an object that was replaced.
// Either is said by omission rather than by failing: everything else read
// here is still true.
func currentPlan(
	ctx context.Context,
	reader client.Reader,
	migration *operatorv1alpha1.PtahMigration,
) (*operatorv1alpha1.PtahMigrationPlan, error) {
	if migration.Status.Plan == nil {
		return nil, nil
	}
	plan := &operatorv1alpha1.PtahMigrationPlan{}
	key := client.ObjectKey{Namespace: migration.Namespace, Name: migration.Status.Plan.Name}
	if err := reader.Get(ctx, key, plan); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("read the migration plan: %w", err)
	}
	if plan.UID != migration.Status.Plan.UID {
		return nil, nil
	}
	return plan, nil
}

func planOf(plan *operatorv1alpha1.PtahMigrationPlan) *PlanView {
	if plan == nil {
		return nil
	}
	view := &PlanView{
		Name:           plan.Name,
		UID:            string(plan.UID),
		Fingerprint:    plan.Spec.Fingerprint,
		CurrentVersion: plan.Spec.CurrentVersion,
		CreatedAt:      plan.Spec.CreatedAt,
	}
	for _, planned := range plan.Spec.Migrations {
		view.Migrations = append(view.Migrations, PlannedMigrationView{
			Version:         planned.Version,
			VersionKey:      planned.VersionKey,
			Description:     planned.Description,
			Checksum:        planned.Checksum,
			Checkpoint:      planned.Checkpoint,
			TransactionMode: planned.TransactionMode,
		})
	}
	return view
}

func runOf(run *operatorv1alpha1.MigrationRunStatus) *RunView {
	if run == nil {
		return nil
	}
	view := &RunView{
		Outcome:         string(run.Outcome),
		JobName:         run.JobName,
		JobUID:          string(run.JobUID),
		StartedAt:       run.StartedAt,
		FinishedAt:      run.FinishedAt,
		AppliedVersions: append([]int64(nil), run.AppliedVersions...),
		Message:         run.Message,

		OperationID:          run.OperationID,
		PlanRef:              run.PlanRef.DeepCopy(),
		PlanFingerprint:      run.PlanFingerprint,
		Authorization:        run.Authorization.DeepCopy(),
		ArtifactDigest:       run.ArtifactDigest,
		Source:               run.Source.DeepCopy(),
		TargetIdentityDigest: run.TargetIdentityDigest,
		ExecutionBindingID:   run.ExecutionBindingID,
		Verification:         run.Verification.DeepCopy(),
	}
	if run.Failure != nil {
		failure := *run.Failure
		view.Failure = &failure
	}
	return view
}

func unresolvedRunOf(run *operatorv1alpha1.UnresolvedMigrationRunStatus) *UnresolvedRunView {
	if run == nil {
		return nil
	}
	return &UnresolvedRunView{
		Outcome:              string(run.Outcome),
		OperationID:          run.OperationID,
		JobName:              run.JobName,
		JobUID:               string(run.JobUID),
		PlanRef:              run.PlanRef,
		TargetIdentityDigest: run.TargetIdentityDigest,
		RecordedAt:           run.RecordedAt,
	}
}

func resolvedRunOf(run *operatorv1alpha1.ResolvedMigrationRunStatus) *ResolvedRunView {
	if run == nil {
		return nil
	}
	view := &ResolvedRunView{
		OperationID: run.OperationID,
		Outcome:     string(run.Outcome),
		Resolution:  string(run.Resolution),
		ResolvedAt:  run.ResolvedAt,
	}
	if run.AcknowledgmentRef != nil {
		view.AcknowledgmentName = run.AcknowledgmentRef.Name
	}
	if run.AcknowledgedBy != nil {
		view.AcknowledgedBy = run.AcknowledgedBy.Username
	}
	return view
}

// namesMigration says a reference is this migration: by name and by UID, since
// a migration deleted and recreated under the same name is another object,
// and a decision written for the first says nothing about the second.
func namesMigration(reference operatorv1alpha1.ImmutableObjectReference, migration *operatorv1alpha1.PtahMigration) bool {
	return reference.Name == migration.Name && reference.UID == migration.UID
}

// approvalsOf lists the approvals that name this migration. Any error is the
// reason the list is missing rather than a failure of the view: reading a
// migration needs no list permission, and Forbidden is what a reader without
// one gets.
func approvalsOf(
	ctx context.Context,
	reader client.Reader,
	migration *operatorv1alpha1.PtahMigration,
	plan *operatorv1alpha1.PtahMigrationPlan,
) (*statusview.Decisions[ApprovalView], *statusview.Unreadable) {
	list := &operatorv1alpha1.PtahMigrationApprovalList{}
	if err := reader.List(ctx, list, client.InNamespace(migration.Namespace)); err != nil {
		return nil, statusview.UnreadableFrom(err)
	}
	var named []ApprovalView
	for index := range list.Items {
		approval := &list.Items[index]
		if !namesMigration(approval.Spec.MigrationRef, migration) {
			continue
		}
		named = append(named, ApprovalView{
			Name:            approval.Name,
			CreatedAt:       approval.CreationTimestamp,
			PlanRef:         approval.Spec.PlanRef,
			PlanFingerprint: approval.Spec.PlanFingerprint,
			CurrentPlan:     approvesCurrentPlan(approval.Spec, plan),
			Approver:        approval.Spec.Approver,
			ApprovedAt:      approval.Spec.ApprovedAt,
			Conditions:      statusview.FromConditions(approval.Status.Conditions),
		})
	}
	return statusview.Newest(named,
		func(approval ApprovalView) metav1.Time { return approval.CreatedAt },
		func(approval ApprovalView) string { return approval.Name }), nil
}

// approvesCurrentPlan says the approval names the plan the status names now,
// as it was read: by name, UID and fingerprint. A plan that could not be read
// is no plan an approval can be consumed for.
func approvesCurrentPlan(approval operatorv1alpha1.PtahMigrationApprovalSpec, plan *operatorv1alpha1.PtahMigrationPlan) bool {
	return plan != nil && plan.UID != "" &&
		approval.PlanRef.Name == plan.Name &&
		approval.PlanRef.UID == plan.UID &&
		approval.PlanFingerprint == plan.Spec.Fingerprint
}

// acknowledgmentsOf lists the run acknowledgments that name this migration,
// the same way approvalsOf lists approvals.
func acknowledgmentsOf(
	ctx context.Context,
	reader client.Reader,
	migration *operatorv1alpha1.PtahMigration,
) (*statusview.Decisions[AcknowledgmentView], *statusview.Unreadable) {
	list := &operatorv1alpha1.PtahMigrationRunAcknowledgmentList{}
	if err := reader.List(ctx, list, client.InNamespace(migration.Namespace)); err != nil {
		return nil, statusview.UnreadableFrom(err)
	}
	unresolved := ""
	if run := migration.Status.UnresolvedRun; run != nil {
		unresolved = run.OperationID
	}
	var named []AcknowledgmentView
	for index := range list.Items {
		acknowledgment := &list.Items[index]
		if !namesMigration(acknowledgment.Spec.MigrationRef, migration) {
			continue
		}
		named = append(named, AcknowledgmentView{
			Name:               acknowledgment.Name,
			CreatedAt:          acknowledgment.CreationTimestamp,
			OperationID:        acknowledgment.Spec.OperationID,
			NamesUnresolvedRun: unresolved != "" && acknowledgment.Spec.OperationID == unresolved,
			AcknowledgedBy:     acknowledgment.Spec.AcknowledgedBy,
			AcknowledgedAt:     acknowledgment.Spec.AcknowledgedAt,
			Conditions:         statusview.FromConditions(acknowledgment.Status.Conditions),
		})
	}
	return statusview.Newest(named,
		func(acknowledgment AcknowledgmentView) metav1.Time { return acknowledgment.CreatedAt },
		func(acknowledgment AcknowledgmentView) string { return acknowledgment.Name }), nil
}
