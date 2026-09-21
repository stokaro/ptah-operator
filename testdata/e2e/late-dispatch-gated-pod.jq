# The Apply Pod the scheduling gate is holding off every node.
#
# The claim is that the gate, and nothing else, is what has kept this run from
# starting -- so an empty Pod list does not satisfy it. A Job whose Pod has not
# been created yet is indistinguishable from one whose Pod cannot be placed,
# and suspending on that reading would carry the claim past its deadline by
# suspension alone: the row would pass with the node selector no longer reaching
# the Pod at all.
#
# Pending is not enough either. A Pod stays Pending while it is bound to a node
# and pulling its image, and the kubelet starts the runner from there. What says
# the gate held is that no Pod has been given a node.
# And the selector has to be on the Pod. A Pod carrying no selector at all is
# also briefly Pending and unbound, in the moment between its creation and the
# scheduler placing it, so without this clause the reading accepts the one state
# it exists to rule out: a builder that stopped propagating the selector, whose
# Pod would have run the moment the scheduler looked at it.
(.items | length) > 0
and all(.items[];
  .status.phase == "Pending"
  and ((.spec.nodeName // "") | length) == 0
  and ((.spec.nodeSelector // {})[$gate] == "open"))
