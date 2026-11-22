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

`prometheus-unresolved-read-gap-schema.json` and
`prometheus-unresolved-read-gap-migration.json` are the native Prometheus
snapshots the `unresolved-apply` row printed for its MySQL lanes when it failed
in the alerting-operations job for Kubernetes 1.35 on PostgreSQL 16 of CI run
43593974962, stored byte for byte. The leader's scrapes at 12:46:42.241 and
12:46:47.241 took 3.005 seconds, the snapshot read's own timeout, and carried
no unresolved gauge. The row queried neither the synchronized view nor the
read-failure counter, so
`TestAlUnresolvedHistoryAcceptsTheCIReadFailureOnlyWhenDeclared` rebuilds both
from the scrapes each snapshot holds. It requires the history to refuse the
snapshot when the leader did not declare the failed reads, and to accept it
when it did.

`prometheus-quiet-window-declared-read.json` is the reading for the
`ordinary-policy-waits` row, which failed in the alerting-operations job for
Kubernetes 1.37 of CI run 43704948556 (job 132786396786) with
`PtahOperatorUnresolvedViewReadFailures` firing and nothing new at the receiver.
That row printed no native history, so the reading is a reconstruction, built
from the two the same job's `unresolved-apply` row printed at 11:06:36 and
11:06:56, for its MySQL migration and schema lanes, from the same manager and
monitoring processes the quiet window ran against. Both are cropped to the
samples a range query at 11:06:27 returns, which end before the migration census
reads 1 again at 11:06:29.670. The series the two share agree sample for sample,
and the merge keeps each family's census. Labels and number text are unchanged,
and both families read zero from 11:03:14.670 on. Before that they count the
incidents of that row's PostgreSQL lanes, which the tests' longer lookbacks
reach the way a lookback can reach the preceding row's incident. The leader's
scrape at 11:05:14.670 is then made a declared failed read: it carries no
census, its view reads 0, its read-failure counter reads 1 from that scrape on,
and it took 3.005 seconds. The quiet-window tests in
`test/e2e/alerting_negative_reads_test.go` rebuild every variant they refuse
from this reading's scrapes, and
`TestAlQuietWindowAcceptsTheCIReadFailureOnlyWhenDeclared` requires the window
to accept the alert only where the leader declared the read.

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
on Kubernetes 1.37.0, in CI job 131270662597 of run 43241757619. The run built
`9b8a762a` on master with the Ptah v0.23.0 executor. It is stored as the row
printed it, and the jobs for Kubernetes 1.35 and 1.36 printed the same findings
and inventory. The schema had already claimed the Plan that follows the drift, so
the phase reads `Planning`. It names Secrets only by name and key and holds no
credentials or plan SQL. Its `inventory` equals the one in
`internal/dataplane/testdata/drift-feature-extension.json`, the report the same
image printed for the same schema shape in the runner harness:
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

`apply-pod-gone-live.json` is what the unresolved-alert row in
`alerting-operations` printed for its PostgreSQL migration on Kubernetes 1.36.4
with the Ptah v0.23.0 executor, on a local kind run on November 12, 2026 that
built `87c1cc56`. It has the same shape, of a migration that was not deleted:
the row deleted the running Apply's Job and the Pod went with it. The claim
started at 06:53:53Z with a 420-second Lease, so its term ended at 07:00:53Z.
The Warning says the Apply waits for the Job, by UID, until that term, and was
first recorded at 06:54:03Z and last at 06:57:34Z, four times.
`TestApplyPodGoneWarningsCountOnlyTheClaimsWarningToItsTerm` holds
`applyPodGoneWarnings` to counting it for a live Apply and not for a deletion,
and `applyPodGoneWarnedInTerm` to accepting it. It names what the deletion
reading names, and holds no credentials or SQL.

`native-refusal-schema.json`, `native-refusal-migration.json`,
`native-refusal-recovered-migration.json`, `approved-run-postgresql.json`,
`partial-run-left-a-dirty-revision.json` and
`isolated-node-durable-migration.json` are what rows printed on Kubernetes
1.36.4 with the Ptah v0.23.0 executor, on two local kind runs on November 12,
2026. The schema reading is from the `data-plane` suite. The migration readings
are from one run of `migrations-postgresql` and `migration-faults-postgresql`,
which built `1bf45520`: `9b8a762a` with the rows' printing and a local catalog
edit that put both phases in one suite. Their `controllerRevision` names that
commit. Each is stored as the row printed it, one line, after the row's scan for
protected credentials. They name the test namespace, the registry, the images,
Jobs, and Secrets by name and key, and hold no credentials or SQL.

`native-refusal-schema.json` is the schema `assertNativePolicyRefusal` matched
once Ptah refused its artifact under
`testdata/e2e/verification-policy-other-type.yaml`, with the Verify result frame
the row read and the `ArtifactVerificationRefused` Warning it matched. The
schema is `Blocked`, and `ArtifactVerified` and `Ready` read `False` with reason
`PolicyRefused`; `ArtifactVerified` names `artifact_types`.
`native-refusal-migration.json` is the same for the migration row
`nativePolicyRefusalProof`, which reads no result frame.
`TestNativelyRefusedSchema`, `TestNativelyRefusedMigration`,
`TestNativeRefusalResult` and `TestNativeRefusalEvent` accept these readings and
refuse copies of them with the one field each mistake would change.
`TestNativeRefusalResult` also requires the policy digest the schema read to be
the digest of that policy file.

`native-refusal-recovered-migration.json` is the same migration once its policy
was edited to one that accepts the artifact: `InSync` under `Always`, with the
run that followed and no condition naming the refusal. `ArtifactVerified` and
`Blocked` moved in the same second, 18:35:01Z, and the history read after the
run dates `Ready`. `TestRecoveredFromRefusal` and
`TestRefusalTransitionsNamesBothTimes` hold the row's predicate and its failure
message to it, and `TestTheRefusalRecoveryReadingIsTheReconcilers` in
`internal/controller` holds the reconciler to its phase and condition reasons.
It replaces a document of the same shape written before any cluster ran the
row.

`approved-run-postgresql.json` is what `assertRunProvenance` matched after the
first approved run: the migration's identity and status, the plan the run
carried out and the approval that allowed it, stamped for the impersonated
`oidc:José García $(HOME)`. `ApprovalRequired` reads `False` with reason
`HistoryMatched`, the reason `Ready` carries, which is what a reading that
publishes no plan has written since #917.
`TestRunIdentityRecordedRefusesEachClause`,
`TestRunApprovedByRefusesEachClause`, `TestRunVerifiedInSyncRefusesEachClause`
and `TestRunJournaledRefusesEachClause` accept it and refuse copies of it with
the one field each mistake would change. It replaces a status written by hand.

`partial-run-left-a-dirty-revision.json` is the first reading the partial row
took during its 90-second hold that found a cycle in progress: the phase is `Resolving` while `Blocked` stays `True` with reason
`HistoryDirty`, the history is dirty at version 3 with one migration pending,
and the partial run is both `lastRun` and `unresolvedRun`. It replaces a
reading of the same moment from an older run, with digests, times and names
redacted, whose `ApprovalRequired` read `False/Satisfied`.
`TestPartialRunRecordedReadsTheRecord`,
`TestDirtyReadingHoldsTheRefusalNotThePhase` and `TestBlockedRefusalHeld` accept
it, and `TestRenderTextShowsNoHoldWithoutAnUnresolvedRun` in
`internal/migrationview` settles the run in a copy of it.

`isolated-node-durable-migration.json` is the migration the isolated-node row
read once its Apply settled: `InSync`, its run `Applied` under the policy with
versions 1, 2 and 3, and a history read twelve seconds after the run ended. It
replaces the reading from CI run 37154162826, whose `ApprovalRequired` read
`False/Satisfied`. `TestIsolatedDurableRunReading` and
`TestRunAllowedByPolicyRefusesADecision` accept it and refuse the copies they
list, and `internal/statusview` reads its conditions.

`teardown-retired-attempt.json` and `teardown-namespace-refused.json` were read
on a local kind run on November 12, 2026, with Kubernetes 1.37.0 and operator
`71d60d6d`, in a lab namespace holding two schemas whose Resolve failed at an
artifact nobody serves. `teardown-retired-attempt.json` is the three records of
schema `teardown-a`, read after that schema was deleted and before the namespace
was: its credential, its result and the `retired` record the collector wrote 16
seconds after the deletion. The list the run read also held the other schema's
two records, which are left out; the three are stored as the API server returned
them. `teardown-namespace-refused.json` is the namespace 15 seconds after its
deletion, with the operator installed: `NamespaceDeletionContentFailure` quotes
the webhook's refusal and the time `teardown-a`'s result could first go.
`TestTeardownRetiredAttemptReadsTheWindowTheRetirementPersisted`,
`TestTeardownRetiredAttemptRefusesWhatIsNotOneRetiredAttempt` and
`TestTeardownNamespaceRefusalNamesWhatTheNamespaceControllerReports` hold the
uninstall-first teardown's predicates to them, and to copies of them with the one
field each mistake would change. They name the lab namespace, the Job and the
Pod, and hold no credentials or SQL.

`view-invoker-created-plan.json`, `view-invoker-turned-on-plan.json` and
`view-invoker-body-kept-plan.json` are the plans the Ptah v0.30.1 release image
`ghcr.io/stokaro/ptah@sha256:dad67242b23d6ffbd0c0ec43b91aefab5a43cbdea1dca3e6361df6ebca631e3f`
(commit `c5fba763a6fb346fc8a35d2f336d75a91385e3b9`) saved on November 18, 2026
for `testdata/e2e/postgresql-external-v4-view.sql`, `-v5-view-invoker.sql` and
`-v6-view-body.sql`, in that order, through the lab of
`support/qualification/probes/schema_sql_readings.py`, against one PostgreSQL 17
database that a plan of `postgresql-external-v3-grant.sql` had been applied to.
Each was applied with `ptah schema apply --plan` before the next was planned,
and each Apply wrote its two schema apply log entries. They are stored as Ptah
wrote them, and hold no credentials. Against the plans they replace, which
v0.29.3 saved and v0.29.5 reproduced, each statement names the view's schema,
as `CREATE VIEW "public"."e2e_widget_names"`, and each plan records that schema
as `placed_schema`
([stokaro/ptah#6132](https://github.com/stokaro/ptah/pull/6132)). Nothing else
moved but the plan name and the fingerprints. On November 18, 2026 the v0.31.0
release image
`ghcr.io/stokaro/ptah@sha256:ffd3bdc6e81e3875df4b7784eaf6cf6732834c70eeef2776edec47caea97a7e2`
(commit `dc2decdee2617a350475d0f17347c7fb09dbce97`) saved all three the same way
with nothing moved but the plan name and the fingerprints again, so they stay.
The same day the v0.32.0 release image
`ghcr.io/stokaro/ptah@sha256:0d4ae16df66558f7710ce968d930c1e7b337f9ac93309e36412841cbdf1bc8fc`
(commit `da2df4a5e7088e3efcdec6b8f08110c23572bb74`) saved all three byte for
byte as v0.31.0 did. On November 19, 2026 the v0.33.0 release image
`ghcr.io/stokaro/ptah@sha256:933cc38f93375e0a83ff49db0cfdcee301ab328e95d13496fb2b2be82be1f906`
(commit `37c98b0024b136dd5079f42dd705d49ab24aeb52`) saved all three with the
same statements and length, and only the plan name and both fingerprints moved,
so they stay. Later that day the v0.33.1 release image
`ghcr.io/stokaro/ptah@sha256:37c16165f4184ddfd80cf61da9a366fe13ab950830b095f401e91de2c2f4383c`
(commit `f7c85bdc579c9e5939108a892f11e3d6d0974248`) saved all three, and the
plan of `postgresql-external-v3-grant.sql` they follow, byte for byte as v0.33.0
did. On November 20, 2026 the v0.34.0 release image
`ghcr.io/stokaro/ptah@sha256:8be4bebe660fcc427028ce854f815f5e5ae24854134d80d6ed9f00ab3aaaa1dc`
(commit `90e98f6ca0b3d77e66b0d5e72a6a9e22da20653a`) saved all three with the same statements and length, in the
same order and each applied before the next, against a disposable PostgreSQL
17.11 server as its superuser rather than through the probe's lab. Only the plan
name and both fingerprints moved, which identify the database the plan read, so
they stay. On November 22, 2026 the v0.35.0 release image
`ghcr.io/stokaro/ptah@sha256:f76221499b83c232d737894daea53f8c3e1ebad20d6602f05b88ffae1134913a`
(commit `1ec0b95eea95afce6f50b5912311ce4822f0636b`) saved all three the same way,
with the same statements and length, and again only the plan name and both
fingerprints moved, so they stay.

Before them the plans were the ones the Ptah v0.29.3 release image
`ghcr.io/stokaro/ptah@sha256:094035745e808214833f5984363f0f36c50db1d3eccb978d6eeeeaaf98512d96`
(commit `dfbda1b29a7e4591a41334c6949dddac3a6bd69d`) saved on November 16, 2026
the same way. Against the plans those replaced, each effect gains
`before_plan`, which says whether the view existed when the plan started and
the options it held then
([stokaro/ptah#6099](https://github.com/stokaro/ptah/pull/6099)), and nothing
else moved but the plan name and the `from_fingerprint`, which identify the
database the plan read. The predicate does not read `before_plan`. On November
16, 2026 the v0.29.4 release image
`ghcr.io/stokaro/ptah@sha256:3b9f644a08221cdcde9062c6d23c55b6fbcccaaa9d4edc2aec10dd80f0f448f1`
(commit `1f1ff93c5ac8fe34f80845bb1600ac6a3bcee788`), run through the same lab,
saved the three plans byte for byte, and each Apply wrote its two schema apply
log entries, so they stayed. On November 17, 2026 the v0.29.5 release image
`ghcr.io/stokaro/ptah@sha256:98874de7994f2314d3acb699430ca5b21025792c44ff8c17086a222226ae1aae`
(commit `8a634c0574427e313392f1131382cf4346d05b0d`) did the same again.

The plans the v0.29.3 ones replaced were the ones the Ptah v0.26.0 release image
`ghcr.io/stokaro/ptah@sha256:247d115a9062ed785501ae05620acea1a176e72776f71b175a244fb056d2174f`
(commit `1cf0563c0951fbf67f13f9b4ca7f9b49bbe2f3a3`) saved on November 13, 2026
the same way, in another setup, against a PostgreSQL 17.12 database. On November 14, 2026 the v0.28.0 release
image (commit `2ee460a6`), run the same way against PostgreSQL 17.11, saved the
same three files byte for byte, and each Apply wrote its two schema apply log
entries. On November 15, 2026 the v0.29.0 release image (commit `75fa9991`)
did the same again. Later that day the v0.29.2 release image (commit
`ba500e11`), run through the lab of
`support/qualification/probes/schema_sql_readings.py` against PostgreSQL 17,
saved the same three plans apart from the plan name and the `from_fingerprint`,
which identify the database it read, and each Apply wrote its two schema apply
log entries. The v0.29.0 image, run through the same lab, saved the three plans
byte for byte as v0.29.2 did. The external lifecycle's security_invoker
row plans the same artifacts on kind; `TestViewInvokerPlannedReadsEachStepOfTheRow`
holds `viewInvokerPlanned` to accepting each for its own step and to refusing it
for the other two, the runner harness's plans of the same shapes over another
view, and copies with the one field each mistake would change.

`refused-log-mysql.json` is what the `migration-faults-mysql` phase's
`mysql-refused-migration-log` row printed on a local kind run on November 13,
2026, with Kubernetes 1.36.4, operator `3bbbcdd1`, the Ptah v0.26.0 release
image `ghcr.io/stokaro/ptah@sha256:247d115a9062ed785501ae05620acea1a176e72776f71b175a244fb056d2174f`
and `mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb`.
The migration ran under `apply: Always` against a database whose
`schema_migrations_log` was created first with a 16-character `actor` column, so
MySQL refused all six entries with error 1406 while the three migrations
applied. It is the status document the row's wait matched, the first that
carried the run's record, with the metadata that identifies it.
`TestEveryLogEntryLostRefusesEachClause` holds `everyLogEntryLost` to accepting
it and to refusing copies with the one field each mistake would change. It
names the lab namespace, the Job and the plan, and holds no credentials or SQL:
the only credential words are the key names of the registry Secret.

`drift-refused-postgresql.json`, `drift-decision-postgresql.json` and
`drift-checked-postgresql.json` are what the `migration-faults-postgresql`
phase's `postgresql-migration-drift-check` row printed on a local kind run on
November 14, 2026, with Kubernetes 1.36.4, operator `03d8dcbc` (a local build
of this change that left the phase's fault rows out), the Ptah v0.28.0 release
image
`ghcr.io/stokaro/ptah@sha256:34356290733b9c7ce526312b8ce89f753c85a04c9c187d71b4cbd63af0080977`
and `postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`.
The migration names a shadow database under `policy.driftCheck` and applied
versions 1 and 2. Version 3 was approved, and while its Apply was held a column
was added to the target by hand. The refused reading is the status the row's
wait matched once the Apply finished: the run `Failed` with no applied version
and no failed statement, and its message counts the difference, says the run
applied nothing and points at the Pod log; `Ready` and `Progressing` read
`False` with reason `SchemaDrifted`. The decision reading is the plan asking
for an approval again after the run, with the column dropped, and the checked
reading is the run under the same check that applied version 3 and returned
the resource to `InSync`. `TestDriftCheckRefusedRunRefusesEachClause`,
`TestDecisionAfterRunRefusesEachClause` and
`TestDriftCheckedRunAppliedRefusesEachClause` hold the row's predicates to
accepting them and to refusing copies with the one field each mistake would
change. They name the test namespace, the Job, the plan, and Secrets by name
and key, and hold no credentials or SQL.

`https-ca-pin-refused-schema.json` and `https-ca-pin-recovered-schema.json` are
what the `https-sources` phase's `https-ca-pin-refused` row printed on a local
kind run on November 15, 2026, with Kubernetes 1.36.4 on arm64, operator
`ae784f9f` (a local build of this change whose suite catalog ran that phase
alone) and the Ptah v0.29.0 release image
`ghcr.io/stokaro/ptah@sha256:1a33d8ef8b6f06226d6ebd202b8ef8620227d7d3346b66a5991afd04844e17e8`.
They are stored as the row printed them, one line each. The schema names a
credential Secret that holds `caSHA256` and grants the server under its cluster
domain name, and its source selects no CA. The refused reading is the status
the row's wait matched at generation 1: `ReconciliationFailed=True` with reason
`OperationFailed` and the runner's sentence under `invalid_https_access`,
`ArtifactResolved=False`, and no source or plan recorded. The recovered reading
is the status matched at generation 2, once the source selected the CA: the
plan waits for an approval, `ReconciliationFailed=False` with reason
`Succeeded`, and `ArtifactResolved=True` with `DownloadVerified`.
`TestHTTPSCAPinRefusedNeedsTheRunnersRefusalAtTheGeneration` and
`TestHTTPSCAPinClearedNeedsTheFailureClearedAtTheGeneration` hold the row's
predicates to accepting them, to refusing the other reading, and to refusing
copies with the one field each mistake would change. They name the test
namespace, a Resolve Job and the plan, and hold no credentials or SQL.

`git-ca-pin-refused-schema.json` and `git-ca-pin-recovered-schema.json` are
what the `git-sources` phase's `git-ca-pin-refused` row printed on a local kind
run on November 15, 2026, with Kubernetes 1.36.4 on arm64, operator `4c920d6c`
(a local build of this change whose suite catalog ran that phase alone) and the
Ptah v0.29.0 release image
`ghcr.io/stokaro/ptah@sha256:1a33d8ef8b6f06226d6ebd202b8ef8620227d7d3346b66a5991afd04844e17e8`.
They are stored as the row printed them, one line each. The schema names a
credential Secret that holds `caSHA256` and grants the Git server under its
cluster domain name, and its `https://` source selects no CA. The refused
reading is the status the row's wait matched at generation 1:
`ReconciliationFailed=True` with reason `OperationFailed` and the runner's
sentence under `invalid_git_access`, `ArtifactResolved=False` with
`ResolveFailed`, and no source or plan recorded. The recovered reading is the
status matched at generation 2, once the source selected the CA: the plan waits
for an approval, `ReconciliationFailed=False` with reason `Succeeded`, and
`ArtifactResolved=True` with `CommitResolved` and the commit main names.
`TestGitCAPinRefusedNeedsTheRunnersRefusalAtTheGeneration` and
`TestGitCAPinClearedNeedsTheFailureClearedAndTheCommitAtTheGeneration` hold the
row's predicates to accepting them, to refusing the other reading, and to
refusing copies with the one field each mistake would change. One of those
copies carries the message the schema reported on a kind run with the runner's
check removed, where Ptah ran and failed its TLS handshake. They name the test
namespace, a Resolve Job and the plan, and hold no credentials or SQL.
