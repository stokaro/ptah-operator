if ($nodes | length) != 1 then false
else
  [$cluster + "-control-plane", $cluster + "-control-plane2", $cluster + "-control-plane3"] as $control_plane_names |
  [$nodes[0].items[]
    | select(.metadata.labels["node-role.kubernetes.io/control-plane"] != null)
    | select(.metadata.name as $name | any($control_plane_names[]; . == $name))
  ] as $control_plane_nodes |
  [$control_plane_nodes[] |
    [(.status.addresses // [])[] | select(.type == "InternalIP") | .address] as $internal_ips |
    select(($internal_ips | length) == 1) |
    $internal_ips[0]
  ] as $control_plane_addresses |
  [.items[] | select(.metadata.labels["kubernetes.io/service-name"] == "kubernetes")] as $slices |
  [$slices[].endpoints[]] as $endpoints |
  [$endpoints[].addresses[]] as $addresses |
  ($control_plane_nodes | length) == 3 and
  ([$control_plane_nodes[].metadata.name] | sort) == ($control_plane_names | sort) and
  ($control_plane_addresses | length) == 3 and
  ($control_plane_addresses | unique | length) == 3 and
  all($control_plane_addresses[]; test("^[0-9]+(\\.[0-9]+){3}$")) and
  ($slices | length) > 0 and
  all($slices[];
    .addressType == "IPv4" and
    (.ports | length) == 1 and
    .ports[0].name == "https" and
    (.ports[0].protocol == null or .ports[0].protocol == "TCP") and
    .ports[0].port == 6443
  ) and
  ($endpoints | length) == 3 and
  all($endpoints[];
    .conditions.ready != false and
    .conditions.serving != false and
    .conditions.terminating != true and
    (.addresses | length) == 1
  ) and
  ($addresses | length) == 3 and
  ($addresses | unique | length) == 3 and
  all($addresses[]; test("^[0-9]+(\\.[0-9]+){3}$")) and
  ($addresses | sort) == ($control_plane_addresses | sort)
end
