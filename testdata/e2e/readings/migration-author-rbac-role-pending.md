# Migration author RBAC refusal before the Role is seen

`migration-author-rbac-role-pending.txt` is the API server's refusal of the
first author CREATE in the PostgreSQL apply-policy guard scenario, as the typed
client returns it. It was captured from
[the Kubernetes 1.36 migrations-postgresql job of master run 44999887648](https://github.com/stokaro/ptah-operator/actions/runs/44999887648/job/137140885716)
on November 22, 2026, at commit `4a05740daa471003b29ae7c23ad62e407cdf0f51`.
The fixture removes the log timestamp and the test's own prefix; it preserves
the API server's error text.

The authorizer reads Roles and RoleBindings from separate caches. Here it had
seen the author's RoleBinding and not yet the Role the binding names, so it
added that the Role was not found to the refusal in
[`migration-author-rbac-denial.txt`](migration-author-rbac-denial.md). The
scenario retries this refusal as it retries that one, and only when the Role
not found is the author's own.
