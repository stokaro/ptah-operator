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
