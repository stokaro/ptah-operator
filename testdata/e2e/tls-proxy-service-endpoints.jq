(if ($podIP | contains(":")) then "IPv6" else "IPv4" end) as $addressType |
[.items[]? | select((.endpoints // []) | length > 0)] as $slices |
($slices | length) == 1 and
$slices[0].addressType == $addressType and
($slices[0].ports | length) == 1 and
$slices[0].ports[0].name == "tls" and
$slices[0].ports[0].protocol == "TCP" and
$slices[0].ports[0].port == 5443 and
($slices[0].endpoints | length) == 1 and
($slices[0].endpoints[0] as $endpoint |
  $endpoint.conditions.ready != false and
  $endpoint.conditions.serving != false and
  $endpoint.conditions.terminating != true and
  ($endpoint.targetRef.apiVersion == null or
    $endpoint.targetRef.apiVersion == "v1") and
  $endpoint.targetRef.kind == "Pod" and
  $endpoint.targetRef.namespace == $namespace and
  $endpoint.targetRef.name == $name and
  $endpoint.targetRef.uid == $uid and
  $endpoint.addresses == [$podIP])
