# Prometheus scrape readings

The three `prometheus-*.json` readings came from
`prom/prometheus:v3.15.0@sha256:efd719c99d83b060d9daefdcf00360461adf279f45ef5391f8d111892118753e`
on September 29, 2026. The process used `--web.enable-lifecycle` and
`--no-config.auto-reload` in an isolated Docker network namespace.

Two HTTP fixtures served `ptah_operator_unresolved_view_synced` at `/metrics`:
the leader at `127.0.0.1:8081` returned 1, and the follower at `127.0.0.1:8082`
returned 0. Both initially scraped successfully at a five-second interval.
A relabel rule selected `__meta_kubernetes_pod_name=leader` and replaced
`__metrics_path__` with `/e2e-missing-metrics`. After `POST /-/reload`, the
leader returned HTTP 404 and the follower still scraped successfully.

`prometheus-one-target-lost.json` is `/api/v1/targets` during that fault.
`prometheus-scrape-fault-config.json` is `/api/v1/status/config` after loading
the rule. `prometheus-scrape-restored-config.json` is the same endpoint after
removing the rule and reloading. The readings retain Prometheus's normalized
configuration and actual target response fields.

These fixtures validate API parsing and fault selection. Cluster discovery,
manager continuity, alert delivery and bounded recovery are exercised by
`TestAlerting/lost-scrape-target` in the Kubernetes matrix.
