---
title: API compatibility
description: What v1alpha1 guarantees, what the gates stop from happening by accident, and what to expect before v1.
---

The API group is `operator.ptah.run` and its only version is `v1alpha1`. Alpha
means here what it means in Kubernetes: there is no compatibility guarantee. A
change that breaks objects you have already stored can be made before `v1`, and
this page does not promise otherwise.

What the project does hold to is that such a change is a decision somebody
makes rather than something that slips in. Each rule below is enforced by a
check, and the check is named, so what you are reading is the code rather than
an intention.

## What cannot change by accident

These are refused when the generated CRDs are compared against the previous
commit, by `make verify-crd-schema-history`:

| Change | Why it is refused |
| --- | --- |
| A required field the new schema does not have | A stored object carries a value the new schema prunes on the next write, and the write then fails the requirement it no longer satisfies. |
| An enum that lost a value | Every stored object holding the removed value stops validating, and there is no migration for it. |
| A default that appeared, changed, or was taken away | It changes what a stored object reads back as, on objects nobody edited. |

The comparison is against the **storage** version of each kind, because that
is the schema an object in etcd is read back through. A kind the baseline does
not carry is new and has nothing stored to break.

Breaking any of them is still possible: it means editing that check, which
puts the consequence in front of whoever makes the change and into the diff of
whoever reviews it. That is the whole of it before `v1` — not that the API
will not move under you, but that it will not move quietly.

## What must move when the schema moves

Every shipped CRD carries `operator.ptah.run/crd-schema-version` and a digest
of its normalized spec. The same verifier requires the version to **strictly
increase** when the specs change, and to stay **exactly equal** when they do
not. A schema that changed without the version moving is refused, and so is a
version bumped without a change.

So the annotation is a fact about the bytes rather than a release number, and
every shipped kind carries the same one.

## What the operator refuses at runtime

The durable state and the release order carry versions of their own, and both
are fences rather than documentation.

**Durable state.** `operator.ptah.run/controller-state-version` says which
status a manager can interpret. A manager reads state stamped at its own
version or below and refuses anything above, at startup and before every CRD
update. Which version carries which state, and what each refusal looks like,
is [the controller-state contract](../releases/#controller-state-contract).

**Release order.** Each published chart advances an append-only rollout
sequence, and a candidate below the recorded active sequence is refused before
any CRD is touched. Upgrading is allowed to move forward and rolling back
below state that has already been written is not.

## The set of kinds cannot change quietly

The generated set is the schema family and the migration family, each with its
plan and its approval. The schema-history verifier refuses a set that added or
removed a kind, so one cannot appear or disappear without that refusal being
addressed deliberately.

## Before v1

There are no deprecations. A field that has to go, goes, in a release that
says so. Nothing is kept working for a window first, because a window is a
compatibility promise and `v1alpha1` does not make one.

There is no dated transition to `v1`. Until there is, the only way to stay
safe is to pin the chart version you deploy and read what a release changed
before you move to it. An upgrade can require you to edit your resources, and
[Release notes](../release-notes/) is where that is said, one entry per
version.
