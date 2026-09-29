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
| A field that goes from optional to required, or a new field that arrives already required, without a default | A stored object that never set the field fails the requirement on its next write. A default rescues this: structural defaulting fills an absent field from its schema default before anything validates it, on every decode, so a field that carries one is exempt. |
| An enum that lost a value | Every stored object holding the removed value stops validating, and there is no migration for it. |
| A numeric or length bound that tightened — a minimum that rose, a maximum that fell, a bound that newly appeared, or one that turned exclusive at the same value | A stored value the old bound allowed and the new one refuses fails on the same next write. |
| A pattern that changed, in either direction | Refused conservatively: proving a changed regular expression only ever widens is its own project, so any change is treated as tightening unless a declared break says otherwise. |
| A new or changed `x-kubernetes-validations` rule on a field that already existed | The rule runs against the stored value the first time anything writes the object, whether or not the write touches that field. |
| A changed `x-kubernetes-list-type` or `x-kubernetes-map-type` | Both are enforced against whatever the field already holds, not just against what a write changes: a list that was fine as `atomic` can hold entries a `set` or `map` list-type refuses. |
| A default that appeared, changed, or was taken away | It changes what a stored object reads back as, on objects nobody edited. |

A field the new schema no longer declares is **not** refused. Structural
pruning drops a value for any property the current schema does not carry when
it decodes a stored object, required or not, so removing a field — and
whatever required-ness it had — reaches no object already in etcd. An earlier
version of this check refused a removed required field anyway, on a claim
about a decode order that pruning code in `k8s.io/apiextensions-apiserver`
did not bear out; two changes that tightened validation the other way,
[stokaro/ptah-operator#470](https://github.com/stokaro/ptah-operator/pull/470)
and [#482](https://github.com/stokaro/ptah-operator/pull/482), went
unrefused because the check was watching the harmless direction.

The comparison is against the **storage** version of each kind, because that
is the schema an object in etcd is read back through. A kind the baseline does
not carry is new and has nothing stored to break.

Breaking any of them is still possible: it means editing that check, which
puts the consequence in front of whoever makes the change and into the diff of
whoever reviews it. The edit is a declared break in
`hack/crdschemahistory/compatibility.go`: it names the schema version that
makes the break, why, and every refusal it excuses, word for word. It excuses
nothing in any other version, and it is refused itself if it names a refusal
the change does not produce. That is the whole of it before `v1` — not that
the API will not move under you, but that it will not move quietly.

## What must move when the schema moves

Every shipped CRD carries `operator.ptah.run/crd-schema-version` and a digest
of its normalized spec. The same verifier requires the version to **strictly
increase** when the specs change, and to stay **exactly equal** when they do
not. A schema that changed without the version moving is refused, and so is a
version bumped without a change.

So the annotation is a fact about the bytes rather than a release number, and
every shipped kind carries the same one.

## Contract counters

The schema version is one of several numbers that version something a release
stores in a cluster or speaks to another component. Each is written down in
code and held there by a check:

| Counter | Where it is declared | What it versions |
| --- | --- | --- |
| CRD schema version | `CRD_SCHEMA_VERSION` in the `Makefile` and `CurrentCRDSchemaVersion` in `internal/crdupgrade`, stamped on every CRD as `operator.ptah.run/crd-schema-version` | The generated schemas, held to the previous commit as described above. |
| Controller-state version | `CONTROLLER_STATE_VERSION` in the `Makefile` and `controllerstate.CurrentVersion`, stamped on every CRD and both webhook configurations as `operator.ptah.run/controller-state-version` | The durable status a manager can read; see [the controller-state contract](../releases/#controller-state-contract). |
| Plan contract | `fingerprint.CurrentPlanContractVersion`, which the `PtahSchemaPlan` `spec.contractVersion` enum and the plan write guard repeat | What a schema plan's fingerprint binds, and so what an approval names. |
| Migration plan contract | `migrationplan.ContractVersion`, which the `PtahMigrationPlan` `spec.contractVersion` bound repeats | The same for a migration plan. |
| Realm digest contract | `coordinationContractVersion` in `internal/fingerprint` | How a coordination key or a `PtahRealm` becomes the digest that names a database. |
| Runner protocol | `runner.ProtocolVersion`, recorded in `support/runner-protocol.json` and in the `edge` row of `support/ptah.json` | What the runner accepts, enforces and returns. `hack/verifyrunnerprotocol` holds it to the runner's source. |
| Drift vocabulary | `dataplane.DriftFindingVocabularyVersion` | The closed set of drift finding categories. |
| Admission contract | `CurrentAdmissionContractVersion` in `internal/crdupgrade`, stamped on both webhook configurations as `operator.ptah.run/admission-contract-version` | The admission configuration every release serves. |
| Certificate staging format | `certrotation.StagingFormat`, the `format` key of the rotator's staging Secret | The record a certificate rotation resumes from after a restart. |
| Admission snapshot format | `podintent.SnapshotVersion`, which the `admissionSnapshot.version` enum in an operation's status repeats | The Pod admission rules an operation's snapshot was taken under. |

Every counter starts at 1 for the first release candidate. Before that
release they counted commits, and no release could hold version 32 of
anything, so they were all reset to 1 during release preparation. The
schema-history check compares each change with the commit before it, and would read 32 to 1 as a rollback. That one restart is
recorded in `hack/crdschemahistory/restart.go`: it leaves only the schemas it
names, by version and by the digest of every CRD, it arrives at version 1 and
at nothing else, and it excuses only the stored-object transitions it lists.
Any other move below the baseline is refused as a rollback, and every change
after it is held to the rules above.

From the first release candidate on, a counter moves only when something a
tagged release could have stored or spoken changes. A change that leaves every stored and spoken
form as it was keeps its number: the runner protocol records such a change in
`support/runner-protocol.json` with its reason instead of moving. The CRD
schema version is the strictest of them, because its check compares every
change with the commit before it: any change to a generated schema moves it.

## What the operator refuses at runtime

The durable state and the release order carry versions of their own, and both
are fences rather than documentation.

**Durable state.** `operator.ptah.run/controller-state-version` says which
status a manager can interpret. A manager reads state stamped at its own
version or below and refuses anything above, at startup and before every CRD
update. Which version carries which state, and what each refusal looks like,
is [the controller-state contract](../releases/#controller-state-contract).

**Chart and image pairing.** The reconcile hook refuses a chart whose
controller-state version its own manager image does not compile, before it
reads or changes anything. That is what stops a `--reuse-values` upgrade that
keeps an old image under a new chart, or an old chart paired with a new image.
`helm rollback` renders nothing: it runs the CRD hook of the release it
returns to, and that hook refuses stored state newer than that release reads.
Upgrading is allowed to move forward and rolling back below state that has
already been written is not.

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
