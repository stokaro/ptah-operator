package e2e

import (
	"strings"
	"testing"
)

// staleApprovalWaitStderr is what kubectl ptah approve --wait wrote in the
// approval-artifact-change row on kind (Kubernetes 1.36.4): the approved plan went
// stale, the replacement was approved and ran, and the wait read again only after
// the replacement's run was read.
const staleApprovalWaitStderr = `Waiting up to 30m0s for ptahmigration/e2e-approval-artifact-postgresql in namespace ptah-test-a-1-36-4-local-54711140fe-51037-9784552362 to reach applied.
kubectl-ptah: ptahmigration/e2e-approval-artifact-postgresql in namespace ptah-test-a-1-36-4-local-54711140fe-51037-9784552362 will not reach applied: OtherPlanApplied.
  Phase:       InSync
  Why:         plan ptah-mplan-e8428b44ecab0fbae23f636b ran, not ptah-mplan-b3c000ca3d1f6c808abcd8c4: the run (Job ptah-m-apply-e2e-approval-artifact-postgresql-ed7b682e810ee53c) ended Applied
kubectl ptah migration e2e-approval-artifact-postgresql -n ptah-test-a-1-36-4-local-54711140fe-51037-9784552362 shows where it stands.
`

const (
	staleApprovedPlan = "ptah-mplan-b3c000ca3d1f6c808abcd8c4"
	staleRanPlan      = "ptah-mplan-e8428b44ecab0fbae23f636b"
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
			stderr:   reason("plan "+staleRanPlan+" ran", "plan ptah-mplan-0c3a5be1d2f4e6a7b8c9d0e1 ran"),
			approved: staleApprovedPlan, ran: staleRanPlan,
		},
		{
			name: "a plan whose name the run's begins with", code: approveWaitWillNotReach,
			stderr: staleApprovalWaitStderr, approved: staleApprovedPlan, ran: "ptah-mplan-e8428b44ecab",
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
