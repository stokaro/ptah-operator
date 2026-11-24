---
title: Credentials and admission
description: Which process holds which credential, and what the admission contract refuses.
---

The rule the rest of the operator is built around is that the controller
never touches a database and the process that touches a database never holds
a Kubernetes credential. This page is that rule in detail, and the admission
contract that keeps it true. The operational view is
[Security model](../../use/security/).

## Credential routing

| Operation | Registry credential | Database credential | Desired input |
| --- | --- | --- | --- |
| Resolve | yes | no | requested OCI reference |
| Verify | yes | no | requested reference plus resolved digest evidence |
| Observe/Plan authority guard | authority and transport grants only | no | digest-pinned reference plus optional CA source bytes |
| Observe fetch init | yes | no | digest-pinned schema artifact plus optional read-only CA snapshot |
| Observe main | no | target only | local read-only schema file |
| Plan fetch init | yes | no | digest-pinned schema artifact plus optional read-only CA snapshot |
| Plan main | no | target and optional dev target | local schema file |
| Apply | no | target only | the plan, projected from its chunks into immutable ConfigMaps |
| Migration Resolve | yes | no | requested OCI reference |
| Migration Verify | yes | no | requested reference plus resolved digest evidence |
| Migration History/Apply authority guard | authority and transport grants only | no | digest-pinned reference plus optional CA source bytes |
| Migration History/Apply fetch init | yes | no | digest-pinned migration directory |
| Migration History main | no | target only | local migration directory |
| Migration Apply main | no | target only | local migration directory |
| Snapshot copy init, ConfigMap source | no | no | the source snapshot ConfigMap, checked against the bound content digest |
| Resolve main, Git source | Git credential and its grants | no | the repository, ref and path, and the optional CA or known hosts |
| Git fetch init, Git source | Git credential and its grants | no | the bound commit, checked against the bound content digest |
| Resolve main, HTTPS source | HTTPS credential and its grants | no | the URL, held to the pinned download digest, and the optional CA |
| HTTPS fetch init, HTTPS source | HTTPS credential and its grants | no | the bound download, held to its digest and to the bound content digest |

The controller sees only the names of Secrets and their keys, and of
SecretProviderClasses and their objects. The kubelet reads the Secrets for the
Job's Pod, and the Secrets Store CSI driver reads a store for it. A migration
reads a local directory rather than an
OCI reference for the same reason the table splits every row: passing
`oci://…` to the process that runs SQL would put registry and database
credentials in one process.

### Credentials are files {#credential-files}

Every credential reaches the container that needs it as a file in a read-only
Secret volume, mounted in that container and no other. A database URL from
[a SecretProviderClass](#secret-provider-class) is a file in a read-only CSI
volume instead, at the same path and named by the same variable:

| Credential | File | Variable naming the file |
| --- | --- | --- |
| Database URL | `/credentials/database/url` | `PTAH_OPERATOR_DB_URL_FILE` |
| Development database URL, Plan only | `/credentials/development-database/url` | `PTAH_OPERATOR_DEV_URL_FILE` |
| Shadow database URL, a migration's History and Apply under a [drift check](../../use/migrations/#drift-check) | `/credentials/shadow-database/url` | `PTAH_OPERATOR_SHADOW_DB_URL_FILE` |
| Registry username, password and token, `Environment` mode | `/credentials/registry/username`, `password`, `token` | `PTAH_OPERATOR_OCI_USERNAME_FILE`, `_PASSWORD_FILE`, `_TOKEN_FILE` |
| Git username and password, `https://` | `/credentials/git/username`, `password` | `PTAH_OPERATOR_GIT_USERNAME_FILE`, `_PASSWORD_FILE` |
| HTTPS username and password, or token | `/credentials/https/username`, `password`, `token` | `PTAH_OPERATOR_HTTPS_USERNAME_FILE`, `_PASSWORD_FILE`, `_TOKEN_FILE` |

The container's environment holds the path and never the credential. The runner
reads each file before anything else uses the value, so the value it redacts from
everything it writes is the value it read. It hands the value to Ptah in the
environment of the one child process it starts: Ptah `v0.35.0`
takes the database URL from `--db-url` or `PTAH_DB_URL` and a registry, Git or
HTTPS credential from its environment, and nothing else, and a command line is visible
to every process in the Pod. The OCI fetch init container runs Ptah's pull through
the runner for the same reason, and the runner refuses any command there but the
`schema pull` or `migrations pull` the manager built. A Docker config file and an
SSH key were files before and are unchanged.

A value the kubelet puts into a container's environment stays in the container
runtime's record of that container until the Pod is removed, for anyone with root
on the node to read, and a failed Apply's Pod is kept a day. A Secret volume is a
memory-backed file system that the kubelet unmounts once the Pod's containers have
stopped, though the Pod is kept. While the
Pod runs, root on the node can still read the file and the environment of the
Ptah process.

Every file of a Secret volume is mode `0440`, readable by root and by group
65532, the Pod's `fsGroup`, and by nobody else. With an `fsGroup` the kubelet gives each file of a
Secret volume to that group with group read, whatever mode the Job declares, so a
declared `0400` reads back as `0440`. Owning the file by the operation's user takes
the `defaultUser` and `user` volume fields, which need the alpha
`AtomicWriteVolumeUserFields` feature gate in every Kubernetes minor the operator
supports and which the Job admission guard refuses. A file a SecretProviderClass
writes is `0644` and owned by root, as
[What the class writes](#secret-provider-class-objects) says.

The registry, Git and HTTPS grants stay in the environment: the `registry`,
`allowPlainHTTP`, `caSHA256`, `host` and `knownHostsSHA256` keys of a credential
Secret reach the runner through a `secretKeyRef`. They say where a credential may
be sent and hold none, and a grant the Secret lacks stops the container with
`CreateContainerConfigError`, as before. The one optional grant is the
`caSHA256` of a registry credential over HTTPS, or of an `https://` Git or
HTTPS credential, under a resource that selects no CA: it reaches the runner
only when the Secret holds it, and the runner then refuses the source rather
than send the credential under the system roots. A registry credential over
plain HTTP carries no `caSHA256` grant.

A volume with a key the Secret may lack is optional, as the selector was: a
registry credential's three keys, a Git credential's two, an HTTPS credential's
three, and a `urlFrom` that sets `optional`. The kubelet then writes no file for that key, the runner leaves the
variable unset, and whatever needs it refuses as it did when the kubelet left an
optional variable unset.

A resource whose source is a ConfigMap has no Resolve or Verify Job and no
registry credential anywhere: the manager reads the ConfigMap and writes the
snapshot, and the Observe, Plan, History and migration Apply Pods replace the
authority guard and the fetch with one init container that checks the
snapshot and copies it into the same local file or directory.

A resource whose source is Git has a Resolve Job and no Verify Job. Its
credential reaches two places and no other: the Resolve Job's operation
container, which holds no database credential, and the Git fetch init
container that replaces the authority guard and the fetch in every later
operation. The runner in either one refuses a credential unless the Secret's
`host` key names the repository's authority, and a CA bundle or known-hosts
file unless the Secret holds its digest, before Ptah starts; Ptah reads private
copies of the trust files and never sees the grants. The container that holds
the database URL mounts only the fetched files, read-only.

A resource whose source is HTTPS has the same shape: a Resolve Job and no
Verify Job, its credential and CA in the Resolve Job's operation container and
in the `fetch-https-source` init container of every later operation, and
nowhere else. The runner in either one refuses a credential unless the
Secret's `host` key names the URL's authority, a CA bundle unless the Secret
holds its digest, and a credential that is incomplete or mixes a token with a
user name or a password, before Ptah starts.

For Resolve and Verify, the runner replaces a mounted custom CA ConfigMap
projection with a private snapshot before starting Ptah. For Observe and Plan,
only the credential-free guard mounts that projection; it validates the fixed
Secret-owned digest grant and copies exact bytes to a dedicated EmptyDir before
the credentialed fetch starts.

The dev database is a scratch database Ptah may use for a comparison. It
reaches `Plan` and nothing else — never Observe, never Apply, never any
migration operation — and the runner redacts it alongside the target.

## Credentials from a SecretProviderClass {#secret-provider-class}

`urlFromSecretProviderClass` reads a database URL from a store with no
Kubernetes Secret in between. It names a `SecretProviderClass` of the
[Secrets Store CSI driver](https://secrets-store-csi-driver.sigs.k8s.io/) in
the resource's namespace, and the object the class writes the URL as. A target,
a `PtahSchema`'s `spec.dev` and a `PtahMigration`'s
`spec.policy.driftCheck.shadow` each name exactly one of `urlFrom` and
`urlFromSecretProviderClass`:

```yaml
spec:
  target:
    engine: PostgreSQL
    coordinationKey: production/orders-primary
    urlFromSecretProviderClass:
      name: orders-database
      objectName: url
  execution:
    serviceAccountName: orders-operations
```

The container that runs Ptah mounts the class as its URL volume: a read-only
CSI volume of the driver `secrets-store.csi.k8s.io` whose one attribute is the
class's name, at the path [the URL file](#credential-files) always has. When
the Pod starts, the kubelet asks the node's driver to mount the volume, the
driver gives the class and a token for the Pod's ServiceAccount to the class's
provider, and the provider reads the store and writes each object the class
lists as a file. The runner reads the file as it reads one from a Secret. The
URL reaches that container and no other. The manager reads neither the class,
the store nor the file, and has no permission to: a class that is missing
reaches the resource the way a missing Secret does, as a
[`PodConfigurationError`](#external-secret-stores-not-synced) that carries the
driver's words. [Admission](#admission) holds every operation Pod to the volume
its Job declares, so neither a webhook nor another writer can point it at
another class or another driver.

The operator names a class and an object and nothing else, so every provider
gets the same volume. How the provider authenticates, Vault's Kubernetes auth
method or a cloud provider's workload identity, is between the provider and the
Pod's ServiceAccount. Only Vault has been run, as
[What was run](#secret-provider-class-measured) says, and this page names a
provider only after a run against it.

### The account the store trusts {#secret-provider-class-account}

The provider authenticates as the Pod's ServiceAccount, so the account the
operation Pods run as becomes an identity the store trusts. A resource that
names a class names that account in `spec.execution.serviceAccountName`, and
the API refuses one that leaves it unset or names `default`:

```text
a URL from a SecretProviderClass needs execution.serviceAccountName to name an account other than default
```

Give that account to the operation Pods and to nothing else. Whoever may create
a Pod as it in the namespace can read the URL. That is the reach the namespace
already gives over a Secret, under
[the release namespace rule](../../use/security/#release-namespace), but the
account is now one a store trusts.

Bind the store's role to an audience that no token in the Pod carries. Every
operation Pod projects a token for the result receiver, with the audience
`operator.ptah.run/results`, and a Vault role that binds no audience accepts
it: in the run, a Pod running as the operation account logged in to Vault with
that token. With `audience=vault` on the role and the same audience in the
driver's `tokenRequests`, the result token and a token for the API server were
both refused, and the volume still mounted. The Pod itself mounts no token:
the kubelet requests the driver's token and hands it to the driver on the
node.

```sh
helm upgrade --install csi-secrets-store \
  secrets-store-csi-driver/secrets-store-csi-driver \
  --namespace kube-system --set 'tokenRequests[0].audience=vault'
vault write auth/kubernetes/role/orders-operations \
  bound_service_account_names=orders-operations \
  bound_service_account_namespaces=application \
  policies=orders-database audience=vault ttl=10m
```

```yaml
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: orders-database
  namespace: application
spec:
  provider: vault
  parameters:
    vaultAddress: http://vault.vault.svc:8200
    roleName: orders-operations
    audience: vault
    objects: |
      - objectName: url
        secretPath: secret/data/ptah/orders
        secretKey: url
```

The operator cannot read the role or the driver's settings, so nothing it
reports says that an audience is missing.

### What the class writes {#secret-provider-class-objects}

The URL is one object, a complete URL, as it is in a Secret: any reserved
character in the password percent-encoded, and no trailing newline. Neither the
driver nor the Vault provider templates a value, so keep the URL in the store as
one field. A store that holds a user name and a password as two fields gives the
Pod two files and no URL.

`objectName` is the file the class writes the URL to: the object's `objectName`
in the class, or its `objectAlias` where the class sets one. The API holds it to
one file name. A name the class does not write still mounts, and leaves no file:

| URL | What the operation reports |
| --- | --- |
| The target's | `missing_target: the operation found no file "url" in the volume of SecretProviderClass "orders-database". Check that the class writes an object under that name, as its objectName or its objectAlias` |
| A drift check's shadow | The history read publishes no plan, with reason `ShadowDatabaseUnavailable`: `missing_shadow: the SecretProviderClass volume holds no shadow database file. Check the URL that SecretProviderClass "orders-shadow-database" writes as object "url"`, and why the shadow has to be named |
| A `PtahSchema`'s development database | Nothing. The Plan runs as it does with no development database, so check that the class writes the object before you rely on it |

The driver writes every object a class lists into every volume that mounts the
class. A class that lists the target's URL beside a development or a shadow URL
puts both files in each of those volumes, so an operation that reads one of
them, such as a `PtahSchema`'s Observe, which reads no development database,
holds the other too. Give each URL a class of its own.

The file is mode `0644` and owned by root, not the `0440` and group 65532 of a
Secret volume's file. The driver declares `fsGroupPolicy:
ReadWriteOnceWithFSType`, and an ephemeral volume has no file system type, so
the kubelet does not give the file to the Pod's `fsGroup`. The volume is mounted
in the container that runs Ptah and in no other, and every process in that
container runs as the operation's user, so the wider mode lets no process read
the file that the mode of a Secret volume's file keeps out. Root on the node can
read either.

### Rotation and generated users {#secret-provider-class-rotation}

The provider reads the store when the Pod starts, so an operation that starts
after the store moved reads the new value, with no lag behind a Secret a tool
writes. In the run, a Pod created in the second the password changed in Vault
and in the database planned with the new one. The runner reads the file once,
before it starts Ptah, so a running operation keeps the URL it started with,
and [When the credential rotates](#external-secret-stores-rotation) holds for
it unchanged: keep the user name, change the password, and rotate with no Apply
running. With the driver's `enableSecretRotation` set, a running Pod's file
took the new value 73 seconds after the store did, and nothing in the Pod read
it again.

A Vault static role (`database/static-roles`) keeps the user name and rotates
the password, which is that rotation. A user Vault's database secrets engine
generates for each read cannot work, whichever way the URL arrives. Every
mount reads the store again: in the run, two Pods that mounted in the same
second got two users, and each republish of a running Pod made another. A plan
binds the identity of its target, the user name included, so a Plan and its
Apply always run as different users, and the runner refuses the Apply:
`database target identity changed after planning`. The engine also writes the
user name and the password as two files, and no URL.

### What was run {#secret-provider-class-measured}

The behavior in this section comes from one run, before the field existed, of
the Plan Job the manager builds with only its URL volume replaced by the class:

| Component | Version |
| --- | --- |
| Operator | the development tree after 0.2.18, with Ptah `v0.35.0` |
| Kubernetes | `v1.36.4`, on kind `v0.33.0` |
| Target | PostgreSQL `17.11` |
| Secrets Store CSI driver | `v1.6.2`, chart `1.6.2` |
| Store | Vault `2.0.4` in dev mode, chart `0.34.1`, `vault-csi-provider` `1.7.4`, KV version 2 |

The runner read the file and Ptah planned. The password appeared in none of the
Pod's logs, the Pod, the Job or the namespace's Events. Pod Security
`restricted` admits the volume, and the Pod's volume came back as the Job's
template wrote it, which is what [Pod admission](#admission) compares. Nothing
else was run: no cloud secret manager, no Vault outside dev mode, no MySQL
target, and no other Kubernetes minor.

## Credentials from an external secret store {#external-secret-stores}

With `urlFrom`, the operator reads a database URL from the key of a Kubernetes
Secret in the resource's namespace. A tool that writes that Secret needs nothing
from the operator, and the operator cannot tell which tool wrote it. The routing
above does not change: the manager still cannot read the Secret, and the kubelet
projects its key into [the database URL file](#credential-files) of the
container that runs Ptah. [A SecretProviderClass](#secret-provider-class) is the
other path, with no Secret in between.

The behavior in this section comes from one run against these versions, not from
reading the code. In that run no password appeared in status, Events, plans,
result records, Jobs, Pods, the manager's logs or the logs of the operation Pods
still there when the run looked.

| Component | Version |
| --- | --- |
| Operator | `v0.2.9`, with Ptah `v0.17.0` |
| Kubernetes | `v1.37.0`, on kind `v0.33.0` |
| Target | PostgreSQL `17.11`, from the image [Databases and privileges](../../support/databases/) pins |
| Store | Vault `2.0.4` in dev mode, chart `0.34.1`, KV version 2 |
| External Secrets Operator | `v2.14.0`, chart `2.14.0` |
| Vault Secrets Operator | `1.6.2`, chart `1.6.2` |

Nothing else was run: no later operator version, no cloud secret manager, no store
other than Vault, no Vault outside dev mode, no database secrets engine or other
source of generated users, and no MySQL target. The messages quoted below are
v0.2.9's. Where v0.2.9
says `child_exit: ptah exited with code 2`, later versions name the failure, as
[A database the operation could not open](../../troubleshoot/condition-reasons/#a-database-the-operation-could-not-open)
lists.

### A tested example {#external-secret-stores-example}

External Secrets Operator took the fewest objects: a `SecretStore` and an
`ExternalSecret`, against three for the Vault Secrets Operator. The commands and
manifests below are the ones the run applied, with the operator and External
Secrets Operator installed and Vault at `vault.vault.svc`.

Put the URL in the store as one field. It has to be a complete URL, with any
reserved character in the password percent-encoded and no trailing newline.
Reading it from a file keeps it out of the command line and the shell history:

```sh
vault kv put -mount=secret ptah/application url=- < "$DATABASE_URL_FILE"
```

Let one ServiceAccount in the namespace read that one path:

```sh
vault auth enable kubernetes
vault write auth/kubernetes/config kubernetes_host=https://kubernetes.default.svc
vault policy write ptah-application - <<'POLICY'
path "secret/data/ptah/application" {
  capabilities = ["read"]
}
POLICY
vault write auth/kubernetes/role/ptah-application \
  bound_service_account_names=vault-reader \
  bound_service_account_namespaces=application \
  policies=ptah-application audience=vault ttl=20m
kubectl create namespace application
kubectl -n application create serviceaccount vault-reader
```

`vault-reader` belongs to the tool. Do not name it in
`spec.execution.serviceAccountName`: an operation Pod needs no credential beyond
the URL.

Apply the store and the Secret to write:

```yaml
apiVersion: external-secrets.io/v1
kind: SecretStore
metadata:
  name: vault
  namespace: application
spec:
  provider:
    vault:
      server: http://vault.vault.svc:8200
      path: secret
      version: v2
      auth:
        kubernetes:
          mountPath: kubernetes
          role: ptah-application
          serviceAccountRef:
            name: vault-reader
            audiences: [vault]
```

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: application-database
  namespace: application
spec:
  refreshInterval: 1m
  secretStoreRef:
    name: vault
    kind: SecretStore
  target:
    name: application-database
    creationPolicy: Owner
  data:
    - secretKey: url
      remoteRef:
        key: ptah/application
        property: url
```

Wait for the Secret, then apply a resource that reads it. This one takes its
schema from a ConfigMap, so it needs no registry:

```sh
kubectl -n application wait --for=condition=Ready \
  externalsecret/application-database --timeout=2m
kubectl -n application create configmap application-schema \
  --from-file=schema.sql=examples/configmap-kustomize/db/schema.sql
```

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchema
metadata:
  name: application
  namespace: application
spec:
  target:
    engine: PostgreSQL
    coordinationKey: production/application-primary
    urlFrom:
      name: application-database
      key: url
  desired:
    configMap:
      name: application-schema
      items:
        - key: schema.sql
          path: schema.sql
  policy:
    apply: OnApproval
```

From there it is [First schema](../../start/first-schema/#it-stops-and-waits-for-you):
the resource reaches `AwaitingApproval`, `kubectl ptah approve` records the
decision and the plan runs. A `PtahMigration` names the Secret the same way, in
its own `spec.target.urlFrom`. In the run one read this same Secret, on the same
database, with `sharedRealm: true` on both resources and the migration's tables in
the schema's `spec.policy.exclude`.

### What each tool writes {#external-secret-stores-tools}

The operator read the Secret from either tool and behaved the same. The tools
differ in what they leave around it:

| | External Secrets Operator | Vault Secrets Operator |
| --- | --- | --- |
| Objects beside the Secret | `SecretStore`, `ExternalSecret` | `VaultConnection`, `VaultAuth`, `VaultStaticSecret` |
| Keys in the Secret | The ones you map: `url` | Every field at the path, plus `_raw`, which holds the whole response and so a second copy of the URL |
| Owner reference | The `ExternalSecret`, as controller | The `VaultStaticSecret`, not as controller |
| Added metadata | A `managed` and a `created-by` label, and a `data-hash` annotation | Four labels, no annotation |
| A changed value | Written in place: same Secret UID | Written in place: same Secret UID |
| The Secret deleted by hand | Written again within a second, under a new UID | Written again within a second, under a new UID |
| The source object deleted | The Secret goes with it | The Secret goes with it |

A new Secret UID changes nothing for the operator, which does not read the
Secret: the plan that was waiting stayed current and ran. Neither tool changes the
password in the database. Rotating it there is your step, or the store's.

The Secrets Store CSI driver can write a Secret too: a class's `secretObjects`
copies what it mounted into a Secret that `urlFrom` can name. A `PtahSchema`
read one and reached `InSync` with the versions
[What was run](#secret-provider-class-measured) lists. The operator does not
prefer this path, and [`urlFromSecretProviderClass`](#secret-provider-class)
replaces it:

- The driver writes the Secret only while some Pod mounts the class. With no
  such Pod, a read stops at `PodConfigurationError` with `secret "<name>" not
  found`, so the path needs a Deployment that mounts the volume and does nothing
  else, and the Secret belongs to that Deployment's ReplicaSet.
- The Secret holds what the last Pod the driver reconciled had mounted, so it
  can move backwards. After a rotation the Secret took the new value from a Pod
  that had just mounted the class, the old value again 44 seconds later from
  the holder's files, and the new value 28 seconds after that. An operation that
  started in between read the old password.
- With the chart's `syncSecret.enabled`, which the path needs, the driver's
  ServiceAccount could `get` and `create` every Secret in the cluster.

If the store holds a username and a password rather than a URL,
`target.template` can build the URL. `urlquery` escapes what a password can hold;
a password holding `@`, `/`, `:`, `%` and `+` connected through this template:

```yaml
target:
  name: application-database
  creationPolicy: Owner
  template:
    engineVersion: v2
    data:
      url: 'postgres://{{ .username | urlquery }}:{{ .password | urlquery }}@db.example:5432/application'
```

A value the operator cannot use is reported when the operation that reads it runs:

| What the Secret holds | What the run reported |
| --- | --- |
| No key `url` | The operation waits, as below, with `references non-existent secret key: url` |
| A `/` in the password, not escaped | `Failed`, `OperationFailed`: `invalid_target: database URL is invalid` |
| A newline after the URL | The same |
| A host outside printable ASCII, such as `db.İnternal` or `db.%C4%B0nternal` | `Failed`, `OperationFailed`: `invalid_target: the database URL: the host "db.\u0130nternal" is not printable ASCII: write an internationalized name in its xn-- form` |

### Before the Secret exists {#external-secret-stores-not-synced}

Created before the tool has written the Secret, a `PtahSchema` stands in `Observing`
with `Ready=False`, reason `PodConfigurationError`, and `ReconciliationFailed=False`.
A `PtahMigration` stands in `Reading` with `Progressing=False` and the same reason.
The database URL is [a file in a Secret volume](#credential-files), so the kubelet
cannot set that volume up and starts none of the Pod's containers. It says why in
a `FailedMount` Event against the Pod, and the message carries those words:

```text
Job <the Job> cannot start its Pod: MountVolume.SetUp failed for volume "database-url" : secret "application-database" not found
```

A Secret that exists without the key reads `MountVolume.SetUp failed for volume
"database-url" : references non-existent secret key: url (volume database-url
reads Secret <namespace>/application-database)`. The kubelet's words name the key
and not the Secret, so the operator adds the volume and the Secret it reads, from
the Pod. A missing registry, Git or HTTPS grant still stops the container that reads it,
and the message carries what the kubelet wrote on that container, such as
`couldn't find key registry in Secret <namespace>/<name>`. Before 0.2.11 the
database URL was a variable too, so the message was the container's
`secret "application-database" not found` or `couldn't find key url in Secret
<namespace>/application-database`.

A URL volume from [a SecretProviderClass](#secret-provider-class) stops the Pod
the same way when the class is missing, with the driver's words:
`MountVolume.SetUp failed for volume "database-url" : rpc error: code = Unknown
desc = failed to get secretproviderclass <namespace>/orders-database, error:
SecretProviderClass.secrets-store.csi.x-k8s.io "orders-database" not found`. It
stops it too when the store refuses the provider's read. The provider's words
then name what it read in the store and not the class, so the operator adds
`(volume database-url reads SecretProviderClass <namespace>/orders-database)`.

A `PodConfigurationError` Warning Event says the same. Those words hold the names
of the Secret and the key and nothing the Secret holds. The resource reports this
within 30 seconds of the Pod reaching that state, which is how often it looks at a
running read-only operation. When the Secret appears, the kubelet sets the volume
up on its next attempt, and it spaces attempts out to at most about two minutes
apart, so the operation goes on within that time, and nobody has to touch
anything.

The Job still has its `spec.execution.activeDeadlineSeconds`. Past that, a
`PtahSchema` read reports `Failed`, `OperationFailed`: `read Observe result: ptah
runner result frame not found; last reported before that:` followed by what it
reported before, and tries again after `spec.execution.failureRetryInterval`. An
Apply in the same position, approved while the Secret was gone, reports
`ApplyOutcomeUnknown` with `apply result is uncertain: ptah runner result frame not
found; last reported before that:` and the same words, though its container never
started. It observes the database, publishes the same plan again and asks for a new
approval. An Apply whose Secret returned before the deadline went on under the
approval it had. A `PtahMigration` Apply in that position is a run nobody
accounted for: `status.lastRun` records outcome `Unknown`, and its message keeps
`read the Apply result: ptah runner result frame not found; last reported before
that:` and the same words after the Pod and its Job are gone. The resource stops
at `Blocked`, `ApplyOutcomeUnknown`, with the same message until the next history
reading rewrites it.

An Apply that ends another way before anything reads its result keeps the same
words after the sentence that says how it ended. Pointing `urlFrom` at the Secret
that exists is one: it changes the inputs the Apply was decided from, and the Job
keeps the Pod it made, which still names the old Secret. Past the deadline a
`PtahMigration` records `the operation inputs changed while the Job was running;
last reported before that:` and the words in `status.lastRun`. A `PtahMigration`
Apply keeps them the same way when its Job was deleted (`the dispatched Apply Job
is missing and will not be recreated`), replaced by another Job under the same name
(`the dispatched Apply Job was replaced`) or left without its owner reference (`the
dispatched Apply Job lost its owner`), when a release changed an execution
component under it and the Apply could not be read under the binding it was
dispatched under (`an execution component changed while the Apply was
dispatched`), and when its database Lease changed hands (`the database lock epoch
changed under the dispatched run`). A `PtahMigration` deleted while its Apply
waited takes its status with it, and its `MigrationRunUncertain` Warning Event
carries `the PtahMigration was deleted while a dispatched Apply was in flight; last
reported before that:` and the words.

An Apply of either kind keeps them too when its Job stopped being its claim's
after the inputs moved, as when someone relabeled it (`dispatched Apply Job is not
its claim's`), when its claim could not build its Job again, as when its plan was
deleted (`rebuild immutable Apply Job intent`), when a newer release builds the Job
differently (`dispatched Apply Job intent changed`), when its Job left more
than one Pod, as when a Pod stands Terminating on a node that stopped answering
(`terminal Job has multiple executor Pods` on a `PtahMigration`, `Job produced
multiple executor Pods` on a `PtahSchema`), and when the Pod its Job left does
not match the Pod its claim built (`terminal Job pod does not match immutable
intent`).

A `PtahSchema` keeps the words in the message of `ReconciliationFailed`, reason
`ApplyOutcomeUnknown`, until the next result rewrites it, and in its
`ApplyOutcomeUnknown` Warning Event: `Apply outcome is uncertain; observing
database state; last reported before that:` and the words. Its other sentences are
`operation inputs changed while the Job was running`, `dispatched Apply Job is
missing and will not be recreated`, `dispatched Apply Job was replaced`,
`dispatched Apply Job lost schema ownership`, `execution binding changed after
Apply dispatch` and `database lock continuity was lost during Apply`, each followed
by `; last reported before that:` and the words. The write that records a lost
Lease leaves an Apply's `Ready` as it was while `Ready` holds the words, and says
the lock changed in `InSync` and in a `LeaseContinuityLost` Event. A read that
loses its Lease does not keep them: nothing looks at its Pod again before its Job
stops, so `Ready` says the lock changed. A `PtahSchema` deleted while its Apply
runs waits for it, and records `dispatched Apply Job identity was lost during
deletion` when its Job went, `database lock continuity was lost during Apply` when
its database Lease changed hands while it waited, `Apply executor ended and its
result was not read during deletion` when the Job stopped and the Job's one Pod
showed its executor run and stop, and `Apply executor termination is not proven
during deletion` when the Job stopped otherwise. A container terminated with
reason `ContainerStatusUnknown` shows neither a run nor a stop: the kubelet gives
that reason to a container that was still waiting when it stopped the Pod, as it
does at the Job's deadline to one blocked on a missing Secret, and to one the
container runtime stopped reporting while it ran. The status goes with the
resource, and the `ApplyOutcomeUnknown` Event is what stays: `Apply
outcome is uncertain; the PtahSchema is being deleted, so nothing will observe the
database:` and the sentence, followed by `; last reported before that:` and the
words unless a Pod the ending listed showed its executor start. A live
`PtahSchema` reads such a container the same way, and reads the database only
once the Apply's execution deadline and its grace period have passed. With
`resultDelivery.enabled=false` it asks for no log and takes the log as gone, so
it records `apply result is uncertain: the result log is gone: the kubelet ended
container "ptah" of Pod <pod> with ContainerStatusUnknown and serves no log for
it` and the words.

Those words are the last report and not a reading taken at the end. Nothing is
added when the claim's own Job reports a Pod that succeeded, and a refusal is not
added once that Job counts a Pod in any state, since the API server created one
after all. On the endings for more than one Pod, for a Pod that does not match
its claim and for a lost database Lease, on a `PtahSchema`'s endings for a
missing Job, for inputs that changed while the Job ran, for a changed execution
component and for an Apply result that was not read, on a `PtahMigration`
Apply whose result was not read, and on every ending of a deleted `PtahSchema`,
nothing is added when a Pod the ending read shows its executor running or
terminated, as a Pod that got its Secret and then lost its node does while its
replacement waits; `ContainerStatusUnknown` shows neither. On a read of either
kind a Pod can still start after the last look and fail without a result, and
the failure reads the same. The
resource looks at an Apply's Pod every 5 seconds, also while a deleted resource
waits for it. Some endings are recorded while the Pod can still start, and a Pod
that starts later does not change the record. A `PtahMigration` records the run
and keeps its claim until the Pod stops when its Job went while the Pod stood, when
an execution component changed and the binding stopped waiting for the run, when
its Lease changed hands, and on any other
ending above that finds its Job running or its Pods standing. A
`PtahSchema` records every ending at once and reads the database again once the
Apply's deadline has passed. So check whether the Secret exists now before you
change it.

### When the credential rotates {#external-secret-stores-rotation}

The runner reads [the database URL file](#credential-files) once, when the
operation container starts, before it starts Ptah. The kubelet refreshes a Secret
volume's files while the Pod runs, and nothing reads them again. A rotated Secret
reaches the next operation and never a running one. A running
operation keeps the old URL for as long as it lives, and PostgreSQL checks a
password only when a connection opens, so what a rotation does to it depends on
whether it opens another one.

In the rows below, "rotated" means the password changed in the store and in the
database, on the same role, while `ALTER ROLE` left open sessions running. PostgreSQL
keeps one password per role, so the old one stops working for new connections at
that moment.

While a plan waits for approval, on a `PtahSchema` unless the row says otherwise:

| What changed | What the resource did |
| --- | --- |
| Rotated | Nothing. The plan kept its name, UID and fingerprint, also across a scheduled re-read. An approval created afterwards bound it, the Apply used the new password, and the schema reached `InSync`. A `PtahMigration`'s plan also stayed the same across a requested read, and ran to `InSync` |
| The Secret and the database disagree, because one moved first | Nothing until a read. The read fails: `Failed`, `OperationFailed`, `child_exit: ptah exited with code 2`, tried again after `failureRetryInterval`. `status.plan` and `ApprovalRequired=True` stay, while admission refuses an approval with `referenced schema is not awaiting approval`, and `kubectl ptah approve` waits five minutes and approves nothing. Once the two agree, the same plan under the same fingerprint is waiting again, and an approval binds it |
| The same, approved before a read noticed | The Apply reported `ApplyOutcomeUnknown` and ran no statement, and the approval was consumed. Reads failed until the two agreed. Then the same plan under the same fingerprint was waiting, and a new approval ran it to `InSync` |
| A new username | The username is part of the plan's target identity. A scheduled re-read publishes a plan with a new fingerprint, a `PlanStale` Warning names both plans, and an approval of the new plan ran it to `InSync`. An Apply approved before that read is refused by the runner, `database target identity changed after planning` in the Pod's log, and runs no statement. The resource reads `ApplyOutcomeUnknown`, then stops as the row for a new username below describes. With the first username back in the store, the read passed and the same plan under the same fingerprint was waiting for a new approval. A `PtahMigration` in the same position records `Unknown` and `Blocked`, naming `target_binding_mismatch: database target identity changed after planning`, and ran no migration. A `PtahMigrationRunAcknowledgment` settled it, and a plan under the new username, with its own approval, reached `InSync` |
| `GRANT role TO role` on any role of the server | See the paragraph after the next table |

While an Apply runs:

| What happened | `PtahSchema`, one `CREATE INDEX` on 15 million rows | `PtahMigration`, a 75-second `pg_sleep` in the second of three files |
| --- | --- | --- |
| Rotated | The statement finished, the post-Apply read used the new password, `InSync` | `Failed` at the next file, on the password the Pod still held: `Migration 3 (Add C) failed and committed nothing; 2 migrations before it are recorded applied`. The history read found one pending and a new plan waited for a new approval, which ran it to `InSync`. With the sleep in the only file, the run ended `Applied` and `InSync` |
| Rotated, and the sessions killed with `pg_terminate_backend` | `ApplyOutcomeUnknown`, no index. The read-only observation republished the same plan under the same fingerprint, for a new approval | `Unknown`, SQLSTATE `57P01`, `Blocked`: Ptah could not reconnect to record what happened. The revision stayed dirty. It took a `PtahMigrationRunAcknowledgment`, then `ptah migrations repair --force`, then a new approval to reach `InSync`. That recorded the file applied without running it, which cost nothing only because this file did nothing but sleep; [a file with real statements is retried instead](../../use/recovery/#failed-statement) |
| The store moved, the database kept the old password | The statement finished. The post-Apply read failed and retried, with `Failed`, `OperationFailed`, until the database took the password, then `InSync` | `Applied`, every file, on the password the Pod held. The history read failed and retried until the database took the password, then `InSync` |
| A new username | The statement finished, the index exists, and the journal says `ApplyFinished`, `Succeeded`. The post-Apply read is bound to the plan's identity, and the runner refuses it before it connects under the new one: `Blocked`, `TargetIdentityChanged`, trying the read again at `failureRetryInterval`. It does not end by itself, and [a person ends it](../../use/recovery/#a-schema-whose-target-moved): with the first username back the proof finishes, and settling it starts again under the new one, with no Apply recorded and, under `apply: Always`, no approval for the next plan unless it is destructive or changes privileges | `Applied`, every file, then `InSync`: a history read is not bound to the plan the run came from |

A plan also records a fingerprint of the database as Ptah found it, and on
PostgreSQL that fingerprint covers the role memberships of the whole server:
`pg_auth_members` is not per database. In the run, `CREATE ROLE`, `CREATE DATABASE`,
`GRANT ALL ON DATABASE`, `ALTER ROLE ... PASSWORD`, `... VALID UNTIL` and `... SET`
left a waiting plan alone. `GRANT unrelated_group TO unrelated`, on two roles with
no tie to the plan's database, replaced it: the next read published a plan with a
new fingerprint, and `REVOKE` brought the first one back.
An Apply approved after the `GRANT` and before that read reported
`ApplyOutcomeUnknown` and ran no statement, and the resource went back to
`AwaitingApproval` with the new plan. The reason is only in the Pod's log, `database
schema no longer matches the approved plan`, and the Job is deleted 24 hours
later. A rotation that creates a user and grants it a group role moves that
fingerprint for every plan waiting on the server, and so would a store that generates
users that way. No such store was run.

So, for a rotation on this path:

- Keep the username, and change the password.
- Rotate with no Apply running. `kubectl get ptahschema,ptahmigration` prints the
  phase, and `status.activeOperation.type` says which operation is out.
- Keep the store's refresh interval short. The disagreement between the Secret and
  the database lasts that long: a read recovers from it by itself, and an Apply
  caught in it costs another approval.
- Do not kill the sessions of a running migration, and do not move a resource to a
  new username while it has an approved plan. Those are the rows a person has to
  clean up after, and [a schema whose target
  moved](../../use/recovery/#a-schema-whose-target-moved) says how for a schema.

## Admission

The manager's own writes pass through two independent fail-closed layers.

Both layers judge the URL volumes. The Job policy admits a
`database-url`, `development-database-url` or `shadow-database-url` volume of
two sources only: a Secret, or a read-only CSI volume of the driver
`secrets-store.csi.k8s.io` with no file system type, no publish Secret, and the
class's name as its one attribute. The controller-write webhook then rebuilds
the Job from the claim, so the class is the one the claim bound, and the
Pod-intent webhook holds each Pod to the volumes its template declares.

**Typed policies**, one per kind and verb, reject objects outside their narrow
structural form: Jobs, plan chunks, the ConfigMaps the manager creates -- a
plan projection an Apply mounts or a source snapshot -- schema plans and
migration plans, the deletion of a ConfigMap that is not a source snapshot,
plus a seventh policy over `PtahSchema` and `PtahMigration` updates.
One policy per kind avoids cross-type CEL assumptions. The Job policy admits a
Job that satisfies the schema shape or the migration shape and nothing else.
The policies are ordinary release objects, and an upgrade updates them in
place. The Job, schema plan and migration plan policies carry the release's
manager image and controller-state version as literals, so a Job or a plan
stamped by another manager release is refused. The chunk, projection and
snapshot policies carry neither: a chunk and its projection are bound to their
plan by name and owner, and the plan names the manager; a snapshot is bound to
its resource, and the controller-write webhook rebuilds its bytes from the
source. Nor does the policy over `PtahSchema` and `PtahMigration`
updates, which judges resources a person owns and no manager stamps.

**Webhooks** perform the stronger semantic check by reading the owning resource
and plan and reconstructing the exact expected object. Eight ship:
`mapproval`, `mmigrationapproval` and `mmigrationrunacknowledgment` on the
mutating side; `vapproval`, `vmigrationapproval`,
`vmigrationrunacknowledgment`, `vpodintent` and `vcontrollerwrite` on the
validating side. The two acknowledgment entries stamp and check the person who
settles a run a migration recorded as unresolved, the way the approval entries
stamp an approver.

**The manager's state** is guarded against everyone else by two more policies.
The first refuses a write to the `status` subresource of any kind in the
`operator.ptah.run` group from anyone but the manager's ServiceAccount, a
cluster administrator included. It names the subresource rather than the
kinds, so a kind added later is covered by the rule already there. Status is
what the controller and the Pod-intent and controller-write webhooks decide
from: the claims, the admission snapshots, the record of a run nobody
accounted for. The second refuses anyone but the manager a change to the
`operator.ptah.run/unresolved-run` annotation of an existing `PtahMigration`,
the copy of that record a restore that drops status keeps. The policy over
`PtahSchema` and `PtahMigration` updates lets the manager move that one
annotation on a migration, and nothing else it did not move before.

**A deletion that would orphan dependents** of a `PtahSchema` or a
`PtahMigration` is refused by one more policy, from everyone. The garbage
collector would have to take the resource's owner reference off the
`PtahResultRecord` objects it owns, the controller-write webhook refuses that
change to a record whoever makes it, and the resource would stay `Terminating`.
The policy decides the way the API server's registry does: `orphanDependents`
when the request sets it, then `propagationPolicy`, and for a request that names
neither, the `orphan` finalizer the resource carries. A deletion that names
`Background` or `Foreground` is admitted, and the API server takes that
finalizer off in the same write. It is a ValidatingAdmissionPolicy and not a
webhook because it needs nothing outside the request: behind a webhook under
`failurePolicy: Fail`, every deletion of the two kinds, a namespace's included,
would fail while the manager is down. The webhook already refuses an orphaning
deletion of a record and of a result credential Secret, which it judges for
other reasons. [Suspension and
deletion](../../use/operations/#suspension-and-deletion) says what to run
instead.

**A deletion that would orphan an operation Job's Pods** is refused by a policy
of its own, from everyone. The garbage collector would have to take the Job's
owner reference off each Pod, the Pod-intent webhook refuses that change, and
the Job would stay `Terminating`. The policy selects a Job whose Pod template,
or whose own metadata, carries the two labels the manager writes on both,
`app.kubernetes.io/managed-by: ptah-operator` and `app.kubernetes.io/component`
`schema-operation` or `migration-operation`, which `spec.execution.podMetadata`
cannot declare. The template is what the Pods were made from, and the API
server refuses a change to it on a Job that is not suspended, so a label taken
off the Job does not take the Job out of scope. A suspended Job's template can
change before the Job starts, and from Kubernetes 1.36, where the
`MutableSchedulingDirectivesForSuspendedJobs` feature gate is on by default,
also after it, once no Pod is active. A Job suspended and stripped in both
places is not judged, but its Pods keep the labels they were made with, by
which the Pod-intent webhook keeps them owned, so an orphaning deletion of it
waits until they have terminated. No other Job is judged. The
Pod-intent webhook decides by the same identity whether a Pod that does not
carry it may lose its Job owner, reading the Job's template and the Job, so a
Job whose name only starts like an operation Job's is neither judged here nor
kept owning its Pods there. The policy decides as the registry does for
`batch/v1`, whose default is to orphan:
`orphanDependents`, then `propagationPolicy`, then the `foregroundDeletion`
finalizer the Job carries, and otherwise a deletion that names no propagation
is refused. It is a policy apart from the one above because the two select
different objects and decide a deletion that names nothing differently: a
`PtahSchema` or `PtahMigration` defaults to `Background`.

A Job carries an annotation envelope that admission checks as a set: eight
annotations, including the manager image, revision and controller-state
version, and two more on a schema Apply for the plan fingerprint and content
digest. A Job without the controller identity is refused on create and on
update.

The manager annotations record which manager built a Job, and bind nothing. A
manager release that keeps the execution binding adopts a Job its predecessor
dispatched, in either family, when it builds the same Job apart from that
identity: it takes the identity from the live Pod template and holds the
template to the admission snapshot the claim persisted before dispatch, so
what it takes from the Job is pinned by the claim rather than by the Job being
checked. A release that changed anything else in the Job cannot confirm it,
and settles a dispatched Apply as outcome unknown.

Upgrade compatibility is deliberately narrower than ordinary reconstruction.
After an upgrade durably retires an execution epoch, the replacement manager
may add only the cleanup TTL to an exact terminal Job retained from the
supported predecessor, and only when the operation name, UID, owner, envelope,
admission snapshot and retirement conditions all match. A dispatched
predecessor Apply is handled only from persisted outcome-unknown evidence: the
replacement waits for every exact-owner Pod and the Job's terminal condition,
then adds only the TTL. It never reads, replays or certifies the retired
result; fresh read-only observation stays mandatory.
