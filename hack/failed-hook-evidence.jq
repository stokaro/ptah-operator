def hook_phase:
  .last_run.phase // "";

def hook_weight:
  if .weight == null then 0 else (.weight | tonumber) end;

(.hooks // []) as $hooks |
($hooks | map(select(hook_phase == "Failed"))) as $failed |
($hooks | map(select(
  .name == $expected_identity_name and
  .kind == "Job" and
  hook_weight == $expected_identity_weight and
  ((.events // []) | index("pre-upgrade") != null)))) as $identity |
(.version == $expected_revision) and
(.info.status == "failed") and
($identity | length == 1) and
($identity[0] |
  hook_phase == "Succeeded" and
  ((.last_run.started_at // "") | length > 0) and
  ((.last_run.completed_at // "") | length > 0)) and
($failed | length == 1) and
($failed[0] |
  .name == $expected_name and
  .kind == "Job" and
  hook_weight == $expected_weight and
  ((.events // []) | index("pre-upgrade") != null) and
  ((.last_run.started_at // "") | length > 0) and
  ((.last_run.completed_at // "") | length > 0)) and
($hooks | all(.[];
  if
    (((.events // []) | index("pre-upgrade")) != null) and
    (hook_weight > $expected_weight)
  then
    hook_phase == ""
  else
    true
  end))
