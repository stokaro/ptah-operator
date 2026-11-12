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
started the Plan that follows the drift, so the phase reads `Planning`. It
names Secrets only by name and key and holds no credentials or plan SQL.
Ptah v0.14.0 counted nothing, so it carries no `inventory`, and
`TestDroppedExtensionObservedRefusesACapturedStatusWithoutTheEntry` requires the
row's predicate to refuse it for that alone.

`postgresql-extension-drift-inventory.json` is the status the same row matched
on Kubernetes 1.37.0, in CI job 128166245075 of run 42295975570. The run built
`63f5de99`, the merge of `1e2fed14` onto `2845812f`, with the Ptah v0.22.0
executor. It is stored as the row printed it, and the jobs for Kubernetes 1.35
and 1.36 printed the same findings and inventory. The schema had already started
the Plan that follows the drift, so the phase reads `Planning`. It names Secrets
only by name and key and holds no credentials or plan SQL. Its `inventory` equals
the one in `internal/dataplane/testdata/drift-feature-extension.json`, the report
the same image printed for the same schema shape in the runner harness:
`TestExtensionReadingCountsWhatPtahPrinted` and
`TestObjectInventoryKeepsTheFramedReport` hold it to that report.
`TestDroppedExtensionObservedRefusesEveryOtherReading` accepts this reading and
refuses copies of it that change the one field each mistake would change.

`postgresql-login-visibility-drift.json` is the status the data-plane row
`postgresql-login-visibility-drift` matched in the same job, stored as the row
printed it. The jobs for Kubernetes 1.35 and 1.36 printed the same findings and
inventory. The login converged a schema it owns, the server's superuser added a
table with a primary key, and the next Observe reported both. The phase reads
`Planning` for the same reason. It names Secrets only by name and key and holds
no credentials or plan SQL.
`TestUnreadableTableObservedRefusesEveryOtherReading` accepts this reading and
refuses copies of it that change the one field each mistake would change.

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

`shared-alias-refusal-deadline.json` keeps the two shared-alias schemas from
the diagnostic projection that CI job 124980815753 of run 41350373785 printed
at 22:20:27Z on November 1, 2026, when the Kubernetes 1.36 schema-faults row
timed out. The run built `1f02b8e`, the merge of `acea5a8a` onto `c5f49a31`.
The entries are the projection's own fields, unchanged; the Leases and the
other schemas are left out. `e2e-fault-pg-alias-a` was still `Blocked` with
`nextReconciliationTime` 23:38:10Z, the next refresh its Resolve claim had set
before the census refused it, which the refusal kept. `TestRealmRecheckScheduled`
refuses that reading as read at 22:20:27Z and accepts copies of it carrying a
deadline a refusal sets. The projection holds no Secret names, credentials or
plan SQL.

`configmap-refused-schema.json`, `configmap-recovered-schema.json`,
`configmap-refused-migration.json` and `configmap-recovered-migration.json` are
the statuses the row `database-reachable-kind` matched on Kubernetes 1.35.8, in CI
job 128045251878 of run 42261374839, with the Ptah v0.18.0 executor. The run built
head `3669373e` of the pull request. They are stored as the row printed them, one
line each. A schema and a migration read a URL whose password the data plane's
PostgreSQL refuses, and each reports `DatabaseReachable=False` with reason
`AuthenticationFailed` and the operator's sentence, beside `ReconciliationFailed`
or `Progressing` with `OperationFailed` and the same sentence. Once the Secret is
corrected, each reports `True` with reason `Observed`. The recovered schema had
already claimed the Plan that follows its Observe, so its phase reads `Planning`,
and the recovered migration had published its plan and waits for an approval.
`TestTargetRefusedCredentialsRefusesEveryOtherReading` and
`TestTargetAnsweredAgainRefusesEveryOtherReading` accept these readings and refuse
copies of them with the one field each mistake would change, including
`DatabaseReachable=True` with reason `Observed` beside the `authentication_failed`
sentence. The readings name Secrets only by name and key and hold no credentials
or plan SQL.

`stopped-run-postgresql.json` and `stopped-run-mysql.json` are the statuses the
stopped row printed on Kubernetes 1.35.8 with the Ptah v0.23.0 executor, in CI
jobs 128491888401 and 128491888423 of run 42400889438, which built the merge of
pull request #937 at head `9f03f6d0`. They are stored as the row printed them, one
line each: the phase, `lastRun` and `unresolvedRun`. Each run was stopped at its
execution deadline inside a sleep that runs outside a transaction, and each
recorded `Unknown` with migration 1 applied and the failure at statement 1 of
migration 2, beside an unresolved run, because Ptah v0.23.0 reports such a
statement's outcome as unknown. `TestStoppedRunPredicatesAcceptTheKindReadings`
holds `stoppedRunRecorded` and `stoppedRunNamesItsMigration` to them. They name
the test namespace, the Job and the images, and hold no credentials or SQL.

`apply-pod-gone-deletion.json` is what the deletion row in
`migration-faults-postgresql` printed on Kubernetes 1.36.4 with the Ptah v0.23.0
executor, on a local kind run on November 12, 2026 that built `81fd99bb`. It is
stored as the row printed it, one line: the deleted migration's name and UID,
its Apply claim, and the `ApplyPodGone` Warning the row matched after it deleted
the running Apply's Job and the Pod went with it. The claim started at 05:16:12Z
with a 420-second Lease, so its term ended at 05:23:12Z. The Warning names the
Job, its UID and that term, and was first recorded at 05:16:44Z and last at
05:20:16Z, four times. `TestApplyLeaseTermIsTheClaimsStartPlusItsLeaseDuration`,
`TestApplyPodGoneWarningsCountOnlyTheClaimsWarningToItsTerm` and
`TestApplyPodGoneWarnedInTermDatesTheWarningByItsOwnTimes` hold the row's
predicates to it, and to copies of it with the one field each mistake would
change. It names the test namespace, the registry, the Job and Secrets by name
and key, and holds no credentials or SQL.
