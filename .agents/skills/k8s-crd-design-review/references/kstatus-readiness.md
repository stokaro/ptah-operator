# kstatus readiness compatibility

Use this when reviewing status/conditions for CRDs that should be legible to generic GitOps/status tools.

## What kstatus is

`kstatus` lives in `kubernetes-sigs/cli-utils` and computes a normalized status for Kubernetes resources:

- `Current`
- `InProgress`
- `Failed`
- `Terminating`
- `NotFound`
- `Unknown`

Current Go import path:

```go
import "sigs.k8s.io/cli-utils/pkg/kstatus/status"
```

The important design point: `status.Compute(resource)` computes from the single resource's own object/status. Do not expect it to inspect the whole dependency graph unless a higher-level poller/status reader adds that behavior.

Local source anchors:

- `docs/kubernetes-sig/cli-utils/pkg/kstatus/README.md`
- `docs/kubernetes-sig/cli-utils/pkg/kstatus/status/status.go`
- `docs/kubernetes-sig/cli-utils/pkg/kstatus/status/generic.go`

## Tooling reality

- Flux uses kstatus directly or through the Flux fork of `cli-utils` for health/status polling. For Flux users, kstatus-compatible resources are much easier to wait on and report as ready/current.
- Argo CD does not mean "every CRD automatically gets kstatus health"; its health docs say CRDs do not have one consistent status format, ask contributors to identify kstatus-formatted CRDs in custom health checks, and recommend using `observedGeneration` when present. For Argo CD users, kstatus-compatible resources make health checks easier to write, review, and eventually standardize.

So review CRDs as "kstatus-compatible and Argo/Flux-friendly", not as "universally ready by magic".

Useful upstream anchors:

- kstatus Conditions: https://github.com/kubernetes-sigs/cli-utils/tree/master/pkg/kstatus#conditions
- Helm HIP-0022, "Wait With kstatus": https://helm.sh/community/hips/hip-0022/
- Flux: `fluxcd/flux2/pkg/status/status.go` imports `github.com/fluxcd/cli-utils/pkg/kstatus/...`.
- Flux: `fluxcd/pkg/runtime/conditions/check/doc.go` describes status checks as mostly based on kstatus and expects `Ready` to be present in strict checks.
- Argo CD: `argoproj/argo-cd/docs/operator-manual/health.md` has "Using kstatus" and "Using K8s observedGeneration field" guidance.

## Why consumers care

Your operator's consumers are not only people reading YAML. They include GitOps controllers, CI pipelines, platform dashboards, alerting rules, support runbooks, and other controllers that need to answer the same boring question: "Is this object done, still progressing, or blocked?"

Without a conventional status contract, every consumer has to learn your CRD's private status language. That creates brittle Lua health checks, one-off scripts, dashboards that disagree with GitOps, and support tickets where nobody knows whether `Ready=False` means "still working" or "will never work without intervention".

kstatus-compatible conditions give consumers a small shared vocabulary:

- `Ready=True`: this resource's latest observed generation satisfies the API contract.
- `Reconciling=True`: work is still in progress.
- `Stalled=True`: progress is blocked or failing.
- `observedGeneration`: the status is fresh enough to trust for the current spec.

That makes the CRD usable in generic workflows:

- Flux can poll and aggregate readiness without custom logic for every CRD.
- Argo CD custom health checks can map your conditions to `Healthy`, `Progressing`, or `Degraded` predictably.
- Helm HIP-0022 proposes using kstatus for `helm install --wait` / `helm upgrade --wait` in Helm 4, specifically so waits can include custom resources with a `Ready` condition.
- `kubectl wait --for=condition=Ready` and printer columns tell the same story as GitOps health.
- CI/CD and release automation can wait for convergence instead of sleeping or scraping logs.
- Humans get a consistent reason/message trail when reconciliation is delayed or blocked.

## CRD design rules

### Do

- Add `status.observedGeneration` and update it after reconciling the current `metadata.generation`, even on failure.
- Set `observedGeneration` on conditions too when using the standard `metav1.Condition` shape.
- Use `status.conditions` as a map keyed by `type`.
- Include a stable aggregate `Ready` condition for long-running resources.
- Add `Reconciling` and `Stalled` when the resource has meaningful async progress or failure modes.
- Set conditions early, not only after success. `Ready=Unknown` or `Ready=False` is better than an absent `Ready` during initial reconciliation.
- Keep `reason` machine-readable CamelCase and `message` human-readable.
- Define `Ready=True` as "the latest observed generation satisfies this API's contract", not "the entire universe is healthy".

### Do not

- Do not rely on a single `status.phase` as the main status API for new CRDs.
- Do not rename `phase` to `state` and keep the same single-state-machine design; conditions should remain the primary readiness contract.
- Do not create `Ready` only when it becomes `True`; kstatus may treat unknown resources without known conditions as current.
- Do not make `Ready=False` carry all meanings. Pair it with `Reconciling=True` for active work or `Stalled=True` for blocked/broken progress.
- Do not claim downstream child/resource health unless this CRD owns and observes that promise.
- Do not put controller-owned observations such as last commit, last handled resource version, resolved UID, or last sync time in `spec`.

## Recommended condition semantics

Use this shape for long-running reconciled resources:

```yaml
status:
  observedGeneration: 7
  conditions:
    - type: Ready
      status: "False"
      observedGeneration: 7
      reason: ReconciliationInProgress
      message: Waiting for the controller to finish processing generation 7.
      lastTransitionTime: "2026-06-27T12:00:00Z"
    - type: Reconciling
      status: "True"
      observedGeneration: 7
      reason: NewGeneration
      message: The latest generation is being reconciled.
      lastTransitionTime: "2026-06-27T12:00:00Z"
    - type: Stalled
      status: "False"
      observedGeneration: 7
      reason: Progressing
      message: Reconciliation is making progress.
      lastTransitionTime: "2026-06-27T12:00:00Z"
```

For a blocked state:

```yaml
status:
  observedGeneration: 7
  conditions:
    - type: Ready
      status: "False"
      observedGeneration: 7
      reason: AuthenticationFailed
      message: Unable to authenticate to the target system.
      lastTransitionTime: "2026-06-27T12:05:00Z"
    - type: Reconciling
      status: "False"
      observedGeneration: 7
      reason: ReconciliationStopped
      message: Reconciliation cannot continue until credentials are fixed.
      lastTransitionTime: "2026-06-27T12:05:00Z"
    - type: Stalled
      status: "True"
      observedGeneration: 7
      reason: AuthenticationFailed
      message: Unable to authenticate to the target system.
      lastTransitionTime: "2026-06-27T12:05:00Z"
```

## Review shortcut

Ask whether these three generic reads tell the truth:

- Stale status: `metadata.generation != status.observedGeneration`, so do not trust `Ready=True` for the latest spec yet.
- Not done yet: `Ready=False`, `Reconciling=True`, `Stalled=False`
- Done/current: `Ready=True`, `Reconciling=False`, `Stalled=False`
- Blocked/broken: `Ready=False`, `Reconciling=False`, `Stalled=True`

If they do not, the status contract is probably too bespoke for generic tooling.
