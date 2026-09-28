# A run nobody accounted for, settled in the name of the person whose
# acknowledgment named it.
#
# Every part is the claim. The record is gone from status and from the copy on
# the resource's metadata a restore would keep; the resolution names the run the
# record named, says a person settled it, and names the acknowledgment and the
# identity admission stamped on it. A reading of the database that settled the
# run names no person, and is not this.
#
# $operation is the record's operationID, $acknowledgment the acknowledgment's
# name, and $person the user the harness acted as when it created it.
(.status.resolvedRun // {}) as $resolved
| (.status.unresolvedRun // null) == null
  and (((.metadata.annotations // {})["operator.ptah.run/unresolved-run"]) // null) == null
  and $resolved.operationID == $operation
  and $resolved.resolution == "Acknowledged"
  and (($resolved.acknowledgmentRef // {}).name) == $acknowledgment
  and (($resolved.acknowledgedBy // {}).username) == $person
