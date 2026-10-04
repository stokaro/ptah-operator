The JSON beside this file preserves status fields from CI run
[36675116019](https://github.com/stokaro/ptah-operator/actions/runs/36675116019),
head `2af63c30d4380d56c900453034b3559aaf960135`:

- [MySQL on Kubernetes 1.35](https://github.com/stokaro/ptah-operator/actions/runs/36675116019/job/109759602202).
- [PostgreSQL on Kubernetes 1.37](https://github.com/stokaro/ptah-operator/actions/runs/36675116019/job/109759602385).

Both jobs failed on the assertion that an unknown Apply must immediately have
no active operation after its Job disappears. Background garbage collection
can leave the owned Pod running. The operator preserves the original claim,
deadlines and realm until that workload stops.

The fixture retains the native phase, conditions, active operation, last run
and unresolved run without rewriting their values. The diagnostic projection's
name becomes `metadata.name`. Other diagnostic fields are omitted. No Pod
reading is claimed by this fixture; the unit test supplies owned running and
terminal Pods to distinguish retention from premature retirement.
