# Capacity cycle reading

`native-migration-cycle.json` retains the compact PtahMigration watch for
`e2e-checkpoint-postgresql` from the Kubernetes 1.37.0 development run
`020-baseline-5101109f`. The operator source is
`5101109f2603c74447c4bf101f76a491aef80ffd`.

The watch ran on 2026-10-01. Resource versions, resource/Job/claim UIDs,
generations and timestamps come from the API. Conditions retain type, status,
reason and observed generation; their human-readable messages are removed.
The retained sequence contains one complete Resolve/Verify/History cycle.
Tests replay the sequence and remove a stage to show that the cycle count
requires the native operation evidence. This is a development reading, not a
soak or capacity result.

The complete private collection contained 206 events and three completed
cycles. Its SHA-256 before selecting this resource and removing messages was
`1d4067fd4eeec8b5a363110e8034badfe74f4a6bd8ae5b88eda3d06fbf719615`.

## Restart recovery

`restart-convergence.json` selects readings from the PostgreSQL lab-20 run
[36855172004](https://github.com/stokaro/ptah-operator/actions/runs/36855172004),
artifact `11161705978`, operator source
`a6f3dbec27179275fe44c9d757ba888c172fb472`. The fixture records the complete
report's SHA-256 and the selection. Each retained field comes from the report;
conditions are limited to Ready and InSync.

The original restart window lasted 274.408466 seconds. The old wait repeatedly
listed resources, requiring simultaneous `InSync` phases and fresh observation
timestamps, and restarted its stopwatch after approval recovery. The replacement
uses the continuous watch: each original resource must accept a fresh database
reading after manager deletion, with unchanged UID and generation. A bound Plan
Job proves two live schema reads; a bound History Job proves a migration ledger
read. Both must start after the fault. Later normal refreshes do not erase an
already completed recovery.

The last qualifying resource recovered after 174.467357646 seconds. Counting
only fresh Ready timestamps would instead report 125.038447364 seconds and
would include Jobs claimed before the fault. The regression requires the bound
post-fault reads and rejects the shorter, unsupported measurement. At the end
of the 180-second window, some recovered resources are refreshing again, so a
simultaneous-idle predicate fails.

Run `go test -race ./hack/capacity` to replay the fixture and the refusal cases.
The full original watch histories were also replayed locally with the same
result. This is a retained development-run measurement; it does not replace
execution of the fixed harness or the complete qualification profile.

`restart-recovered-claim.json` selects readings and Jobs from PostgreSQL CI
run [36885096764](https://github.com/stokaro/ptah-operator/actions/runs/36885096764)
on `1fcdde7ea5534bbec0b5a4c48a7468b2ed6c6426`. The source report SHA-256 is
`bc0c83c8cdec7b8e8a59394b83c37cd778a2e58004998f4f2a94c054248e5ae8`.
The fixture describes its projection and preserves every selected field value.

The manager claimed schema 007's Plan at 16:04:48 UTC. The fault began at
16:04:50.104738332, and the replacement manager created that claim's exact Job
UID at 16:05:12. The Job completed at 16:05:22. Requiring a new claim wrongly
waited for the next cycle and reported 187.385885574 seconds. Creation of the
bound Job after the fault proves its SQL could not have run before the fault;
its start time or completion time alone would not prove that. With the exact
Job identity and successful terminal result, all twenty resources have accepted
fresh convergence within 177.950996257 seconds. The regression keeps the
180-second deadline and rejects missing, mismatched, duplicate, old, failed,
uncompleted and chronologically impossible Job evidence.

`churn-plan-export.json` retains a 3,582-byte applied MySQL schema plan, its
committed chunk and its Apply projection from the native run on `c5e42956`.
The source manifest identity is recorded in the fixture. The export regression
reconstructs the captured chunk bytes through the production plan store,
checks the projection and reimports the emitted archive. Replacement chunk
UIDs, corrupt payloads/projections and missing pins must stop before deletion.
