# The step that ended a run before the runner could speak.
#
# Without it every such failure reads as a missing result frame, which is also
# what a crashed runner, an evicted node and a truncated log produce. The name
# of the init step is the one distinction a reader can act on: the runner was
# never installed, the source authority was refused, or the artifact never
# arrived are three different answers, and none of them is a retry.
#
# Matching the step rather than the phrasing is deliberate. The sentence around
# it may be reworded; the boundary it names is the claim.
#
# Inputs: $step, the init container whose failure is being proved.
[.status.conditions[]? | select(.type == "Progressing") | .message]
| any(test($step))
