# What a run stopped at its execution deadline left in the record.
#
# The run the claim dispatched, identified by its Job's UID; the outcome the
# report gave; and the versions it applied, every version below the one it
# stopped in. Those three are what the runner's report carries and a run with no
# report cannot: without the report the outcome is Unknown and nothing is named
# applied, which is what every stopped run recorded before the runner passed
# SIGTERM on and read what Ptah wrote.
#
# Failed rather than Partial, because the migration that was stopped is a single
# sleep and committed nothing; the row reads the revision table to show the
# database says the same. The phase is deliberately absent: the reading
# that follows the run moves the resource on, and this is a proof about the run.
.status.lastRun as $run
| $run != null
  and $run.jobUID == $job
  and $run.outcome == "Failed"
  and ($run.appliedVersions // []) == [range(1; $stoppedAt)]
