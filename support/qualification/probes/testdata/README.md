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
