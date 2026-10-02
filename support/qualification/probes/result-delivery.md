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

## Receiver replacement during retry

Set `RESULT_PROBE_RESTART_RECEIVER=1` for the same migration probe to replace
both manager Pods between the first persisted receipt and its redelivery.
The fixture's `--pause-retry` flag holds the retry before any upstream request;
`retryWaits` records that the runner actually reached this boundary. The gate
honors request cancellation and does not change the runner's retry budget.
`POST /resume-retry` is refused until a retry has reached the gate after a lost
acknowledgment. Resuming the gate is idempotent.

The harness creates a temporary backend Service with the ordinary manager
selector, separate from the result Service routed through the proxy. After
independently reconstructing the first publication and observing one SQL effect,
it deletes both original manager Pods, verifies that neither UID remains, waits
for two distinct ready replacements and their backend endpoints, and resumes
the retry. The replacement receivers must return the original receipt. Restoring
the ordinary result Service and releasing the held response then permits the
original runner to finish. The backend Service is removed in `finally`.

`receiver-restart.json` records the old and new UID sets, readiness observation,
and persisted receipt identity. Version 3 evidence rejects overlapping or
missing replicas, an absent retry gate, or a second receipt obtained before
replacement. This row does not prove certificate rotation, concurrent duplicate
requests, or behavior after the runner exhausts its delivery deadline.

## Job and Pod loss before publication

Set `RESULT_PROBE_RUNNER_LOSS=1` and `RESULT_PROBE_RESTART_RECEIVER=0` to run the
same native migration setup with a publication quota instead of the proxy fault.
The engine, namespace, and database inputs remain required. The probe sets a
30-second reconciliation interval for recovery observations.

After the original Apply credential is recorded but before its Secret is
projected, the probe fills a namespace quota for result records at its current
census. It waits for the API to report both the hard limit and used count, then
releases execution. After observing one committed SQL effect while the Pod still
reports `Running`, it requires the original Apply intent to be absent and removes
the original Job and Pod. The quota prevents a result from being persisted while
the producer is stopped; deleting both objects also removes termination-summary
recovery from this row.

The controller must record the exact operation and Job UID as `Unknown`, with
an identical unresolved-run copy in metadata. Only after retaining that record
does the probe remove the quota. A fresh native history reading must resolve
the same operation through `HistoryRead`, reach current-generation
`HistoryMatched`, and leave the calibrated SQL counter at one. No replacement
Apply Job may exist, and the original Apply publication must still be absent.
`runner-loss.json` retains these observations and both procedure hashes;
`result_runner_loss.verify_evidence` and its refusal tests replay the verdict.
The retained installed evidence also includes the independently reconstructed
fresh history publication and exact runtime identities.

A failed probe suspends its migration and removes the quota. As with the other
rows, namespaces and databases remain until the owning lab is removed. This
case loses both the Job and Pod after a fully applied migration. It does not
prove recovery from a partially applied migration, the human acknowledgment and
fresh-approval path, or the complete supported-minor matrix.

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

## Concurrent redelivery of a committed result

Set `RESULT_PROBE_CONCURRENT=1` with both replacement and runner-loss modes off.
The shared migration setup holds the runner's retry before forwarding it, after
its first result has been durably accepted. `result_concurrent.py` opens a
port forward to each manager Pod and verifies TLS against the receiver Service
name. The privileged harness borrows the original Pod's client credential;
0600 files live in a temporary directory removed in `finally`. The runner's
permissions do not change. Both port forwards are also stopped in `finally`.

Each request pair waits at a barrier after TLS and HTTP headers, before sending
its body. The evidence must show overlapping request intervals on distinct
receiver Pod UIDs. Both identical requests must return the original receipt.
Both changed requests must return 409, and a mixed pair must return that same
receipt and 409 respectively. The changed document preserves canonical encoding
and alters only the native migration description; a 422 validation failure, 403
permission refusal, or 503 temporary error cannot satisfy this row.

The probe independently reconstructs the original publication before and after
all six requests and checks every stored member remains unchanged. It then
resumes the original runner retry and requires the ordinary lost-ACK proof,
including one calibrated SQL effect and original Job/Pod completion.
`concurrent.json` records each response, payload digest, receiver UID, monotonic
request timestamps, receipt, and procedure hash, including on verdict failure.
This tests overlapping redelivery after publication. A race between first
publishers of an absent intent and single-replica saturation need separate rows.

## First-publication race

Set `RESULT_PROBE_FIRST_PUBLICATION=1`; leave the replacement and runner-loss
modes off. This includes the concurrent-delivery client. The test-only proxy
holds the original native result before forwarding its first PUT. Its loopback
`/pending-first` endpoint exposes those bytes only to the privileged harness;
ordinary evidence and logs contain no payload. Resuming the original request
clears this temporary copy.

A temporary mutating webhook matches only intent CREATEs in this probe's
namespace. The proxy serves it over verified TLS on a temporary Service port.
A server dry run must reach that webhook before execution is released. The
harness verifies the original intent and completion are absent, then sends the
original bytes concurrently to both installed receivers. The webhook holds
neither write past its eight-second bound and admits neither until two distinct
manager Pod identities have reached the same intent with the original digest.
Admission request UIDs and entry/release times prove both CREATEs overlapped;
client starts alone do not establish that property.

Both requests must return the same durable receipt. The subsequent conflicting
and mixed pairs retain the ordinary concurrent-delivery requirements. The
harness independently reconstructs the publication, removes the temporary
webhook, and resumes the original runner, which must complete the lost-ACK row
with one SQL effect. The Service ports and selector are restored, and the
webhook, proxy, copied credential, and port forwards are removed in `finally`.

`first-publication.json` retains the absent-record census, both blocked
admissions, concurrent responses, and procedure digest. The absent read and
client headers are ordered by the harness's monotonic clock; admission entry
and release use the fixture's own clock. No cross-machine clock comparison is
needed. This row forces identical first writes. Different first payloads racing
to choose a winner and same-receiver saturation remain separate cases.

## Slow uploads and independent migration progress

Set `RESULT_PROBE_UPLOAD_BUDGET=1` with the other fault flags unset or zero.
The shared native setup holds the original Apply Pod before execution. The
privileged harness uses that Pod's recorded credential over verified mTLS;
the runner receives no API permission. It sends one byte of a declared 4096-byte
body to the receiver on the current leader. A second request must receive 503
and `Retry-After: 1`, while the other receiver still admits authenticated HEAD.

The result Service temporarily selects only the free receiver. A second native
migration on a separate database must converge before the stalled upload ends.
The harness then occupies the second receiver and requires both to refuse excess
requests. Each incomplete body must receive 408 after the installed two-minute
limit, and each slot must admit HEAD again after its timeout. Neither incomplete
body may create the original intent. The original Apply Lease is sampled
throughout; its UID, holder and epoch must remain unchanged with renewal gaps
no greater than the case's predeclared 15 seconds. Responses to the extra
requests must complete within five seconds. These are focused fault-case
budgets, not replacements for the frozen capacity profile.

After restoring the Service, the original runner must finish its single Apply.
Both native databases must show one committed effect and no rolled-back replay,
using the calibrated counters from the shared setup. Evidence includes both
immutable publications, receiver identities, monotonic request intervals, raw
Lease samples and independent-resource convergence. Cleanup restores the
Service selector and removes the temporary Pod label and credential gate.

`result_upload_budget.verify_evidence` replays these bounds. Its refusal tests
reject absent or short faults, serial saturation, unbounded responses, a changed
or stalled Lease, SQL replay and stale convergence. This case covers migration
progress with one available receiver and renewal with both upload slots busy.
It does not claim that new delivery succeeds while all receivers are unavailable,
or supply the other controller-family and installed-failure requirements.

Set `RESULT_PROBE_FAMILY=PtahSchema` with the upload-budget mode to exercise the
other controller family. The default remains `PtahMigration`. The schema case
starts two empty databases, publishes a small native schema, and holds the
original Apply credential. An independent schema must reach current-generation
`InSync` through the free receiver before the first upload times out. Both
final database readings must contain the exact declared column types,
nullability, and primary key. Empty metadata, a preexisting fixture, a missing
key, or status without the database effect fails the evidence verifier.

Schema evidence is version 2 and records the resource family explicitly. It
proves two distinct original Apply Jobs and native schema convergence, without
claiming the sequence/auto-increment SQL counters used by migration evidence.
The upload, Lease, authentication, cleanup and observation budgets are unchanged.

## Interrupted result serving-key renewal before first harvest

Set `RESULT_PROBE_ROTATE_LEAF=1` when running `result_first_harvest.py` in the
owned lab. The existing first-harvest procedure holds leadership, obtains and
independently verifies the native 8 MiB Plan receipt, and removes the producing
Pod. Before restarting the receiving managers and restoring leadership, it runs
`result_rotation.py` against the installed certificate-rotator Deployment.

A temporary admission policy holds changes to the exact result trust projection
Secret. A server dry run must prove the hold. The probe moves the rotator's
renewal threshold to 2160 hours and replacement leaf validity to 2400 hours;
these values retain the installed 168-hour scheduling margin. It changes no CA,
credential lifetime or retirement fence. The installed rotator journals a new
leaf and key, but cannot project them. The probe removes that rotator Pod and
requires its replacement to resume the same persisted candidate. It then removes
the hold and waits for the journal to return to `stable` and rotator readiness.
The production rotator only completes that transition after verifying every
ready receiver endpoint serves the expected certificate over authenticated TLS.
Both manager UIDs must remain unchanged during projection, so process restart
cannot substitute for kubelet projection and trust reload.

The evidence retains leaf/key hashes, unchanged CA hashes and enrollment policy,
trust-object UIDs, manager and rotator UIDs, and restoration of the original
rotator arguments. It never records private journal or projection bytes. The
surrounding first-harvest proof then replaces both managers and requires the
original receipt, exact reconstructed plan bytes, ordinary approval, one Apply,
and the native database result. Its `finally` handlers restore the admission
gate, rotator arguments and leadership on failure.

This case proves installed leaf/key renewal and interruption recovery. It does
not exercise CA replacement or either credential retirement wait. Those remain
required alongside the existing local state-machine tests.

## Expired pending CA recovery before first harvest

Build the existing fixture for the host with
`GOFLAGS=-p=1 go build -o /tmp/ptah-result-trust-fixture ./test/e2e/handcraftoci`.
Set `RESULT_TRUST_FIXTURE_BINARY` to that path and `RESULT_PROBE_ROTATE_CA=1`
when running the first-harvest probe; leave the leaf-renewal flag off.

After the original Plan receipt is verified and its Pod is removed, the harness
stops the installed rotator and reads its stable journal. The fixture accepts
only that canonical journal, preserves its installation UIDs, and creates a
pending `prepare` transition with correctly signed expired current and candidate
certificates. Their signed expiration dates precede the test by more than the
five-minute skew allowance. The candidate has distinct private keys. Private
material travels only through subprocess pipes and API requests. Retained
evidence contains the public certificates, expiration dates and key hashes.

This is an injected expired-state fixture, not elapsed real-world certificate
lifetime. No cluster clock, production lifetime or retirement bound is changed.
It models recovery after the rotator was unavailable long enough for a pending
candidate and the current authorities to expire. Local state-machine tests
remain the evidence for both full credential-retirement waits.

The harness installs the expired journal, enrollment policy and projection while
the rotator is stopped. It replaces both managers and requires new unavailable
Pod identities. An exact-Secret admission hold then keeps the recovery projection
from moving. The installed rotator must discard the expired candidate and
persist fresh authorities plus an enrollment fence. Replacing the rotator at
that point must preserve those authorities and that fence. Removing the hold
lets the unchanged production expiry-plus-skew path switch and retire trust.
Both receivers and the rotator must become ready with the new authorities.

The enclosing first-harvest case then replaces the receiving processes again,
restores leadership and requires the original receipt, identical 8 MiB plan,
ordinary approval, one Apply and database-verified convergence. A failed CA
probe restores the original journal, policy, projection and replica count before
restoring leadership. A successful probe keeps the new stable trust. Neither
case leaves its admission hold installed.
