def selected_runtime_pod:
  .metadata.labels["app.kubernetes.io/instance"] == $release and
  if $scope == "controller" then
    .metadata.labels["app.kubernetes.io/component"] == "controller"
  else
    (.metadata.labels["app.kubernetes.io/component"] == "controller" or
     .metadata.labels["app.kubernetes.io/component"] == "certificate-rotation")
  end;

def main_containers_never_started:
  all((.status.containerStatuses // [])[];
    (.restartCount // 0) == 0 and
    (.started // false) == false and
    .state.running == null and
    .state.terminated == null and
    .lastState.running == null and
    .lastState.terminated == null and
    (.state.waiting.reason // "") == "PodInitializing"
  );

def explicit_verifier_failure:
  any((.status.initContainerStatuses // [])[];
    .name == "verify-candidate-runtime" and
    (((.state.terminated.exitCode // 0) != 0) or
     ((.lastState.terminated.exitCode // 0) != 0))
  );

[.items[] | select(selected_runtime_pod)] as $pods |
{
  podCount: ($pods | length),
  podUIDs: ([$pods[].metadata.uid] | sort | join(",")),
  mainContainersNeverStarted: all($pods[]; main_containers_never_started),
  explicitVerifierFailures:
    (($pods | length) == $expected and all($pods[]; explicit_verifier_failure))
}
