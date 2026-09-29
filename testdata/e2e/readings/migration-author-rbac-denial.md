# Migration author RBAC refusal

`migration-author-rbac-denial.txt` is kubectl's refusal from the first author
CREATE in the MySQL apply-policy guard scenario. It was captured from
[PR #572's Kubernetes 1.36 job](https://github.com/stokaro/ptah-operator/actions/runs/36591988252/job/109491133191)
on September 29, 2026, at commit `5e55c08642cb2b4a942105aa23ecd71ddfb86a1b`.
The fixture removes the log timestamp and test-report prefix; it preserves the
API server's error text.

The test had installed the author Role and RoleBinding immediately before the
request. The refusal names RBAC authorization, before the admission-policy
assertions. The scenario now retries only this definite refusal of the initial
CREATE for up to 30 seconds. Persistent missing permissions still fail; policy
refusals, transport errors, and ambiguous write results are not retried. The
subsequent prohibited writes remain direct refusal assertions.
