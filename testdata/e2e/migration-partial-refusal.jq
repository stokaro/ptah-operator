# The refusal a stopped migration holds, as distinct from the phase it passes
# through while holding it.
#
# A resource that has stopped still resolves, verifies and reads its history at
# its interval, so it is legitimately out of Blocked for part of every cycle.
# What may never lapse is the refusal itself: the condition, the absence of a
# plan, and the absence of a Ready that would invite work.
#
# This is a file rather than an inline filter because a self-test reads the same
# bytes the proof does. A second copy would pass while the proof drifted.
.status as $status
| (any($status.conditions[]; .type == "Blocked" and .status == "True"))
  and ($status.plan // null) == null
  and (any($status.conditions[]; .type == "Ready" and .status == "True") | not)
