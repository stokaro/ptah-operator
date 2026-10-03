# Native regression inputs

These 94 JSON files are the native readings directly consumed by the adjacent
Python regression tests. They are copied byte for byte from the development
evidence at commit `b708df65f30e3f66b0ff16126fa83d61711df3dc` and retain the original
subpaths so internal manifest references and checksums still resolve.

They test evidence readers and refusal predicates, including mutations of real
readings. They do not qualify a newer operator runtime. Logs, probe executions,
and unused snapshots belong in the external evidence bundle described by
`../../../evidence-index.json`, not in the source tree.
