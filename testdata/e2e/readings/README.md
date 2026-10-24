# Native readings

`alert-schema-recovered.json` and `alert-schema-original-approval.json` were
captured after the Kubernetes 1.37 amd64 development run of operator
`5101109f2603c74447c4bf101f76a491aef80ffd` stopped in the unresolved-schema alert
scenario on October 1, 2026. They contain no Secret payloads.

The original approval was consumed and later marked stale. The recovered schema
had completed read-only recovery and was awaiting approval, with no active
operation or pending observation. Its immutable plan retained the original UID
and fingerprint because the database change had not committed. A new plan UID
is therefore not a prerequisite for a new approval decision.

`TestRecoveredSchemaMayReuseItsImmutablePlan` holds the recovery predicate to
these readings and rejects stale observations, unresolved work, replacements
and mutation authority. The separate recorded-history tests require the actual
Observe and Plan sequence; the final snapshot alone does not prove that history
or successful delivery of the recovery notification.

`alert-schema-recovery-wait.json` is a native PostgreSQL schema reading from
Kubernetes 1.37.0 at source `b1a2e99c1ad121bb2103e9f15b82cf47679307ce`.
The schema has resumed after an interrupted Apply but must still wait until
`pendingObservation.observeAfter`. The original alert proof exhausted its
six-minute wait during the later Plan. The deadline regression reads this
persisted horizon and places its test clock before it; it does not infer the
resume time from the resource phase or claim this snapshot proves convergence.

`prometheus-plan-store-export-gap.json` and
`prometheus-plan-store-stale-gauge.json` contain time-cropped native Prometheus
samples from CI jobs 111691203546 and 111785841643. Their timestamps, values and
labels are unchanged. The plan-store interval tests require a complete, fresh
baseline before pruning and reject a missing gauge inside either timing window.
An earlier gap during export cannot invalidate a later complete baseline.

`upgrade-recovery-boundaries.json` projects the four native upgrade probes from
Kubernetes 1.35.8 at `5b572b0f`. It keeps their identities, policy, interval,
read timestamp, plan reference, and conditions. Both schema readings completed
planning fourteen seconds after reading the database while `Ready=False` kept
its pre-upgrade transition time. `PlanReady=True` and `Progressing=False` date
the completed cycle. The projection omits artifact and target configuration and unrelated status
fields. No credentials or plan payloads are included.

`postgresql-extension-drift.json` is the status the data-plane row
`postgresql-extension-drift` matched on Kubernetes 1.35.8, in CI job
119523276862 of run 39685906128. The run built `90d8dec1`, the merge of
`d156c8fa` onto `6e5df759`, with the Ptah v0.14.0 executor. It is stored as the
row printed it. After the schema converged on its artifact, the extension was
dropped by hand, and the next Observe stored one safe finding,
`feature_changes:ptah.run/pgextension/extension-change`, counted once; the jobs
for Kubernetes 1.36 and 1.37 printed the same finding. The schema had already
started the Plan that follows the drift, so the phase reads `Planning`.
`TestDroppedExtensionObservedRefusesEveryOtherReading` accepts this reading and
refuses copies of it that change the one field each mistake would change. It
names Secrets only by name and key and holds no credentials or plan SQL.

`journal-plan-published-again.json` is the journal of the schema `storefront`
in the Ptah Console acceptance lab, read on October 24, 2026 from Kubernetes
1.37.0 running operator `70bd7654e4a8a694ef8ab41e769535f456263c4b` with the
Ptah v0.15.0 executor. It keeps the schema's identity, phase, applied plan
reference and journal, and nothing else from the status. The schema applied
three plans, and after each Apply the next status write recorded the plan's
publication again, dated when the plan was created, because the plan stays in
status until the proof that follows settles.
`TestApprovedApplyJournaledRefusesThePlanItsApplyPublishedAgain` refuses this
reading and admits it once the three repeated publications are left out. It
holds no Secret names, credentials or plan SQL.
