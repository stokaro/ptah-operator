# External upgrade observation

This operations tool watches one declared Helm upgrade from outside the release
being upgraded. It retains a failed hook after Helm deletes the Job, preserves
the original fifteen-minute deadline across retries and observer restarts, and
exports metrics for `rules.yaml`. It is not installed by the operator chart.

The code and rule tests do not complete #581. Live failed-hook and interrupted
upgrade proofs, receiver timing, operational ownership, and final-build evidence
remain required. Do not treat this tool's existence as production acceptance.

## Prepare the transaction

Run the observer on an independent Linux or macOS host. Keep its state directory
on durable storage outside the upgraded cluster and release. Use a private
state directory and retain the exact candidate chart, values, intent, state and
observer binary identity with the upgrade evidence. An existing state file is
never replaced by `prepare`; retries use that same file.

Build from the source being qualified:

```sh
go build -o /tmp/ptah-upgrade-observer ./hack/upgradealert
```

Create `intent.json` with these fields:

| Field | Source |
| --- | --- |
| `namespace`, `release` | The installation being upgraded |
| `hookJob` | The rendered CRD reconcile hook Job name |
| `manager`, `rotator` | The rendered manager and certificate-rotator Deployment names |
| `image` | The candidate operator image pinned by digest |
| `hookArgs` | The rendered hook container's complete argument array |
| `chartDigest`, `valuesDigest` | `sha256:` plus the digest of the exact packaged chart and values file |
| `crdDigests` | All nine CRD names mapped to the candidate's normalized schema digests |
| `probes` | Existing `PtahSchema` or `PtahMigration` resources, each with `kind`, `namespace`, `name`, immutable `uid` and current `generation` |

Include both resource families when both are in service. Choose controlled probes
that will execute a fresh read after the hook, reach a healthy policy boundary,
and remain unchanged throughout the upgrade. Suspended resources, unresolved
work, stale observations and changed generations cannot prove recovery. Ordinary
approval waits and `Never` policies are allowed once current read-only progress
is established. The tool never approves an Apply or edits a stored workload.

```sh
/tmp/ptah-upgrade-observer prepare \
  --state /srv/ptah-upgrade/state.json \
  --kubeconfig /srv/ptah-upgrade/kubeconfig \
  --context qualification \
  --intent /srv/ptah-upgrade/intent.json \
  --chart /srv/ptah-upgrade/candidate.tgz \
  --values /srv/ptah-upgrade/values.yaml

/tmp/ptah-upgrade-observer serve \
  --state /srv/ptah-upgrade/state.json \
  --kubeconfig /srv/ptah-upgrade/kubeconfig \
  --context qualification \
  --listen 127.0.0.1:9812
```

Read a consistent state snapshot while the observer is running:

```sh
/tmp/ptah-upgrade-observer inspect --state /srv/ptah-upgrade/state.json
```

Inspection takes no writer lock and does not alter the retained transaction.

The deadline starts at `prepare`. Start the observer, require HTTP 200 from
`/readyz`, and confirm a successful Prometheus scrape before running Helm.
Keep the observer alive through recovery. Serialize upgrade attempts and retry
the identical packaged chart, image and values using the
[upgrade recovery runbook](https://operator.ptah.run/use/operations/#retry-upgrade).
Do not start an unrelated upgrade with a completed transaction's state.

The observer checks the candidate hook image and arguments and hashes the input
files at preparation. It does not launch Helm or enforce which files an external
Helm process uses. The upgrade procedure must retain the actual Helm command and
identical input files; this observer alone cannot prove that part of the retry.

## Permissions and monitoring

The observer needs the following API access, limited to the installation and
explicit probes. It never reads Secrets.

- In the operator namespace: get/list/watch Jobs, get Deployments and
  ReplicaSets, and list Pods.
- Cluster-wide: get the nine operator CRDs.
- In each probe namespace: get and patch the named Schema or Migration only.
  Kubernetes RBAC cannot distinguish a dry-run patch from a stored patch; the
  tool sends `dryRun=All` and an optimistic resource-version precondition.

Use an existing independently managed Prometheus and Alertmanager. Load
`rules.yaml`, scrape `/metrics` under job name `ptah-upgrade-observer` every five
seconds, evaluate every five seconds, and route both alerts to the incident
receiver with `group_wait: 5s` and `send_resolved: true`. Keep that monitoring
stack outside the release. If Prometheus is on another host, bind the observer
to a reachable private address and restrict ingress to that Prometheus; the
HTTP endpoint has no authentication. Retain the effective scrape and receiver
configuration in the qualification evidence.

`PtahOperatorUpgradeFailed` fires for a retained failed hook, the original
fifteen-minute deadline without verified recovery, or a compacted hook history.
Its labels identify the installation, not Job UIDs or image digests. A missing
observer also fires `PtahOperatorUpgradeObserverUnavailable`. That alert also
covers a reachable HTTP endpoint whose Kubernetes watch is unavailable or whose
readiness metric is missing. It waits fifteen seconds to tolerate ordinary watch
reconnections; `/readyz` and `ptah_operator_upgrade_observer_ready` report the
same observation state. Losing API access cannot remain healthy just because
Prometheus can still scrape the process. Keep an independently configured inventory
of the expected observer target so removal of a scrape target is investigated.

The observer automatically marks recovery only after a completed candidate hook,
all candidate CRD digests and availability conditions, both available candidate
Deployments and their owned Pods, fresh probe progress, successful server-side
admission dry runs, and a second runtime availability check. Job success, Job
deletion and retry creation do not clear the incident. A lost watch history is
latched and cannot clear automatically: retain the state and investigate the
missing evidence before declaring a new transaction.

After the receiver reports verified resolution, archive the transaction evidence.
Remove this transaction's scrape target and external rules together, then stop
the observer. Removing only the process intentionally raises the observer-loss
alert. A later upgrade needs a new state file and a new pre-Helm boundary.

## Validation

```sh
go test -race -count=1 ./hack/upgradealert
promtool test rules hack/upgradealert/rules.test.yaml
```

`go test ./hack/...` also runs the rule tests. CI requires the promtool version
pinned in `support/tools.json`. Rule evaluation and mocked API tests cannot
replace a real failed hook, the full fifteen-minute interruption, or firing and
resolution observed at the configured receiver within the profile's bounds.
