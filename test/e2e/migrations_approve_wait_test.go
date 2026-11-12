package e2e

import (
	"strings"
	"testing"
)

// staleApprovalWaitStderr is what kubectl ptah approve --wait wrote in the
// approval-artifact-change row on kind: the approved plan went stale, the
// replacement ran, and the wait read again only after it was read.
const staleApprovalWaitStderr = `Waiting up to 30m0s for ptahmigration/e2e-approval-artifact-postgresql in namespace ptah-e2e to reach applied.
kubectl-ptah: ptahmigration/e2e-approval-artifact-postgresql in namespace ptah-e2e will not reach applied: OtherPlanApplied.
  Phase:       InSync
  Why:         plan e2e-approval-artifact-postgresql-mplan-b ran, not e2e-approval-artifact-postgresql-mplan-a: the run (Job e2e-approval-artifact-postgresql-apply-1) ended Applied
kubectl ptah migration e2e-approval-artifact-postgresql -n ptah-e2e shows where it stands.
`

const (
	staleApprovedPlan = "e2e-approval-artifact-postgresql-mplan-a"
	staleRanPlan      = "e2e-approval-artifact-postgresql-mplan-b"
)

func TestApproveWaitStoppedOnAnotherRun(t *testing.T) {
	t.Parallel()
	if err := approveWaitStoppedOnAnotherRun(approveWaitWillNotReach, staleApprovalWaitStderr, staleApprovedPlan, staleRanPlan); err != nil {
		t.Fatalf("refused the stop the stale approval's wait printed: %v", err)
	}
	reason := func(from, to string) string { return strings.Replace(staleApprovalWaitStderr, from, to, 1) }
	for _, test := range []struct {
		name          string
		code          int
		stderr        string
		approved, ran string
	}{
		{
			// The defect: the replacement's run read as this approval's.
			name: "the replacement's run taken as the answer", code: 0,
			stderr:   "Waiting up to 30m0s for ptahmigration/e2e-approval-artifact-postgresql in namespace ptah-e2e to reach applied.\n",
			approved: staleApprovedPlan, ran: staleRanPlan,
		},
		{name: "a timeout", code: 7, stderr: staleApprovalWaitStderr, approved: staleApprovedPlan, ran: staleRanPlan},
		{name: "a failed command", code: 1, stderr: staleApprovalWaitStderr, approved: staleApprovedPlan, ran: staleRanPlan},
		{
			// A wait that read the replacement before it ran. True, and not the order
			// the row holds.
			name: "the replacement published", code: approveWaitWillNotReach,
			stderr:   reason("applied: OtherPlanApplied.", "applied: PlanSuperseded."),
			approved: staleApprovedPlan, ran: staleRanPlan,
		},
		{
			name: "the plan spent under another approval", code: approveWaitWillNotReach,
			stderr:   reason("applied: OtherPlanApplied.", "applied: OtherApprovalConsumed."),
			approved: staleApprovedPlan, ran: staleRanPlan,
		},
		{
			name: "the plans the other way round", code: approveWaitWillNotReach,
			stderr: staleApprovalWaitStderr, approved: staleRanPlan, ran: staleApprovedPlan,
		},
		{
			name: "a third plan ran", code: approveWaitWillNotReach,
			stderr:   reason("plan "+staleRanPlan+" ran", "plan e2e-approval-artifact-postgresql-mplan-c ran"),
			approved: staleApprovedPlan, ran: staleRanPlan,
		},
		{
			name: "a plan whose name the run's begins with", code: approveWaitWillNotReach,
			stderr: staleApprovalWaitStderr, approved: staleApprovedPlan, ran: "e2e-approval-artifact-postgresql-mplan",
		},
		{
			name: "one plan named twice", code: approveWaitWillNotReach,
			stderr: staleApprovalWaitStderr, approved: staleApprovedPlan, ran: staleApprovedPlan,
		},
		{name: "no plan named", code: approveWaitWillNotReach, stderr: staleApprovalWaitStderr},
		{name: "nothing written", code: approveWaitWillNotReach, approved: staleApprovedPlan, ran: staleRanPlan},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := approveWaitStoppedOnAnotherRun(test.code, test.stderr, test.approved, test.ran); err == nil {
				t.Fatalf("accepted exit %d with stderr %q for plan %q replaced by %q", test.code, test.stderr, test.approved, test.ran)
			}
		})
	}
}
