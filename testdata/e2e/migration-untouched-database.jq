# A resource that has acted on nothing.
#
# The claim is negative, so every clause is an absence: no plan was published,
# no run was recorded, the resource never reached a phase that invites work,
# and nothing counts as applied. An absence is cheap to assert and easy to
# assert wrongly, which is why the phases are named rather than inferred.
.status as $status
| ($status.plan // null) == null
  and ($status.lastRun // null) == null
  and $status.phase != "AwaitingApproval"
  and $status.phase != "InSync"
  and (($status.history.appliedCount // 0) == 0)
