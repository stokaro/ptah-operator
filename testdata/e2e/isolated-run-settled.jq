# An isolated Apply's unresolved record, settled by a reading of the database.
#
# The run finished all three migrations while its node was cut off, so a
# history read of that database after the run finds nothing pending, and that
# reading is the one thing that removes the record. What is asserted is the
# settlement and its evidence: no record, the run still named as the one that
# was never accounted for, and a reading taken after it with nothing pending.
# A record dropped without such a reading, or by one taken before the run
# ended, is what a replay would need, and both are refused.
def instant: sub("\\.[0-9]+"; "") | fromdateiso8601;
.status as $status
| ($status.activeOperation // null) == null
and ($status.unresolvedRun // null) == null
and $status.lastRun.jobUID == $job
and $status.lastRun.outcome == "Unknown"
and $status.history.pendingCount == 0
and ($status.history.observedAt | instant) > ($status.lastRun.finishedAt | instant)
