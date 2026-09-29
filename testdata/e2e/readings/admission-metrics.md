# API server admission metrics readings

Captured on September 29, 2026, from an isolated kind v0.33.0 cluster on
`remote-dev-container`. The cluster ran Kubernetes v1.35.8 from
`kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0`.
Prometheus used
`prom/prometheus:v3.15.0@sha256:efd719c99d83b060d9daefdcf00360461adf279f45ef5391f8d111892118753e`.

The probe used the repository's kubelet user-namespace setting and 1.35 API
server feature gate. It generated its monitoring configuration and grants with
`alControlPlaneTargets`, `alPrometheusConfig`, `alAPIMetricsRBAC`, and
`alDiscoveryRBAC`. Prometheus authenticated with its projected ServiceAccount
token and verified the API server with the projected CA and
`kubernetes.default.svc` server name. It scraped the actual control-plane
endpoint directly.

A validating webhook named `vapproval.operator.ptah.run` selected only a probe
ConfigMap's dry-run updates. Its unavailable Service produced three real API
server `calling_webhook_error` refusals. The resulting 60-second counter increase
was queried with `alAdmissionRejections`.

- `kubernetes-api-scrape-nodes.json` retains the nodes' names, labels, and
  addresses from the API server's NodeList.
- `kubernetes-api-scrape-endpoints.json` retains EndpointSlice identity, labels,
  address type, ports, endpoints, and readiness from the Kubernetes Service.
- `prometheus-api-server-targets.json` retains the target fields the readiness
  predicate reads from Prometheus's `/api/v1/targets` response.
- `prometheus-admission-rejections.json` is the complete instant query response.

The same predicates the live scenario uses accepted these readings. Unit tests
also require a synthetic three-control-plane inventory and reject missing,
duplicate, unrelated, unready, and failed scrape targets. The probe cluster
and its temporary credentials were removed.

This probe establishes the metrics connection and counter shape. Certificate
expiry, alert delivery, recovery, and the full three-control-plane inventory
remain claims of the acceptance scenario and its Kubernetes matrix.
