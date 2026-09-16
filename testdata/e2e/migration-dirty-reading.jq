# The reading a partially applied migration leaves behind.
#
# Three things, and no fourth. The history says a revision is dirty, it says
# which version the run stopped at, and the refusal names that reading.
#
# The pending count is deliberately absent. Ptah counts a migration whose
# revision is not recorded applied as pending, and a dirty revision is exactly
# that, so the number answers its bookkeeping rather than anything this proof
# claims -- and pinning it made the proof fail on a change that never touched
# what it measures.
#
# Inputs: $stoppedAt, the version the interrupted run was applying.
.status as $status
| $status.phase == "Blocked"
  and ($status.history.dirty // false) == true
  and $status.history.currentVersion == $stoppedAt
  and (any($status.conditions[];
        .type == "Blocked" and .status == "True" and .reason == "HistoryDirty"))
