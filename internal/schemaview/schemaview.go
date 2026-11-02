// Package schemaview reads where one PtahSchema stands.
//
// It is the read side of the declarative path, and the companion to
// [ptah.run/ptah-operator/internal/planview]: that package reads the SQL a plan
// holds, this one reads what the operator observed and what it would do about
// it. It connects to no database, reads no Secret, and reads no plan chunk.
//
// The reference-data section is why this exists separately from the plan view.
// A plan is SQL, and reading a data plan is reading values; the observation is
// counts, and a reader who may not see the rows can still see that three of
// them differ and which way.
//
// Beside the schema it lists the PtahSchemaApprovals that name it, because an
// approval's conditions are where the controller says whether it was accepted,
// consumed or made stale. Listing them takes a permission reading the schema
// does not, so a reader without it still gets the schema, and a line that
// says why the approvals are missing.
package schemaview

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourceview"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// ErrSchemaNotFound is returned when the named PtahSchema is not there. It is
// separate from a read failure: "there is nothing by that name" is an answer.
var ErrSchemaNotFound = errors.New("no PtahSchema by that name")

// Reference-data drift categories, as the native drift report names them.
//
// They are the whole reference-data contribution to an observation: a count
// each, and nothing that identifies a row. Naming them here rather than
// matching a prefix keeps a future structural category that happens to start
// with the same letters out of the reference-data section.
const (
	rowsInsertedCategory = "data_rows_inserted"
	rowsUpdatedCategory  = "data_rows_updated"
	rowsDeletedCategory  = "data_rows_deleted"
)

// View is everything this command prints, resolved before anything is written.
type View struct {
	Namespace string `json:"namespace"`
	Schema    string `json:"schema"`
	UID       string `json:"uid,omitempty"`
	// Generation is the spec's, and ObservedGeneration the one the status
	// was written for. They differ while the controller has not yet acted
	// on a spec change.
	Generation         int64            `json:"generation,omitempty"`
	ObservedGeneration int64            `json:"observedGeneration,omitempty"`
	Phase              string           `json:"phase,omitempty"`
	Suspended          bool             `json:"suspended,omitempty"`
	Policy             *PolicyView      `json:"policy,omitempty"`
	Source             *SourceView      `json:"source,omitempty"`
	Observation        *ObservationView `json:"observation,omitempty"`
	Plan               *PlanView        `json:"plan,omitempty"`
	Applied            *AppliedView     `json:"applied,omitempty"`
	Conditions         []ConditionView  `json:"conditions,omitempty"`
	// Approvals are the newest PtahSchemaApprovals that name this schema, by
	// name and UID. It is absent when they could not be listed, and then
	// ApprovalsUnreadable says why: no list is not an empty list.
	Approvals           *statusview.Decisions[ApprovalView] `json:"approvals,omitempty"`
	ApprovalsUnreadable *statusview.Unreadable              `json:"approvalsUnreadable,omitempty"`
}

// PolicyView is the spec's policy as far as it decides whether a plan runs:
// the apply mode, whether a destructive plan is permitted at all, which drift
// counts, and the tables nothing may change. It is what the spec asks for
// now; the conditions say what the controller made of it.
type PolicyView struct {
	Apply            string   `json:"apply,omitempty"`
	AllowDestructive bool     `json:"allowDestructive"`
	DriftSeverity    string   `json:"driftSeverity,omitempty"`
	ProtectedTables  []string `json:"protectedTables,omitempty"`
}

// ApprovalView is one PtahSchemaApproval that names this schema.
type ApprovalView = statusview.Approval

// SourceView is the resolved, credential-free source binding: an OCI
// artifact, a ConfigMap and its snapshot, or a Git commit.
type SourceView = sourceview.View

// ObservationView is what the last drift read found.
//
// Findings holds every category the report carried, structural and
// reference-data alike, because the count a policy threshold acts on is the
// whole report. ReferenceData restates the three row categories as one row so
// a reader deciding about a data change does not have to pick them out.
type ObservationView struct {
	ObservedAt      *metav1.Time       `json:"observedAt,omitempty"`
	Drift           bool               `json:"drift"`
	HighestSeverity string             `json:"highestSeverity,omitempty"`
	FindingCount    int32              `json:"findingCount,omitempty"`
	Findings        []FindingView      `json:"findings,omitempty"`
	ReferenceData   *ReferenceDataView `json:"referenceData,omitempty"`
}

// FindingView is one category aggregate of the observation.
type FindingView struct {
	Category string `json:"category"`
	Count    int32  `json:"count"`
	Severity string `json:"severity"`
}

// ReferenceDataView is how far the declared rows sit from the database, as
// counts. It carries no key, no column name and no value: a reader who needs
// to know which rows reads the plan, which is data access.
//
// It is present whenever the observation carried a reference-data category and
// absent otherwise, so a schema that declares no rows says nothing here rather
// than reporting three zeroes a reader would have to interpret.
type ReferenceDataView struct {
	Inserts int32 `json:"inserts"`
	Updates int32 `json:"updates"`
	Deletes int32 `json:"deletes"`
}

// PlanView is the plan the operator would run next, named rather than printed:
// the SQL is what `kubectl ptah plan` is for.
type PlanView struct {
	Name             string       `json:"name"`
	UID              string       `json:"uid,omitempty"`
	Fingerprint      string       `json:"fingerprint,omitempty"`
	StatementCount   int32        `json:"statementCount"`
	Destructive      bool         `json:"destructive,omitempty"`
	PrivilegeChanges []string     `json:"privilegeChanges,omitempty"`
	CreatedAt        metav1.Time  `json:"createdAt"`
	Approved         bool         `json:"approved"`
	ApprovedAt       *metav1.Time `json:"approvedAt,omitempty"`
	ApprovalName     string       `json:"approvalName,omitempty"`
	ApprovalApprover string       `json:"approvalApprover,omitempty"`
}

// AppliedView is what the last confirmed apply ran.
type AppliedView struct {
	PlanName        string      `json:"planName,omitempty"`
	PlanFingerprint string      `json:"planFingerprint"`
	PtahVersion     string      `json:"ptahVersion,omitempty"`
	CompletedAt     metav1.Time `json:"completedAt"`
}

// ConditionView is one published condition: what it says, and when and for
// which generation it was written.
type ConditionView = statusview.Condition

// Load reads one schema and says where it stands.
//
// Everything about the schema comes from the schema object itself. No plan
// object is read: the status records what a reader of this view acts on, and
// reading the plan as well would make this command fail on a plan that was
// collected between the two reads, about a schema whose status is perfectly
// readable. The approvals are listed after it, and a list that fails leaves
// the rest of the view as it was.
func Load(ctx context.Context, reader client.Reader, namespace, name string) (View, error) {
	if reader == nil {
		return View{}, errors.New("a cluster reader is required")
	}
	if namespace == "" || name == "" {
		return View{}, errors.New("a namespace and a schema name are required")
	}
	schema := &operatorv1alpha1.PtahSchema{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, schema); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return View{}, fmt.Errorf("%w: %s/%s", ErrSchemaNotFound, namespace, name)
		}
		return View{}, fmt.Errorf("read the schema: %w", err)
	}

	view := View{
		Namespace:          schema.Namespace,
		Schema:             schema.Name,
		UID:                string(schema.UID),
		Generation:         schema.Generation,
		ObservedGeneration: schema.Status.ObservedGeneration,
		Phase:              string(schema.Status.Phase),
		Suspended:          schema.Spec.Suspend,
		Policy:             policyOf(schema.Spec.Policy),
	}
	view.Source = sourceview.FromSchema(schema.Status.Source)
	view.Observation = observationOf(schema.Status.Target)
	if plan := schema.Status.Plan; plan != nil {
		view.Plan = &PlanView{
			Name:           plan.Name,
			UID:            string(plan.UID),
			Fingerprint:    plan.Fingerprint,
			StatementCount: plan.StatementCount,
			Destructive:    plan.Destructive,
			CreatedAt:      plan.CreatedAt,
		}
		for _, kind := range plan.PrivilegeChanges {
			view.Plan.PrivilegeChanges = append(view.Plan.PrivilegeChanges, string(kind))
		}
		if approval := plan.Approval; approval != nil {
			view.Plan.Approved = true
			view.Plan.ApprovedAt = &approval.ApprovedAt
			view.Plan.ApprovalName = approval.Name
			view.Plan.ApprovalApprover = approval.Approver.Username
		}
	}
	if applied := schema.Status.Applied; applied != nil {
		view.Applied = &AppliedView{
			PlanName:        applied.PlanRef.Name,
			PlanFingerprint: applied.PlanFingerprint,
			PtahVersion:     applied.PtahVersion,
			CompletedAt:     applied.CompletedAt,
		}
	}
	view.Conditions = statusview.FromConditions(schema.Status.Conditions)
	view.Approvals, view.ApprovalsUnreadable = approvalsOf(ctx, reader, schema)
	return view, nil
}

func policyOf(policy operatorv1alpha1.ReconciliationPolicy) *PolicyView {
	return &PolicyView{
		Apply:            string(policy.Apply),
		AllowDestructive: policy.AllowDestructive,
		DriftSeverity:    policy.DriftSeverity,
		ProtectedTables:  slices.Clone(policy.ProtectedTables),
	}
}

// approvalsOf lists the approvals that name this schema.
//
// An approval names a schema by name and UID, and only both together are this
// schema: a schema deleted and recreated under the same name is another
// object, and an approval written for the first one says nothing about the
// second. Admission refuses an approval whose UID is not the live schema's,
// so one that differs was written for a predecessor.
//
// Any error is returned as the reason the list is missing rather than as a
// failure of the view. Forbidden is the expected one: reading a schema needs
// get on it and nothing else, and a reader granted only that still gets the
// schema.
func approvalsOf(
	ctx context.Context,
	reader client.Reader,
	schema *operatorv1alpha1.PtahSchema,
) (*statusview.Decisions[ApprovalView], *statusview.Unreadable) {
	list := &operatorv1alpha1.PtahSchemaApprovalList{}
	if err := reader.List(ctx, list, client.InNamespace(schema.Namespace)); err != nil {
		return nil, statusview.UnreadableFrom(err)
	}
	var named []ApprovalView
	for index := range list.Items {
		approval := &list.Items[index]
		if approval.Spec.SchemaRef.Name != schema.Name || approval.Spec.SchemaRef.UID != schema.UID {
			continue
		}
		named = append(named, ApprovalView{
			Name:            approval.Name,
			CreatedAt:       approval.CreationTimestamp,
			PlanRef:         approval.Spec.PlanRef,
			PlanFingerprint: approval.Spec.PlanFingerprint,
			CurrentPlan:     approvesCurrentPlan(approval.Spec, schema.Status.Plan),
			AppliedPlan:     approvesAppliedPlan(approval.Spec, schema.Status.Applied),
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
// by name, UID and fingerprint: a plan recreated under the same name is
// another plan, and the fingerprint is the plan's whole binding.
func approvesCurrentPlan(approval operatorv1alpha1.PtahSchemaApprovalSpec, plan *operatorv1alpha1.CurrentPlanStatus) bool {
	return plan != nil && plan.UID != "" &&
		approval.PlanRef.Name == plan.Name &&
		approval.PlanRef.UID == plan.UID &&
		approval.PlanFingerprint == plan.Fingerprint
}

// approvesAppliedPlan says the approval names the plan the last confirmed
// apply ran, the same way. Once a schema converges it has no current plan, and
// the approval that apply consumed would otherwise read as one of a plan
// nothing remembers.
func approvesAppliedPlan(approval operatorv1alpha1.PtahSchemaApprovalSpec, applied *operatorv1alpha1.AppliedStatus) bool {
	return applied != nil && applied.PlanRef.UID != "" &&
		approval.PlanRef.Name == applied.PlanRef.Name &&
		approval.PlanRef.UID == applied.PlanRef.UID &&
		approval.PlanFingerprint == applied.PlanFingerprint
}

// observationOf reduces the target status to what a reader acts on.
//
// A target that was never observed has no timestamp and no findings, and says
// nothing rather than reporting a converged database it never looked at.
//
// Drift is read from the highest severity, which the operator records exactly
// when the report found drift, and not from the findings: a difference the
// report lists under no category is drift with no findings.
func observationOf(target operatorv1alpha1.TargetStatus) *ObservationView {
	if target.LastObservedAt == nil && len(target.DriftFindings) == 0 {
		return nil
	}
	observation := &ObservationView{
		ObservedAt:      target.LastObservedAt,
		Drift:           target.HighestDriftSeverity != "",
		HighestSeverity: target.HighestDriftSeverity,
		FindingCount:    target.DriftFindingCount,
	}
	for _, finding := range target.DriftFindings {
		observation.Findings = append(observation.Findings, FindingView{
			Category: finding.Category, Count: finding.Count, Severity: finding.Severity,
		})
	}
	slices.SortFunc(observation.Findings, func(left, right FindingView) int {
		return strings.Compare(left.Category, right.Category)
	})
	observation.ReferenceData = referenceDataOf(observation.Findings)
	return observation
}

// referenceDataOf restates the three row categories as one row, and returns
// nothing when the observation carried none of them.
func referenceDataOf(findings []FindingView) *ReferenceDataView {
	rows := ReferenceDataView{}
	carried := false
	for _, finding := range findings {
		switch finding.Category {
		case rowsInsertedCategory:
			rows.Inserts = finding.Count
			carried = true
		case rowsUpdatedCategory:
			rows.Updates = finding.Count
			carried = true
		case rowsDeletedCategory:
			rows.Deletes = finding.Count
			carried = true
		}
	}
	if !carried {
		return nil
	}
	return &rows
}
