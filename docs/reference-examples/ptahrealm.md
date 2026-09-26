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

A listed name is a grant to whoever holds a namespace of that name, now or
later. Remove a namespace from every realm before deleting it, and where
tenants choose their own namespace names, do not list one that does not exist
yet.

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
