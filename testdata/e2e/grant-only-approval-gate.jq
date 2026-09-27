# A change that touches only a grant, observed, planned, and held for a person
# under apply: Always.
#
# The fixture's artifact adds one GRANT to the converged external schema. Ptah's
# drift report has no category for a grant, so the observation says drift with
# no finding, and Ptah rates the GRANT statement safe. The filter matches the
# settled reading, in which the controller has published:
#
# - the observed generation, for the digest under test, with no operation
#   claimed;
# - the raw observation as drift in no category: a safe highest severity, no
#   finding and no count. An empty severity would be an observation that found
#   no drift, and a grant it did not see;
# - a plan the scoped Plan found changes in, that names Grant and nothing else,
#   is not destructive, and carries no recorded approval;
# - ApprovalRequired True for reason PrivilegeChanges, naming the kind.
#
# And neither the observation nor any condition message carries what the
# statement says: not the table, not the privilege, not the grantee. The check
# reads status.target and the messages and nothing else in status, which
# carries the test namespace in image references.
.status as $status
| ($status.plan // {}) as $plan
| ($status.target // {}) as $target
| .metadata.generation == $status.observedGeneration
and $status.source.digest == $digest
and ($status.activeOperation // null) == null
and $target.highestDriftSeverity == "safe"
and ($target.driftFindingCount // 0) == 0
and ($target.driftFindings // []) == []
and ($target.driftFindingsTruncated // false) == false
and $plan.privilegeChanges == ["Grant"]
and $plan.destructive == false
and ($plan.approval // null) == null
and any($status.conditions[]?;
  .type == "DriftDetected" and .status == "True" and .reason == "ScopedChanges")
and any($status.conditions[]?;
  .type == "ApprovalRequired" and .status == "True" and .reason == "PrivilegeChanges"
  and (.message | contains("(Grant)")))
and ([$target, [$status.conditions[]? | .message // ""]] | tostring
  | test("e2e_widgets|SELECT|PUBLIC") | not)
