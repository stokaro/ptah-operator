# The first reading after an isolated Apply's claim is retired: the run
# recorded against its own Job as one nobody accounted for.
#
# Unknown is what the code produces here, and Applied is refused on purpose.
# The Job turns Failed only once its Pod stops terminating, and the kubelet
# deletes a Pod the Job controller has let go of in the same sync that reports
# it finished, so by the time the result is read there is no Pod and no log to
# read it from. A run read as Applied would mean the controller read a log this
# row expects to be gone, and the row would no longer be measuring the case it
# was built for.
#
# The record is asserted with the outcome because the record is what refuses a
# replay: an outcome without it would leave the resource free to plan again.
.status as $status
| ($status.activeOperation // null) == null
and $status.lastRun.jobUID == $job
and $status.lastRun.outcome == "Unknown"
and $status.unresolvedRun.jobUID == $job
and $status.unresolvedRun.outcome == "Unknown"
and any($status.conditions[]?;
  .type == "Blocked" and .status == "True" and .reason == "ApplyOutcomeUnknown")
