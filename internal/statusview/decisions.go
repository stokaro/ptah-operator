package statusview

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// MaxDecisions bounds how many approvals or acknowledgments one view prints.
// A resource collects one for every plan somebody approved, and a terminal
// is for the ones that explain where it stands now: the newest.
const MaxDecisions = 5

// Decisions is the newest decisions that name one resource, newest first.
type Decisions[T any] struct {
	// Total is how many name the resource. Items holds at most MaxDecisions
	// of them, so a Total above len(Items) is a list that was cut.
	Total int `json:"total"`
	Items []T `json:"items"`
}

// Newest orders decisions newest first, by the creation time the API server
// stamped and then by name, and keeps the first MaxDecisions.
func Newest[T any](items []T, created func(T) metav1.Time, name func(T) string) *Decisions[T] {
	sorted := slices.Clone(items)
	slices.SortStableFunc(sorted, func(left, right T) int {
		if order := created(right).Compare(created(left).Time); order != 0 {
			return order
		}
		return cmp.Compare(name(left), name(right))
	})
	kept := sorted[:min(len(sorted), MaxDecisions)]
	if kept == nil {
		kept = []T{}
	}
	return &Decisions[T]{Total: len(items), Items: kept}
}

// Unreadable is why a list the view asked for could not be read. The view
// is still printed: what the resource itself says does not depend on it.
type Unreadable struct {
	// Forbidden says the API server refused the list for want of permission.
	// The plugin needs only get on the resource it reads, so a reader without
	// list on the decisions is an expected reader, not a failure.
	Forbidden bool `json:"forbidden"`
	// Message is the error the list returned: the API server's answer, or the
	// client's when no answer came.
	Message string `json:"message"`
}

// UnreadableFrom records why a list failed.
func UnreadableFrom(err error) *Unreadable {
	return &Unreadable{Forbidden: apierrors.IsForbidden(err), Message: err.Error()}
}

// Approval is one approval of either family: the plan it names, the identity
// admission stamped on it, and what the controller answered.
type Approval struct {
	Name      string      `json:"name"`
	CreatedAt metav1.Time `json:"createdAt"`
	// PlanRef and PlanFingerprint are the exact plan the approval names.
	PlanRef         operatorv1alpha1.ImmutableObjectReference `json:"planRef"`
	PlanFingerprint string                                    `json:"planFingerprint"`
	// CurrentPlan says the plan it names is the resource's current plan, by
	// name, UID and fingerprint. An approval of any other plan cannot
	// authorize anything, whatever its own conditions say.
	CurrentPlan bool `json:"currentPlan"`
	// AppliedPlan says the plan it names is the one the resource's last
	// confirmed apply ran, by name, UID and fingerprint: the approval that
	// apply consumed, once the plan is no longer current. A migration records
	// no applied plan, so it is false for a migration approval.
	AppliedPlan bool `json:"appliedPlan"`
	// Approver and ApprovedAt are what admission stamped from the
	// authenticated request.
	Approver   operatorv1alpha1.ApprovalIdentity `json:"approver"`
	ApprovedAt metav1.Time                       `json:"approvedAt"`
	Conditions []Condition                       `json:"conditions,omitempty"`
}

// WriteListHeading writes the line that opens a list of decisions: how many
// follow, that there are none, or why they could not be read. It reports
// whether entries follow.
//
// title names the list, resource is the plural a reader passes to `kubectl
// get` to see them all, and namespace is where they live. That command lists
// every one in the namespace, whichever resource it names, and the line says
// so.
func WriteListHeading[T any](
	text *strings.Builder,
	title, resource, namespace string,
	decisions *Decisions[T],
	unreadable *Unreadable,
) bool {
	switch {
	case unreadable != nil && unreadable.Forbidden:
		fmt.Fprintf(text, "\n%s: not shown; listing %s is forbidden to you (RBAC): %s\n",
			title, resource, Clean(unreadable.Message))
		return false
	case unreadable != nil:
		fmt.Fprintf(text, "\n%s: not shown; listing %s failed: %s\n", title, resource, Clean(unreadable.Message))
		return false
	case decisions == nil || decisions.Total == 0:
		fmt.Fprintf(text, "\n%s: none\n", title)
		return false
	case len(decisions.Items) < decisions.Total:
		fmt.Fprintf(text, "\n%s, the newest %d of %d; kubectl get %s -n %s lists all %s in the namespace:\n",
			title, len(decisions.Items), decisions.Total, resource, namespace, strings.ToLower(title))
	default:
		fmt.Fprintf(text, "\n%s, newest first:\n", title)
	}
	return true
}

// WriteApproval writes one approval: who approved which plan when, which of
// the resource's plans that is, and every condition the controller wrote on
// it. none is what to say when there is no condition, which differs by
// family: the controller answers a schema approval as soon as it reads it, and
// writes nothing on a migration approval until its plan is dispatched or a read
// withdraws it.
func WriteApproval(text *strings.Builder, approval Approval, none string) {
	fmt.Fprintf(text, "  %s\n", Clean(approval.Name))
	fmt.Fprintf(text, "    approved by %s at %s\n", identity(approval.Approver.Username), Time(approval.ApprovedAt))
	var relations []string
	if approval.CurrentPlan {
		relations = append(relations, "the current plan")
	}
	if approval.AppliedPlan {
		relations = append(relations, "the applied plan")
	}
	relation := "not the current plan"
	if len(relations) > 0 {
		relation = strings.Join(relations, " and ")
	}
	fmt.Fprintf(text, "    plan %s, %s\n", Clean(approval.PlanRef.Name), relation)
	WriteDecisionConditions(text, approval.Conditions, none)
}

// WriteDecisionConditions writes the conditions of one decision, or none when
// it has no condition.
func WriteDecisionConditions(text *strings.Builder, conditions []Condition, none string) {
	if len(conditions) == 0 {
		fmt.Fprintf(text, "    %s\n", none)
		return
	}
	WriteTable(text, "    ", conditions, 0)
}

func identity(username string) string {
	if username == "" {
		return "(no identity stamped)"
	}
	return Clean(username)
}
