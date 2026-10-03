# Durable-result HA integration

The enabled receiver passed the existing upgrade phase, including CRD guards,
runtime loss and singleton recovery. HA then exposed two obsolete acceptance
assumptions: the custom metric inventory omitted the result cleanup counter,
and namespace cleanup required deletion before durable records could finish
their mandatory one-hour retention window.

The corrected HA phase still requires the post-failover operation, a positive
Resolve failure delta, exact required metric families, and removal of the schema,
Job and operation Pods. It recognizes the declared background cleanup counter
and requests namespace deletion without bypassing retention. The full-hour
collection proof remains in `../result-namespace-cleanup-2026-10-02/`.

The original metric rejection, failing local regression, subsequent namespace
bound failure, metadata-only retained-object census, and successful corrected
HA run are retained. A retry that reused a terminating namespace is retained
separately as an invocation error. No Secret data is included.

Run the compiled E2E binary from `test/e2e`, using the environment produced by
`E2E_SUITE=lifecycle E2E_STOP_AFTER=bootstrap make e2e`. Set
`resultDelivery.enabled=true` in both candidate and synthetic-next Helm values,
upgrade the candidate, then run phases `upgrade`, `ha`, and `uninstall` in order.
Use a new HA namespace for a retry while the preceding namespace retains its
records. The source and image identities are recorded in `inputs.json`; the
changed test sources and validation outcomes are in `summary.json`.

All six upgrade, three HA, and five uninstall phase scenarios passed. The
uninstall phase holds a native Apply across a late upgrade failure and retry,
checks SQL non-repetition, exercises rollback, reinstalls over retained CRDs,
and recovers the exported chart installation from quota refusal under restricted
Pod Security. After its final uninstall, independent API reads find no receiver
Service, projection or journal Secret, enrollment ConfigMap, or rotation Lease.
Seven proof objects remain. The owned cluster, containers, volumes, images and
API tunnel were removed by the normal lab teardown; the independent ownership
census is empty.

Together with the linked certificate, network, one-hour retention, quota,
partial cleanup, and record/key restore reports in `summary.json`, this completes
#586's lifecycle and bounded-storage inventory. It does not complete #579's
loss/timing matrix or final production acceptance. Default activation and the
remaining #586 documentation integration are still open.
