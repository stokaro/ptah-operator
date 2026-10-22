# ConfigMap snapshot refusal

`configmap-snapshot-refusal.txt` is the termination message of the
`copy-source-snapshot` init container in the ConfigMap source phase's
replaced-snapshot row. It was captured from
[master's Kubernetes 1.36 schema-approvals job](https://github.com/stokaro/ptah-operator/actions/runs/39799216304/job/119899923148)
on October 22, 2026, at commit `f7337d4bf9e651d5a52320647c9342e59e88ee59`; the
1.35 and 1.37 jobs of that run reported the same text.

The row replaces the snapshot's file with longer bytes and keeps its manifest,
so the manifest still gives the bound digest and the copy refuses the file for
holding more bytes than its manifest line names. The message carries the
refusal code and the content-mismatch text; it names neither the bound digest
nor "does not match", which the row had required, so it failed on a correct
refusal. `TestSnapshotRefusedNeedsTheCopyToRefuseAndTheOperationNotToStart`
holds the predicate to this reading.
