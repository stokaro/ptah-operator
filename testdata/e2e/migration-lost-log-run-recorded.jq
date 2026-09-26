# What a run whose log was lost left in the record, when the termination
# summary stood in for it.
#
# The run the claim dispatched, identified by its Job's UID; the outcome the
# summary carried; and the message the controller writes only when it settles a
# run from a summary, naming the frame the summary was bound to and the range of
# versions it reported. A run settled from its frame says neither, and a run
# settled from nothing is Unknown, so a record that passes here came from the
# termination message and nowhere else.
#
# $stoppedAt is the last version of the sequence, all of which the run applied,
# and $digest the frame digest read out of the Pod's own termination message.
.status.lastRun as $run
| $run != null
  and $run.jobUID == $job
  and $run.outcome == "Applied"
  and ($run.message | contains("termination message, bound to frame " + $digest + ","))
  and ($run.message | contains(
    "\($stoppedAt) migrations recorded applied, from version 1 to version \($stoppedAt)"))
