The JSON beside this file preserves status fields from CI run
[43164616178](https://github.com/stokaro/ptah-operator/actions/runs/43164616178),
head `af82127405996d3833b5b5520061bc310f38e9a6`:

- [PostgreSQL on Kubernetes 1.36](https://github.com/stokaro/ptah-operator/actions/runs/43164616178/job/131006812417).
- [MySQL on Kubernetes 1.36](https://github.com/stokaro/ptah-operator/actions/runs/43164616178/job/131006812375).

That head holds the claim of an Apply whose Job and Pod are gone to the end of
its Lease term. The uncertain row's resource reads every thirty seconds, so its
next reading was overdue when the claim retired, and the operator ran Resolve,
Verify and History at once. The History read found the revision the interrupted
migration left dirty and gave the Blocked condition the reason `HistoryDirty`.
Both jobs failed on the assertion that the settled refusal is Blocked by
`ApplyOutcomeUnknown`, within a second of that read: PostgreSQL started its run
at 06:44:00Z and read its history at 06:51:35Z, MySQL at 06:47:07Z and
06:54:45Z.

The fixture is the failure dump the row printed after the assertion. It keeps
the native phase, conditions, last run, unresolved run and history without
rewriting their values; the dump's name becomes `metadata.name`, and the other
fields are omitted. Neither status carries an active operation, a pending
realm release or a plan. The original claim is not in the dump, so the unit
test rebuilds it from the last run's own record: the operation, the Job by name
and UID, the plan and the start. It names the test namespace's registry and
holds no credentials or SQL.
