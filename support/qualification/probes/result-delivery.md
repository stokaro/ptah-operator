# Installed result delivery probes

## Lost acknowledgment fixture

The isolated fixture image provides `result-ack-proxy`. It accepts only one
original Pod's mTLS certificate and forwards that same credential to the real
receiver. After a valid successful receipt, it closes the downstream connection
without sending an HTTP status. It forwards one identical retry and holds that
response while the harness restores the ordinary receiver Service route.
Conflicting payloads, changed receipt UIDs, invalid receipts, and upstream
refusals cannot count as successful redelivery.

The command takes `--backend-address=<manager-pod-ip>:9444`,
`--server-name=<receiver-service>.<operator-namespace>.svc`,
`--trust-directory=/trust`, and `--credential-directory=/credential`.
The trust mount contains only `tls.crt`, `tls.key`, and `client-trust.crt` from
the installed receiver projection. The credential mount contains the original
operation Pod's `tls.crt`, `tls.key`, and `ca.crt`. Copying that credential into
the fixture is privileged test setup; runner permissions remain unchanged.
Use this fixture only in an owned disposable installation with other operations
idle. Remove its Pod and copied Secret when the probe ends.

The data listener uses mTLS on port 9444. The control listener binds only
`127.0.0.1:8081`; access it through a Pod port-forward. `GET /evidence` returns
receipt metadata, timestamps, counters, and the client certificate digest,
without payload or credential bytes. Once it reports two identical receipts,
restore the Service selector and confirm its normal endpoints before sending
`POST /release`. A premature release is refused. The process is limited to ten
minutes, one in-flight request, and two successful deliveries.

The real-TLS fixture tests establish that the first response is actually lost,
the second cannot escape before release, and false receipt evidence is refused.
They do not prove installed runner behavior or SQL execution counts. Installed
acceptance must also retain the original Job/Pod identities, successful runner
completion, the matching persisted receipt, and an independent database witness
showing one execution before and after redelivery.

`result_lost_ack.py` runs that installed PostgreSQL or MySQL migration proof. Use the
owned bootstrap environment and fixture digest described below, with the lab's
`demo-migration-verification-policy` and completed `result-schema-publish` Job
available in the source namespace. Choose a fresh namespace and database:

```sh
export LAB_ENVIRONMENT=/path/to/owned-bootstrap.env
export LAB_WORK=/path/to/owned-lab-work
export RESULT_PROBE_ENGINE=PostgreSQL  # Or MySQL; the engine is required.
export RESULT_PROBE_NAMESPACE=ptah-result-lost-ack
export RESULT_PROBE_DATABASE=result_lost_ack
export RESULT_PROBE_EVIDENCE_DIR=/path/to/lost-ack-evidence
python3 support/qualification/probes/result_lost_ack.py
```

The probe gates Apply credentials before execution, creates the test proxy,
and moves only the receiver Service selector. It checks two identical receipts
while the original runner is still running. A migration inserts a row using a
PostgreSQL sequence or an InnoDB `AUTO_INCREMENT` column. A committed replay
changes the row count; a rolled-back replay still advances the allocated counter.
The MySQL witness reads `information_schema.TABLES.AUTO_INCREMENT` with
`information_schema_stats_expiry=0` to bypass cached statistics. Both counts must
remain one before and after the retry is released. Before execution, the probe
inserts and rolls back a row, requires zero rows and an advanced counter, and
resets the witness. It refuses an ineffective witness or a non-InnoDB table.
MySQL uses the lab's `demo-mysql` Deployment and `demo-mysql-database` Secret;
its migration keeps the ordinary file transaction mode. See MySQL's
[auto-increment behavior](https://dev.mysql.com/doc/refman/8.4/en/innodb-auto-increment-handling.html)
and [statistics cache](https://dev.mysql.com/doc/refman/8.4/en/information-schema-tables-table.html).
The original Job and Pod must complete without replacements or restarts, and
migration history must converge at the current generation.

The probe reconstructs the persisted publication independently and records its
binding and receipt identity in `lost-ack.json`, the held retry in
`held-retry.json`, and the exact immutable publication in `publication.json`.
The procedure hash identifies the executed script. `verify_evidence` and its
refusal tests reject missing faults, changed receipts or bindings, empty or
repeated SQL, replaced executions, repeated preflight, and stale convergence.
This synthetic migration contains no private SQL or credentials in its result.

The Service selector is restored and the proxy Pod and copied credential are
removed in `finally`. `receiver-service-before.json` retains the original
selector for recovery after a killed probe process; verify its UID before
restoring it. The namespace and database remain until the owning lab is removed.
Each run proves a single lost acknowledgment for its recorded engine and
Kubernetes minor. Concurrent duplicate delivery, receiver failure during retry,
and the other engine/minor combinations remain separate requirements.

## First harvest

`result_first_harvest.py` runs a native PostgreSQL plan of exactly 8 MiB
through acknowledgment, Pod/log deletion, manager restart, first controller
consumption, approval, Apply, and database-verified convergence. It requires an
owned disposable cluster prepared by `hack/e2e-kind.sh` and `demo/bin/lab`.
It temporarily removes manager leadership permissions across that cluster.
Run it only when other operations are idle.

The bootstrap must use the candidate's packaged chart, two manager replicas,
and `resultDelivery.enabled=true`. Every kubelet must report its default
`containerLogMaxSize: 10Mi` through configz. The probe refuses a different value;
it does not change kubelet configuration itself. `E2E_DOCKER_CONTEXT` must
address the endpoint recorded by the bootstrap, and every cluster node must
belong to `E2E_KIND_CLUSTER_NAME` on that daemon.

Source the environment written by the bootstrap. The source workload namespace
must contain the native PostgreSQL `storefront` schema, the completed
`result-schema-publish` Job, `demo-registry`, `demo-registry-pull`, and
`demo-verification-policy`. The lab's external PostgreSQL container and its
credential file must still exist. These objects supply the pinned executor,
registry, verification settings, and database fixture; they are not altered.

Provide fresh names and an evidence directory outside the repository:

```sh
set -a
. /path/to/owned-bootstrap.env
set +a
export DOCKER_CONFIG="$E2E_DOCKER_CONFIG"
export RESULT_PROBE_NAMESPACE=ptah-result-first-harvest
export RESULT_PROBE_DATABASE=result_first_harvest
export RESULT_PROBE_EVIDENCE_DIR=/path/to/private-evidence
export RESULT_PROBE_FIXTURE_IMAGE="$E2E_REGISTRY_HOST/e2e-fixture@sha256:<task-fixture-digest>"
python3 support/qualification/probes/result_first_harvest.py
```

The fixture image must be the task-built image. Its `plan-size-schema` command
uses the native executor's JSON escaping to produce the 8 MiB plan. The probe
checks the resulting size, all 16 planstore chunks, and their reconstructed
SHA-256. A changed serializer that produces a different size fails the probe.
It does not silently lower the maximum-size requirement.

## Fault and assertions

A namespace-scoped admission binding holds only Plan credential projections.
Resolve, Verify, and Observe complete normally. Once the canonical Plan
credential exists and its Pod is still pending, the probe saves the manager's
RoleBinding, removes its subjects, and replaces both manager processes. It
requires two new ready Pods and an explicit denial of leader Lease access.
No old process may remain to consume the result.

The probe removes the gate and creates the exact immutable Secret projection
from the canonical credential, impersonating the manager's identity. It retries
only the demonstrated admission-cache delay. The runner retains its ordinary
Pod and credentials and sends its result to the ordinary receiver Service.
Credential bytes stay in memory and kubectl pipes; evidence files contain no
private keys or database credentials.

After the Job completes, the probe rebuilds its complete durable publication
and verifies intent, completion, and chunk UIDs, ownership, lengths, and hashes.
The active claim must remain unchanged and no plan may have been published.
It deletes the producing Pod, confirms absence, replaces both manager
processes again, and verifies the same receipt and unconsumed claim.

Restoring the RoleBinding allows the first harvest. The probe requires the
same plan digest, independently reconstructs its stored bytes, and refuses a
replacement Plan attempt. It then submits the ordinary approval, requires one
Apply publication and current-generation `InSync`, and checks the generated
PostgreSQL default by inserting and reading a row.

`first-harvest-maximum.json` records the binding, UIDs, digests, source revision,
procedure digest, and outcomes. A complete run must exit zero and contain
`converged: true`, `approvedApplyJobs: 1`, and the database witness. A file
written after first harvest alone does not establish the later Apply result.
`result_first_harvest_test.py` holds the publication verifier to missing,
replaced, foreign, and corrupted evidence. `make test-qualification-probes`
runs those refusal tests with the other qualification checks.

## Restoration and scope

The probe restores leadership and removes its admission gate in `finally`.
`manager-rolebinding-before.json` also preserves the original subjects for
manual restoration after a killed Python process. Verify the saved RoleBinding
UID against the live object before restoring subjects. The gate is named
`<RESULT_PROBE_NAMESPACE>-gate`; it consists of a ValidatingAdmissionPolicy
and a ValidatingAdmissionPolicyBinding.

Namespaces, databases, and completed fixture Jobs remain for inspection until
the owning lab is torn down. Use `demo/bin/lab down` with the original
`LAB_ENVIRONMENT` and `LAB_WORK` after all concurrent acceptance probes finish.
That teardown checks ownership and removes the task cluster and its database
container. Do not prune the shared Docker daemon.

This row retains the completed Job while removing the Pod and logs. It does
not establish first consumption after Job deletion, a lost HTTP acknowledgment,
SQL replay behavior after a lost acknowledgment, or recovery from Pod death
between SQL commit and publication. It covers PostgreSQL on the recorded
Kubernetes version; it is not the complete supported-version or engine matrix.

## Retention audit replay

`result_retention_evidence.py` verifies a completed installed retention run
without requiring the cluster to remain alive:

```sh
python3 support/qualification/probes/result_retention_evidence.py /path/to/retention-evidence
```

The input directory contains `retention-before.json`, `retention-after.json`,
`retention-markers.json`, and `retention-delete-audit.json`. The before record
freezes the eligible and pinned record names and UIDs, receipt hashes, plan
identities and hashes, Secret ownership, and deletion deadlines. The marker
file contains the immutable retirement policy and API-assigned creation time
for each frozen attempt. The after record contains the remaining pinned
records, plan hashes, and collection counts.

The verifier independently recomputes every deadline from the marker's saved
window and the later of the marker's and member's API creation times. It
rejects shortened windows, changed sources, incomplete or overlapping cohorts,
changed pins or plans, and missing successful DELETE events. A matching event
must name the exact namespace, resource, object name, and UID precondition.
Its API `requestReceivedTimestamp` must follow the deadline; the time a polling
loop noticed absence cannot substitute for it. Secret deletion must name the
Kubernetes controller manager or garbage-collector ServiceAccount, not the
operator manager or an administrator.

For capture, audit DELETE requests at Request level for `PtahResultRecord` and
Secret objects, and other result-record operations at Metadata level. DELETE
bodies contain DeleteOptions and UID preconditions. Do not audit Secret or
result CREATE/UPDATE request bodies or response bodies: those contain private
credentials or operation data. Retain every API server's events until the
cohort is collected. The three-server installed run uses real one-hour windows;
no test clock is advanced. Its fixed cohort covers both resource families and
all nine operation kinds, with the latest migration run held as a live pin.

A passed audit replay establishes the recorded cohort's deletion timing and
ownership. It does not establish backup/restore, cleanup of every abandoned
partial publication, or a capacity bound under sustained load.
