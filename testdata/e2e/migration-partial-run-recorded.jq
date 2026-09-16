# What a run that committed half of itself left in the record.
#
# The outcome, and that the run claimed no version as applied. Both are facts
# about a run that is over, so they hold whatever the resource is doing when the
# status is read.
#
# The phase and the active operation are deliberately absent, though the reading
# they came from had both. A resource that has stopped goes on resolving and
# reading at its interval, so `Blocked` and `activeOperation == null` are true
# only between cycles; asserting them turns a proof about a run into a proof
# about when the poll landed. What replaces them is the hold that follows, which
# watches for a second run rather than for a moment of quiet.
.status as $status
| $status.lastRun.outcome == "Partial"
  and ($status.lastRun.appliedVersions // []) == []
