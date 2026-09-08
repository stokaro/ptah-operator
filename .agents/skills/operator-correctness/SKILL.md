---
name: operator-correctness
description: Use when writing or reviewing reconciler, webhook, finalizer, ownership, or controller test code in this operator — the reconcile return contract, requeue semantics, what a status write does to events, cached versus direct reads, conflict handling, and what envtest can and cannot prove. Read it before changing anything under internal/controller or test/e2e.
---

# Operator correctness

The API-design skills beside this one stop at the CRD. This one covers what
happens after: the reconcile loop, the objects it owns, and the tests that are
allowed to claim it works.

Everything below is measured against the versions this repository pins in
`go.mod` — `sigs.k8s.io/controller-runtime v0.24.1`, `k8s.io/api v0.36.1`. When
those move, re-read the source rather than this file: the whole point of the
facts here is that they were read rather than remembered.

## The reconcile return contract

`Reconcile` returns `(ctrl.Result, error)`, and the two are not combined the way
most guidance on the internet says.

**A non-nil error requeues by itself.** In `pkg/internal/controller/controller.go`
the error branch adds the request back to the queue with the rate limiter and
never reads `result`:

```go
switch {
case err != nil:
    if errors.Is(err, reconcile.TerminalError(nil)) { /* counted, not requeued */ }
    else { c.Queue.AddWithOpts(priorityqueue.AddOpts{RateLimited: true, ...}, req) }
```

So `return ctrl.Result{}, err` is the correct shape. Setting `Requeue: true`
beside an error adds nothing.

**`Requeue` is deprecated.** In `pkg/reconcile/reconcile.go` the field carries
`Deprecated: Use RequeueAfter instead`, with the reason stated: it produced an
interval from a rate limiter whose job is retry-on-error, which is not what a
caller waiting for an external event wants. Use `RequeueAfter` with a duration
you can justify, or return an error.

**`TerminalError` is the one thing that stops the retry.** Wrap an error in
`reconcile.TerminalError` when retrying cannot help — a malformed spec the user
must edit, a permanent rejection from an external system. Anything else will be
retried forever, which is usually right.

Review questions:

- Does any `return` pair a non-zero `Result` with a non-nil error? Drop the
  result.
- Is a failure that no retry can fix returned as an ordinary error? It will spin.
- Is `RequeueAfter` a number someone chose, or a number someone can explain?

## A status write still produces an event

The claim that a status subresource prevents a reconcile loop is a half-truth
worth getting right, because it decides whether the controller spins.

What the subresource does: a write to `/status` does not increment
`metadata.generation`. What it does not do: stop the update event. The watch
still fires, and `Reconcile` still runs.

The skip comes from a predicate. `pkg/predicate/predicate.go` says it in the
type's own words — `subresource status update will not increase Generation`, and
`GenerationChangedPredicate` `will skip update events that have no change in the
object's metadata.generation field`.

Two caveats it also states, both easy to be bitten by:

- generation is not spec-only for every kind. A `Deployment` increments it on
  writes to `metadata.annotations` too, so a controller watching Deployments
  cannot read a generation bump as "the spec changed";
- with the predicate on, a status block someone else overwrites is never
  restored, because the controller does not see the event.

Review questions:

- Does the controller write status on every pass, including when nothing
  changed? That is a self-inflicted event even with the subresource.
- Where a `GenerationChangedPredicate` is used, is the watched kind one whose
  generation really tracks the spec?
- Is `observedGeneration` set on the same write as the conditions it explains?

## Reads: cache, conflicts, and what the client actually returned

The manager's client reads from a cache. That is what makes a controller cheap
and what makes two of its failure modes possible.

- **A cached read can be stale.** A `Get` immediately after a write may return
  the value from before it. Code that writes and then reads back to confirm is
  asserting against a cache, not against the API server.
- **An update carries the `resourceVersion` it read.** A conflict means somebody
  else wrote in between, and the correct answer is to re-read and retry rather
  than to force. `apierrors.IsConflict` is a routine outcome, not an incident.
- **Reading the whole object to change one field is how a controller clobbers
  another writer.** Prefer a patch, and prefer Server-Side Apply where the field
  ownership matters.

Review questions:

- Does an error path distinguish `IsNotFound` from a real failure? A missing
  object is usually a normal terminal state for that pass.
- Does anything escape the cache with a direct reader, and is the reason written
  down beside it?
- After a conflict, does the code re-read, or does it retry with the same stale
  object?

## Ownership, finalizers, deletion

- An owner reference is what makes garbage collection delete the child. It is
  set on the child, points at the owner, and requires the same namespace for a
  namespaced owner.
- A finalizer is a promise the controller can keep. Adding one means every path
  that can remove the external thing must also remove the finalizer, including
  the path where the external thing is already gone.
- External cleanup has to be repeatable. A finalizer runs again after a restart
  in the middle, so "delete the remote object" must tolerate the object being
  absent.

Review questions:

- If the process dies between the external delete and the finalizer removal,
  what happens on the next pass?
- Is the finalizer removed on the path where the referenced object was never
  created?
- Does anything rely on a child being deleted that carries no owner reference?

## What a test proves

- **envtest runs the API server and etcd, and no built-in controllers.** Nothing
  reconciles a Deployment into Pods, nothing garbage-collects by owner
  reference, nothing evicts. A green envtest is evidence about the API contract
  and about this controller's own logic — not about anything Kubernetes would
  have done.
- **A real cluster is what proves the rest.** The e2e suite in `test/e2e` runs
  against kind for exactly this reason: ownership-driven deletion, workload
  rollout, admission, and certificate rotation only happen where the controllers
  that do them are running.
- **A test that waits for a condition should fail loudly on the timeout**, naming
  what it waited for and what it saw. A bare "timed out" costs the next person
  the whole investigation.

Review questions:

- Does this test claim a lifecycle property envtest cannot produce?
- When it polls, does the failure say which object and which field disagreed?
- Does the e2e leave the cluster inspectable on failure, or delete the evidence?

## What this repository has already been bitten by

The commit log is the honest source for this section; each of these is a shape
that reached master and had to be fixed, so a change touching the same area is
worth reading them for:

- a runtime `Deployment` restored without the manager that owned it;
- a converged candidate stopped on a repeated upgrade that should have left it
  running;
- a webhook that rejected a `Pod` carrying no `ownerReferences`;
- a certificate rotation whose canary call answered after the API server had
  stopped waiting;
- e2e adversaries that read `kubectl`'s exit status instead of the API's answer.

Every one is a reconcile-time or admission-time behavior that no CRD schema
could have caught.
