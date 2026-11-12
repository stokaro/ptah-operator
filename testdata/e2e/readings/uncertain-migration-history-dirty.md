The JSON beside this file preserves status fields from CI run
[43164616178](https://github.com/stokaro/ptah-operator/actions/runs/43164616178),
head `af82127405996d3833b5b5520061bc310f38e9a6`:

- [PostgreSQL on Kubernetes 1.36](https://github.com/stokaro/ptah-operator/actions/runs/43164616178/job/131006812417).
- [MySQL on Kubernetes 1.36](https://github.com/stokaro/ptah-operator/actions/runs/43164616178/job/131006812375).

That head holds the claim of an Apply whose Job and Pod are gone to the end of
its Lease term, `startedAt` plus 420 seconds here. The run went `Unknown` within
seven seconds, when the row's watch barrier annotated the Job (the last run's
"intent changed"), and the operator set the next reading thirty seconds out.
The claim held to the term, so that reading was overdue when the claim
retired, and Resolve, Verify and History ran at once. The History read found
the revision the interrupted migration left dirty and gave the Blocked
condition the reason `HistoryDirty`; the condition's transition time stayed at
the moment the run went `Unknown`, so the refusal never lapsed. Both jobs
failed on the assertion that the settled refusal is Blocked by
`ApplyOutcomeUnknown`, within a second of that read: PostgreSQL started its run
at 06:44:00Z, its term ended at 06:51:00Z and it read its history at
06:51:35Z; MySQL at 06:47:07Z, 06:54:07Z and 06:54:45Z. Before the hold, the
claim retired a few seconds after the Job went, and the row saw it in the
twenty-odd seconds before the next reading was due.

The fixture is the failure dump the row printed after the assertion. It keeps
the native phase, conditions, last run, unresolved run and history without
rewriting their values; the dump's name becomes `metadata.name`, and the other
fields are omitted. Neither status carries an active operation, a pending
realm release or a plan. The original claim is not in the dump, so the unit
test rebuilds it from the last run's own record: the operation, the Job by name
and UID, the plan and the start. It names the test namespace's registry and
holds no credentials or SQL.
