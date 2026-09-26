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
	Namespace   string           `json:"namespace"`
	Schema      string           `json:"schema"`
	Phase       string           `json:"phase,omitempty"`
	Suspended   bool             `json:"suspended,omitempty"`
	Source      *SourceView      `json:"source,omitempty"`
	Observation *ObservationView `json:"observation,omitempty"`
	Plan        *PlanView        `json:"plan,omitempty"`
	Applied     *AppliedView     `json:"applied,omitempty"`
	Conditions  []ConditionView  `json:"conditions,omitempty"`
}

// SourceView is the resolved, credential-free artifact binding.
type SourceView struct {
	Reference string `json:"reference,omitempty"`
	Digest    string `json:"digest,omitempty"`
	Verified  bool   `json:"verified,omitempty"`
}

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
	Truncated       bool               `json:"findingsTruncated,omitempty"`
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
	Fingerprint      string       `json:"fingerprint,omitempty"`
	StatementCount   int32        `json:"statementCount"`
	Destructive      bool         `json:"destructive,omitempty"`
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

// ConditionView is one published condition, reduced to what a reader acts on.
type ConditionView struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Load reads one schema and says where it stands.
//
// Everything comes from the schema object itself. No plan object is read: the
// status records what a reader of this view acts on, and reading the plan as
// well would make this command fail on a plan that was collected between the
// two reads, about a schema whose status is perfectly readable.
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
		Namespace: schema.Namespace,
		Schema:    schema.Name,
		Phase:     string(schema.Status.Phase),
		Suspended: schema.Spec.Suspend,
	}
	if source := schema.Status.Source; source.ResolvedReference != "" || source.Digest != "" {
		view.Source = &SourceView{
			Reference: source.ResolvedReference,
			Digest:    source.Digest,
			Verified:  source.Verified,
		}
	}
	view.Observation = observationOf(schema.Status.Target)
	if plan := schema.Status.Plan; plan != nil {
		view.Plan = &PlanView{
			Name:           plan.Name,
			Fingerprint:    plan.Fingerprint,
			StatementCount: plan.StatementCount,
			Destructive:    plan.Destructive,
			CreatedAt:      plan.CreatedAt,
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
	for _, condition := range schema.Status.Conditions {
		view.Conditions = append(view.Conditions, ConditionView{
			Type: condition.Type, Status: string(condition.Status),
			Reason: condition.Reason, Message: condition.Message,
		})
	}
	return view, nil
}

// observationOf reduces the target status to what a reader acts on.
//
// A target that was never observed has no timestamp and no findings, and says
// nothing rather than reporting a converged database it never looked at.
func observationOf(target operatorv1alpha1.TargetStatus) *ObservationView {
	if target.LastObservedAt == nil && len(target.DriftFindings) == 0 {
		return nil
	}
	observation := &ObservationView{
		ObservedAt:      target.LastObservedAt,
		Drift:           target.DriftFindingCount > 0,
		HighestSeverity: target.HighestDriftSeverity,
		FindingCount:    target.DriftFindingCount,
		Truncated:       target.DriftFindingsTruncated,
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
