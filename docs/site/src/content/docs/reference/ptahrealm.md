---
title: PtahRealm
description: Every field of the PtahRealm resource, generated from the API types.
---

`PtahRealm` is a cluster-scoped resource in `operator.ptah.run`, served as `v1alpha1`.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## Examples

A `PtahRealm` names one physical database at cluster scope and says which
namespaces may manage it. An administrator writes it; the operator reads it and
never writes it. A `PtahSchema` or `PtahMigration` joins the realm with
`spec.target.realmRef.name`, and the realm decides whether that claim stands.

A resource in a namespace the realm does not list is refused with reason
`RealmNotAuthorized` and runs nothing. It is also left out of the count the
listed resources see, so creating one cannot block them. The refusal reads the
same whether the realm does not exist, does not list the namespace, or names
another engine, so a tenant learns nothing about realms it may not use.

A database that only one namespace manages needs no realm: a
`spec.target.coordinationKey` names a realm inside its own namespace, and the
same key in another namespace is another realm. See
[One database, one manager](../../use/operations/#one-database-one-manager).

Only an administrator should be able to write a realm.
`examples/realm-administrator-role.yaml` is a ClusterRole for that, and none of
the author, approver or diagnostic roles beside it grants any access to realms.

### One database, one manager at a time

Two namespaces may manage the database, but not at once. A second claimant in
either namespace refuses both until one of them is deleted or suspended, which
is how a database moves from one team to another.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahRealm
metadata:
  name: production-orders-primary
spec:
  engine: PostgreSQL
  namespaces:
    - orders
    - orders-next
  sharing: Exclusive
```

### A database two teams share

`Shared` permits more than one claimant, and each claimant still has to set
`spec.target.sharedRealm: true`. One that has not refuses all of them. The
administrator allows the sharing and each team states that its resource manages
only part of the database; neither statement stands in for the other.

The [PtahMigration examples](../ptahmigration/#examples) show a resource that
names this realm.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahRealm
metadata:
  name: production-application-primary
spec:
  engine: PostgreSQL
  namespaces:
    - application
    - reporting
  sharing: Shared
```

### Who claims a realm

The realm carries no status, and a refused resource names only counts. An
administrator, who can read every namespace, finds the claimants of a realm by
its name:

```sh
kubectl get ptahschemas,ptahmigrations -A -o json |
  jq -r '.items[] | select(.spec.target.realmRef.name == "production-application-primary")
    | "\(.kind) \(.metadata.namespace)/\(.metadata.name) \(.status.phase)"'
```

## spec

| Field | Type | What it does |
| --- | --- | --- |
| `spec.engine` | `string`, required | Engine is the database family of this realm. A resource that names the realm with another engine is refused. It cannot change: a realm is one physical database, and a database does not change engine. |
| `spec.namespaces` | `[]string`, required | Namespaces are the namespaces whose resources may claim this realm, written out by name. There is deliberately no selector. Namespace labels are often writable by whoever administers the namespace, and a selector would let that person admit their own namespace to a database somebody else runs. Removing a namespace does not stop an operation already running there, the same as a conflict does not: the resource is refused at its next claim, before any Job. |
| `spec.sharing` | `string`, required, one of `Exclusive`, `Shared` | Sharing is whether more than one resource may manage the database at once. Exclusive admits one claimant. A second one, in any listed namespace, is a conflict that refuses every claimant, whatever each declared. Shared admits several, on the rule a namespace-local key has: every claimant sets spec.target.sharedRealm, and one that has not refuses them all. Sharing is a statement both the administrator and each claimant make; neither can make it for the other. |

## status

This resource declares no `status`.

