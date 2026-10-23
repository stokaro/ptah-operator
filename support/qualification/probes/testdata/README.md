# Native recovery readings

These readings came from Kubernetes 1.37.0 linux/amd64 running operator commit
`5843171f41794fa58e7145e43f1fd5bf40dc93a1` on September 30, 2026.

`workload-history.json` retains the original Apply Job and Pod from the
PostgreSQL migration checkpoint. Its other operation Pods came from the
preceding interrupted native attempt and test command classification only.
Managed fields were omitted. `recovery-states.json` projects generation, UID,
suspension and status from the checkpoint's suspended and initially applied
resources. It contains no target Secret, backup payload or credential.

| Checkpoint | Procedure SHA-256 | Decrypted checkpoint SHA-256 |
| --- | --- | --- |
| postgresql-migration-04 | `680fd5462c67f3bb6e24a15a133f5e29b07b3380a626b29227442f14939bba1b` | `e806524b9310108ebaa22d4a9b8e2f6da083913befed9550770a807c55066a6c` |
| postgresql-schema-02 | `dcebae3dee4d99b38cb9222c5f5f633c77d0a7ee343516a98a7d8898f571da2c` | `65e76b76c7609dcec14a1060b72bbc9166c84387a60bde69926fdf674ce3f724` |

The private encrypted checkpoints remain outside the repository. These are
inputs to rejection controls, not evidence that the full recovery matrix passed.

`recovery-webhooks.json` contains the public admission configurations observed
on operator commit `4560860c717b5a69ed0bafecb12378b6728b72f4` during the
PostgreSQL schema namespace-restore pilot. It retains public CA certificates,
not private keys. The cold-restore controls allow regenerated API identities,
service addresses and CAs while requiring the admission rules to remain equal.

`restore/namespace-held-by-result-retention.json` is the recovery namespace of
the 0.2.2 PostgreSQL schema database-loss cell, read on Kubernetes 1.37.0
linux/amd64 on October 12, 2026, 95 seconds after its deletion was requested,
from release v0.2.2 (`81b154ef69ae709a4a1aa39bab9db93fedb9b10d`). Its durable
result records were the only content left, refused by the retention admission.
Managed fields were omitted.

`monitoring-check/` holds readings of the v0.2.4 monitoring deployment check on
kind with Kubernetes 1.35.8, October 19, 2026, with kube-prometheus-stack
92.10.1 and release v0.2.4 (`d1859174288e7f67873c9f15950b7e6fabf8fd58`).
`receiver-v0.2.4.jsonl` is the receiver's log of the run that passed, and
`receiver-v0.2.4-first-attempt.jsonl` the log of the first attempt, which
passed on the startup incident's resolution and was discarded.
`targets-v0.2.4.json` is Prometheus's active targets, trimmed to the scrape
pool, address, health and identifying labels, read while the chart's own
ServiceMonitor was the only one in the release namespace: it selected no
Service, so no target there.

The `-v0.2.5` readings are the check's own run on the published v0.2.5
(`b4269ebd7853688b2a33ef8423bf579f913510e7`), linux/amd64, October 21, 2026,
with kube-prometheus-stack 92.11.0, fresh-installation run 39563266483: the
receiver's log, the active targets trimmed the same way, and the release's
ServiceMonitor with its metadata trimmed to name, namespace and labels.

`fresh-install/upgrade-moved-executor.json` is the v0.2.6 upgrade over v0.2.5
on Kubernetes 1.37 linux/amd64 with PostgreSQL, from Fresh installation run
39916361220 on October 23, 2026. The upgrade moved the executor from Ptah
v0.13.0 to v0.14.0. `after` is the schema status the probe refused, copied from
the run's log. `before` is the predecessor's applied record and its bound
components, with the epoch the transcript recorded. `started` and `candidate`
are the transcript's `runtime-on-candidate` and last `release-identities`
steps.

`restore/waiter-postgresql-ptah-v0.14.0.json` and
`restore/waiter-mysql-ptah-v0.14.0.json` are the in-flight Apply's server wait
as `restore_during_apply.ApplyLoss.waiters` reads it, taken on October 23, 2026
from PostgreSQL 17 (psql `-At`) and MySQL 8.4 (`--batch --skip-column-names`).
A session held a lock on `recovery_canary` while the Ptah v0.14.0 release image
(`sha256:6c7a5d62…`) applied the fixture's revision 2 with `ptah schema apply`.
Ptah v0.14.0 sends the plan's comment lines with the statement; v0.13.0 sent the
bare `ALTER TABLE`. Only the account filter differs from the probe's query.
