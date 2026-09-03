def hook_phase:
  .last_run.phase // "";

(.hooks // []) as $hooks |
($hooks | map(select(hook_phase == "Failed"))) as $failed |
(.version == $expected_revision) and
(.info.status == "failed") and
($failed | length == 1) and
($failed[0] |
  .name == $expected_name and
  .kind == "Job" and
  (.weight | tonumber) == $expected_weight and
  ((.events // []) | index("pre-upgrade") != null) and
  ((.last_run.started_at // "") | length > 0) and
  ((.last_run.completed_at // "") | length > 0)) and
($hooks | all(.[];
  if
    (((.events // []) | index("pre-upgrade")) != null) and
    ((.weight | tonumber) > $expected_weight)
  then
    hook_phase == ""
  else
    true
  end))
