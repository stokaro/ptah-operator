.metadata.generation as $generation |
($generation | type) == "number" and
$generation > 0 and
.status.observedGeneration == $generation and
.status.phase == "AwaitingApproval" and
.status.activeOperation == null and
.status.nextReconciliationTime as $deadline |
(try ($deadline | fromdateiso8601) catch null) as $deadlineEpoch |
($deadline | type) == "string" and
($deadlineEpoch | type) == "number" and
$deadlineEpoch > now and
(.status.plan | type) == "object" and
(.status.plan.name | type) == "string" and
(.status.plan.name | length) > 0 and
(.status.plan.uid | type) == "string" and
(.status.plan.uid | length) > 0 and
(.status.plan.fingerprint | type) == "string" and
(.status.plan.fingerprint | test("^sha256:[0-9a-f]{64}$")) and
(.status.plan.contentDigest | type) == "string" and
(.status.plan.contentDigest | test("^sha256:[0-9a-f]{64}$")) and
.status.plan.approval == null and
(.status.conditions | any(
  .type == "ApprovalRequired" and
  .status == "True" and
  .observedGeneration == $generation and
  .reason == "Waiting"
)) and
(.status.conditions | any(
  .type == "PlanReady" and
  .status == "True" and
  .reason == "Published" and
  .observedGeneration == $generation
))
