# A database that has run more than the artifact carries.
#
# Nothing is pending here, and that is the trap this reading exists to catch:
# pending is a statement about the artifact's own migrations, so a revision the
# artifact does not carry is in no state at all, and a proof that counted only
# pending work would read this as success.
#
# The two numbers that disagree are both asserted, because either one alone is
# satisfied by an ordinary settled database.
#
# Inputs: $digest, the artifact the tag now names; $databaseAt, the version the
# database reached; $artifactCovers, how many of its migrations the artifact
# accounts for.
.status as $status
| $status.artifact.digest == $digest
  and $status.history.currentVersion == $databaseAt
  and $status.history.appliedCount == $artifactCovers
  and $status.history.pendingCount == 0
  and ($status.history.dirty // false) == false
  and ($status.history.modifiedVersions // []) == []
  and ($status.plan // null) == null
  and ($status.activeOperation // null) == null
  and (any($status.conditions[]; .type == "Ready" and .status == "True") | not)
