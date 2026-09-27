def hook_phase:
  .last_run.phase // "";

def hook_weight:
  if .weight == null then 0 else (.weight | tonumber) end;

(.hooks // []) as $hooks |
($hooks | map(select(hook_phase == "Failed"))) as $failed |
(.version == $expected_revision) and
(.info.status == "failed") and
($failed | length == 1) and
($failed[0] |
  .name == $expected_name and
  .kind == "Job" and
  hook_weight == 0 and
  ((.events // []) | index("pre-upgrade") != null) and
  ((.last_run.started_at // "") | length > 0) and
  ((.last_run.completed_at // "") | length > 0)) and
($hooks | all(.[];
  if
    (((.events // []) | index("pre-upgrade")) != null) and
    (hook_weight > 0)
  then
    hook_phase == ""
  else
    true
  end))
