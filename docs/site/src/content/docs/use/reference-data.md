---
title: Manage reference data
description: What a declared row set owns, how a data-only change reaches the database, and why ending management is not deleting rows.
---

Some tables hold rows the application reads rather than writes: country codes,
currencies, plan tiers, the states a workflow may be in. They belong to the
schema in the same way a column does, and they change on the same schedule.

Ptah lets you declare those rows beside the tables that hold them, in the same
source the schema comes from. The operator reconciles them through the same
`PtahSchema` that reconciles the structure. There is no second resource, no
second artifact and no second approval path.

This is the declarative workflow. If the rows change through a sequence
somebody wrote by hand, that is a
[versioned migration](../migrations/) instead.

## Declaring a row set

A declaration names the table, the column that identifies a row, and the file
the rows live in:

```go
//ptah:schema:table name="regions"
//ptah:schema:data table="regions" key="code" file="regions.yaml"
type Region struct {
	//ptah:schema:field name="code" type="VARCHAR(8)" primary="true"
	Code string

	//ptah:schema:field name="name" type="VARCHAR(64)" not_null="true"
	Name string
}
```

The file is a list of rows, each one a map from column name to value:

```yaml
- code: emea
  name: Europe, Middle East and Africa
- code: amer
  name: Americas
```

`ptah schema push` puts both into the artifact, so the rows are covered by the
digest your `PtahSchema` resolves and your verification policy checks. Nothing
later reads the author's working copy: the artifact carries the declaration.

## What a change looks like

The cycle is the one a structural change goes through, and the steps do not
change when only rows differ:

```
Resolve   the artifact reference to a digest
Verify    that digest against the verification policy
Observe   the structure and the declared rows in the database
Plan      the difference, and publish it
Approve   that exact plan, if the policy asks for one
Apply     the approved statements
Verify    the structure and the rows again
```

An empty schema diff is not a reason to skip the rest. A revision that changes
one value and no column still produces a plan with statements in it, still
takes an approval if your policy requires one, and still has to be verified
before the resource reports `InSync`.

A reconciliation that finds nothing to change issues no statements at all. The
resource cycles through its interval and stays converged.

## What the declaration owns

The declaration owns the rows of the table it names, identified by `key`. What
the operator reconciles is the set of rows in the file: a row the file adds is
inserted, a row whose declared columns disagree with the database is updated
back to the declaration.

A value edited outside the operator is drift, not a new declaration. The next
observation sees it, plans the difference, and the apply puts the declared
value back.

That also settles what an approval means for a data change. The plan records
the state the rows were observed in, so an approval naming a plan computed
before the edit no longer applies once the edit lands. The admission webhook
refuses it. Approve the plan that was computed after the change, and it
applies.

## Ending management is not deleting rows

Removing the `//ptah:schema:data` line stops the operator reconciling that
table. It does not remove what is there. The rows stay exactly as they were,
and the table keeps them until something else removes them.

Deleting managed rows is a change you declare and approve, the same as any
other.

## Protecting a table

Some tables should not change from the declarative path at all: a rate card a
finance team owns, a lookup someone maintains by hand, a table whose rows are
audited elsewhere. Name it, and any plan that would change its rows is refused:

```yaml
spec:
  policy:
    protectedTables:
      - countries
      - ref.regions
```

An entry is a table, or a schema and a table, as the declaration names it, and
matching is case-insensitive. An entry on a table the artifact already agrees
with refuses nothing, so an entry can sit in a policy permanently.

An approval, `allowDestructive` and a permissive `driftSeverity` cannot
override the refusal. The resource reports `Ready=False` with reason
`ProtectedTable`, publishes no plan, and leaves the rows as they are; it is
blocked rather than failed, and stays blocked until the policy or the artifact
changes.

Where the change is wanted, the policy is what changes: remove the entry, or
write the rows as a migration, which is the path that asks a person for
`--allow-prod`. Editing the list also invalidates any plan already waiting for
approval, because a plan carries the policy it was computed under.

## Rows do not leave the database

No declared value reaches the resource's status, an Event, or the controller's
log. Reading the rows means reading the artifact, querying the database, or
reading the plan.

The plan is the exception, and deliberately so: `kubectl ptah plan` prints the
statements a plan holds, and for a data change those statements carry the
values. Access to a plan is access to data. Treat it that way when you decide
who may read plans.

That includes the Plan Pod's log. The runner hands the plan to the controller
through it, so the values are in that log, on the node, and in any log store
the cluster ships container logs to.
[Pod logs carry plans](../security/#pod-logs-carry-plans) says who should be
able to read it and how to keep it out of shared stores.

## Order between tables

One limitation is worth planning around. Ptah emits declared rows grouped by
table, in an order that does not follow the dependency order it uses for the
tables themselves
([stokaro/ptah#3252](https://github.com/stokaro/ptah/issues/3252)). A child row
can therefore be offered before the parent row it references, and the foreign
key refuses it.

Where a foreign key crosses two declared row sets, declare the parent's rows
first and add the child's in a later revision. The tables can be created
together; it is the rows that need the two steps.
