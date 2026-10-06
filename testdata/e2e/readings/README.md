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
