# A plan that changes privileges, held for a person under apply: Always.
#
# The fixture's artifact adds one SECURITY DEFINER function, which Ptah rates
# safe and writes as CREATE OR REPLACE FUNCTION. The gate is the condition and
# the plan it is about, not the phase: a resource waiting for an approval still
# resolves, verifies, observes and plans at its interval, and is out of
# AwaitingApproval for part of every cycle while the requirement stands. So the
# filter matches the settled reading, in which the controller has published:
#
# - the observed generation, for the digest under test, with no operation
#   claimed and a refresh deadline persisted;
# - a current plan that names the two kinds the statement changes, is not
#   destructive, and carries no recorded approval;
# - ApprovalRequired True for reason PrivilegeChanges, naming the kinds.
#
# And every condition message names kinds and nothing the statement says: not
# the function, not its clauses, not its body. The check reads the messages and
# nothing else in status, which carries the test namespace in image references.
# It cannot pass on an empty list, because the requirement above is one of the
# messages it reads.
.status as $status
| ($status.plan // {}) as $plan
| .metadata.generation == $status.observedGeneration
and $status.source.digest == $digest
and ($status.activeOperation // null) == null
and ($status.nextReconciliationTime // null) != null
and $plan.privilegeChanges == ["SecurityDefiner", "FunctionReplacement"]
and $plan.destructive == false
and ($plan.approval // null) == null
and any($status.conditions[]?;
  .type == "ApprovalRequired" and .status == "True" and .reason == "PrivilegeChanges"
  and (.message | contains("(SecurityDefiner, FunctionReplacement)")))
and all($status.conditions[]?;
  (.message // "") | test("e2e_widget_count|SECURITY DEFINER|search_path|count\\(|e2e_widgets"; "i") | not)
