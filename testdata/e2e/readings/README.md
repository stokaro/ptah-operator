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
